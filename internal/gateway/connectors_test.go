package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
	"github.com/adeci/mandate/store"
)

var testToken = strings.Repeat("t", 40)

func serveUnix(t *testing.T, name string, h http.Handler) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), name+".sock")
	ln, err := connector.ListenUnix(socket)
	mustDo(t, err)
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	return socket
}

// fakeDiscord is a connector served over the real protocol.
type fakeDiscord struct {
	t        *testing.T
	reads    atomic.Int32
	prepares atomic.Int32
	read     func(ctx context.Context, op string, args map[string]string) (json.RawMessage, error)
	prepare  func(args map[string]string) (connector.Prepared, error)
	health   connector.Health
}

func (f *fakeDiscord) Health(context.Context) connector.Health { return f.health }

func (f *fakeDiscord) Spec() connector.Spec {
	return connector.Spec{Name: "discord", Ops: []connector.Op{
		{Name: "sync", Kind: connector.Read, Params: []connector.Param{{Name: "guild_id", Container: true}}},
		{Name: "fetch", Kind: connector.Read, Params: []connector.Param{
			{Name: "channel_id", Required: true, Container: true},
			{Name: "limit"},
		}},
		{Name: "send", Kind: connector.Write, Params: []connector.Param{
			{Name: "chat", Required: true, Container: true},
			{Name: "text", Required: true, Editable: true},
		}},
	}}
}

func (f *fakeDiscord) Read(ctx context.Context, op string, args map[string]string) (json.RawMessage, error) {
	f.reads.Add(1)
	return f.read(ctx, op, args)
}

func (f *fakeDiscord) Prepare(_ context.Context, _ string, args map[string]string) (connector.Prepared, error) {
	f.prepares.Add(1)
	return f.prepare(args)
}

func (f *fakeDiscord) Execute(context.Context, approval.Record) connector.Result {
	f.t.Error("gateway executed a write")
	return connector.Result{Status: connector.Failed}
}

type fakeConsole struct {
	calls     atomic.Int32
	mu        sync.Mutex
	submitted []proposal.Request
	proposals map[string]proposal.Proposal
	gets      int
}

func (a *fakeConsole) set(p proposal.Proposal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.proposals[p.ID] = p
}

func (a *fakeConsole) getCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gets
}

func (a *fakeConsole) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /proposals", func(w http.ResponseWriter, r *http.Request) {
		a.calls.Add(1)
		var req proposal.Request
		if !connector.DecodeJSON(w, r, &req, maxRequestBytes) {
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.submitted = append(a.submitted, req)
		p := proposal.Proposal{ID: "p1", Connector: req.Connector, Op: req.Op, Note: req.Note, Status: proposal.Pending}
		a.proposals[p.ID] = p
		connector.WriteJSON(w, http.StatusOK, p)
	})
	mux.HandleFunc("GET /proposals/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.calls.Add(1)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.gets++
		p, ok := a.proposals[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "no such proposal")
			return
		}
		connector.WriteJSON(w, http.StatusOK, p)
	})
	return mux
}

type rig struct {
	h       *handler
	discord *fakeDiscord
	console *fakeConsole
}

func newRig(t *testing.T, edit func(*Config)) *rig {
	t.Helper()
	exposure := filepath.Join(t.TempDir(), "exposure.toml")
	mustDo(t, os.WriteFile(exposure, []byte(testExposure), 0o600))
	d := &fakeDiscord{
		t:      t,
		health: connector.Health{State: connector.Healthy},
		read: func(context.Context, string, map[string]string) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
		prepare: func(args map[string]string) (connector.Prepared, error) {
			return connector.Prepared{Args: args, Container: args["chat"]}, nil
		},
	}
	a := &fakeConsole{proposals: map[string]proposal.Proposal{}}
	srv := &connector.Server{Connector: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg := Config{
		Sources: fixture(t), ExposureFile: exposure, Timeout: 2 * time.Second,
		Connectors:    map[string]string{"discord": serveUnix(t, "discord", srv.Handler())},
		ConsoleSocket: serveUnix(t, "console", a.handler()),
	}
	if edit != nil {
		edit(&cfg)
	}
	return &rig{h: newHandler(cfg, testToken), discord: d, console: a}
}

func call(h *handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestOpsRefuseUnknownAndHiddenChatsBeforeTheConnector(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	for name, tc := range map[string]struct {
		path, body string
		status     int
	}{
		"read in hidden guild":       {"/v1/discord/sync", `{"args":{"guild_id":"g-work"}}`, 404},
		"read in channel of hidden":  {"/v1/discord/fetch", `{"args":{"channel_id":"c-general"}}`, 404},
		"read in unknown channel":    {"/v1/discord/fetch", `{"args":{"channel_id":"999"}}`, 404},
		"send to hidden guild":       {"/v1/discord/send", `{"args":{"chat":"g-work","text":"hi"}}`, 404},
		"send to channel of hidden":  {"/v1/discord/send", `{"args":{"chat":"c-general","text":"hi"}}`, 404},
		"send to unknown chat":       {"/v1/discord/send", `{"args":{"chat":"dm-new","text":"hi"}}`, 404},
		"undeclared param":           {"/v1/discord/fetch", `{"args":{"channel_id":"c-memes","guild":"g-work"}}`, 400},
		"missing required container": {"/v1/discord/fetch", `{"args":{}}`, 400},
		"unknown op":                 {"/v1/discord/delete", `{"args":{}}`, 404},
		"unconfigured connector":     {"/v1/signal/send", `{"args":{}}`, 404},
		"unknown request field":      {"/v1/discord/fetch", `{"args":{"channel_id":"c-memes"},"evil":1}`, 400},
	} {
		rec := call(r.h, http.MethodPost, tc.path, tc.body)
		if rec.Code != tc.status {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.status)
		}
	}
	if n, m, c := r.discord.reads.Load(), r.discord.prepares.Load(), r.console.calls.Load(); n+m+c != 0 {
		t.Fatalf("refused ops reached the connector (%d reads, %d prepares) or console (%d)", n, m, c)
	}
}

func TestReadOpRunsAndAnswersThroughFilter(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	dbPath := r.h.cfg.Sources["discord"]
	r.discord.read = func(ctx context.Context, op string, args map[string]string) (json.RawMessage, error) {
		if op != "fetch" || args["channel_id"] != "c-memes" || args["limit"] != "50" {
			t.Errorf("unexpected read %s %v", op, args)
		}
		// The connector stores what it fetched, including an OTP, a message
		// from a hidden sender, and one from a hidden guild's channel.
		st, err := store.Open(ctx, dbPath)
		mustDo(t, err)
		defer st.Close()
		at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
		for _, m := range []store.Message{
			{ID: "d3", ContainerID: "c-memes", SenderID: "u2", Body: "Your login code is 482913"},
			{ID: "d4", ContainerID: "c-memes", SenderID: "u2", Body: "mounting brackets"},
			{ID: "d5", ContainerID: "c-memes", SenderID: "u-hidden", Body: "do not show"},
		} {
			m.SentAt = at
			mustDo(t, st.UpsertMessage(ctx, m))
		}
		return json.Marshal(connector.ReadResult{MessageIDs: []string{"d4", "d1", "d5", "d3", "d2"}, TotalResults: 40})
	}

	rec := call(r.h, http.MethodPost, "/v1/discord/fetch", `{"args":{"channel_id":"c-memes","limit":"50"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch: %d %s", rec.Code, rec.Body)
	}
	var res table
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &res))
	want := []any{"mounting brackets", "[login code hidden]", "lol"}
	if got := column(t, res, "body"); !slices.Equal(got, want) {
		t.Errorf("bodies = %v, want %v (connector order; hidden sender and hidden guild dropped, OTP replaced)", got, want)
	}
	if res.TotalResults != 40 {
		t.Errorf("total_results = %d, want 40", res.TotalResults)
	}
	if n := r.console.calls.Load(); n != 0 {
		t.Errorf("a read op reached the console %d times", n)
	}
}

func TestReadOpRelaysConnectorRefusal(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.discord.read = func(context.Context, string, map[string]string) (json.RawMessage, error) {
		return nil, &connector.Error{Status: http.StatusTooManyRequests, Message: "daily budget used", RetryAfter: 3600}
	}
	rec := call(r.h, http.MethodPost, "/v1/discord/sync", `{"args":{}}`)
	var e connector.Error
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &e))
	if rec.Code != http.StatusTooManyRequests || e.Message != "daily budget used" || e.RetryAfter != 3600 {
		t.Errorf("relayed %d %+v", rec.Code, e)
	}
}

func TestWriteOpProposesPreparedArgs(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.discord.prepare = func(args map[string]string) (connector.Prepared, error) {
		chat := args["chat"]
		if chat == "g-fun" {
			// A connector that normalizes a target into a hidden one.
			chat = "c-general"
		}
		return connector.Prepared{Args: map[string]string{"chat": chat, "text": strings.TrimSpace(args["text"])}, Container: chat}, nil
	}

	rec := call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"c-memes","text":" hi "},"note":"asked by Alex"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	var p proposal.Proposal
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &p))
	if p.ID != "p1" || p.Status != proposal.Pending {
		t.Errorf("proposal %+v", p)
	}
	r.console.mu.Lock()
	got := r.console.submitted
	r.console.mu.Unlock()
	if len(got) != 1 || got[0].Connector != "discord" || got[0].Op != "send" || got[0].Note != "asked by Alex" ||
		!maps.Equal(got[0].Args, map[string]string{"chat": "c-memes", "text": "hi"}) {
		t.Fatalf("submitted %+v", got)
	}

	if rec := call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"g-fun","text":"hi"}}`); rec.Code != http.StatusNotFound {
		t.Errorf("prepared into a hidden chat: %d %s", rec.Code, rec.Body)
	}
	if n := r.console.calls.Load(); n != 1 {
		t.Errorf("console calls = %d, want 1", n)
	}
}

func TestUnhealthyConnectorIsNotCalled(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *Config) { c.ConsoleURL = "https://console.example/" })
	read := func() *httptest.ResponseRecorder {
		return call(r.h, http.MethodPost, "/v1/discord/fetch", `{"args":{"channel_id":"c-memes"}}`)
	}
	send := func() *httptest.ResponseRecorder {
		return call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"c-memes","text":"hi"}}`)
	}

	r.discord.health = connector.Health{State: connector.NeedsSetup, Detail: "Discord token was revoked"}
	for name, rec := range map[string]*httptest.ResponseRecorder{"read": read(), "send": send()} {
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "https://console.example/connections") {
			t.Errorf("%s while needing setup: %d %s", name, rec.Code, rec.Body)
		}
	}

	r.discord.health = connector.Health{State: connector.Resting, Detail: "daily budget used", Until: time.Now().Add(90 * time.Second)}
	for name, rec := range map[string]*httptest.ResponseRecorder{"read": read(), "send": send()} {
		var e connector.Error
		mustDo(t, json.Unmarshal(rec.Body.Bytes(), &e))
		if rec.Code != http.StatusTooManyRequests || e.RetryAfter < 85 || e.RetryAfter > 91 {
			t.Errorf("%s while resting: %d %s, want 429 retry after ~90s", name, rec.Code, rec.Body)
		}
	}
	if n, m := r.discord.reads.Load(), r.discord.prepares.Load(); n+m != 0 {
		t.Errorf("unhealthy connector called: %d reads, %d prepares", n, m)
	}
	if n := r.console.calls.Load(); n != 0 {
		t.Errorf("console called %d times", n)
	}
}

func TestStatusPassThrough(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	want := proposal.Proposal{
		ID: "p7", Connector: "discord", Op: "send", Status: proposal.Done,
		Display:   []approval.Field{{Label: "To", Value: "#memes"}},
		Result:    &connector.Result{Status: connector.Done, Ref: "m9"},
		CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	}
	r.console.set(want)

	rec := call(r.h, http.MethodGet, "/v1/proposals/p7", "")
	wantJSON, _ := json.Marshal(want)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != string(wantJSON) {
		t.Errorf("got %d %s\nwant %s", rec.Code, rec.Body, wantJSON)
	}
	if rec := call(r.h, http.MethodGet, "/v1/proposals/p8", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r.h, http.MethodGet, "/v1/proposals/p7%3Fx=1", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("id with query characters: %d %s", rec.Code, rec.Body)
	}
	for _, wait := range []string{"-1", "soon"} {
		if rec := call(r.h, http.MethodGet, "/v1/proposals/p7?wait="+wait, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("wait=%s: %d %s", wait, rec.Code, rec.Body)
		}
	}
}

func TestStatusLongPollReturnsOnDecision(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	pending := proposal.Proposal{ID: "p1", Connector: "discord", Op: "send", Status: proposal.Pending}
	r.console.set(pending)
	go func() {
		for r.console.getCount() < 2 {
			time.Sleep(10 * time.Millisecond)
		}
		pending.Status = proposal.Executing
		r.console.set(pending)
		for r.console.getCount() < 3 {
			time.Sleep(10 * time.Millisecond)
		}
		pending.Status = proposal.Rejected
		r.console.set(pending)
	}()
	start := time.Now()
	rec := call(r.h, http.MethodGet, "/v1/proposals/p1?wait=30", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"rejected"`) {
		t.Fatalf("long-poll: %d %s, want rejected", rec.Code, rec.Body)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("long-poll took %v after the decision", d)
	}
}

func TestStatusLongPollTimesOutPending(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.console.set(proposal.Proposal{ID: "p1", Status: proposal.Pending})
	start := time.Now()
	rec := call(r.h, http.MethodGet, "/v1/proposals/p1?wait=1", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"pending"`) {
		t.Fatalf("timed-out long-poll: %d %s, want pending", rec.Code, rec.Body)
	}
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Errorf("wait=1 answered after %v", d)
	}
	if n := r.console.getCount(); n < 2 {
		t.Errorf("console asked %d times, want a re-check before giving up", n)
	}

	start = time.Now()
	if rec := call(r.h, http.MethodGet, "/v1/proposals/p1", ""); rec.Code != http.StatusOK || time.Since(start) > time.Second {
		t.Errorf("no wait: %d after %v, want an immediate answer", rec.Code, time.Since(start))
	}
}

func TestStatusLongPollStopsWhenAgentLeaves(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.console.set(proposal.Proposal{ID: "p1", Status: proposal.Pending})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/proposals/p1?wait=900", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	start := time.Now()
	r.h.ServeHTTP(httptest.NewRecorder(), req)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("long-poll outlived its client by %v", d)
	}
}

func TestWritesDisabledWithoutConsole(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *Config) { c.ConsoleSocket = "" })
	for _, rec := range []*httptest.ResponseRecorder{
		call(r.h, http.MethodPost, "/v1/discord/send", `{"args":{"chat":"c-memes","text":"hi"}}`),
		call(r.h, http.MethodGet, "/v1/proposals/p1", ""),
	} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%d %s, want 404", rec.Code, rec.Body)
		}
	}
	if n := r.discord.prepares.Load(); n != 0 {
		t.Errorf("prepared %d times without the console", n)
	}
}

func TestConnectorsListsDownAndOtherProtocolAsBroken(t *testing.T) {
	t.Parallel()
	var called atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /spec", func(w http.ResponseWriter, _ *http.Request) {
		connector.WriteJSON(w, http.StatusOK, connector.Spec{Protocol: 2, Name: "next", Ops: []connector.Op{
			{Name: "fetch", Kind: connector.Read}, {Name: "send", Kind: connector.Write},
		}})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		connector.WriteJSON(w, http.StatusOK, connector.Health{State: connector.Healthy})
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		connector.WriteJSON(w, http.StatusOK, map[string]any{})
	})
	r := newRig(t, func(c *Config) {
		c.Connectors["next"] = serveUnix(t, "next", mux)
		c.Connectors["whatsapp"] = filepath.Join(t.TempDir(), "down.sock")
	})

	rec := call(r.h, http.MethodGet, "/v1/connectors", "")
	var list []connectorInfo
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &list))
	got := map[string]connectorInfo{}
	for _, c := range list {
		got[c.Name] = c
	}
	if d := got["discord"]; len(d.Ops) != 3 || d.Health.State != connector.Healthy || !d.Source {
		t.Errorf("discord = %+v, want its ops, healthy, a source", d)
	}
	for _, name := range []string{"next", "whatsapp"} {
		if c := got[name]; len(c.Ops) != 0 || c.Health == nil || c.Health.State != connector.Broken {
			t.Errorf("%s = %+v, want listed broken without ops", name, c)
		}
	}
	if rec := call(r.h, http.MethodPost, "/v1/next/send", `{"args":{}}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("op on other protocol: %d %s", rec.Code, rec.Body)
	}
	if n := called.Load(); n != 0 {
		t.Errorf("connector speaking another protocol was called %d times", n)
	}
	if code, _, _ := getTable(t, r.h, "/v1/whatsapp/chats"); code != http.StatusOK {
		t.Errorf("chats of a source whose connector is down: %d", code)
	}
}

// fakeHTTP is a connector with no source of the same name on the gateway.
type fakeHTTP struct {
	calls     atomic.Int32
	container string
}

func (f *fakeHTTP) Spec() connector.Spec {
	return connector.Spec{Name: "http", Ops: []connector.Op{
		{Name: "request", Kind: connector.Write, Params: []connector.Param{
			{Name: "service", Required: true},
			{Name: "path", Required: true},
		}},
		{Name: "notify", Kind: connector.Write, Params: []connector.Param{{Name: "chat", Container: true}}},
	}}
}

func (f *fakeHTTP) Prepare(_ context.Context, _ string, args map[string]string) (connector.Prepared, error) {
	f.calls.Add(1)
	return connector.Prepared{Args: args, Container: f.container}, nil
}

func (f *fakeHTTP) Execute(context.Context, approval.Record) connector.Result {
	return connector.Result{Status: connector.Failed}
}

func TestSourcelessConnectorNeverTargetsContainers(t *testing.T) {
	t.Parallel()
	f := &fakeHTTP{}
	srv := &connector.Server{Connector: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := newRig(t, func(c *Config) { c.Connectors["http"] = serveUnix(t, "http", srv.Handler()) })

	if rec := call(r.h, http.MethodPost, "/v1/http/request", `{"args":{"service":"github","path":"/repos/a/b/issues"}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r.h, http.MethodPost, "/v1/http/notify", `{"args":{"chat":"c-memes"}}`); rec.Code != http.StatusForbidden {
		t.Errorf("container param: %d %s, want 403", rec.Code, rec.Body)
	}
	if code, _, _ := getTable(t, r.h, "/v1/http/chats"); code != http.StatusNotFound {
		t.Errorf("chats of a sourceless connector: %d", code)
	}
	f.container = "c-memes"
	if rec := call(r.h, http.MethodPost, "/v1/http/request", `{"args":{"service":"github","path":"/"}}`); rec.Code != http.StatusForbidden {
		t.Errorf("prepared container without source: %d %s, want 403", rec.Code, rec.Body)
	}
	if n, c := f.calls.Load(), r.console.calls.Load(); n != 2 || c != 1 {
		t.Errorf("connector calls = %d, console calls = %d, want 2 and 1", n, c)
	}
}
