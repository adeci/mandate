package approvals

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/approval/approvaltest"
	"github.com/adeci/mandate/internal/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const (
	rpID   = "box.example.ts.net"
	origin = "https://box.example.ts.net:8781"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fake sends "messages" to a fixed contact book.
type fake struct {
	mu       sync.Mutex
	executed []approval.Record
	// abort drops the connection after "sending", like a network failure
	// mid-request.
	abort atomic.Bool
}

var contacts = map[string]string{"Farid": "id-farid", "Mallory": "id-mallory"}

func (f *fake) Spec() connector.Spec {
	return connector.Spec{Name: "fake", Ops: []connector.Op{
		{Name: "send", Kind: connector.Write, Params: []connector.Param{
			{Name: "to", Required: true, Container: true},
			{Name: "text", Required: true, Editable: true},
		}},
		{Name: "fetch", Kind: connector.Read},
	}}
}

func (f *fake) Prepare(_ context.Context, _ string, args map[string]string) (connector.Prepared, error) {
	to, name := args["to"], ""
	if id, ok := contacts[to]; ok {
		to = id
	}
	for n, id := range contacts {
		if id == to {
			name = n
		}
	}
	if name == "" {
		return connector.Prepared{}, connector.Invalid("no contact %q", args["to"])
	}
	if strings.TrimSpace(args["text"]) == "" {
		return connector.Prepared{}, connector.Invalid("text is empty")
	}
	return connector.Prepared{
		Args:      map[string]string{"to": to, "text": args["text"]},
		Display:   []approval.Field{{Label: "To", Value: name}, {Label: "Text", Value: args["text"], Param: "text"}},
		Container: to,
	}, nil
}

func (f *fake) Execute(_ context.Context, rec approval.Record) connector.Result {
	f.mu.Lock()
	f.executed = append(f.executed, rec)
	f.mu.Unlock()
	if f.abort.Load() {
		panic(http.ErrAbortHandler)
	}
	return connector.Result{Status: connector.Done, Ref: "m1"}
}

func (f *fake) sent() []approval.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.executed)
}

// clock is real time plus an offset, so connectors verifying with the real
// clock still accept fresh approvals.
type clock struct{ off atomic.Int64 }

func (c *clock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *clock) advance(d time.Duration) { c.off.Add(int64(d)) }

type ntfyRecorder struct {
	mu   sync.Mutex
	msgs []recorded
}

type recorded struct {
	auth string
	raw  string
	msg  ntfyMessage
}

func (n *ntfyRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m ntfyMessage
	json.Unmarshal(b, &m)
	n.mu.Lock()
	n.msgs = append(n.msgs, recorded{auth: r.Header.Get("Authorization"), raw: string(b), msg: m})
	n.mu.Unlock()
}

func (n *ntfyRecorder) all() []recorded {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.msgs)
}

type harness struct {
	srv           *Server
	cfg           Config
	pages         *httptest.Server
	admin         *httptest.Server
	api           *proposal.Client
	fake          *fake
	key           *approvaltest.Authenticator
	clock         *clock
	ntfy          *ntfyRecorder
	stopConnector func()
}

// shortDir keeps Unix socket paths under the 108-byte limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mandate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func serveSocket(t *testing.T, path string, h http.Handler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- connector.ServeUnix(ctx, path, h) }()
	for i := 0; ; i++ {
		if _, err := os.Stat(path); err == nil {
			break
		} else if i == 200 {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func start(t *testing.T) *harness {
	t.Helper()
	sockets := shortDir(t)
	key := approvaltest.New(t, rpID, origin)
	verifier := &approval.Verifier{RPID: rpID, Origin: origin, Approvers: []approval.Approver{key.Approver("phone")}}
	seen, err := approval.OpenSeen(filepath.Join(t.TempDir(), "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	connSock := filepath.Join(sockets, "fake.sock")
	stopConnector := serveSocket(t, connSock, (&connector.Server{
		Connector: f, Verifier: verifier, Seen: seen, Log: discard,
	}).Handler())

	rec := &ntfyRecorder{}
	ntfySrv := httptest.NewServer(rec)
	t.Cleanup(ntfySrv.Close)

	clk := &clock{}
	cfg := Config{
		RPID:       rpID,
		Origin:     origin,
		Approvers:  []approval.Approver{key.Approver("phone")},
		Connectors: map[string]string{"fake": connSock},
		StateDir:   t.TempDir(),
		Ntfy:       &Ntfy{URL: ntfySrv.URL, Topic: "mandate", Token: "tk_secret"},
		Log:        discard,
		Now:        clk.now,
	}
	srv, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	apiSock := filepath.Join(sockets, "api.sock")
	serveSocket(t, apiSock, srv.ProposalHandler())
	pages := httptest.NewServer(srv.PageHandler())
	t.Cleanup(pages.Close)
	admin := httptest.NewServer(srv.AdminHandler())
	t.Cleanup(admin.Close)

	return &harness{
		srv: srv, cfg: cfg, pages: pages, admin: admin, api: proposal.Dial(apiSock),
		fake: f, key: key, clock: clk, ntfy: rec, stopConnector: stopConnector,
	}
}

func (h *harness) submit(t *testing.T, text string) proposal.Proposal {
	t.Helper()
	p, err := h.api.Submit(context.Background(), proposal.Request{
		Connector: "fake", Op: "send", Args: map[string]string{"to": "Farid", "text": text}, Note: "she asked",
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *harness) get(t *testing.T, id string) proposal.Proposal {
	t.Helper()
	p, err := h.api.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (h *harness) post(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirect.Post(h.pages.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// challenge asks for request options and returns the challenge to sign.
func (h *harness) challenge(t *testing.T, id string, edits map[string]string) []byte {
	t.Helper()
	status, body := h.post(t, "/p/"+id+"/challenge", challengeRequest{Edits: edits})
	if status != http.StatusOK {
		t.Fatalf("challenge: %d %s", status, body)
	}
	var opts requestOptions
	if err := json.Unmarshal(body, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.RPID != rpID || opts.UserVerification != "required" || len(opts.AllowCredentials) != 1 ||
		opts.AllowCredentials[0].ID != h.key.Approver("").CredentialID {
		t.Fatalf("request options = %+v", opts)
	}
	c, err := base64.RawURLEncoding.DecodeString(opts.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (h *harness) approve(t *testing.T, id string, assertion json.RawMessage) (int, proposal.Proposal) {
	t.Helper()
	status, body := h.post(t, "/p/"+id+"/approve", approveRequest{Assertion: assertion})
	var p proposal.Proposal
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
	}
	return status, p
}

func (h *harness) kinds(t *testing.T) []string {
	t.Helper()
	evs, err := h.srv.db.events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestApproveExecutesOnce(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	if p.Status != proposal.Pending || p.Display[0].Value != "Farid" || p.Note != "she asked" {
		t.Fatalf("submitted = %+v", p)
	}
	if !p.ExpiresAt.Equal(p.CreatedAt.Add(24 * time.Hour)) {
		t.Fatalf("ttl = %v", p.ExpiresAt.Sub(p.CreatedAt))
	}

	assertion := h.key.Assert(h.challenge(t, p.ID, nil))
	status, done := h.approve(t, p.ID, assertion)
	if status != http.StatusOK || done.Status != proposal.Done || done.Result == nil || done.Result.Ref != "m1" {
		t.Fatalf("approve = %d %+v", status, done)
	}
	sent := h.fake.sent()
	if len(sent) != 1 {
		t.Fatalf("executed %d times", len(sent))
	}
	stored, err := h.srv.db.get(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := approval.Decode(stored.Record)
	if err != nil {
		t.Fatal(err)
	}
	if got := sent[0]; got.ID != p.ID || got.Args["to"] != "id-farid" || got.Args["text"] != "running late" ||
		got.Note != "she asked" || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("executed %+v, approved %+v", got, want)
	}

	if status, _ := h.approve(t, p.ID, assertion); status != http.StatusConflict {
		t.Fatalf("replayed approve = %d, want 409", status)
	}
	if status, _ := h.post(t, "/p/"+p.ID+"/challenge", challengeRequest{}); status != http.StatusConflict {
		t.Fatalf("challenge after done = %d, want 409", status)
	}
	if n := len(h.fake.sent()); n != 1 {
		t.Fatalf("executed %d times", n)
	}
	if got := h.get(t, p.ID); got.Status != proposal.Done || got.Result.Ref != "m1" {
		t.Fatalf("get = %+v", got)
	}
	if got, want := h.kinds(t), []string{evProposed, evApproved, evExecuted}; !slices.Equal(got, want) {
		t.Fatalf("audit = %v, want %v", got, want)
	}
	if err := h.srv.db.verifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentApproveExecutesOnce(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	assertion := h.key.Assert(h.challenge(t, p.ID, nil))

	body, _ := json.Marshal(approveRequest{Assertion: assertion})
	var wg sync.WaitGroup
	statuses := make([]int, 8)
	for i := range statuses {
		wg.Go(func() {
			resp, err := http.Post(h.pages.URL+"/p/"+p.ID+"/approve", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			statuses[i] = resp.StatusCode
		})
	}
	wg.Wait()
	ok := 0
	for _, s := range statuses {
		switch s {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
		default:
			t.Fatalf("statuses = %v", statuses)
		}
	}
	if ok != 1 || len(h.fake.sent()) != 1 {
		t.Fatalf("statuses = %v, executed %d times", statuses, len(h.fake.sent()))
	}
}

func TestEditReachesExecutedRecord(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")

	// A first challenge with the original text, superseded by the edit.
	stale := h.key.Assert(h.challenge(t, p.ID, nil))
	assertion := h.key.Assert(h.challenge(t, p.ID, map[string]string{"text": "on my way"}))
	if status, _ := h.approve(t, p.ID, stale); status != http.StatusForbidden {
		t.Fatalf("superseded challenge = %d, want 403", status)
	}
	status, done := h.approve(t, p.ID, assertion)
	if status != http.StatusOK || done.Status != proposal.Done {
		t.Fatalf("approve = %d %+v", status, done)
	}
	sent := h.fake.sent()
	if len(sent) != 1 || sent[0].Args["text"] != "on my way" || sent[0].Display[1].Value != "on my way" {
		t.Fatalf("executed %+v", sent)
	}
	if got := h.get(t, p.ID); got.Display[1].Value != "on my way" {
		t.Fatalf("proposal display = %+v", got.Display)
	}
	evs, err := h.srv.db.events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evs[1].Kind != evEdited || string(evs[1].Detail) != `{"params":["text"]}` {
		t.Fatalf("edit event = %+v", evs[1])
	}
}

func TestEditNonEditableRefused(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	for _, edits := range []map[string]string{
		{"to": "Mallory"},
		{"to": "id-farid"},
		{"text": "fine", "to": "Mallory"},
		{"bcc": "Mallory"},
	} {
		if status, body := h.post(t, "/p/"+p.ID+"/challenge", challengeRequest{Edits: edits}); status != http.StatusBadRequest {
			t.Errorf("edits %v = %d %s, want 400", edits, status, body)
		}
	}
	// Nothing was stored to approve.
	if status, _ := h.approve(t, p.ID, h.key.Assert([]byte("anything"))); status != http.StatusConflict {
		t.Fatalf("approve without challenge = %d, want 409", status)
	}
}

func TestApprovalRefused(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	c := h.challenge(t, p.ID, nil)

	stranger := approvaltest.New(t, rpID, origin)
	noUV := approvaltest.New(t, rpID, origin)
	noUV.Flags = approvaltest.FlagUP
	h.srv.cfg.Approvers = append(h.srv.cfg.Approvers, noUV.Approver("no-uv"))
	h.srv.verifier.Approvers = h.srv.cfg.Approvers
	phishing := approvaltest.New(t, rpID, "https://evil.example")
	h.srv.verifier.Approvers = append(h.srv.verifier.Approvers, phishing.Approver("phished"))

	for name, assertion := range map[string]json.RawMessage{
		"unenrolled key":      stranger.Assert(c),
		"other challenge":     h.key.Assert([]byte("some other challenge")),
		"no user verified":    noUV.Assert(c),
		"wrong origin":        phishing.Assert(c),
		"not a webauthn blob": json.RawMessage(`{"id":"x"}`),
	} {
		if status, _ := h.approve(t, p.ID, assertion); status != http.StatusForbidden {
			t.Errorf("%s: approve = %d, want 403", name, status)
		}
	}
	if n := len(h.fake.sent()); n != 0 {
		t.Fatalf("executed %d times", n)
	}
	if got := h.get(t, p.ID); got.Status != proposal.Pending {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestEnrollmentOnlyCannotApprove(t *testing.T) {
	h := start(t)
	h.srv.cfg.Approvers, h.srv.verifier.Approvers = nil, nil
	p := h.submit(t, "running late")
	if status, _ := h.post(t, "/p/"+p.ID+"/challenge", challengeRequest{}); status != http.StatusServiceUnavailable {
		t.Fatalf("challenge = %d, want 503", status)
	}
}

func TestExpiry(t *testing.T) {
	h := start(t)

	t.Run("stale challenge", func(t *testing.T) {
		p := h.submit(t, "running late")
		assertion := h.key.Assert(h.challenge(t, p.ID, nil))
		h.clock.advance(approval.MaxAge + 2*time.Minute)
		if status, _ := h.approve(t, p.ID, assertion); status != http.StatusForbidden {
			t.Fatalf("stale approve = %d, want 403", status)
		}
	})

	t.Run("proposal", func(t *testing.T) {
		p := h.submit(t, "running late")
		assertion := h.key.Assert(h.challenge(t, p.ID, nil))
		h.clock.advance(24 * time.Hour)
		if err := h.srv.db.expireDue(context.Background(), h.srv.now()); err != nil {
			t.Fatal(err)
		}
		if got := h.get(t, p.ID); got.Status != proposal.Expired {
			t.Fatalf("status = %s, want expired", got.Status)
		}
		if status, _ := h.approve(t, p.ID, assertion); status != http.StatusConflict {
			t.Fatalf("approve = %d, want 409", status)
		}
		if status, _ := h.post(t, "/p/"+p.ID+"/challenge", challengeRequest{}); status != http.StatusConflict {
			t.Fatalf("challenge = %d, want 409", status)
		}
	})

	t.Run("found on read without a sweep", func(t *testing.T) {
		p := h.submit(t, "running late")
		h.clock.advance(24*time.Hour + time.Second)
		if got := h.get(t, p.ID); got.Status != proposal.Expired {
			t.Fatalf("status = %s, want expired", got.Status)
		}
	})

	if n := len(h.fake.sent()); n != 0 {
		t.Fatalf("executed %d times", n)
	}
	if got := h.kinds(t); slices.Index(got, evExpired) < 0 {
		t.Fatalf("audit = %v, want an expired event", got)
	}
}

func TestPendingCap(t *testing.T) {
	h := start(t)
	var first proposal.Proposal
	for i := range maxPending {
		p := h.submit(t, "hi")
		if i == 0 {
			first = p
		}
	}
	_, err := h.api.Submit(context.Background(), proposal.Request{
		Connector: "fake", Op: "send", Args: map[string]string{"to": "Farid", "text": "one more"},
	})
	var e *connector.Error
	if !errors.As(err, &e) || e.Status != http.StatusTooManyRequests {
		t.Fatalf("submit over cap = %v, want 429", err)
	}
	if status, _ := h.post(t, "/p/"+first.ID+"/reject", nil); status != http.StatusSeeOther {
		t.Fatalf("reject = %d", status)
	}
	h.submit(t, "now there's room")

	// Expired proposals free their slot too.
	h.clock.advance(25 * time.Hour)
	h.submit(t, "a day later")
}

func TestReject(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	assertion := h.key.Assert(h.challenge(t, p.ID, nil))

	status, _ := h.post(t, "/p/"+p.ID+"/reject", nil)
	if status != http.StatusSeeOther {
		t.Fatalf("reject = %d, want 303", status)
	}
	if got := h.get(t, p.ID); got.Status != proposal.Rejected {
		t.Fatalf("status = %s", got.Status)
	}
	if status, _ := h.approve(t, p.ID, assertion); status != http.StatusConflict {
		t.Fatalf("approve after reject = %d, want 409", status)
	}
	if status, _ := h.post(t, "/p/"+p.ID+"/reject", nil); status != http.StatusConflict {
		t.Fatalf("second reject = %d, want 409", status)
	}
	if status, _ := h.post(t, "/p/nope/reject", nil); status != http.StatusNotFound {
		t.Fatalf("reject unknown = %d, want 404", status)
	}
	if n := len(h.fake.sent()); n != 0 {
		t.Fatalf("executed %d times", n)
	}
}

func TestConnectorFailures(t *testing.T) {
	t.Run("connection lost mid-execute is unknown", func(t *testing.T) {
		h := start(t)
		h.fake.abort.Store(true)
		p := h.submit(t, "running late")
		status, got := h.approve(t, p.ID, h.key.Assert(h.challenge(t, p.ID, nil)))
		if status != http.StatusOK || got.Status != proposal.Unknown {
			t.Fatalf("approve = %d %+v, want unknown", status, got)
		}
		if n := len(h.fake.sent()); n != 1 {
			t.Fatalf("executed %d times; unknown must never retry", n)
		}
	})

	t.Run("unreachable connector is failed", func(t *testing.T) {
		h := start(t)
		p := h.submit(t, "running late")
		assertion := h.key.Assert(h.challenge(t, p.ID, nil))
		h.stopConnector()
		status, got := h.approve(t, p.ID, assertion)
		if status != http.StatusOK || got.Status != proposal.Failed {
			t.Fatalf("approve = %d %+v, want failed", status, got)
		}
	})

	t.Run("interrupted execution is unknown after restart", func(t *testing.T) {
		h := start(t)
		p := h.submit(t, "running late")
		h.challenge(t, p.ID, nil)
		stored, err := h.srv.db.get(context.Background(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := approval.Decode(stored.Record)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.srv.db.claim(context.Background(), p.ID, stored.Nonce, rec, "phone", h.srv.now()); err != nil {
			t.Fatal(err)
		}
		h.srv.Close()
		srv, err := New(context.Background(), h.cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()
		got, err := srv.db.get(context.Background(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != proposal.Unknown {
			t.Fatalf("status = %s, want unknown", got.Status)
		}
	})
}

func TestSubmitRefusals(t *testing.T) {
	h := start(t)
	for _, tt := range []struct {
		name   string
		req    proposal.Request
		status int
		msg    string
	}{
		{"unknown connector", proposal.Request{Connector: "telegram", Op: "send"}, http.StatusNotFound, "no connector"},
		{"read op", proposal.Request{Connector: "fake", Op: "fetch"}, http.StatusNotFound, "no write op"},
		{"unknown op", proposal.Request{Connector: "fake", Op: "delete"}, http.StatusNotFound, "no write op"},
		{"connector refuses", proposal.Request{Connector: "fake", Op: "send", Args: map[string]string{"to": "Eve", "text": "x"}}, http.StatusBadRequest, `no contact "Eve"`},
	} {
		_, err := h.api.Submit(context.Background(), tt.req)
		var e *connector.Error
		if !errors.As(err, &e) || e.Status != tt.status || !strings.Contains(e.Message, tt.msg) {
			t.Errorf("%s: err = %v, want %d %q", tt.name, err, tt.status, tt.msg)
		}
	}
	if _, err := h.api.Get(context.Background(), "nope"); err == nil {
		t.Fatal("get unknown proposal succeeded")
	}
	if n := len(h.ntfy.all()); n != 0 {
		t.Fatalf("%d notifications for refused proposals", n)
	}
}

func TestNtfyCarriesNoBody(t *testing.T) {
	h := start(t)
	p := h.submit(t, "the code is 4711, burn after reading")
	msgs := h.ntfy.all()
	if len(msgs) != 1 {
		t.Fatalf("%d notifications", len(msgs))
	}
	m := msgs[0]
	if strings.Contains(m.raw, "4711") || strings.Contains(m.raw, "burn") || strings.Contains(m.raw, "she asked") {
		t.Fatalf("notification leaks content: %s", m.raw)
	}
	want := ntfyMessage{Topic: "mandate", Title: "fake send", Message: "to Farid: waiting for approval", Click: origin + "/p/" + p.ID}
	if m.msg != want || m.auth != "Bearer tk_secret" {
		t.Fatalf("notification = %+v auth %q, want %+v", m.msg, m.auth, want)
	}
}

func TestNtfyFailureIsNotFatal(t *testing.T) {
	h := start(t)
	h.srv.cfg.Ntfy.URL = "http://127.0.0.1:1"
	if p := h.submit(t, "hi"); p.Status != proposal.Pending {
		t.Fatalf("status = %s", p.Status)
	}
}

func TestPages(t *testing.T) {
	h := start(t)
	p := h.submit(t, "<script>alert(1)</script>")

	for _, path := range []string{"/", "/p/" + p.ID, "/p/nope", "/enroll/nope", "/static/app.js"} {
		resp, err := http.Get(h.pages.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "script-src 'self'"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: CSP %q lacks %q", path, got, want)
			}
		}
		if strings.Contains(got, "unsafe") {
			t.Errorf("%s: CSP %q allows unsafe sources", path, got)
		}
		if strings.Contains(resp.Header.Get("Content-Type"), "html") &&
			strings.Count(string(body), "<script") != strings.Count(string(body), `<script src="/static/app.js"`) {
			t.Errorf("%s: page has an inline script:\n%s", path, body)
		}
	}

	resp, err := http.Get(h.pages.URL + "/p/" + p.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"Farid", "untrusted", "she asked", `data-param="text"`, "&lt;script&gt;alert(1)&lt;/script&gt;", "data-approve"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("proposal page lacks %q", want)
		}
	}

	// A cross-site form post can't reject on the owner's behalf.
	req, _ := http.NewRequest(http.MethodPost, h.pages.URL+"/p/"+p.ID+"/reject", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site reject = %d, want 403", resp.StatusCode)
	}
	if got := h.get(t, p.ID); got.Status != proposal.Pending {
		t.Fatalf("status = %s", got.Status)
	}
}
