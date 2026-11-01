package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	testToken   = "tok-secret.abc.def"
	selfUser    = "1000000000000000001"
	aliceUser   = "1000000000000000002"
	guildID     = "1100000000000000001"
	chanGeneral = "1100000000000000002"
	chanRandom  = "1100000000000000003"
	threadID    = "1100000000000000004"
	dmID        = "1100000000000000005"
	otherChan   = "1100000000000000009"
	msg1        = "1300000000000000001"
	msg2        = "1300000000000000002"
	msg3        = "1300000000000000003"
	msg4        = "1300000000000000004"
	secretBody  = "rig build log"
)

type hit struct {
	method, path string
	query        url.Values
	header       http.Header
	at           time.Time
}

// Handlers get n, the 0-based count of earlier requests to the same path.
type fakeDiscord struct {
	clock    *fakeClock
	mu       sync.Mutex
	hits     []hit
	handlers map[string]func(w http.ResponseWriter, r *http.Request, n int)
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	n := 0
	for _, h := range f.hits {
		if h.path == r.URL.Path {
			n++
		}
	}
	f.hits = append(f.hits, hit{method: r.Method, path: r.URL.Path, query: r.URL.Query(), header: r.Header.Clone(), at: f.clock.Now()})
	h := f.handlers[r.URL.Path]
	f.mu.Unlock()
	if h == nil {
		connector.WriteJSON(w, http.StatusNotFound, map[string]any{"message": "404: Not Found", "code": 0})
		return
	}
	h(w, r, n)
}

func (f *fakeDiscord) handle(path string, h func(w http.ResponseWriter, r *http.Request, n int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[path] = h
}

func (f *fakeDiscord) reply(path string, status int, body any) {
	f.handle(path, func(w http.ResponseWriter, _ *http.Request, _ int) { connector.WriteJSON(w, status, body) })
}

func (f *fakeDiscord) all() []hit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hit(nil), f.hits...)
}

func (f *fakeDiscord) paths() []string {
	var ps []string
	for _, h := range f.all() {
		ps = append(ps, h.path)
	}
	return ps
}

func (f *fakeDiscord) to(path string) []hit {
	var hs []hit
	for _, h := range f.all() {
		if h.path == path {
			hs = append(hs, h)
		}
	}
	return hs
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t         *testing.T
	clock     *fakeClock
	discord   *fakeDiscord
	gw        *fakeGateway
	startup   []hit // what start sent before the gateway connected
	srv       *httptest.Server
	st        *store.Store
	statePath string
	token     connector.Secret
	socket    string
	logs      *syncBuffer
	cancel    context.CancelFunc
	done      chan error
	client    *connector.Client
	w         *worker
	// keys and seen, if set before start, let the server execute approvals.
	keys *approval.Keys
	seen *approval.Seen
}

func api(p string) string { return "/api/v9" + p }

var (
	guildsBody   = []map[string]any{{"id": guildID, "name": "Club", "icon": nil, "owner": false, "permissions": "0"}}
	channelsBody = []map[string]any{
		{"id": "1100000000000000010", "type": 4, "guild_id": guildID, "name": "Text", "position": 0},
		{
			"id": chanRandom, "type": 0, "guild_id": guildID, "name": "random", "position": 2, "parent_id": "1100000000000000010",
			"last_message_id": snowflakeAt(noon.Add(-48 * time.Hour)),
		},
		{
			"id": chanGeneral, "type": 0, "guild_id": guildID, "name": "general", "position": 1, "parent_id": "1100000000000000010",
			"last_message_id": snowflakeAt(noon.Add(-time.Hour)),
		},
	}
	dmsBody = []map[string]any{{
		"id": dmID, "type": 1, "last_message_id": msg4,
		"recipients": []map[string]any{{"id": aliceUser, "username": "alice", "global_name": "Alice A"}},
	}}
	searchBody = map[string]any{
		"total_results": 2,
		"messages": [][]map[string]any{
			{{
				"id": msg1, "channel_id": chanGeneral, "type": 0, "content": secretBody, "timestamp": "2026-10-28T10:00:00.000000+00:00",
				"author": map[string]any{"id": aliceUser, "username": "alice", "global_name": "Alice A"},
			}},
			{{
				"id": msg2, "channel_id": threadID, "type": 0, "content": "rig parts", "timestamp": "2026-10-28T11:00:00.000000+00:00",
				"author": map[string]any{"id": selfUser, "username": "me"},
			}},
		},
		"threads": []map[string]any{{"id": threadID, "type": 11, "guild_id": guildID, "parent_id": chanGeneral, "name": "rig thread"}},
	}
	dmMessagesBody = []map[string]any{
		{
			"id": msg4, "channel_id": dmID, "type": 0, "content": "see you", "timestamp": "2026-10-28T12:00:00.000000+00:00",
			"author": map[string]any{"id": selfUser, "username": "me"},
		},
		{
			"id": msg3, "channel_id": dmID, "type": 3, "content": "", "timestamp": "2026-10-28T11:59:00.000000+00:00",
			"author": map[string]any{"id": aliceUser, "username": "alice"},
		},
	}
)

func newHarness(t *testing.T, seed func(*state)) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:         t,
		clock:     &fakeClock{now: noon},
		statePath: filepath.Join(dir, "private", "state.json"),
		token:     (&connector.Env{StateDir: dir}).Secret("token"),
		socket:    filepath.Join(dir, "s.sock"),
		logs:      &syncBuffer{},
	}
	if err := h.token.Store([]byte(testToken)); err != nil {
		t.Fatal(err)
	}
	h.discord = &fakeDiscord{clock: h.clock, handlers: make(map[string]func(http.ResponseWriter, *http.Request, int))}
	h.discord.reply(api("/users/@me"), 200, map[string]any{"id": selfUser, "username": "me"})
	h.discord.reply(api("/users/@me/guilds"), 200, guildsBody)
	h.discord.reply(api("/users/@me/channels"), 200, dmsBody)
	h.discord.reply(api("/guilds/"+guildID+"/channels"), 200, channelsBody)
	h.discord.reply(api("/guilds/"+guildID+"/messages/search"), 200, searchBody)
	h.discord.reply(api("/channels/"+dmID+"/messages"), 200, dmMessagesBody)
	h.srv = httptest.NewServer(h.discord)
	t.Cleanup(h.srv.Close)
	h.gw = newFakeGateway(t)

	st, err := store.Open(context.Background(), filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st

	s := &state{BuildNumber: defaultBuildNumber, BuildCheckedAt: noon, InstallationID: testInstallation}
	if seed != nil {
		seed(s)
	}
	if err := s.save(h.statePath); err != nil {
		t.Fatal(err)
	}
	h.client = connector.Dial(h.socket, 10*time.Second)
	t.Cleanup(func() {
		if h.cancel != nil {
			h.stop()
		}
		for _, x := range h.discord.all() {
			write := x.method == http.MethodPost && strings.HasPrefix(x.path, api("/channels/")) &&
				(strings.HasSuffix(x.path, "/messages") || strings.HasSuffix(x.path, "/typing"))
			if x.method != http.MethodGet && !write {
				t.Errorf("upstream %s %s: only GET, typing and sending a message are allowed", x.method, x.path)
			}
		}
		logs := h.logs.String()
		for _, secret := range []string{testToken, secretBody, "see you"} {
			if strings.Contains(logs, secret) {
				t.Errorf("logs contain %q", secret)
			}
		}
	})
	return h
}

func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	h.done = make(chan error, 1)
	mark := len(h.discord.all())
	cfg := config{
		store:       h.st,
		statePath:   h.statePath,
		token:       h.token,
		timezone:    "America/New_York",
		transport:   &fhttp.Client{},
		apiBase:     h.srv.URL + "/api/v9",
		webBase:     h.srv.URL,
		gatewayURL:  h.gw.url,
		dialGateway: testDial,
		clock:       h.clock,
		rng:         fixedRand(0.5),
		log:         slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		fail:        cancel,
	}
	w, err := start(ctx, cfg)
	if err != nil {
		cancel(nil)
		h.t.Fatal(err)
	}
	h.cancel = func() { cancel(nil) }
	h.w = w
	go func() {
		srv := &connector.Server{Connector: w, Keys: h.keys, Seen: h.seen, Log: cfg.log}
		h.done <- errors.Join(srv.Serve(ctx, h.socket), w.Close())
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := net.Dial("unix", h.socket); err == nil {
			c.Close()
			break
		}
		select {
		case err := <-h.done:
			h.t.Fatalf("serve exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatal("service did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A linked service is up once it opened the app and its gateway session
	// is; tests count the requests after that.
	eventually(h.t, "the gateway session", func() bool { return w.svc.gw.health() == "" })
	h.discord.mu.Lock()
	h.startup = slices.Clone(h.discord.hits[mark:])
	h.discord.hits = h.discord.hits[:mark]
	h.discord.mu.Unlock()
}

func (h *harness) stop() error {
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatal("serve did not return")
		return nil
	}
}

func (h *harness) restart() {
	h.t.Helper()
	if err := h.stop(); err != nil {
		h.t.Fatalf("stop: %v", err)
	}
	h.start()
}

func (h *harness) read(op string, args map[string]string) (connector.ReadResult, error) {
	h.t.Helper()
	raw, err := h.client.Read(context.Background(), op, args)
	if err != nil {
		return connector.ReadResult{}, err
	}
	var res connector.ReadResult
	if err := json.Unmarshal(raw, &res); err != nil {
		h.t.Fatalf("%s: decode %s: %v", op, raw, err)
	}
	return res, nil
}

func (h *harness) callOK(op string, args map[string]string, out *connector.ReadResult) {
	h.t.Helper()
	res, err := h.read(op, args)
	if err != nil {
		h.t.Fatalf("%s: %v", op, err)
	}
	*out = res
}

func (h *harness) callErr(op string, args map[string]string, status int) *connector.Error {
	h.t.Helper()
	_, err := h.read(op, args)
	var e *connector.Error
	if !errors.As(err, &e) || e.Status != status {
		h.t.Fatalf("%s: got %v, want status %d", op, err, status)
	}
	return e
}

func (h *harness) state() *state {
	h.t.Helper()
	s, err := loadState(h.statePath)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) queryRow(q string, args ...any) []any {
	h.t.Helper()
	rows, err := h.st.DB().Query(q, args...)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !rows.Next() {
		h.t.Fatalf("no row for %s %v", q, args)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		h.t.Fatal(err)
	}
	return vals
}

func superProps(t *testing.T, hd http.Header) map[string]any {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(hd.Get("X-Super-Properties"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSearchUpsertsAndCaches(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	var resp connector.ReadResult
	h.callOK("guild-search", map[string]string{"guild_id": guildID, "content": "rig"}, &resp)
	if strings.Join(resp.MessageIDs, ",") != msg1+","+msg2 || resp.TotalResults != 2 {
		t.Errorf("response %+v", resp)
	}
	wantPaths := []string{
		api("/users/@me/guilds"), api("/guilds/" + guildID + "/channels"), api("/users/@me"),
		api("/guilds/" + guildID + "/messages/search"),
	}
	if got := h.discord.paths(); strings.Join(got, " ") != strings.Join(wantPaths, " ") {
		t.Fatalf("upstream requests\n got %v\nwant %v", got, wantPaths)
	}
	hits := h.discord.all()
	for i := 1; i < len(hits); i++ {
		if gap := hits[i].at.Sub(hits[i-1].at); gap < gapMin {
			t.Errorf("request %d followed the previous after %v", i, gap)
		}
	}
	search := hits[3]
	if q := search.query; q.Get("content") != "rig" || q.Get("limit") != "25" || q.Has("offset") {
		t.Errorf("search query %v", q)
	}
	if got := search.header.Get("Referer"); got != "https://discord.com/channels/"+guildID+"/"+chanGeneral {
		t.Errorf("search referer %q", got)
	}

	// The thread's name comes from the search response; is_from_me from /users/@me.
	if row := h.queryRow(`SELECT kind, name FROM containers WHERE id = ?`, guildID); row[0] != "guild" || row[1] != "Club" {
		t.Errorf("guild row %v", row)
	}
	if row := h.queryRow(`SELECT kind, parent_id, name FROM containers WHERE id = ?`, chanGeneral); row[0] != "channel" || row[1] != guildID || row[2] != "#general" {
		t.Errorf("channel row %v", row)
	}
	if row := h.queryRow(`SELECT kind, parent_id, name FROM containers WHERE id = ?`, threadID); row[0] != "thread" || row[1] != guildID || row[2] != "rig thread" {
		t.Errorf("thread row %v", row)
	}
	if row := h.queryRow(`SELECT container_id, sender_name, is_from_me, body FROM messages WHERE id = ?`, msg1); row[0] != chanGeneral || row[1] != "Alice A" || row[2] != int64(0) || row[3] != secretBody {
		t.Errorf("msg1 row %v", row)
	}
	if row := h.queryRow(`SELECT container_id, is_from_me FROM messages WHERE id = ?`, msg2); row[0] != threadID || row[1] != int64(1) {
		t.Errorf("msg2 row %v", row)
	}

	// An identical search within the hour is answered from the cache.
	var again connector.ReadResult
	h.callOK("guild-search", map[string]string{"guild_id": guildID, "content": "rig"}, &again)
	if n := len(h.discord.all()); n != 4 {
		t.Errorf("cache hit made upstream requests: %d total", n)
	}
	if strings.Join(again.MessageIDs, ",") != strings.Join(resp.MessageIDs, ",") || again.TotalResults != 2 {
		t.Errorf("cached response %+v", again)
	}
	// The next page needs only the search itself; lists and self are cached.
	h.callOK("guild-search", map[string]string{"guild_id": guildID, "content": "rig", "offset": "25"}, &again)
	if got := h.discord.paths(); len(got) != 5 || got[4] != api("/guilds/"+guildID+"/messages/search") || h.discord.all()[4].query.Get("offset") != "25" {
		t.Errorf("paged search requests %v", got)
	}
	// After an hour the cache expires.
	h.clock.Advance(time.Hour)
	h.callOK("guild-search", map[string]string{"guild_id": guildID, "content": "rig"}, &again)
	if n := len(h.discord.to(api("/guilds/" + guildID + "/messages/search"))); n != 3 {
		t.Errorf("search requests after expiry: %d", n)
	}
	if n := len(h.state().Requests); n != 7 { // with the page load at startup
		t.Errorf("budget counted %d requests, want 7", n)
	}
}

func TestSyncCounts(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	var resp connector.ReadResult
	h.callOK("sync", map[string]string{}, &resp)
	if resp.Containers != 2 { // guild + DM
		t.Errorf("sync upserted %d", resp.Containers)
	}
	if row := h.queryRow(`SELECT kind, name, participants_json FROM containers WHERE id = ?`, dmID); row[0] != "dm" || row[1] != "Alice A" ||
		!strings.Contains(fmt.Sprint(row[2]), aliceUser) {
		t.Errorf("dm row %v", row)
	}
	h.callOK("sync", map[string]string{"guild_id": guildID}, &resp)
	if resp.Containers != 4 { // guild + DM + two text channels (category skipped)
		t.Errorf("guild sync upserted %d", resp.Containers)
	}
	// Chats are dated by last_message_id, so the gateway lists them most
	// recent first before any message is fetched.
	rows, err := h.st.DB().Query(`SELECT id, last_message_at FROM containers WHERE last_message_at IS NOT NULL ORDER BY last_message_at DESC`)
	if err != nil {
		t.Fatal(err)
	}
	var dated []string
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		dated = append(dated, id+" "+at)
	}
	rows.Close()
	want := []string{
		chanGeneral + " " + noon.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"),
		chanRandom + " " + noon.Add(-48*time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"),
		dmID + " 2024-10-27T07:35:52.832Z",
	}
	if !slices.Equal(dated, want) {
		t.Errorf("dated chats\n got %v\nwant %v", dated, want)
	}
	// Cached lists cost nothing.
	h.callOK("sync", map[string]string{"guild_id": guildID}, &resp)
	if got := h.discord.paths(); len(got) != 3 {
		t.Errorf("upstream requests %v", got)
	}
	// A guild the account has not joined is refused without a request.
	h.callErr("sync", map[string]string{"guild_id": "1199999999999999999"}, http.StatusNotFound)
	if got := h.discord.paths(); len(got) != 3 {
		t.Errorf("unjoined guild caused requests %v", got)
	}
}

func TestFetchUpsertsMessages(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	var resp connector.ReadResult
	h.callOK("fetch", map[string]string{"channel_id": dmID}, &resp)
	if strings.Join(resp.MessageIDs, ",") != msg4 {
		t.Errorf("ids %v (system message must be skipped)", resp.MessageIDs)
	}
	wantPaths := []string{api("/users/@me/channels"), api("/users/@me"), api("/channels/" + dmID + "/messages")}
	if got := h.discord.paths(); strings.Join(got, " ") != strings.Join(wantPaths, " ") {
		t.Fatalf("upstream requests\n got %v\nwant %v", got, wantPaths)
	}
	m := h.discord.all()[2]
	if m.query.Get("limit") != "10" || len(m.query) != 1 {
		t.Errorf("fetch query %v", m.query)
	}
	if got := m.header.Get("Referer"); got != "https://discord.com/channels/@me/"+dmID {
		t.Errorf("referer %q", got)
	}
	if row := h.queryRow(`SELECT kind, name FROM containers WHERE id = ?`, dmID); row[0] != "dm" || row[1] != "Alice A" {
		t.Errorf("dm row %v", row)
	}
	if row := h.queryRow(`SELECT container_id, is_from_me, body FROM messages WHERE id = ?`, msg4); row[0] != dmID || row[1] != int64(1) || row[2] != "see you" {
		t.Errorf("message row %v", row)
	}
	seq := h.queryRow(`SELECT seq FROM messages WHERE id = ?`, msg4)[0].(int64)
	h.callOK("fetch", map[string]string{"channel_id": dmID, "before": strconv.FormatInt(seq, 10), "limit": "10"}, &resp)
	hits := h.discord.all()
	if len(hits) != 4 {
		t.Fatalf("second fetch requests %v", h.discord.paths())
	}
	if q := hits[3].query; q.Get("before") != msg4 || q.Get("limit") != "10" {
		t.Errorf("second fetch query %v", q)
	}
}

// Opening a chat loads the newest 10 messages and right away the 20 before
// them, as the web client does; scrolling up loads 20 more each time.
func TestFetchOpensLikeTheWebClient(t *testing.T) {
	path := api("/channels/" + chanGeneral + "/messages")
	const newest = 1300000000000001000
	history := func(r *http.Request) []map[string]any {
		start := newest
		if b := r.URL.Query().Get("before"); b != "" {
			start, _ = strconv.Atoi(b)
			start--
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var out []map[string]any
		for id := start; id > start-n && id > newest-45; id-- {
			out = append(out, map[string]any{
				"id": strconv.Itoa(id), "channel_id": chanGeneral, "type": 0, "content": "x", "timestamp": "2026-10-28T12:00:00.000000+00:00",
				"author": map[string]any{"id": aliceUser, "username": "alice"},
			})
		}
		return out
	}
	tests := []struct {
		limit string
		pages []string // queries, in order
		ids   int
	}{
		{"5", []string{"limit=10"}, 5},
		{"30", []string{"limit=10", "before=1300000000000000991&limit=20"}, 30},
		{"", []string{"limit=10", "before=1300000000000000991&limit=20", "before=1300000000000000971&limit=20"}, 45},
	}
	for _, tt := range tests {
		t.Run("limit "+tt.limit, func(t *testing.T) {
			h := newHarness(t, func(s *state) { s.SelfID, s.SelfCheckedAt = selfUser, noon })
			if err := h.st.UpsertContainer(context.Background(), store.Container{ID: chanGeneral, Kind: kindChannel, ParentID: guildID}); err != nil {
				t.Fatal(err)
			}
			h.discord.handle(path, func(w http.ResponseWriter, r *http.Request, _ int) {
				connector.WriteJSON(w, http.StatusOK, history(r))
			})
			h.start()
			args := map[string]string{"channel_id": chanGeneral}
			if tt.limit != "" {
				args["limit"] = tt.limit
			}
			var resp connector.ReadResult
			h.callOK("fetch", args, &resp)
			hits := h.discord.all()
			var got []string
			for _, x := range hits {
				got = append(got, x.query.Encode())
			}
			if !slices.Equal(got, tt.pages) {
				t.Fatalf("pages\n got %v\nwant %v", got, tt.pages)
			}
			if len(resp.MessageIDs) != tt.ids || resp.MessageIDs[0] != strconv.Itoa(newest) {
				t.Errorf("%d ids, first %v", len(resp.MessageIDs), resp.MessageIDs[:1])
			}
			// The second page follows the first at once; later pages are paced.
			if len(hits) > 1 {
				if d := hits[1].at.Sub(hits[0].at); d > followMax {
					t.Errorf("second page %v after the first", d)
				}
			}
			if len(hits) > 2 {
				if d := hits[2].at.Sub(hits[1].at); d < gapMin {
					t.Errorf("third page %v after the second", d)
				}
			}
		})
	}
}

func TestPageLoadBuildNumber(t *testing.T) {
	h := newHarness(t, func(s *state) { *s = state{} })
	html, err := os.ReadFile("testdata/app.html")
	if err != nil {
		t.Fatal(err)
	}
	h.discord.handle("/channels/@me", func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Set-Cookie", "__dcfduid=abc; Expires=Fri, 29 Oct 2027 12:00:00 GMT; Path=/; Secure; HttpOnly")
		w.Write(html)
	})
	h.discord.handle("/assets/web.b2b84438f3c83e84.js", func(w http.ResponseWriter, _ *http.Request, _ int) {
		io.WriteString(w, `x={releaseChannel:"stable",buildNumber:"700001",versionHash:"0"}`)
	})
	h.start()
	// The service opens the app before it connects, as a browser restoring
	// the tab does; the gateway identifies with what the page reported.
	startup := h.startup
	if len(startup) != 2 || startup[0].path != "/channels/@me" || startup[1].path != "/assets/web.b2b84438f3c83e84.js" {
		t.Fatalf("startup requests %+v", startup)
	}
	if identify := h.gw.conn(t).next(t); !strings.Contains(identify.raw, `"client_build_number":700001`) {
		t.Errorf("identify %s", identify.raw)
	}
	var resp connector.ReadResult
	h.callOK("sync", map[string]string{}, &resp)
	want := []string{api("/users/@me/guilds"), api("/users/@me/channels")}
	if got := h.discord.paths(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("requests\n got %v\nwant %v", got, want)
	}
	doc, js, first := startup[0], startup[1], h.discord.all()[0]
	if doc.header.Get("Sec-Fetch-Dest") != "document" || doc.header.Get("Authorization") != "" ||
		doc.header.Get("X-Super-Properties") != "" || doc.header.Get("Referer") != "" {
		t.Errorf("document headers %v", doc.header)
	}
	if js.header.Get("Sec-Fetch-Dest") != "script" || js.header.Get("Authorization") != "" {
		t.Errorf("script headers %v", js.header)
	}
	if sp := superProps(t, first.header); sp["client_build_number"] != float64(700001) {
		t.Errorf("API request used build %v", sp["client_build_number"])
	}
	s := h.state()
	if s.BuildNumber != 700001 || s.BuildCheckedAt.Before(doc.at) || !s.BuildCheckedAt.Before(js.at) {
		t.Errorf("state build %d at %v", s.BuildNumber, s.BuildCheckedAt)
	}
	if len(s.Cookies) != 1 || s.Cookies[0].Name != "__dcfduid" {
		t.Errorf("persisted cookies %+v", s.Cookies)
	}
	if s.LaunchID == "" || s.LaunchSignature == "" || s.HeartbeatSessionID == "" {
		t.Errorf("identity not persisted: %+v", s)
	}
	if len(s.Requests) != 4 {
		t.Errorf("page load not counted: %d requests", len(s.Requests))
	}

	// A restart loads the page again, a new launch of the same profile.
	h.restart()
	if len(h.startup) != 2 {
		t.Errorf("restart sent %d requests before connecting", len(h.startup))
	}
	h.clock.Advance(2 * time.Hour)
	h.callOK("fetch", map[string]string{"channel_id": dmID}, &connector.ReadResult{})
	last := h.discord.all()[len(h.discord.all())-1]
	restarted := h.state()
	if sp := superProps(t, last.header); sp["client_launch_id"] == s.LaunchID || sp["client_launch_id"] != restarted.LaunchID ||
		len(restarted.Cookies) != 1 {
		t.Errorf("launch after restart: %v", sp)
	}
	// After 30 idle minutes the heartbeat session rotates.
	if sp := superProps(t, last.header); sp["client_heartbeat_session_id"] == s.HeartbeatSessionID {
		t.Error("heartbeat session not rotated after two idle hours")
	}

	// A failing page keeps the last build number and is not retried that day.
	h.discord.reply("/channels/@me", http.StatusServiceUnavailable, map[string]any{})
	h.clock.Advance(25 * time.Hour)
	h.callOK("sync", map[string]string{}, &resp)
	h.clock.Advance(time.Hour)
	h.callOK("guild-search", map[string]string{"guild_id": guildID, "content": "x"}, &connector.ReadResult{})
	if n := len(h.discord.to("/channels/@me")); n != 1 {
		t.Errorf("page loaded %d times after the restart, want 1", n)
	}
	if s := h.state(); s.BuildNumber != 700001 {
		t.Errorf("build number %d after failed refresh", s.BuildNumber)
	}
}

func TestRateLimitRetriesOnce(t *testing.T) {
	path := api("/channels/" + dmID + "/messages")
	h := newHarness(t, nil)
	h.discord.handle(path, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 0 {
			w.Header().Set("Retry-After", "3")
			connector.WriteJSON(w, http.StatusTooManyRequests, map[string]any{"message": "You are being rate limited.", "retry_after": 2.5, "global": false})
			return
		}
		connector.WriteJSON(w, http.StatusOK, dmMessagesBody)
	})
	h.start()
	h.callOK("fetch", map[string]string{"channel_id": dmID}, &connector.ReadResult{})
	hits := h.discord.to(path)
	if len(hits) != 2 {
		t.Fatalf("%d message requests, want 2", len(hits))
	}
	if d := hits[1].at.Sub(hits[0].at); d < 2500*time.Millisecond {
		t.Errorf("retried after %v, retry_after was 2.5s", d)
	}
	if s := h.state(); len(s.RateLimits) != 1 || !s.CooldownUntil.IsZero() {
		t.Errorf("rate limits %v cooldown %v", s.RateLimits, s.CooldownUntil)
	}
}

func TestRateLimitTwiceGivesUp(t *testing.T) {
	path := api("/channels/" + dmID + "/messages")
	h := newHarness(t, nil)
	h.discord.reply(path, http.StatusTooManyRequests, map[string]any{"message": "You are being rate limited.", "retry_after": 1.2, "global": false})
	h.start()
	e := h.callErr("fetch", map[string]string{"channel_id": dmID}, http.StatusTooManyRequests)
	if e.RetryAfter != 2 {
		t.Errorf("retry_after_seconds = %d, want 2", e.RetryAfter)
	}
	if n := len(h.discord.to(path)); n != 2 {
		t.Errorf("%d message requests, want exactly 2", n)
	}
	if !h.state().CooldownUntil.IsZero() {
		t.Error("two 429s must not trigger the cooldown")
	}
}

func TestCooldown(t *testing.T) {
	path := api("/channels/" + dmID + "/messages")
	tests := []struct {
		name   string
		seed   func(*state)
		status int
		body   any
		header map[string]string
	}{
		{name: "global", status: 429, body: map[string]any{"message": "You are being rate limited.", "retry_after": 1, "global": true}},
		{name: "global header", status: 429, body: map[string]any{"retry_after": 1}, header: map[string]string{"X-RateLimit-Global": "true"}},
		{name: "long retry_after", status: 429, body: map[string]any{"retry_after": 301.5, "global": false}},
		{name: "no retry_after", status: 429, body: "error code: 1015"},
		{
			name: "third 429 in an hour", status: 429, body: map[string]any{"retry_after": 1, "global": false},
			seed: func(s *state) { s.RateLimits = []time.Time{noon.Add(-50 * time.Minute), noon.Add(-10 * time.Minute)} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.seed)
			h.discord.handle(path, func(w http.ResponseWriter, _ *http.Request, _ int) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				if s, ok := tt.body.(string); ok {
					w.WriteHeader(tt.status)
					io.WriteString(w, s)
					return
				}
				connector.WriteJSON(w, tt.status, tt.body)
			})
			h.start()
			e := h.callErr("fetch", map[string]string{"channel_id": dmID}, http.StatusTooManyRequests)
			if e.RetryAfter != int(cooldownLength/time.Second) {
				t.Errorf("retry_after_seconds = %d, want 6 h", e.RetryAfter)
			}
			if n := len(h.discord.to(path)); n != 1 {
				t.Errorf("%d requests: a cooldown must not retry", n)
			}
			s := h.state()
			if d := s.CooldownUntil.Sub(h.clock.Now()); d < cooldownLength-time.Minute || d > cooldownLength {
				t.Errorf("cooldown until %v (in %v)", s.CooldownUntil, d)
			}
			if health, err := h.client.Health(context.Background()); err != nil || health.State != connector.Resting || !health.Until.Equal(s.CooldownUntil) {
				t.Errorf("health after cooldown = %+v, %v", health, err)
			}

			// The cooldown survives a restart and refuses without a request.
			before := len(h.discord.all())
			h.restart()
			h.clock.Advance(time.Hour)
			e = h.callErr("sync", map[string]string{}, http.StatusTooManyRequests)
			if e.RetryAfter < int((cooldownLength-time.Hour)/time.Second) {
				t.Errorf("retry_after_seconds = %d after restart", e.RetryAfter)
			}
			if len(h.discord.all()) != before {
				t.Error("request sent during cooldown")
			}
		})
	}
}

func TestSearchNotReadyRetries(t *testing.T) {
	path := api("/guilds/" + guildID + "/messages/search")
	h := newHarness(t, nil)
	h.discord.handle(path, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 0 {
			connector.WriteJSON(w, http.StatusAccepted, map[string]any{"message": "Index not yet available. Try again later", "code": 110000, "retry_after": 3})
			return
		}
		connector.WriteJSON(w, http.StatusOK, searchBody)
	})
	h.start()
	var resp connector.ReadResult
	h.callOK("guild-search", map[string]string{"guild_id": guildID, "content": "rig"}, &resp)
	hits := h.discord.to(path)
	if len(hits) != 2 || len(resp.MessageIDs) != 2 {
		t.Fatalf("%d search requests, %d ids", len(hits), len(resp.MessageIDs))
	}
	if d := hits[1].at.Sub(hits[0].at); d < 3*time.Second {
		t.Errorf("retried after %v", d)
	}
	if n := len(h.state().Requests); n != 6 {
		t.Errorf("budget counted %d requests, want 6 (startup page and both searches)", n)
	}

	// Still not ready after the retry: 503 with a retry hint, no third try.
	h.discord.reply(path, http.StatusAccepted, map[string]any{"code": 110000, "retry_after": 0})
	e := h.callErr("guild-search", map[string]string{"guild_id": guildID, "content": "other"}, http.StatusServiceUnavailable)
	if e.RetryAfter != 5 {
		t.Errorf("retry_after_seconds = %d, want the 5 s default", e.RetryAfter)
	}
	if n := len(h.discord.to(path)); n != 4 {
		t.Errorf("%d search requests, want 4", n)
	}
}

// An owner stop keeps the service up, reporting what the owner must do,
// and nothing reaches Discord until it restarts.
func TestOwnerStopConditions(t *testing.T) {
	path := api("/channels/" + dmID + "/messages")
	tests := []struct {
		name   string
		status int
		body   any
		fix    string
	}{
		{"captcha", 400, map[string]any{"captcha_key": []string{"captcha-required"}, "captcha_sitekey": "a9b5fb07", "captcha_service": "hcaptcha"}, fixVerify},
		{"account verification", 403, map[string]any{"message": "You need to verify your account in order to perform this action.", "code": 40002}, fixVerify},
		{"anti-abuse block", 403, map[string]any{"message": "Unknown Message", "code": 10008}, fixAccount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.discord.reply(path, tt.status, tt.body)
			h.start()
			e := h.callErr("fetch", map[string]string{"channel_id": dmID}, http.StatusServiceUnavailable)
			if !strings.HasSuffix(e.Message, tt.fix) {
				t.Errorf("refusal %q does not say what to do", e.Message)
			}
			before := len(h.discord.all())
			health, err := h.client.Health(context.Background())
			if err != nil || health.State != connector.NeedsSetup || health.Detail != e.Message {
				t.Errorf("health = %+v, %v", health, err)
			}
			if again := h.callErr("sync", map[string]string{}, http.StatusServiceUnavailable); again.Message != e.Message {
				t.Errorf("later refusal %q", again.Message)
			}
			select {
			case err := <-h.done:
				t.Fatalf("service exited: %v", err)
			default:
			}
			if n := len(h.discord.all()) - before; n != 0 {
				t.Errorf("%d requests after a stop condition", n)
			}
			if err := h.stop(); err != nil {
				t.Errorf("stop: %v", err)
			}
		})
	}
}

func (h *harness) setup(token string) error {
	h.t.Helper()
	step, err := h.client.Setup(context.Background(), map[string]string{"token": token})
	if err == nil && !step.Done {
		h.t.Fatalf("setup step %+v, want done", step)
	}
	return err
}

func (h *harness) health() connector.Health {
	h.t.Helper()
	health, err := h.client.Health(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return health
}

func (h *harness) storedToken() string {
	h.t.Helper()
	b, err := h.token.Load()
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func errStatus(err error) int {
	if e, ok := errors.AsType[*connector.Error](err); ok {
		return e.Status
	}
	return 0
}

func TestLinkUnlinkRelink(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.token.Delete(); err != nil {
		t.Fatal(err)
	}
	h.start()
	if got := h.health(); got.State != connector.NeedsSetup || got.Detail != "needs a token" {
		t.Errorf("unlinked health = %+v", got)
	}
	if e := h.callErr("sync", map[string]string{}, http.StatusServiceUnavailable); !strings.Contains(e.Message, "console") {
		t.Errorf("unlinked read: %q", e.Message)
	}
	if n := len(h.discord.all()); n != 0 {
		t.Fatalf("%d requests while unlinked", n)
	}

	const fresh = "fresh-token.abc.def"
	if err := h.setup(" " + fresh + "\n"); err != nil {
		t.Fatal(err)
	}
	me := h.discord.to(api("/users/@me"))
	if len(me) != 1 || me[0].header.Get("Authorization") != fresh {
		t.Fatalf("setup checked the token with %d requests", len(me))
	}
	if h.storedToken() != fresh || h.health().State != connector.Healthy {
		t.Fatalf("after setup: health %+v", h.health())
	}
	if fi, err := os.Stat(filepath.Join(filepath.Dir(h.statePath), "token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if err := h.setup("other-token.abc.def"); errStatus(err) != http.StatusConflict {
		t.Fatalf("setup while linked: %v", err)
	}
	if h.storedToken() != fresh {
		t.Fatal("a refused setup replaced the token")
	}

	h.restart()
	h.callOK("sync", map[string]string{}, &connector.ReadResult{})
	guilds := h.discord.to(api("/users/@me/guilds"))
	if len(guilds) != 1 || guilds[0].header.Get("Authorization") != fresh {
		t.Fatal("the stored token was not used after a restart")
	}

	if err := h.client.Unlink(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.storedToken() != "" || h.health().State != connector.NeedsSetup {
		t.Fatalf("after unlink: health %+v", h.health())
	}
	before := len(h.discord.all())
	h.callErr("sync", map[string]string{}, http.StatusServiceUnavailable)
	if n := len(h.discord.all()) - before; n != 0 {
		t.Fatalf("%d requests after unlink", n)
	}
	if row := h.queryRow(`SELECT count(*) FROM containers`); row[0] == int64(0) {
		t.Error("unlink dropped stored data")
	}
	if err := h.setup(fresh); err != nil {
		t.Fatalf("relink: %v", err)
	}
	if h.health().State != connector.Healthy {
		t.Fatalf("after relink: health %+v", h.health())
	}
}

// Discord issues the installation ID to a logged-out app, so linking starts
// like the app before login; the ID then goes with every request and
// identify, and outlives an unlink.
func TestLinkStartsLoggedOut(t *testing.T) {
	const (
		fingerprint  = "1566000000000000001.ZmluZ2VycHJpbnRmaW5nZXJwcmlu"
		installation = "1566000000000000002.aW5zdGFsbGF0aW9uaW5zdGFsbGF0"
	)
	h := newHarness(t, func(s *state) { s.InstallationID = "" })
	if err := h.token.Delete(); err != nil {
		t.Fatal(err)
	}
	h.discord.reply(api("/experiments"), 200, map[string]any{"fingerprint": fingerprint, "assignments": []any{}, "guild_experiments": []any{}})
	h.discord.reply(api("/apex/experiments"), 200, map[string]any{"installation": installation, "assignments": []any{}})
	h.start()
	if err := h.setup(testToken); err != nil {
		t.Fatal(err)
	}
	want := []string{"/channels/@me", api("/experiments"), api("/apex/experiments"), api("/users/@me")}
	if got := h.discord.paths(); !slices.Equal(got, want) {
		t.Fatalf("requests\n got %v\nwant %v", got, want)
	}
	hits := h.discord.all()
	exp, apex, me := hits[1], hits[2], hits[3]
	if exp.header.Get("Authorization") != "" || exp.header.Get("X-Installation-Id") != "" ||
		exp.header.Get("X-Context-Properties") != base64.StdEncoding.EncodeToString([]byte(`{"location":"Login"}`)) ||
		exp.query.Get("with_guild_experiments") != "true" || exp.header.Get("Referer") != "https://discord.com/login" {
		t.Errorf("/experiments %v %v", exp.query, exp.header)
	}
	if apex.header.Get("Authorization") != "" || apex.header.Get("X-Fingerprint") != fingerprint || apex.query.Get("surface") != "2" ||
		apex.at.Sub(exp.at) > followMax {
		t.Errorf("/apex/experiments %v %v", apex.query, apex.header)
	}
	if me.header.Get("Authorization") != testToken || me.header.Get("X-Installation-Id") != installation || me.header.Get("X-Fingerprint") != "" {
		t.Errorf("/users/@me %v", me.header)
	}
	if h.state().InstallationID != installation {
		t.Errorf("installation not kept: %q", h.state().InstallationID)
	}
	identify := h.gw.conn(t).next(t)
	if !strings.Contains(identify.raw, `"installation_id":"`+installation+`"`) {
		t.Errorf("identify %s", identify.D)
	}
	if err := h.client.Unlink(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.setup(testToken); err != nil {
		t.Fatal(err)
	}
	if n := len(h.discord.to(api("/experiments"))); n != 1 || h.state().InstallationID != installation {
		t.Errorf("relink fetched /experiments %d times; installation %q", n, h.state().InstallationID)
	}
}

func TestSetupRefusesBadToken(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.token.Delete(); err != nil {
		t.Fatal(err)
	}
	h.discord.reply(api("/users/@me"), 401, map[string]any{"message": "401: Unauthorized", "code": 0})
	h.start()
	if err := h.setup("Bot abc.def"); errStatus(err) != http.StatusBadRequest {
		t.Fatalf("setup with a bot token: %v", err)
	}
	if n := len(h.discord.all()); n != 0 {
		t.Fatal("a malformed token reached Discord")
	}
	err := h.setup("bad-token.abc.def")
	if e, ok := errors.AsType[*connector.Error](err); !ok || !strings.Contains(e.Message, "401") {
		t.Fatalf("setup with a rejected token: %v", err)
	}
	if h.storedToken() != "" || h.health().State != connector.NeedsSetup {
		t.Fatalf("a rejected token was kept: health %+v", h.health())
	}
	h.discord.reply(api("/users/@me"), 200, map[string]any{"id": selfUser, "username": "me"})
	if err := h.setup("good-token.abc.def"); err != nil {
		t.Fatalf("setup after a rejected token: %v", err)
	}
}

// A token Discord later rejects is discarded, and nothing reaches Discord
// until the owner pastes a new one.
func TestRejectedTokenIsDiscarded(t *testing.T) {
	path := api("/channels/" + dmID + "/messages")
	h := newHarness(t, nil)
	h.discord.reply(path, 401, map[string]any{"message": "401: Unauthorized", "code": 0})
	h.start()
	e := h.callErr("fetch", map[string]string{"channel_id": dmID}, http.StatusServiceUnavailable)
	if !strings.HasSuffix(e.Message, fixToken) {
		t.Errorf("refusal %q does not say what to do", e.Message)
	}
	if got := h.health(); got.State != connector.NeedsSetup || got.Detail != e.Message {
		t.Errorf("health = %+v", got)
	}
	if h.storedToken() != "" {
		t.Error("the rejected token is still stored")
	}
	before := len(h.discord.all())
	if again := h.callErr("sync", map[string]string{}, http.StatusServiceUnavailable); !strings.Contains(again.Message, "console") {
		t.Errorf("later refusal %q", again.Message)
	}
	if n := len(h.discord.all()) - before; n != 0 {
		t.Errorf("%d requests after the token was rejected", n)
	}
	if err := h.setup("new-token.abc.def"); err != nil {
		t.Fatalf("setup after rejection: %v", err)
	}
	if got := h.health(); got.State != connector.Healthy {
		t.Errorf("health after relink = %+v", got)
	}
}

func TestHealthResting(t *testing.T) {
	budget := func(s *state) {
		for i := range dailyBudget {
			s.Requests = append(s.Requests, noon.Add(-23*time.Hour+time.Duration(i)*time.Second))
		}
	}
	tests := []struct {
		name   string
		seed   func(*state)
		at     time.Time
		until  time.Time
		reason string
	}{
		{name: "ok", at: noon},
		{name: "cooldown", seed: func(s *state) { s.CooldownUntil = noon.Add(3 * time.Hour) }, at: noon, until: noon.Add(3 * time.Hour), reason: "cooldown"},
		{name: "daily budget", seed: budget, at: noon, until: noon.Add(time.Hour + time.Millisecond), reason: "daily budget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.seed)
			h.clock.now = tt.at
			h.start()
			health, err := h.client.Health(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := connector.Health{State: connector.Healthy}
			if tt.reason != "" {
				want = connector.Health{State: connector.Resting, Detail: tt.reason, Until: tt.until}
			}
			if health.State != want.State || health.Detail != want.Detail || !health.Until.Equal(want.Until) {
				t.Fatalf("health = %+v, want %+v", health, want)
			}
		})
	}
}

func TestDenialCached(t *testing.T) {
	path := api("/channels/" + otherChan + "/messages")
	h := newHarness(t, nil)
	h.discord.reply(path, http.StatusForbidden, map[string]any{"message": "Missing Access", "code": 50001})
	h.start()
	h.callErr("fetch", map[string]string{"channel_id": otherChan}, http.StatusNotFound)
	before := len(h.discord.all())
	h.callErr("fetch", map[string]string{"channel_id": otherChan, "before": "1"}, http.StatusNotFound)
	h.restart()
	h.clock.Advance(23 * time.Hour)
	h.callErr("fetch", map[string]string{"channel_id": otherChan}, http.StatusNotFound)
	h.callErr("guild-search", map[string]string{"guild_id": guildID, "channel_id": otherChan, "content": "x"}, http.StatusNotFound)
	if len(h.discord.all()) != before {
		t.Errorf("denied channel requested again: %v", h.discord.paths()[before:])
	}
	if n := len(h.discord.to(path)); n != 1 {
		t.Errorf("%d requests to the denied channel", n)
	}
	// A 404 for an unknown guild channel list is cached the same way.
	h.discord.reply(api("/guilds/"+guildID+"/channels"), http.StatusNotFound, map[string]any{"message": "Unknown Guild", "code": 10004})
	h.callErr("sync", map[string]string{"guild_id": guildID}, http.StatusNotFound)
	h.callErr("guild-search", map[string]string{"guild_id": guildID, "content": "x"}, http.StatusNotFound)
	if n := len(h.discord.to(api("/guilds/" + guildID + "/channels"))); n != 1 {
		t.Errorf("%d requests to the denied guild", n)
	}
}

func TestValidation(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	tests := []struct {
		name string
		op   string
		args map[string]string
	}{
		{"fetch path traversal", "fetch", map[string]string{"channel_id": "../../users/@me"}},
		{"fetch encoded slash", "fetch", map[string]string{"channel_id": "110000000000000%2F"}},
		{"fetch missing", "fetch", map[string]string{}},
		{"fetch bad before", "fetch", map[string]string{"channel_id": dmID, "before": "x"}},
		{"fetch two anchors", "fetch", map[string]string{"channel_id": dmID, "before": "1", "after": "2"}},
		{"fetch unstored anchor", "fetch", map[string]string{"channel_id": dmID, "before": "999"}},
		{"fetch limit too big", "fetch", map[string]string{"channel_id": dmID, "limit": "101"}},
		{"fetch unknown field", "fetch", map[string]string{"channel_id": dmID, "method": "DELETE"}},
		{"search missing guild", "guild-search", map[string]string{"content": "x"}},
		{"search bad author", "guild-search", map[string]string{"guild_id": guildID, "author_id": "me"}},
		{"search bad channel", "guild-search", map[string]string{"guild_id": guildID, "channel_id": "#general"}},
		{"search bad has", "guild-search", map[string]string{"guild_id": guildID, "has": "pdf"}},
		{"search huge offset", "guild-search", map[string]string{"guild_id": guildID, "content": "x", "offset": "10000"}},
		{"search no criteria", "guild-search", map[string]string{"guild_id": guildID}},
		{"search long content", "guild-search", map[string]string{"guild_id": guildID, "content": strings.Repeat("a", 513)}},
		{"sync bad guild", "sync", map[string]string{"guild_id": "x"}},
		{"fetch limit not a number", "fetch", map[string]string{"channel_id": dmID, "limit": "ten"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.callErr(tt.op, tt.args, http.StatusBadRequest)
		})
	}
	if hits := h.discord.all(); len(hits) != 0 {
		t.Errorf("invalid calls reached Discord: %v", h.discord.paths())
	}
}

func TestIsSnowflake(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"175928847299117063", true},
		{"123456789012345", true},       // 15 digits
		{"123456789012345678901", true}, // 21 digits
		{"12345678901234", false},
		{"1234567890123456789012", false},
		{"17592884729911706a", false},
		{"175928847299117063\n", false},
		{"１７５９２８８４７２９９１１７０６３", false}, // full-width digits
	}
	for _, tt := range tests {
		if got := isSnowflake(tt.in); got != tt.want {
			t.Errorf("isSnowflake(%q) = %v", tt.in, got)
		}
	}
}

// A long block is refused at once with an honest wait, not slept through.
func TestPacedRefusal(t *testing.T) {
	h := newHarness(t, func(s *state) { s.CooldownUntil = noon.Add(2 * time.Hour) })
	h.clock.now = noon
	h.start()
	e := h.callErr("fetch", map[string]string{"channel_id": dmID}, http.StatusTooManyRequests)
	// The cooldown plus the 2 s app-open delay.
	if want := 2*3600 + 2; e.RetryAfter != want {
		t.Errorf("retry_after_seconds = %d, want %d", e.RetryAfter, want)
	}
	if len(h.discord.all()) != 0 || !h.clock.Now().Equal(noon) {
		t.Error("refusal waited or sent a request")
	}
}

func TestSlotWaitCap(t *testing.T) {
	tests := []struct {
		name      string
		breakLeft time.Duration
		served    bool
	}{
		// The break plus the 2 s app-open delay: 89 s is within the cap ...
		{"within 90 s", 87 * time.Second, true},
		// ... 91 s is not.
		{"beyond 90 s", 89 * time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(s *state) {
				s.BreakUntil = noon.Add(tt.breakLeft)
				s.LastRequest = noon.Add(-time.Minute)
				s.SelfID, s.SelfCheckedAt = selfUser, noon
			})
			if err := h.st.UpsertContainer(context.Background(), store.Container{ID: dmID, Kind: kindDM, Name: "Alice A"}); err != nil {
				t.Fatal(err)
			}
			h.start()
			if tt.served {
				// Opening the app at startup is the first request to wait.
				if len(h.startup) != 1 || !h.startup[0].at.Equal(noon.Add(89*time.Second)) {
					t.Errorf("startup %+v", h.startup)
				}
				h.callOK("fetch", map[string]string{"channel_id": dmID}, &connector.ReadResult{})
				return
			}
			e := h.callErr("fetch", map[string]string{"channel_id": dmID}, http.StatusTooManyRequests)
			if e.RetryAfter != 91 {
				t.Errorf("retry_after_seconds = %d, want 91", e.RetryAfter)
			}
			if len(h.discord.all()) != 0 || !h.clock.Now().Equal(noon) {
				t.Error("refused call waited or sent a request")
			}
		})
	}
}

func TestStateFilePrivate(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.callOK("sync", map[string]string{}, &connector.ReadResult{})
	fi, err := os.Stat(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode %v", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(h.statePath)
	if bytes.Contains(b, []byte(testToken)) || bytes.Contains(b, []byte("Alice")) {
		t.Error("state file holds the token or names")
	}
}
