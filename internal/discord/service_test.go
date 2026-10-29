package discord

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
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"

	"github.com/adeci/sieve/internal/discordapi"
	"github.com/adeci/sieve/internal/exitcode"
	"github.com/adeci/sieve/internal/store"
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
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404: Not Found", "code": 0})
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
	f.handle(path, func(w http.ResponseWriter, _ *http.Request, _ int) { writeJSON(w, status, body) })
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
	srv       *httptest.Server
	st        *store.Store
	statePath string
	socket    string
	hours     activeHours
	logs      *syncBuffer
	cancel    context.CancelFunc
	done      chan error
	client    *http.Client
}

func api(p string) string { return "/api/v9" + p }

var (
	guildsBody   = []map[string]any{{"id": guildID, "name": "Club", "icon": nil, "owner": false, "permissions": "0"}}
	channelsBody = []map[string]any{
		{"id": "1100000000000000010", "type": 4, "guild_id": guildID, "name": "Text", "position": 0},
		{"id": chanRandom, "type": 0, "guild_id": guildID, "name": "random", "position": 2, "parent_id": "1100000000000000010"},
		{"id": chanGeneral, "type": 0, "guild_id": guildID, "name": "general", "position": 1, "parent_id": "1100000000000000010"},
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
	h := &harness{
		t:         t,
		clock:     &fakeClock{now: noon},
		statePath: filepath.Join(dir, "state.json"),
		socket:    filepath.Join(dir, "s.sock"),
		hours:     activeHours{start: 8, end: 24, loc: newYork},
		logs:      &syncBuffer{},
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

	st, err := store.Open(context.Background(), filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st

	s := &state{BuildNumber: defaultBuildNumber, BuildCheckedAt: noon}
	if seed != nil {
		seed(s)
	}
	if err := s.save(h.statePath); err != nil {
		t.Fatal(err)
	}
	h.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", h.socket)
		},
	}}
	t.Cleanup(func() {
		if h.cancel != nil {
			h.stop()
		}
		for _, x := range h.discord.all() {
			if x.method != http.MethodGet {
				t.Errorf("upstream %s %s: only GET is allowed", x.method, x.path)
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
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	cfg := config{
		store:     h.st,
		statePath: h.statePath,
		listen:    h.socket,
		token:     testToken,
		timezone:  "America/New_York",
		hours:     h.hours,
		transport: &fhttp.Client{},
		apiBase:   h.srv.URL + "/api/v9",
		webBase:   h.srv.URL,
		clock:     h.clock,
		rng:       fixedRand(0.5),
		log:       slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	go func() { h.done <- run(ctx, cfg) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := net.Dial("unix", h.socket); err == nil {
			c.Close()
			return
		}
		select {
		case err := <-h.done:
			h.t.Fatalf("run exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatal("service did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) stop() error {
	h.cancel()
	h.cancel = nil
	return h.wait()
}

func (h *harness) wait() error {
	select {
	case err := <-h.done:
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatal("run did not return")
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

func (h *harness) call(path string, body any) (int, []byte) {
	h.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err := h.client.Post("http://discord"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		h.t.Fatalf("call %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, data
}

func (h *harness) callOK(path string, body, out any) {
	h.t.Helper()
	status, data := h.call(path, body)
	if status != http.StatusOK {
		h.t.Fatalf("%s: status %d: %s", path, status, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) callErr(path string, body any, status int) discordapi.ErrorResponse {
	h.t.Helper()
	got, data := h.call(path, body)
	var e discordapi.ErrorResponse
	if got != status || json.Unmarshal(data, &e) != nil {
		h.t.Fatalf("%s: status %d (%s), want %d", path, got, data, status)
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
	var resp discordapi.MessagesResponse
	h.callOK(discordapi.PathSearch, discordapi.SearchRequest{GuildID: guildID, Content: "rig"}, &resp)
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
	if search.header.Get("Authorization") != testToken || search.header.Get("X-Discord-Timezone") != "America/New_York" {
		t.Errorf("search headers %v", search.header)
	}
	sp := superProps(t, search.header)
	if sp["client_build_number"] != float64(defaultBuildNumber) || sp["os"] != "Linux" || sp["client_launch_id"] != h.state().LaunchID {
		t.Errorf("super properties %v", sp)
	}

	// Containers before messages, with names from the channel list and the
	// thread from the search response; is_from_me from /users/@me.
	if row := h.queryRow(`SELECT kind, name FROM containers WHERE id = ?`, guildID); row[0] != "guild" || row[1] != "Club" {
		t.Errorf("guild row %v", row)
	}
	if row := h.queryRow(`SELECT kind, parent_id, name FROM containers WHERE id = ?`, chanGeneral); row[0] != "channel" || row[1] != guildID || row[2] != "general" {
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
	var again discordapi.MessagesResponse
	h.callOK(discordapi.PathSearch, discordapi.SearchRequest{GuildID: guildID, Content: "rig"}, &again)
	if n := len(h.discord.all()); n != 4 {
		t.Errorf("cache hit made upstream requests: %d total", n)
	}
	if strings.Join(again.MessageIDs, ",") != strings.Join(resp.MessageIDs, ",") || again.TotalResults != 2 {
		t.Errorf("cached response %+v", again)
	}
	// The next page needs only the search itself; lists and self are cached.
	h.callOK(discordapi.PathSearch, discordapi.SearchRequest{GuildID: guildID, Content: "rig", Offset: 25}, &again)
	if got := h.discord.paths(); len(got) != 5 || got[4] != api("/guilds/"+guildID+"/messages/search") || h.discord.all()[4].query.Get("offset") != "25" {
		t.Errorf("paged search requests %v", got)
	}
	// After an hour the cache expires.
	h.clock.Advance(time.Hour)
	h.callOK(discordapi.PathSearch, discordapi.SearchRequest{GuildID: guildID, Content: "rig"}, &again)
	if n := len(h.discord.to(api("/guilds/" + guildID + "/messages/search"))); n != 3 {
		t.Errorf("search requests after expiry: %d", n)
	}
	if n := len(h.state().Requests); n != 6 {
		t.Errorf("budget counted %d requests, want 6", n)
	}
}

func TestSyncCounts(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	var resp discordapi.SyncResponse
	h.callOK(discordapi.PathSync, discordapi.SyncRequest{}, &resp)
	if resp.Containers != 2 { // guild + DM
		t.Errorf("sync upserted %d", resp.Containers)
	}
	if row := h.queryRow(`SELECT kind, name, participants_json FROM containers WHERE id = ?`, dmID); row[0] != "dm" || row[1] != "Alice A" ||
		!strings.Contains(fmt.Sprint(row[2]), aliceUser) {
		t.Errorf("dm row %v", row)
	}
	h.callOK(discordapi.PathSync, discordapi.SyncRequest{GuildID: guildID}, &resp)
	if resp.Containers != 4 { // guild + DM + two text channels (category skipped)
		t.Errorf("guild sync upserted %d", resp.Containers)
	}
	// Cached lists cost nothing.
	h.callOK(discordapi.PathSync, discordapi.SyncRequest{GuildID: guildID}, &resp)
	if got := h.discord.paths(); len(got) != 3 {
		t.Errorf("upstream requests %v", got)
	}
	// A guild the account has not joined is refused without a request.
	h.callErr(discordapi.PathSync, discordapi.SyncRequest{GuildID: "1199999999999999999"}, http.StatusNotFound)
	if got := h.discord.paths(); len(got) != 3 {
		t.Errorf("unjoined guild caused requests %v", got)
	}
}

func TestFetchUpsertsMessages(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	var resp discordapi.MessagesResponse
	h.callOK(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID}, &resp)
	if strings.Join(resp.MessageIDs, ",") != msg4 {
		t.Errorf("ids %v (system message must be skipped)", resp.MessageIDs)
	}
	wantPaths := []string{api("/users/@me/channels"), api("/users/@me"), api("/channels/" + dmID + "/messages")}
	if got := h.discord.paths(); strings.Join(got, " ") != strings.Join(wantPaths, " ") {
		t.Fatalf("upstream requests\n got %v\nwant %v", got, wantPaths)
	}
	m := h.discord.all()[2]
	if m.query.Get("limit") != "50" || len(m.query) != 1 {
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
	h.callOK(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID, Before: msg4, Limit: 10}, &resp)
	hits := h.discord.all()
	if len(hits) != 4 {
		t.Fatalf("second fetch requests %v", h.discord.paths())
	}
	if q := hits[3].query; q.Get("before") != msg4 || q.Get("limit") != "10" {
		t.Errorf("second fetch query %v", q)
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
	var resp discordapi.SyncResponse
	h.callOK(discordapi.PathSync, discordapi.SyncRequest{}, &resp)
	want := []string{"/channels/@me", "/assets/web.b2b84438f3c83e84.js", api("/users/@me/guilds"), api("/users/@me/channels")}
	if got := h.discord.paths(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("requests\n got %v\nwant %v", got, want)
	}
	hits := h.discord.all()
	doc, js, first := hits[0], hits[1], hits[2]
	if doc.header.Get("Sec-Fetch-Dest") != "document" || doc.header.Get("Authorization") != "" || doc.header.Get("X-Super-Properties") != "" {
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

	// Same identity after a restart; no second page load within 24 h.
	h.restart()
	h.clock.Advance(2 * time.Hour)
	h.callOK(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID}, &discordapi.MessagesResponse{})
	if n := len(h.discord.to("/channels/@me")); n != 1 {
		t.Errorf("page loaded %d times", n)
	}
	last := h.discord.all()[len(h.discord.all())-1]
	if sp := superProps(t, last.header); sp["client_launch_id"] != s.LaunchID || sp["launch_signature"] != s.LaunchSignature {
		t.Errorf("identity changed across restart: %v", sp)
	}
	// After 30 idle minutes the heartbeat session rotates.
	if sp := superProps(t, last.header); sp["client_heartbeat_session_id"] == s.HeartbeatSessionID {
		t.Error("heartbeat session not rotated after two idle hours")
	}

	// A failing page keeps the last build number and is not retried that day.
	h.discord.reply("/channels/@me", http.StatusServiceUnavailable, map[string]any{})
	h.clock.Advance(25 * time.Hour)
	h.callOK(discordapi.PathSync, discordapi.SyncRequest{}, &resp)
	h.clock.Advance(time.Hour)
	h.callOK(discordapi.PathSearch, discordapi.SearchRequest{GuildID: guildID, Content: "x"}, &discordapi.MessagesResponse{})
	if n := len(h.discord.to("/channels/@me")); n != 2 {
		t.Errorf("page loaded %d times, want 2", n)
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
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"message": "You are being rate limited.", "retry_after": 2.5, "global": false})
			return
		}
		writeJSON(w, http.StatusOK, dmMessagesBody)
	})
	h.start()
	h.callOK(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID}, &discordapi.MessagesResponse{})
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
	e := h.callErr(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID}, http.StatusTooManyRequests)
	if e.RetryAfterSeconds != 2 {
		t.Errorf("retry_after_seconds = %d, want 2", e.RetryAfterSeconds)
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
				writeJSON(w, tt.status, tt.body)
			})
			h.start()
			e := h.callErr(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID}, http.StatusTooManyRequests)
			if e.RetryAfterSeconds != int(cooldownLength/time.Second) {
				t.Errorf("retry_after_seconds = %d, want 6 h", e.RetryAfterSeconds)
			}
			if n := len(h.discord.to(path)); n != 1 {
				t.Errorf("%d requests: a cooldown must not retry", n)
			}
			s := h.state()
			if d := s.CooldownUntil.Sub(h.clock.Now()); d < cooldownLength-time.Minute || d > cooldownLength {
				t.Errorf("cooldown until %v (in %v)", s.CooldownUntil, d)
			}
			if !strings.Contains(h.logs.String(), "level=WARN") {
				t.Error("cooldown not logged at warn")
			}

			// The cooldown survives a restart and refuses without a request.
			before := len(h.discord.all())
			h.restart()
			h.clock.Advance(time.Hour)
			e = h.callErr(discordapi.PathSync, discordapi.SyncRequest{}, http.StatusTooManyRequests)
			if e.RetryAfterSeconds < int((cooldownLength-time.Hour)/time.Second) {
				t.Errorf("retry_after_seconds = %d after restart", e.RetryAfterSeconds)
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
			writeJSON(w, http.StatusAccepted, map[string]any{"message": "Index not yet available. Try again later", "code": 110000, "retry_after": 3})
			return
		}
		writeJSON(w, http.StatusOK, searchBody)
	})
	h.start()
	var resp discordapi.MessagesResponse
	h.callOK(discordapi.PathSearch, discordapi.SearchRequest{GuildID: guildID, Content: "rig"}, &resp)
	hits := h.discord.to(path)
	if len(hits) != 2 || len(resp.MessageIDs) != 2 {
		t.Fatalf("%d search requests, %d ids", len(hits), len(resp.MessageIDs))
	}
	if d := hits[1].at.Sub(hits[0].at); d < 3*time.Second {
		t.Errorf("retried after %v", d)
	}
	if n := len(h.state().Requests); n != 5 {
		t.Errorf("budget counted %d requests, want 5 (both searches)", n)
	}

	// Still not ready after the retry: 503 with a retry hint, no third try.
	h.discord.reply(path, http.StatusAccepted, map[string]any{"code": 110000, "retry_after": 0})
	e := h.callErr(discordapi.PathSearch, discordapi.SearchRequest{GuildID: guildID, Content: "other"}, http.StatusServiceUnavailable)
	if e.RetryAfterSeconds != 5 {
		t.Errorf("retry_after_seconds = %d, want the 5 s default", e.RetryAfterSeconds)
	}
	if n := len(h.discord.to(path)); n != 4 {
		t.Errorf("%d search requests, want 4", n)
	}
}

func TestOwnerStopConditions(t *testing.T) {
	path := api("/channels/" + dmID + "/messages")
	tests := []struct {
		name   string
		status int
		body   any
	}{
		{"401", 401, map[string]any{"message": "401: Unauthorized", "code": 0}},
		{"captcha", 400, map[string]any{"captcha_key": []string{"captcha-required"}, "captcha_sitekey": "a9b5fb07", "captcha_service": "hcaptcha"}},
		{"account verification", 403, map[string]any{"message": "You need to verify your account in order to perform this action.", "code": 40002}},
		{"phone verification", 403, map[string]any{"message": "You need to verify your phone number in order to perform this action.", "code": 70007}},
		{"unauthorized code", 403, map[string]any{"message": "Unauthorized", "code": 40001}},
		{"anti-abuse block", 403, map[string]any{"message": "Unknown Message", "code": 10008}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.discord.reply(path, tt.status, tt.body)
			h.start()
			h.callErr(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID}, http.StatusServiceUnavailable)
			err := h.wait()
			h.cancel()
			h.cancel = nil
			if !errors.Is(err, exitcode.ErrNeedsOwner) {
				t.Fatalf("run returned %v, want ErrNeedsOwner", err)
			}
			if exitcode.For(err) != exitcode.NeedsOwner {
				t.Errorf("exit code %d", exitcode.For(err))
			}
			if n := len(h.discord.to(path)); n != 1 {
				t.Errorf("%d requests after a stop condition", n)
			}
		})
	}
}

func TestDenialCached(t *testing.T) {
	path := api("/channels/" + otherChan + "/messages")
	h := newHarness(t, nil)
	h.discord.reply(path, http.StatusForbidden, map[string]any{"message": "Missing Access", "code": 50001})
	h.start()
	h.callErr(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: otherChan}, http.StatusForbidden)
	before := len(h.discord.all())
	h.callErr(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: otherChan, Before: msg1}, http.StatusForbidden)
	h.restart()
	h.clock.Advance(23 * time.Hour)
	h.callErr(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: otherChan}, http.StatusForbidden)
	h.callErr(discordapi.PathSearch, discordapi.SearchRequest{GuildID: guildID, ChannelID: otherChan, Content: "x"}, http.StatusForbidden)
	if len(h.discord.all()) != before {
		t.Errorf("denied channel requested again: %v", h.discord.paths()[before:])
	}
	if n := len(h.discord.to(path)); n != 1 {
		t.Errorf("%d requests to the denied channel", n)
	}
	// A 404 for an unknown guild channel list is cached the same way.
	h.discord.reply(api("/guilds/"+guildID+"/channels"), http.StatusNotFound, map[string]any{"message": "Unknown Guild", "code": 10004})
	h.callErr(discordapi.PathSync, discordapi.SyncRequest{GuildID: guildID}, http.StatusNotFound)
	h.callErr(discordapi.PathSearch, discordapi.SearchRequest{GuildID: guildID, Content: "x"}, http.StatusNotFound)
	if n := len(h.discord.to(api("/guilds/" + guildID + "/channels"))); n != 1 {
		t.Errorf("%d requests to the denied guild", n)
	}
}

func TestValidation(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	tests := []struct {
		name string
		path string
		body any
	}{
		{"fetch letters", discordapi.PathFetch, map[string]any{"channel_id": "abc"}},
		{"fetch short id", discordapi.PathFetch, map[string]any{"channel_id": "12345678901234"}},
		{"fetch long id", discordapi.PathFetch, map[string]any{"channel_id": "1234567890123456789012"}},
		{"fetch path traversal", discordapi.PathFetch, map[string]any{"channel_id": "../../users/@me"}},
		{"fetch encoded slash", discordapi.PathFetch, map[string]any{"channel_id": "110000000000000%2F"}},
		{"fetch missing", discordapi.PathFetch, map[string]any{}},
		{"fetch bad before", discordapi.PathFetch, map[string]any{"channel_id": dmID, "before": "x"}},
		{"fetch two anchors", discordapi.PathFetch, map[string]any{"channel_id": dmID, "before": msg1, "after": msg2}},
		{"fetch limit too big", discordapi.PathFetch, map[string]any{"channel_id": dmID, "limit": 101}},
		{"fetch negative limit", discordapi.PathFetch, map[string]any{"channel_id": dmID, "limit": -1}},
		{"fetch unknown field", discordapi.PathFetch, map[string]any{"channel_id": dmID, "method": "DELETE"}},
		{"search missing guild", discordapi.PathSearch, map[string]any{"content": "x"}},
		{"search bad guild", discordapi.PathSearch, map[string]any{"guild_id": "1; DROP", "content": "x"}},
		{"search bad author", discordapi.PathSearch, map[string]any{"guild_id": guildID, "author_id": "me"}},
		{"search bad channel", discordapi.PathSearch, map[string]any{"guild_id": guildID, "channel_id": "#general"}},
		{"search bad has", discordapi.PathSearch, map[string]any{"guild_id": guildID, "has": "pdf"}},
		{"search negative offset", discordapi.PathSearch, map[string]any{"guild_id": guildID, "content": "x", "offset": -25}},
		{"search huge offset", discordapi.PathSearch, map[string]any{"guild_id": guildID, "content": "x", "offset": 10000}},
		{"search no criteria", discordapi.PathSearch, map[string]any{"guild_id": guildID}},
		{"search long content", discordapi.PathSearch, map[string]any{"guild_id": guildID, "content": strings.Repeat("a", 513)}},
		{"sync bad guild", discordapi.PathSync, map[string]any{"guild_id": "x"}},
		{"not json", discordapi.PathSync, "{"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.callErr(tt.path, tt.body, http.StatusBadRequest)
		})
	}
	if hits := h.discord.all(); len(hits) != 0 {
		t.Errorf("invalid calls reached Discord: %v", h.discord.paths())
	}
	resp, err := h.client.Get("http://discord" + discordapi.PathFetch)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /fetch: %d", resp.StatusCode)
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
		{"-75928847299117063", false},
		{" 175928847299117063", false},
		{"175928847299117063\n", false},
		{"１７５９２８８４７２９９１１７０６３", false}, // full-width digits
		{"", false},
	}
	for _, tt := range tests {
		if got := isSnowflake(tt.in); got != tt.want {
			t.Errorf("isSnowflake(%q) = %v", tt.in, got)
		}
	}
}

func TestActiveHoursRefusal(t *testing.T) {
	h := newHarness(t, nil)
	h.clock.now = time.Date(2026, 10, 29, 2, 0, 0, 0, newYork)
	h.start()
	e := h.callErr(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID}, http.StatusTooManyRequests)
	// 08:00 local plus the 2 s app-open delay.
	if want := 6*3600 + 2; e.RetryAfterSeconds != want {
		t.Errorf("retry_after_seconds = %d, want %d", e.RetryAfterSeconds, want)
	}
	if !strings.Contains(e.Error, "outside active hours") {
		t.Errorf("error %q", e.Error)
	}
	if len(h.discord.all()) != 0 || !h.clock.Now().Equal(time.Date(2026, 10, 29, 2, 0, 0, 0, newYork)) {
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
				h.callOK(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID}, &discordapi.MessagesResponse{})
				if hits := h.discord.all(); len(hits) != 1 || !hits[0].at.Equal(noon.Add(89*time.Second)) {
					t.Errorf("hits %v", h.discord.paths())
				}
				return
			}
			e := h.callErr(discordapi.PathFetch, discordapi.FetchRequest{ChannelID: dmID}, http.StatusTooManyRequests)
			if e.RetryAfterSeconds != 91 {
				t.Errorf("retry_after_seconds = %d, want 91", e.RetryAfterSeconds)
			}
			if len(h.discord.all()) != 0 || !h.clock.Now().Equal(noon) {
				t.Error("refused call waited or sent a request")
			}
		})
	}
}

func TestStateFileMode(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.callOK(discordapi.PathSync, discordapi.SyncRequest{}, &discordapi.SyncResponse{})
	fi, err := os.Stat(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode %v", fi.Mode().Perm())
	}
	si, err := os.Stat(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	if si.Mode().Perm() != 0o660 {
		t.Errorf("socket mode %v", si.Mode().Perm())
	}
	b, _ := os.ReadFile(h.statePath)
	if bytes.Contains(b, []byte(testToken)) || bytes.Contains(b, []byte("Alice")) {
		t.Error("state file holds the token or names")
	}
}
