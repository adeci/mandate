package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
	"github.com/adeci/mandate/store"
)

// testAccess knows one agent, holding testToken and grants.
func testAccess(grants ...approval.Grant) *access {
	sum := sha256.Sum256([]byte(testToken))
	a := &access{}
	a.cur.Store(&[]*agent{{Agent: approval.Agent{ID: "a1", Name: "Test Agent"}, sum: sum[:], grants: grants}})
	return a
}

// oneOff is an open grant on connector for the next hour.
func oneOff(id, connector string, horizon time.Duration, chats ...string) approval.Grant {
	now := time.Now()
	return approval.Grant{
		ID: id, Agent: "a1", Connector: connector, Chats: chats, Horizon: horizon,
		From: now.Add(-time.Minute), Until: now.Add(time.Hour),
	}
}

// testHandler opens every source and connector to the test agent.
func testHandler(cfg Config) *handler {
	var grants []approval.Grant
	for _, name := range slices.Sorted(maps.Keys(cfg.Sources)) {
		grants = append(grants, oneOff("g-"+name, name, 0))
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Connectors)) {
		if _, ok := cfg.Sources[name]; !ok {
			grants = append(grants, oneOff("g-"+name, name, 0))
		}
	}
	return newHandler(cfg, testAccess(grants...))
}

func TestNoGrantRefusesReadsAndWrites(t *testing.T) {
	t.Parallel()
	r, f := forwardRig(t, func(http.ResponseWriter) {})
	// An open grant elsewhere and one that ended over a day ago open nothing
	// here, not even writes.
	ended := oneOff("g-ended", "discord", 0)
	ended.From, ended.Until = ended.From.Add(-50*time.Hour), ended.From.Add(-49*time.Hour)
	r.h.access = testAccess(oneOff("g-wa", "whatsapp", 0), ended)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/discord/chats", ""},
		{http.MethodGet, "/v1/discord/messages?chat=c-memes", ""},
		{http.MethodGet, "/v1/discord/search?text=lol", ""},
		{http.MethodGet, "/v1/discord/media?chat=c-memes&seq=2", ""},
		{http.MethodPost, "/v1/discord/fetch", `{"args":{"channel_id":"c-memes"}}`},
		{http.MethodPost, "/v1/discord/sync", `{"args":{}}`},
		{http.MethodGet, "/v1/forward/api/github/user", ""},
		{http.MethodPost, "/v1/discord/send", `{"args":{"chat":"c-memes","text":"hi"}}`},
	} {
		rec := call(r.h, tc.method, tc.path, tc.body)
		var e noAccess
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != http.StatusForbidden || !strings.Contains(e.Error, "no read access to") || !strings.HasPrefix(e.Request, "mandate request ") {
			t.Errorf("%s %s: %d %s, want 403 with the request command", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
	if n, m := r.discord.reads.Load(), r.discord.prepares.Load(); n+m != 0 || f.got.Load() != nil {
		t.Errorf("refused calls reached the connectors: %d reads, %d prepares, forward %v", n, m, f.got.Load())
	}
	if code, _, body := getTable(t, r.h, "/v1/whatsapp/chats"); code != http.StatusOK {
		t.Errorf("whatsapp chats under its grant: %d %s", code, body)
	}
}

// Grants on the same connector combine: each chat is read under the
// longest horizon of the grants that cover it. A grant covers exactly the
// chats it names, not those under them.
func TestGrantScopeHorizonAndUnion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "whatsapp.db")
	st, err := store.Open(t.Context(), path)
	mustDo(t, err)
	now := time.Now()
	old, recent := now.Add(-10*24*time.Hour), now.Add(-time.Hour)
	for _, c := range []store.Container{
		{ID: "c-recent", Kind: "group", Name: "Recent"},
		{ID: "c-quiet", Kind: "group", Name: "Quiet"},
		{ID: "c-parent", Kind: "group", Name: "Parent"},
		{ID: "c-child", Kind: "thread", ParentID: "c-parent", Name: "child"},
	} {
		mustDo(t, st.UpsertContainer(t.Context(), c))
	}
	photo := []store.Media{{Type: "image/jpeg", Name: "cat.jpg", Size: 5}}
	for _, m := range []store.Message{
		{ID: "r-old", ContainerID: "c-recent", Body: "old news", SentAt: old, Media: photo},
		{ID: "r-new", ContainerID: "c-recent", Body: "new news", SentAt: recent, Media: photo},
		{ID: "q-old", ContainerID: "c-quiet", Body: "quiet news", SentAt: old},
		{ID: "k-new", ContainerID: "c-child", Body: "child news", SentAt: recent.Add(-time.Hour)},
	} {
		m.SenderID = "u1"
		mustDo(t, st.UpsertMessage(t.Context(), m))
	}
	mustDo(t, st.Close())
	f := &fakeMedia{files: map[string][]byte{"r-old": []byte("old!!"), "r-new": []byte("new!!")}}
	srv := &connector.Server{Connector: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := newHandler(Config{
		Sources: map[string]string{"whatsapp": path}, ExposureFile: filepath.Join(dir, "exposure.toml"),
		Timeout: testTimeout, MaxMedia: 1000,
		Connectors: map[string]string{"whatsapp": serveUnix(t, "whatsapp", srv.Handler())},
	}, testAccess(oneOff("g-week", "whatsapp", 7*24*time.Hour, "c-recent", "c-parent")))

	bodies := func(path string) []string {
		t.Helper()
		code, res, body := getTable(t, h, path)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		var out []string
		for _, b := range column(t, res, "body") {
			out = append(out, b.(string))
		}
		return out
	}
	chats := func() []any {
		t.Helper()
		_, res, _ := getTable(t, h, "/v1/whatsapp/chats")
		return column(t, res, "id")
	}

	if got := chats(); !slices.Equal(got, []any{"c-recent"}) {
		t.Errorf("chats = %v, want the covered chats active within the horizon", got)
	}
	if got := bodies("/v1/whatsapp/messages?chat=c-recent"); !slices.Equal(got, []string{"[image/jpeg cat.jpg] new news"}) {
		t.Errorf("messages = %q, want only the one within the horizon", got)
	}
	for _, chat := range []string{"c-quiet", "c-child"} {
		if code, _, _ := getTable(t, h, "/v1/whatsapp/messages?chat="+chat); code != http.StatusNotFound {
			t.Errorf("messages in uncovered %s: %d, want 404", chat, code)
		}
	}
	if got := bodies("/v1/whatsapp/search?text=news&since=2000-01-01"); !slices.Equal(got, []string{"[image/jpeg cat.jpg] new news"}) {
		t.Errorf("search = %q, want covered chats within the horizon, whatever --since", got)
	}
	// r-old is seq 1, r-new seq 2.
	if rec := call(h, http.MethodGet, "/v1/whatsapp/media?chat=c-recent&seq=1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("media before the horizon: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, http.MethodGet, "/v1/whatsapp/media?chat=c-recent&seq=2", ""); rec.Code != http.StatusOK || rec.Body.String() != "new!!" {
		t.Errorf("media within the horizon: %d %s", rec.Code, rec.Body)
	}
	var ev proposal.ReadEvent
	for len(h.reads) > 0 {
		ev = <-h.reads
	}
	if ev.AgentID != "a1" || ev.Verb != "media" || ev.Chat != "c-recent" || ev.ChatName != "Recent" ||
		!slices.Equal(ev.Grants, []string{"g-week"}) || ev.Count != 1 {
		t.Errorf("last read event = %+v", ev)
	}

	h.access = testAccess(oneOff("g-week", "whatsapp", 7*24*time.Hour, "c-recent", "c-parent"), oneOff("g-quiet", "whatsapp", 0, "c-quiet"))
	if got := chats(); !slices.Equal(got, []any{"c-recent", "c-quiet"}) {
		t.Errorf("chats under both grants = %v", got)
	}
	if got := bodies("/v1/whatsapp/messages?chat=c-quiet"); !slices.Equal(got, []string{"quiet news"}) {
		t.Errorf("messages in the chat granted without horizon = %q", got)
	}
	if got := bodies("/v1/whatsapp/search?text=news"); !slices.Equal(got, []string{"[image/jpeg cat.jpg] new news", "quiet news"}) {
		t.Errorf("search under both grants = %q", got)
	}
}

func TestScheduleOutsideWindowRefuses(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	opens := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	r.h.access = testAccess(approval.Grant{
		ID: "g-evening", Agent: "a1", Connector: "discord",
		Schedule: &approval.Schedule{Start: opens.Format("15:04"), Length: time.Hour, Zone: "UTC"},
	})
	rec := call(r.h, http.MethodGet, "/v1/discord/chats", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "a grant opens "+opens.Format("2006-01-02 15:04")) {
		t.Errorf("outside the window: %d %s, want 403 naming when it opens", rec.Code, rec.Body)
	}
	rec = call(r.h, http.MethodGet, "/v1/connectors", "")
	var list []connectorInfo
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &list))
	for _, c := range list {
		if c.Name == "discord" && (c.Access.Open || !c.Access.Next.Equal(opens)) {
			t.Errorf("discord access = %+v, want closed until %v", c.Access, opens)
		}
	}
}

// A console that serves the key log and access records.
type recordConsole struct {
	mu         sync.Mutex
	down       bool
	accessDown bool
	log        []approval.SignedChange
	records    []approval.Signed
}

func (c *recordConsole) set(records ...approval.Signed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = records
}

func (c *recordConsole) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.down {
			writeError(w, http.StatusServiceUnavailable, "down")
			return
		}
		connector.WriteJSON(w, http.StatusOK, c.log)
	})
	mux.HandleFunc("GET /access", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.down || c.accessDown {
			writeError(w, http.StatusServiceUnavailable, "down")
			return
		}
		connector.WriteJSON(w, http.StatusOK, map[string]any{"records": c.records})
	})
	return mux
}

func TestRevokedStaysRevoked(t *testing.T) {
	t.Parallel()
	const rpID, origin = "console.example", "https://console.example"
	owner := approvaltest.New(t, rpID, origin)
	stranger := approvaltest.New(t, rpID, origin)
	sum := sha256.Sum256([]byte(testToken))
	now := time.Now()
	sign := func(a *approvaltest.Authenticator, kind string, v any) approval.Signed {
		var b []byte
		var err error
		switch v := v.(type) {
		case approval.Agent:
			b, err = approval.EncodeAgent(v)
		case approval.Grant:
			b, err = approval.EncodeGrant(v)
		}
		mustDo(t, err)
		return a.SignAccess(kind, b, now)
	}
	agentRec := sign(owner, approval.KindAgent, approval.Agent{ID: "a1", Name: "Bot", TokenSHA256: hex.EncodeToString(sum[:]), CreatedAt: now})
	grant := oneOff("g1", "discord", 0)
	grantRec := sign(owner, approval.KindGrant, grant)
	forged := grant
	forged.ID = "g-forged"

	console := &recordConsole{log: approvaltest.Keys(t, owner).Log()}
	console.set(agentRec, grantRec, sign(stranger, approval.KindGrant, forged))
	socket := serveUnix(t, "console", console.handler())
	dir := t.TempDir()
	open := func() *access {
		t.Helper()
		a, err := openAccess(dir, rpID, origin, proposal.Dial(socket), nil)
		mustDo(t, err)
		return a
	}
	grants := func(a *access) []string {
		t.Helper()
		ag := a.find(testToken)
		if ag == nil {
			return nil
		}
		var ids []string
		for _, g := range ag.grants {
			ids = append(ids, g.ID)
		}
		return ids
	}

	a := open()
	mustDo(t, a.sync(t.Context()))
	if got := grants(a); !slices.Equal(got, []string{"g1"}) {
		t.Fatalf("grants = %v, want g1 only (the stranger's grant refused)", got)
	}
	if a.find("wrong") != nil {
		t.Error("an unknown token found an agent")
	}

	console.mu.Lock()
	console.down = true
	console.mu.Unlock()
	if err := a.sync(t.Context()); err == nil {
		t.Error("sync with the console down succeeded")
	}
	if got := grants(a); !slices.Equal(got, []string{"g1"}) {
		t.Errorf("grants with the console down = %v, want the last snapshot", got)
	}
	console.mu.Lock()
	console.down = false
	console.mu.Unlock()

	console.set(agentRec)
	mustDo(t, a.sync(t.Context()))
	console.set(agentRec, grantRec)
	mustDo(t, a.sync(t.Context()))
	if got := grants(a); len(got) != 0 {
		t.Errorf("grants after the revoked one came back = %v", got)
	}
	a = open()
	mustDo(t, a.sync(t.Context()))
	if got := grants(a); len(got) != 0 {
		t.Errorf("grants after a restart = %v", got)
	}

	console.set(grantRec)
	mustDo(t, a.sync(t.Context()))
	console.set(agentRec, grantRec)
	mustDo(t, a.sync(t.Context()))
	if a.find(testToken) != nil {
		t.Error("a deleted agent came back")
	}
	b, err := os.ReadFile(filepath.Join(dir, "access.json"))
	mustDo(t, err)
	var known knownIDs
	mustDo(t, json.Unmarshal(b, &known))
	if !slices.Equal(known.Revoked, []string{"agent:a1", "grant:g1"}) {
		t.Errorf("access.json revoked = %v", known.Revoked)
	}
}

func TestAccessRequestHidesWhatItResolves(t *testing.T) {
	t.Parallel()
	r, _ := forwardRig(t, func(http.ResponseWriter) {})
	r.h.access = testAccess()

	// Whatever matched, the agent gets the same answer; the owner sees which
	// terms matched nothing.
	var answers []string
	for _, terms := range []string{`["mem","plans"]`, `["secret"]`} {
		rec := call(r.h, http.MethodPost, "/v1/access/requests", `{"connector":"discord","chats":`+terms+`,"reason":"summarize memes"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %s: %d %s, want 202", terms, rec.Code, rec.Body)
		}
		answers = append(answers, rec.Body.String())
	}
	if answers[0] != answers[1] {
		t.Errorf("answers differ by match: %q", answers)
	}
	r.console.mu.Lock()
	got := slices.Clone(r.console.requests)
	r.console.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("console got %+v", got)
	}
	if want := []proposal.Chat{{ID: "c-memes", Name: "Fun / memes"}, {ID: "t-cats", Name: "Fun / memes / cats"}}; !slices.Equal(got[0].Chats, want) ||
		!slices.Equal(got[0].Unmatched, []string{"plans"}) ||
		got[0].AgentID != "a1" || got[0].AgentName != "Test Agent" || got[0].For != defaultFor || got[0].Horizon != defaultHorizon {
		t.Errorf("forwarded %+v, want chats %v, plans unmatched and the defaults", got[0], want)
	}
	if len(got[1].Chats) != 0 || !slices.Equal(got[1].Unmatched, []string{"secret"}) {
		t.Errorf("forwarded %+v, want the hidden chat's name unmatched", got[1])
	}

	// Bad input is refused before anything is looked up, whatever it would
	// match: "Fun / memes" names a chat.
	quotas := r.console.quotas.Load()
	long := strings.Repeat("x", maxTerm+1)
	for _, body := range []string{
		`{"connector":"discord","reason":" "}`,
		`{"connector":"discord","for":` + strconv.FormatInt(int64(13*time.Hour), 10) + `,"reason":"x"}`,
		`{"connector":"discord","chats":["` + long + `"],"reason":"x"}`,
		`{"connector":"discord","chats":["Fun / memes ` + long + `"],"reason":"x"}`,
		`{"connector":"discord","chats":["a","b","c","d","e","f","g","h","i","j","k"],"reason":"x"}`,
		`{"connector":"api","chats":["x"],"reason":"x"}`,
	} {
		if rec := call(r.h, http.MethodPost, "/v1/access/requests", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	if n := r.console.quotas.Load(); n != quotas {
		t.Errorf("invalid requests asked the console for quota %d times", n-quotas)
	}
	// Duplicates count once.
	if rec := call(r.h, http.MethodPost, "/v1/access/requests", `{"connector":"discord","chats":["q1","q2","q3","q4","q5","q6","q7","q8","q9","q10","Q1"," q10 "],"reason":"x"}`); rec.Code != http.StatusAccepted {
		t.Errorf("ten terms with duplicates: %d %s", rec.Code, rec.Body)
	}
	// A forwarding connector is asked for whole history.
	if rec := call(r.h, http.MethodPost, "/v1/access/requests", `{"connector":"api","reason":"x"}`); rec.Code != http.StatusAccepted {
		t.Errorf("forward request: %d %s", rec.Code, rec.Body)
	}
	r.console.overQuota.Store(true)
	if rec := call(r.h, http.MethodPost, "/v1/access/requests", `{"connector":"discord","chats":["mem"],"reason":"x"}`); rec.Code != http.StatusTooManyRequests {
		t.Errorf("over quota: %d %s, want 429", rec.Code, rec.Body)
	}
	r.console.mu.Lock()
	if n := len(r.console.requests); n != 4 {
		t.Fatalf("console got %d requests, want only the valid ones under quota", n)
	}
	if req := r.console.requests[2]; len(req.Terms) != 10 || !slices.Equal(req.Terms, req.Unmatched) {
		t.Errorf("deduplicated terms sent %q, unmatched %q; want the 10 distinct ones", req.Terms, req.Unmatched)
	}
	if req := r.console.requests[3]; req.Horizon != 0 {
		t.Errorf("forward request horizon = %v, want whole history", req.Horizon)
	}
	r.console.statuses["r1"] = proposal.AccessStatus{Status: proposal.AccessGranted, AgentID: "a1", GrantID: "g9"}
	r.console.statuses["r2"] = proposal.AccessStatus{Status: proposal.AccessPending, AgentID: "a2"}
	r.console.mu.Unlock()
	if rec := call(r.h, http.MethodGet, "/v1/access/requests/r1", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"granted"`) {
		t.Errorf("own request: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r.h, http.MethodGet, "/v1/access/requests/r2", ""); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "pending") {
		t.Errorf("another agent's request: %d %s, want 404", rec.Code, rec.Body)
	}
}

func TestAccessListsOpenAndComingGrants(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	ended := oneOff("g-ended", "discord", 0)
	ended.From, ended.Until = ended.From.Add(-2*time.Hour), ended.From.Add(-time.Hour)
	r.h.access = testAccess(oneOff("g-memes", "discord", 24*time.Hour, "c-memes", "c-secret"), ended)
	rec := call(r.h, http.MethodGet, "/v1/access", "")
	var got []grantInfo
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &got))
	if len(got) != 1 || got[0].ID != "g-memes" || !got[0].Now.Open || got[0].Horizon != 24*time.Hour ||
		!slices.Equal(got[0].Chats, []chatRef{{ID: "c-memes", Name: "Fun / memes"}, {ID: "c-secret"}}) {
		t.Errorf("access = %d %s, want the open grant with its visible chat named", rec.Code, rec.Body)
	}
}

// Writes target and answer only what the agent may read: unknown, hidden
// and not granted chats get the same 404, before the connector is asked.
func TestWritesNeedGrantOnTheChat(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.h.access = testAccess(oneOff("g", "discord", 0, "c-memes", "g-fun"))
	r.discord.prepare = func(args map[string]string) (connector.Prepared, error) {
		chat := args["chat"]
		if chat == "g-fun" {
			chat = "t-cats"
		}
		return connector.Prepared{Args: args, Container: chat, Message: map[string]string{"2": "d2"}[args["seq"]]}, nil
	}
	rec := call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"c-memes","text":"hi"},"note":"asked by Alex"}`)
	if rec.Code != http.StatusAccepted || strings.Contains(rec.Body.String(), "Alex") || strings.Contains(rec.Body.String(), "display") {
		t.Errorf("send to a granted chat: %d %s, want 202 without note or display", rec.Code, rec.Body)
	}
	for _, chat := range []string{"t-cats", "c-secret", "nope"} {
		rec := call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"`+chat+`","text":"hi"}}`)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown chat") {
			t.Errorf("send to %s: %d %s, want 404 unknown chat", chat, rec.Code, rec.Body)
		}
	}
	if n := r.discord.prepares.Load(); n != 1 {
		t.Errorf("connector prepared %d sends, want only the granted one", n)
	}
	if rec := call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"g-fun","text":"hi"}}`); rec.Code != http.StatusNotFound {
		t.Errorf("prepared into a chat outside the grant: %d %s", rec.Code, rec.Body)
	}

	// d2 was sent before the horizon of a one-hour grant.
	r.h.access = testAccess(oneOff("g", "discord", time.Hour, "c-memes"))
	if rec := call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"c-memes","seq":"2","text":"hi"}}`); rec.Code != http.StatusNotFound {
		t.Errorf("answering a message before the horizon: %d %s", rec.Code, rec.Body)
	}
	if n := r.console.calls.Load(); n != 1 {
		t.Errorf("console got %d proposals, want 1", n)
	}
}

func TestRevokedDuringConnectorReadShowsNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.discord.read = func(context.Context, string, map[string]string) (json.RawMessage, error) {
		r.h.access.cur.Store(testAccess().cur.Load())
		return json.Marshal(connector.ReadResult{MessageIDs: []string{"d2"}})
	}
	rec := call(r.h, http.MethodPost, "/v1/discord/fetch", `{"args":{"channel_id":"c-memes"}}`)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "lol") {
		t.Errorf("read whose grant went away: %d %s, want 403", rec.Code, rec.Body)
	}
}

func TestForwardNeedsGrantOnEverythingWithWholeHistory(t *testing.T) {
	t.Parallel()
	r, f := forwardRig(t, func(http.ResponseWriter) {})
	for _, g := range []approval.Grant{oneOff("g", "api", time.Hour), oneOff("g", "api", 0, "repo")} {
		r.h.access = testAccess(g)
		if rec := call(r.h, http.MethodGet, "/v1/forward/api/github/user", ""); rec.Code != http.StatusForbidden {
			t.Errorf("forward under %+v: %d %s, want 403", g, rec.Code, rec.Body)
		}
	}
	if f.got.Load() != nil {
		t.Error("a refused forward reached the connector")
	}
	r.h.access = testAccess(oneOff("g", "api", 0))
	if rec := call(r.h, http.MethodGet, "/v1/forward/api/github/user", ""); rec.Code != http.StatusOK {
		t.Errorf("forward under an unlimited grant: %d %s", rec.Code, rec.Body)
	}
}

func TestDroppedReadsRideOnTheNextEvent(t *testing.T) {
	t.Parallel()
	h := newRig(t, nil).h
	ctx := withAgent(t.Context(), h.access.agents()[0])
	for range readQueue + 2 {
		h.report(ctx, "discord", "chats", "", "", nil, 1)
	}
	<-h.reads
	h.report(ctx, "discord", "chats", "", "", nil, 1)
	var last proposal.ReadEvent
	for len(h.reads) > 0 {
		last = <-h.reads
	}
	if last.Dropped != 2 {
		t.Errorf("last event dropped = %d, want the 2 reads before it", last.Dropped)
	}
	h.report(ctx, "discord", "chats", "", "", nil, 1)
	if ev := <-h.reads; ev.Dropped != 0 {
		t.Errorf("dropped counted twice: %d", ev.Dropped)
	}
}

// keyPeer is a connector that has pinned a key log.
func keyPeer(t *testing.T, head connector.KeysHead) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) { connector.WriteJSON(w, http.StatusOK, head) })
	return serveUnix(t, "peer", mux)
}

func TestKeyLogBootstrapAndRemovedKey(t *testing.T) {
	t.Parallel()
	const rpID, origin = "console.example", "https://console.example"
	owner, second := approvaltest.New(t, rpID, origin), approvaltest.New(t, rpID, origin)
	keys := approvaltest.Keys(t, owner, second)
	log := keys.Log()
	sum := sha256.Sum256([]byte(testToken))
	now := time.Now()
	agentRec, err := approval.EncodeAgent(approval.Agent{ID: "a1", Name: "Bot", TokenSHA256: hex.EncodeToString(sum[:]), CreatedAt: now})
	mustDo(t, err)
	grantRec, err := approval.EncodeGrant(oneOff("g2", "discord", 0))
	mustDo(t, err)
	console := &recordConsole{log: log}
	console.set(owner.SignAccess(approval.KindAgent, agentRec, now), second.SignAccess(approval.KindGrant, grantRec, now))
	socket := serveUnix(t, "console", console.handler())
	pinned := connector.KeysHead{Length: 2, Head: approval.ChangeHash(log[1].Change)}

	for name, peers := range map[string][]connector.KeysHead{
		"connectors disagree":      {pinned, {Length: 1, Head: approval.ChangeHash(log[0].Change)}},
		"console lacks their log":  {{Length: 2, Head: "feed"}, {Length: 2, Head: "feed"}},
		"console log is too short": {{Length: 3, Head: "feed"}},
	} {
		sockets := map[string]string{}
		for i, h := range peers {
			sockets["c"+strconv.Itoa(i)] = keyPeer(t, h)
		}
		a, err := openAccess(t.TempDir(), rpID, origin, proposal.Dial(socket), sockets)
		mustDo(t, err)
		if err := a.sync(t.Context()); err == nil || a.find(testToken) != nil {
			t.Errorf("%s: sync = %v, want refused with no agents", name, err)
		}
		if n, _ := a.verifier.Keys.Head(); n != 0 {
			t.Errorf("%s: trusted a key log of %d entries", name, n)
		}
	}

	dir := t.TempDir()
	a, err := openAccess(dir, rpID, origin, proposal.Dial(socket), map[string]string{"c0": keyPeer(t, pinned), "c1": keyPeer(t, pinned)})
	mustDo(t, err)
	mustDo(t, a.sync(t.Context()))
	if ag := a.find(testToken); ag == nil || len(ag.grants) != 1 {
		t.Fatalf("after bootstrap: agent %+v, want it with g2", ag)
	}

	// The second key is removed while the console withholds its records: the
	// grant it signed goes at once, for good.
	change, err := keys.Next(approval.OpRemove, second.Key("key2"), now)
	mustDo(t, err)
	mustDo(t, keys.Apply(owner.Sign(change)))
	console.mu.Lock()
	console.log, console.accessDown = keys.Log(), true
	console.mu.Unlock()
	if err := a.sync(t.Context()); err == nil {
		t.Error("sync without access records succeeded")
	}
	if ag := a.find(testToken); ag == nil || len(ag.grants) != 0 {
		t.Errorf("after the key removal: agent %+v, want it without grants", ag)
	}
	b, err := os.ReadFile(filepath.Join(dir, "access.json"))
	mustDo(t, err)
	if !strings.Contains(string(b), "grant:g2") {
		t.Errorf("access.json = %s, want g2 revoked", b)
	}
}

// A message param is checked against the agent's scope before the connector
// looks it up, and the connector's answers about stored messages are not
// relayed, so seq probes tell nothing past the grant.
func TestMessageParamsCheckedBeforeConnector(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	// d2 is seq 2 in c-memes, sent before the horizon of a one-hour grant.
	r.h.access = testAccess(oneOff("g", "discord", time.Hour, "c-memes"))
	for _, seq := range []string{"2", "6", "999"} {
		rec := call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"c-memes","seq":"`+seq+`","text":"hi"}}`)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no message at chat=c-memes seq="+seq) {
			t.Errorf("seq %s: %d %s, want the one 404", seq, rec.Code, rec.Body)
		}
	}
	if n := r.discord.prepares.Load(); n != 0 {
		t.Errorf("connector looked up %d messages the agent may not read", n)
	}

	r.h.access = testAccess(oneOff("g", "discord", 0, "c-memes"))
	r.discord.prepare = func(map[string]string) (connector.Prepared, error) {
		return connector.Prepared{}, &connector.Error{Status: http.StatusGone, Message: "message 2 was deleted on Discord"}
	}
	rec := call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"c-memes","seq":"2","text":"hi"}}`)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "deleted") {
		t.Errorf("connector refusal about a message: %d %s, want the one 404", rec.Code, rec.Body)
	}
}

func TestAncestorNamedOnlyWhenCovered(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.h.access = testAccess(oneOff("g", "discord", 0, "t-cats"))
	_, res, _ := getTable(t, r.h, "/v1/discord/chats")
	if got := column(t, res, "name"); !slices.Equal(got, []any{"cats"}) {
		t.Errorf("thread granted alone is named %v, want its own name", got)
	}
	r.h.access = testAccess(oneOff("g", "discord", 0, "t-cats", "c-memes"))
	_, res, _ = getTable(t, r.h, "/v1/discord/chats?search=cats")
	if got := column(t, res, "name"); !slices.Equal(got, []any{"memes / cats"}) {
		t.Errorf("thread granted with its channel is named %v, want the channel's name before it", got)
	}
}

func TestFailedReadEventsCountAsDropped(t *testing.T) {
	t.Parallel()
	h := newRig(t, nil).h // the fake console has no /access/reads
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.reportReads(ctx)
	h.dropMu.Lock()
	h.dropped["a1"] = 2
	h.dropMu.Unlock()
	h.report(withAgent(t.Context(), h.access.agents()[0]), "discord", "chats", "", "", nil, 1)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		h.dropMu.Lock()
		n := h.dropped["a1"]
		h.dropMu.Unlock()
		if n == 3 {
			return
		}
	}
	t.Error("a read event the console refused was not counted as dropped with what it carried")
}

// Writes may answer what the agent read for a day after its window closed,
// as it was then; a grant ended longer ago, or revoked, no longer counts.
func TestWritesAfterTheWindow(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	ended := func(id string, ago time.Duration, chats ...string) approval.Grant {
		g := oneOff(id, "discord", 0, chats...)
		g.From, g.Until = time.Now().Add(-ago-time.Hour), time.Now().Add(-ago)
		return g
	}
	send := func(chat string) int {
		return call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"`+chat+`","text":"hi"}}`).Code
	}
	r.h.access = testAccess(ended("g-memes", 2*time.Hour, "c-memes"), ended("g-cats", 25*time.Hour, "t-cats"))
	if code := send("c-memes"); code != http.StatusAccepted {
		t.Errorf("send to a chat whose grant ended 2h ago: %d, want 202", code)
	}
	if code := send("t-cats"); code != http.StatusNotFound {
		t.Errorf("send to a chat whose grant ended 25h ago: %d, want 404", code)
	}
	if code, _, _ := getTable(t, r.h, "/v1/discord/messages?chat=c-memes"); code != http.StatusForbidden {
		t.Errorf("read after the window: %d, want 403", code)
	}
	// Revoking drops the grant from the snapshot.
	r.h.access = testAccess(ended("g-cats", 25*time.Hour, "t-cats"), oneOff("g-other", "discord", 0, "t-cats"))
	if code := send("c-memes"); code != http.StatusNotFound {
		t.Errorf("send after the grant was revoked: %d, want 404", code)
	}
}

func TestLastEnd(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Minute)
	daily := approval.Grant{ID: "g", Agent: "a1", Connector: "c", Schedule: &approval.Schedule{
		Start: now.Add(-3 * time.Hour).Format("15:04"), Length: time.Hour, Zone: "UTC",
	}}
	if end, ok := lastEnd(daily, now); !ok || !end.Equal(now.Add(-2*time.Hour)) {
		t.Errorf("daily routine that ran until 2h ago: %v %v", end, ok)
	}
	open := oneOff("g", "c", 0)
	if end, ok := lastEnd(open, now); !ok || !end.Equal(open.Until) {
		t.Errorf("open grant: %v %v, want its end", end, ok)
	}
	future := open
	future.From, future.Until = now.Add(time.Hour), now.Add(2*time.Hour)
	if _, ok := lastEnd(future, now); ok {
		t.Error("a grant not yet open counts for writes")
	}
}

func TestSeenReportedOncePerMinute(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	for range 3 {
		call(r.h, http.MethodGet, "/v1/access", "")
	}
	for deadline := time.Now().Add(5 * time.Second); r.console.seen.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := r.console.seen.Load(); n != 1 {
		t.Errorf("console told %d times the agent was seen, want once", n)
	}
}

func TestRoutineAndExtendRequests(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.h.access = testAccess()
	ns := func(d time.Duration) string { return strconv.FormatInt(int64(d), 10) }
	for _, body := range []string{
		`{"connector":"discord","chats":["mem"],"schedule":{"days":[5,1,1],"start":"07:00","length":` + ns(20*time.Minute) + `},"reason":"standup notes"}`,
		`{"connector":"discord","extend":true,"reason":"still summarizing"}`,
	} {
		if rec := call(r.h, http.MethodPost, "/v1/access/requests", body); rec.Code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	r.console.mu.Lock()
	got := slices.Clone(r.console.requests)
	r.console.mu.Unlock()
	if s := got[0].Schedule; s == nil || !slices.Equal(s.Days, []time.Weekday{time.Monday, time.Friday}) ||
		s.Start != "07:00" || s.Length != 20*time.Minute || got[0].For != 0 || len(got[0].Chats) == 0 {
		t.Errorf("routine forwarded as %+v (schedule %+v)", got[0], got[0].Schedule)
	}
	if e := got[1]; !e.Extend || e.For != defaultFor || e.Horizon != 0 || e.Schedule != nil || len(e.Terms) != 0 {
		t.Errorf("extend forwarded as %+v", e)
	}

	for _, body := range []string{
		`{"connector":"discord","schedule":{"start":"7am","length":` + ns(time.Hour) + `},"reason":"x"}`,
		`{"connector":"discord","schedule":{"start":"07:00","length":` + ns(25*time.Hour) + `},"reason":"x"}`,
		`{"connector":"discord","schedule":{"days":[7],"start":"07:00","length":` + ns(time.Hour) + `},"reason":"x"}`,
		`{"connector":"discord","schedule":{"start":"07:00","length":` + ns(time.Hour) + `},"for":` + ns(time.Hour) + `,"reason":"x"}`,
		`{"connector":"discord","extend":true,"chats":["mem"],"reason":"x"}`,
	} {
		if rec := call(r.h, http.MethodPost, "/v1/access/requests", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
}
