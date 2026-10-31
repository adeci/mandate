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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/connector"
	"github.com/adeci/mandate/internal/proposal"
	"github.com/adeci/mandate/internal/store"
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
		{Name: "search", Kind: connector.Read, Params: []connector.Param{
			{Name: "guild_id", Required: true, Container: true},
			{Name: "channel_id", Container: true},
			{Name: "content"},
		}},
		{Name: "fetch", Kind: connector.Read, Params: []connector.Param{
			{Name: "channel_id", Required: true, Container: true},
			{Name: "limit"},
		}},
		{Name: "send", Kind: connector.Write, Params: []connector.Param{
			{Name: "to", Required: true},
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.submitted = append(a.submitted, req)
		p := proposal.Proposal{ID: "p1", Connector: req.Connector, Op: req.Op, Note: req.Note, Status: proposal.Pending}
		a.proposals[p.ID] = p
		writeJSON(w, http.StatusOK, p)
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
		writeJSON(w, http.StatusOK, p)
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
			return connector.Prepared{Args: args, Container: args["to"]}, nil
		},
	}
	a := &fakeConsole{proposals: map[string]proposal.Proposal{}}
	srv := &connector.Server{Connector: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg := Config{
		Sources: fixture(t), ExposureFile: exposure, MaxRows: 100, Timeout: 2 * time.Second,
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

func TestReadRefusesHiddenAndUnknownWithoutConnectorCall(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	for name, tc := range map[string]struct {
		path, body string
		status     int
		msg        string
	}{
		"hidden guild search":         {"/v1/read/discord/search", `{"args":{"guild_id":"g-work","content":"x"}}`, 404, "unknown or hidden"},
		"channel in hidden guild":     {"/v1/read/discord/fetch", `{"args":{"channel_id":"c-general"}}`, 404, "unknown or hidden"},
		"unknown channel":             {"/v1/read/discord/fetch", `{"args":{"channel_id":"999"}}`, 404, "unknown or hidden"},
		"hidden channel in visible g": {"/v1/read/discord/search", `{"args":{"guild_id":"g-fun","channel_id":"c-general"}}`, 404, "unknown or hidden"},
		"hidden guild sync":           {"/v1/read/discord/sync", `{"args":{"guild_id":"g-work"}}`, 404, "unknown or hidden"},
		"undeclared param":            {"/v1/read/discord/fetch", `{"args":{"channel_id":"c-memes","guild":"g-work"}}`, 400, `unknown param \"guild\"`},
		"missing required container":  {"/v1/read/discord/fetch", `{"args":{}}`, 400, `missing required param \"channel_id\"`},
		"write op through read":       {"/v1/read/discord/send", `{"args":{"to":"c-memes","text":"hi"}}`, 404, "no read op"},
		"unconfigured connector":      {"/v1/read/whatsapp/fetch", `{"args":{}}`, 404, `no connector \"whatsapp\"`},
		"unknown request field":       {"/v1/read/discord/fetch", `{"args":{"channel_id":"c-memes"},"evil":1}`, 400, "invalid request"},
	} {
		rec := call(r.h, http.MethodPost, tc.path, tc.body)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.msg) {
			t.Errorf("%s: %d %s, want %d %q", name, rec.Code, rec.Body, tc.status, tc.msg)
		}
	}
	if n := r.discord.reads.Load(); n != 0 {
		t.Fatalf("refused reads reached the connector %d times", n)
	}

	// An absent optional container param is not a container to check.
	if rec := call(r.h, http.MethodPost, "/v1/read/discord/sync", `{"args":{}}`); rec.Code != http.StatusOK {
		t.Errorf("sync without guild: %d %s", rec.Code, rec.Body)
	}
}

func TestReadAnswersThroughFilteredViews(t *testing.T) {
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

	rec := call(r.h, http.MethodPost, "/v1/read/discord/fetch", `{"args":{"channel_id":"c-memes","limit":"50"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch: %d %s", rec.Code, rec.Body)
	}
	var res struct {
		queryResult
		TotalResults int `json:"total_results"`
	}
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &res))
	col := map[string]int{}
	for i, c := range res.Columns {
		col[c] = i
	}
	var ids []string
	for _, row := range res.Rows {
		ids = append(ids, row[col["id"]].(string))
		if row[col["id"]] == "d3" && row[col["body"]] != nil {
			t.Errorf("OTP body served: %v", row[col["body"]])
		}
	}
	if got := strings.Join(ids, ","); got != "d4,d3,d2" {
		t.Errorf("ids = %s, want d4,d3,d2 (connector order; hidden sender d5 and hidden-guild d1 dropped)", got)
	}
	if res.TotalResults != 40 {
		t.Errorf("total_results = %d, want 40", res.TotalResults)
	}
}

func TestReadWithoutMessagesReportsCounts(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.discord.read = func(context.Context, string, map[string]string) (json.RawMessage, error) {
		return json.RawMessage(`{"containers":3}`), nil
	}
	rec := call(r.h, http.MethodPost, "/v1/read/discord/sync", `{"args":{"guild_id":"g-fun"}}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"containers":3,"total_results":0}` {
		t.Errorf("sync: %d %s", rec.Code, rec.Body)
	}
}

func TestReadRelaysConnectorRefusal(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.discord.read = func(context.Context, string, map[string]string) (json.RawMessage, error) {
		return nil, &connector.Error{Status: http.StatusTooManyRequests, Message: "daily budget used", RetryAfter: 3600}
	}
	rec := call(r.h, http.MethodPost, "/v1/read/discord/search", `{"args":{"guild_id":"g-fun","content":"rig"}}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	var e connector.Error
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &e))
	if e.Message != "daily budget used" || e.RetryAfter != 3600 {
		t.Errorf("relayed %+v", e)
	}
}

func TestProposeRefusesHiddenTargetWithoutSubmitting(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	for name, target := range map[string]string{
		"hidden guild":            "g-work",
		"channel in hidden guild": "c-general",
		"hidden person":           "u-hidden",
	} {
		rec := call(r.h, http.MethodPost, "/v1/actions",
			`{"connector":"discord","op":"send","args":{"to":"`+target+`","text":"hi"}}`)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown or hidden") {
			t.Errorf("%s: %d %s, want 404 unknown or hidden", name, rec.Code, rec.Body)
		}
	}
	if n := r.console.calls.Load(); n != 0 {
		t.Fatalf("hidden targets reached the console %d times", n)
	}
}

func TestProposeSubmitsPreparedArgs(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.discord.prepare = func(args map[string]string) (connector.Prepared, error) {
		switch args["to"] {
		case "memes":
			return connector.Prepared{Args: map[string]string{"to": "c-memes", "text": args["text"]}, Container: "c-memes"}, nil
		case "new-dm":
			// A target with no stored row yet is not hidden.
			return connector.Prepared{Args: map[string]string{"to": "dm-7", "text": args["text"]}, Container: "dm-7"}, nil
		}
		return connector.Prepared{}, connector.Invalid("%q matches 2 channels: #memes (Fun), #memes (Work)", args["to"])
	}

	for _, to := range []string{"memes", "new-dm"} {
		rec := call(r.h, http.MethodPost, "/v1/actions",
			`{"connector":"discord","op":"send","args":{"to":"`+to+`","text":"hi"},"note":"asked by Alex"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("propose to %s: %d %s", to, rec.Code, rec.Body)
		}
		var p proposal.Proposal
		mustDo(t, json.Unmarshal(rec.Body.Bytes(), &p))
		if p.ID != "p1" || p.Status != proposal.Pending {
			t.Errorf("proposal %+v", p)
		}
	}
	want := []proposal.Request{
		{Connector: "discord", Op: "send", Args: map[string]string{"to": "c-memes", "text": "hi"}, Note: "asked by Alex"},
		{Connector: "discord", Op: "send", Args: map[string]string{"to": "dm-7", "text": "hi"}, Note: "asked by Alex"},
	}
	r.console.mu.Lock()
	got := r.console.submitted
	r.console.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("submitted %+v", got)
	}
	for i := range want {
		if got[i].Connector != want[i].Connector || got[i].Op != want[i].Op || got[i].Note != want[i].Note || !maps.Equal(got[i].Args, want[i].Args) {
			t.Errorf("submitted %+v, want %+v", got[i], want[i])
		}
	}

	rec := call(r.h, http.MethodPost, "/v1/actions", `{"connector":"discord","op":"send","args":{"to":"#memes","text":"hi"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `\"#memes\" matches 2 channels: #memes (Fun), #memes (Work)`) {
		t.Errorf("ambiguous: %d %s", rec.Code, rec.Body)
	}
	if n := r.console.calls.Load(); n != 2 {
		t.Errorf("console calls = %d, want 2 (ambiguous prepare must not submit)", n)
	}

	for name, body := range map[string]string{
		"read op":           `{"connector":"discord","op":"fetch","args":{"channel_id":"c-memes"}}`,
		"missing text":      `{"connector":"discord","op":"send","args":{"to":"memes"}}`,
		"unknown connector": `{"connector":"bank","op":"send","args":{}}`,
	} {
		if rec := call(r.h, http.MethodPost, "/v1/actions", body); rec.Code/100 != 4 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if n := r.discord.prepares.Load(); n != 3 {
		t.Errorf("prepares = %d, want 3 (invalid requests must not reach the connector)", n)
	}
}

func TestProposeRefusesHiddenAlias(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.discord.prepare = func(args map[string]string) (connector.Prepared, error) {
		// A new DM, found by a name whose other ID is the hidden person.
		return connector.Prepared{Args: args, Container: "dm-new", Aliases: []string{"dm-new-lid", args["to"]}}, nil
	}
	for _, alias := range []string{"u-hidden", "g-work", "c-general"} {
		rec := call(r.h, http.MethodPost, "/v1/actions", `{"connector":"discord","op":"send","args":{"to":"`+alias+`","text":"hi"}}`)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown or hidden") {
			t.Errorf("alias %s: %d %s, want 404", alias, rec.Code, rec.Body)
		}
	}
	if n := r.console.calls.Load(); n != 0 {
		t.Fatalf("hidden aliases reached the console %d times", n)
	}
	rec := call(r.h, http.MethodPost, "/v1/actions", `{"connector":"discord","op":"send","args":{"to":"u2","text":"hi"}}`)
	if rec.Code != http.StatusAccepted {
		t.Errorf("visible aliases: %d %s", rec.Code, rec.Body)
	}
}

func TestAmbiguousNameHidesHiddenCandidates(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	visible := connector.Candidate{ID: "c-memes", Label: "#memes (Fun)"}
	hiddenOnes := []connector.Candidate{
		{ID: "c-general", Label: "#general (Work)"},
		{ID: "dm-3", Aliases: []string{"u-hidden"}, Label: "Hidden Person"},
		{ID: "g-work", Label: "Work"},
	}
	var candidates []connector.Candidate
	r.discord.prepare = func(args map[string]string) (connector.Prepared, error) {
		if args["to"] == "g-work" {
			return connector.Prepared{Args: args, Container: "g-work"}, nil
		}
		return connector.Prepared{}, &connector.Error{Status: http.StatusConflict, Message: `"me" matches several chats`, Candidates: candidates}
	}
	propose := func(to string) *httptest.ResponseRecorder {
		return call(r.h, http.MethodPost, "/v1/actions", `{"connector":"discord","op":"send","args":{"to":"`+to+`","text":"hi"}}`)
	}

	candidates = append([]connector.Candidate{visible}, hiddenOnes...)
	rec := propose("me")
	var e connector.Error
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &e))
	if rec.Code != http.StatusConflict || !strings.Contains(e.Message, "#memes (Fun) (c-memes)") ||
		len(e.Candidates) != 1 || e.Candidates[0].ID != "c-memes" {
		t.Errorf("mixed candidates: %d %s", rec.Code, rec.Body)
	}
	for _, c := range hiddenOnes {
		for _, leak := range append([]string{c.ID, c.Label}, c.Aliases...) {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("hidden candidate leaked %q: %s", leak, rec.Body)
			}
		}
	}

	candidates = hiddenOnes
	rec = propose("me")
	unknown := propose("g-work")
	if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
		t.Errorf("all-hidden candidates: %d %s\nwant exactly the unknown-target answer: %d %s", rec.Code, rec.Body, unknown.Code, unknown.Body)
	}
	if n := r.console.calls.Load(); n != 0 {
		t.Errorf("ambiguous names reached the console %d times", n)
	}
}

func TestUnhealthyConnectorIsNotCalled(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *Config) { c.ConsoleURL = "https://console.example/" })
	read := func() *httptest.ResponseRecorder {
		return call(r.h, http.MethodPost, "/v1/read/discord/fetch", `{"args":{"channel_id":"c-memes"}}`)
	}
	propose := func() *httptest.ResponseRecorder {
		return call(r.h, http.MethodPost, "/v1/actions", `{"connector":"discord","op":"send","args":{"to":"c-memes","text":"hi"}}`)
	}

	r.discord.health = connector.Health{State: connector.NeedsSetup, Detail: "Discord token was revoked"}
	for name, rec := range map[string]*httptest.ResponseRecorder{"read": read(), "propose": propose()} {
		want := "discord needs the owner: Discord token was revoked. Ask them to check https://console.example/connections"
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s while needing setup: %d %s", name, rec.Code, rec.Body)
		}
	}

	r.discord.health = connector.Health{State: connector.Resting, Detail: "daily budget used", Until: time.Now().Add(90 * time.Second)}
	for name, rec := range map[string]*httptest.ResponseRecorder{"read": read(), "propose": propose()} {
		var e connector.Error
		mustDo(t, json.Unmarshal(rec.Body.Bytes(), &e))
		if rec.Code != http.StatusTooManyRequests || e.RetryAfter < 85 || e.RetryAfter > 91 {
			t.Errorf("%s while resting: %d %s, want 429 retry after ~90s", name, rec.Code, rec.Body)
		}
	}

	rec := call(r.h, http.MethodGet, "/v1/schema", "")
	if !strings.Contains(rec.Body.String(), `"health":{"state":"resting","detail":"daily budget used"`) {
		t.Errorf("schema lacks health: %s", rec.Body)
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

	rec := call(r.h, http.MethodGet, "/v1/actions/p7", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	wantJSON, _ := json.Marshal(want)
	if strings.TrimSpace(rec.Body.String()) != string(wantJSON) {
		t.Errorf("got %s\nwant %s", rec.Body, wantJSON)
	}
	if rec := call(r.h, http.MethodGet, "/v1/actions/p8", ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no such proposal") {
		t.Errorf("unknown id: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r.h, http.MethodGet, "/v1/actions/p7%3Fx=1", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("id with query characters: %d %s", rec.Code, rec.Body)
	}
	for _, wait := range []string{"-1", "1.5", "soon"} {
		if rec := call(r.h, http.MethodGet, "/v1/actions/p7?wait="+wait, ""); rec.Code != http.StatusBadRequest {
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
	rec := call(r.h, http.MethodGet, "/v1/actions/p1?wait=30", "")
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
	rec := call(r.h, http.MethodGet, "/v1/actions/p1?wait=1", "")
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
	if rec := call(r.h, http.MethodGet, "/v1/actions/p1", ""); rec.Code != http.StatusOK || time.Since(start) > time.Second {
		t.Errorf("no wait: %d after %v, want an immediate answer", rec.Code, time.Since(start))
	}
}

func TestStatusLongPollStopsWhenAgentLeaves(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.console.set(proposal.Proposal{ID: "p1", Status: proposal.Pending})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/actions/p1?wait=900", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	start := time.Now()
	r.h.ServeHTTP(httptest.NewRecorder(), req)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("long-poll outlived its client by %v", d)
	}
}

func TestActionsDisabledWithoutConsole(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *Config) { c.ConsoleSocket = "" })
	for _, rec := range []*httptest.ResponseRecorder{
		call(r.h, http.MethodPost, "/v1/actions", `{"connector":"discord","op":"send","args":{"to":"c-memes","text":"hi"}}`),
		call(r.h, http.MethodGet, "/v1/actions/p1", ""),
	} {
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not enabled") {
			t.Errorf("%d %s, want a clear 404", rec.Code, rec.Body)
		}
	}
	if n := r.discord.prepares.Load(); n != 0 {
		t.Errorf("prepared %d times without the console", n)
	}
}

func TestSchemaListsReachableConnectors(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *Config) {
		c.Connectors["whatsapp"] = filepath.Join(t.TempDir(), "down.sock")
	})
	rec := call(r.h, http.MethodGet, "/v1/schema", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("schema: %d %s", rec.Code, rec.Body)
	}
	var s struct {
		Connectors []connectorInfo `json:"connectors"`
	}
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &s))
	if len(s.Connectors) != 1 || s.Connectors[0].Name != "discord" || len(s.Connectors[0].Ops) != 4 ||
		s.Connectors[0].Health.State != connector.Healthy {
		t.Errorf("connectors = %+v, want only discord with its ops, healthy", s.Connectors)
	}
	if rec := call(r.h, http.MethodPost, "/v1/read/whatsapp/fetch", `{"args":{}}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("read from down connector: %d %s", rec.Code, rec.Body)
	}
}

// fakeHTTP is shaped like the HTTP proxy connector: it has no data source.
type fakeHTTP struct {
	calls     atomic.Int32
	container string
}

func (f *fakeHTTP) Spec() connector.Spec {
	return connector.Spec{Name: "http", Ops: []connector.Op{
		{Name: "request", Kind: connector.Write, Params: []connector.Param{
			{Name: "service", Required: true},
			{Name: "method", Required: true},
			{Name: "path", Required: true},
			{Name: "body", Editable: true},
		}},
		{Name: "notify", Kind: connector.Write, Params: []connector.Param{{Name: "chat", Container: true}}},
		{Name: "peek", Kind: connector.Read, Params: []connector.Param{{Name: "chat", Container: true}}},
	}}
}

func (f *fakeHTTP) Read(context.Context, string, map[string]string) (json.RawMessage, error) {
	f.calls.Add(1)
	return json.RawMessage(`{}`), nil
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

	rec := call(r.h, http.MethodPost, "/v1/actions",
		`{"connector":"http","op":"request","args":{"service":"github","method":"POST","path":"/repos/a/b/issues","body":"{}"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	r.console.mu.Lock()
	got := r.console.submitted
	r.console.mu.Unlock()
	if len(got) != 1 || got[0].Connector != "http" || got[0].Args["path"] != "/repos/a/b/issues" {
		t.Fatalf("submitted %+v", got)
	}

	for name, tc := range map[string]struct{ path, body string }{
		"write with container param": {"/v1/actions", `{"connector":"http","op":"notify","args":{"chat":"c-memes"}}`},
		"read with container param":  {"/v1/read/http/peek", `{"args":{"chat":"c-memes"}}`},
	} {
		if rec := call(r.h, http.MethodPost, tc.path, tc.body); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no data source") {
			t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("connector calls = %d, want 1 (container args refused before the connector)", n)
	}

	f.container = "c-memes"
	rec = call(r.h, http.MethodPost, "/v1/actions",
		`{"connector":"http","op":"request","args":{"service":"github","method":"GET","path":"/"}}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("prepared container without source: %d %s, want 403", rec.Code, rec.Body)
	}
	if n := r.console.calls.Load(); n != 1 {
		t.Errorf("console calls = %d, want 1", n)
	}
}

func TestConnectorFlagNames(t *testing.T) {
	t.Parallel()
	for v, ok := range map[string]bool{
		"http=/run/h.sock":     true,
		"my-api2=/run/a.sock":  true,
		"Http=/run/h.sock":     false,
		"2fa=/run/x.sock":      false,
		"a/b=/run/x.sock":      false,
		"http=":                false,
		"discord=/run/d.sock":  true,
		"-discord=/run/d.sock": false,
		"disc ord=/run/d.sock": false,
	} {
		if err := (connectorFlag{}).Set(v); (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", v, err, ok)
		}
	}
}
