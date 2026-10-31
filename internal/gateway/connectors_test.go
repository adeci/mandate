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
}

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

type fakeApprovals struct {
	calls     atomic.Int32
	mu        sync.Mutex
	submitted []proposal.Request
	proposals map[string]proposal.Proposal
}

func (a *fakeApprovals) handler() http.Handler {
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
	h         *handler
	discord   *fakeDiscord
	approvals *fakeApprovals
}

func newRig(t *testing.T, edit func(*Config)) *rig {
	t.Helper()
	exposure := filepath.Join(t.TempDir(), "exposure.toml")
	mustDo(t, os.WriteFile(exposure, []byte(testExposure), 0o600))
	d := &fakeDiscord{
		t: t,
		read: func(context.Context, string, map[string]string) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
		prepare: func(args map[string]string) (connector.Prepared, error) {
			return connector.Prepared{Args: args, Container: args["to"]}, nil
		},
	}
	a := &fakeApprovals{proposals: map[string]proposal.Proposal{}}
	srv := &connector.Server{Connector: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg := Config{
		Sources: fixture(t), ExposureFile: exposure, MaxRows: 100, Timeout: 2 * time.Second,
		Connectors:      map[string]string{"discord": serveUnix(t, "discord", srv.Handler())},
		ApprovalsSocket: serveUnix(t, "approvals", a.handler()),
	}
	if edit != nil {
		edit(&cfg)
	}
	return &rig{h: newHandler(cfg, testToken), discord: d, approvals: a}
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
	if n := r.approvals.calls.Load(); n != 0 {
		t.Fatalf("hidden targets reached approvals %d times", n)
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
	r.approvals.mu.Lock()
	got := r.approvals.submitted
	r.approvals.mu.Unlock()
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
	if n := r.approvals.calls.Load(); n != 2 {
		t.Errorf("approvals calls = %d, want 2 (ambiguous prepare must not submit)", n)
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

func TestActionStatusPassThrough(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	want := proposal.Proposal{
		ID: "p7", Connector: "discord", Op: "send", Status: proposal.Done,
		Display:   []approval.Field{{Label: "To", Value: "#memes"}},
		Result:    &connector.Result{Status: connector.Done, Ref: "m9"},
		CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	}
	r.approvals.proposals["p7"] = want

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
}

func TestActionsDisabledWithoutApprovals(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *Config) { c.ApprovalsSocket = "" })
	for _, rec := range []*httptest.ResponseRecorder{
		call(r.h, http.MethodPost, "/v1/actions", `{"connector":"discord","op":"send","args":{"to":"c-memes","text":"hi"}}`),
		call(r.h, http.MethodGet, "/v1/actions/p1", ""),
	} {
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not enabled") {
			t.Errorf("%d %s, want a clear 404", rec.Code, rec.Body)
		}
	}
	if n := r.discord.prepares.Load(); n != 0 {
		t.Errorf("prepared %d times without approvals", n)
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
		Connectors []connector.Spec `json:"connectors"`
	}
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &s))
	if len(s.Connectors) != 1 || s.Connectors[0].Name != "discord" || len(s.Connectors[0].Ops) != 4 {
		t.Errorf("connectors = %+v, want only discord with its ops", s.Connectors)
	}
	if rec := call(r.h, http.MethodPost, "/v1/read/whatsapp/fetch", `{"args":{}}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("read from down connector: %d %s", rec.Code, rec.Body)
	}
}
