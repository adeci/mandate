package console

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

	"golang.org/x/crypto/bcrypt"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const (
	rpID   = "box.example.ts.net"
	origin = "https://box.example.ts.net"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fake sends "messages" to a fixed contact book. Linking shows a QR code, or
// a code to type with the optional "account" setup input; a "token" links
// at once.
type fake struct {
	name     string
	mu       sync.Mutex
	executed []approval.Record
	// abort drops the connection after "sending", like a network failure
	// mid-request.
	abort atomic.Bool

	linked     bool
	setupCalls int
	unlinks    int
}

var contacts = map[string]string{"Farid": "id-farid", "Mallory": "id-mallory"}

func (f *fake) Spec() connector.Spec {
	return connector.Spec{Name: f.name, Ops: []connector.Op{
		{Name: "send", Kind: connector.Write, Params: []connector.Param{
			{Name: "to", Required: true, Container: true},
			{Name: "text", Required: true, Editable: true},
		}},
		{Name: "fetch", Kind: connector.Read},
	}, Setup: []connector.Param{
		{Name: "account", Doc: "link by typing a code for this account"},
		{Name: "token", Secret: true},
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
	// Length derives from the editable arg, so edits must re-render the display.
	return connector.Prepared{
		Args: map[string]string{"to": to, "text": args["text"]},
		Display: []approval.Field{
			{Label: "To", Value: name},
			{Label: "Text", Value: args["text"], Param: "text"},
			{Label: "Length", Value: strings.Repeat("#", len(args["text"])%40)},
		},
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
	return connector.Result{Status: connector.Done, Ref: "m1", Output: "<b>sent</b>"}
}

func (f *fake) Health(context.Context) connector.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.linked {
		return connector.Health{State: connector.NeedsSetup, Detail: "not linked"}
	}
	return connector.Health{State: connector.Healthy}
}

// Setup hands out a fresh QR code per poll and links on the third. The
// account "compare" asks whether two codes match instead.
func (f *fake) Setup(_ context.Context, args map[string]string) (connector.SetupStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.linked {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "already linked"}
	}
	if args["token"] != "" {
		f.linked = true
		return connector.SetupStep{Done: true}, nil
	}
	if answer, ok := args[connector.SetupConfirm]; ok {
		return connector.SetupStep{Instructions: "answered " + answer}, nil
	}
	if args["account"] == "compare" {
		return connector.SetupStep{Code: "🐶 🔑", Instructions: "Dog, Key", Confirm: "Same emoji?"}, nil
	}
	f.setupCalls++
	if f.setupCalls == 3 {
		f.linked = true
		return connector.SetupStep{Done: true}, nil
	}
	if account := args["account"]; account != "" {
		return connector.SetupStep{Code: "K7QF-2M9X", Instructions: "type it on " + account}, nil
	}
	return connector.SetupStep{QR: "2@ref" + strings.Repeat("x", f.setupCalls), Instructions: "scan it"}, nil
}

func (f *fake) Unlink(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linked, f.setupCalls = false, 0
	f.unlinks++
	return nil
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

type harness struct {
	srv     *Server
	cfg     Config
	pages   *httptest.Server
	api     *proposal.Client
	fake    *fake
	key     *approvaltest.Authenticator
	clock   *clock
	session string
	// connKeys is the fake connector's key log.
	connKeys      *approval.Keys
	stopConnector func()
}

const password = "correct horse"

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

func emptyKeys(t *testing.T) *approval.Keys {
	t.Helper()
	k, err := approval.OpenKeys(filepath.Join(t.TempDir(), "keys.json"), rpID, origin)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// serveFake runs a fake connector called name that trusts keys.
func serveFake(t *testing.T, sock, name string, keys *approval.Keys) (*fake, func()) {
	t.Helper()
	seen, err := approval.OpenSeen(filepath.Join(t.TempDir(), "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{name: name}
	return f, serveSocket(t, sock, (&connector.Server{Connector: f, Keys: keys, Seen: seen, Log: discard}).Handler())
}

// start runs a console whose first key, "phone", was added with the
// password, and signs in with that key.
func start(t *testing.T) *harness {
	t.Helper()
	return startWith(t, nil)
}

// startWith lets edit add connectors with sockets in dir before the console
// starts.
func startWith(t *testing.T, edit func(cfg *Config, dir string)) *harness {
	t.Helper()
	h := bare(t, edit)
	if !h.addKey(t, h.session, h.key, "phone") {
		t.Fatal("first key is not trusted")
	}
	h.session = h.login(t, h.key)
	return h
}

// bare runs a console with no keys, signed in with the password.
func bare(t *testing.T, edit func(cfg *Config, dir string)) *harness {
	t.Helper()
	sockets := shortDir(t)
	connKeys := emptyKeys(t)
	connSock := filepath.Join(sockets, "fake.sock")
	f, stopConnector := serveFake(t, connSock, "fake", connKeys)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	clk := &clock{}
	cfg := Config{
		RPID:         rpID,
		Origin:       origin,
		PasswordHash: hash,
		Connectors:   map[string]string{"fake": connSock},
		StateDir:     shortDir(t),
		Log:          discard,
		Now:          clk.now,
	}
	if edit != nil {
		edit(&cfg, sockets)
	}
	srv, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	apiSock := filepath.Join(sockets, "api.sock")
	serveSocket(t, apiSock, srv.ProposalHandler())
	serveSocket(t, adminSocket(cfg.StateDir), srv.AdminHandler())
	pages := httptest.NewServer(srv.Handler())
	t.Cleanup(pages.Close)

	h := &harness{
		srv: srv, cfg: cfg, pages: pages, api: proposal.Dial(apiSock),
		fake: f, key: approvaltest.New(t, rpID, origin), clock: clk, connKeys: connKeys, stopConnector: stopConnector,
	}
	h.session = h.passwordLogin(t)
	return h
}

func (h *harness) submit(t *testing.T, text string) proposal.Proposal {
	t.Helper()
	return h.submitTo(t, "fake", text)
}

func (h *harness) submitTo(t *testing.T, conn, text string) proposal.Proposal {
	t.Helper()
	p, err := h.api.Submit(context.Background(), proposal.Request{
		Connector: conn, Op: "send", Args: map[string]string{"to": "Farid", "text": text}, Note: "she asked",
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

func (h *harness) do(t *testing.T, method, path, token string, body any) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.pages.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (h *harness) call(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	resp := h.do(t, method, path, token, body)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (h *harness) post(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	return h.call(t, http.MethodPost, path, h.session, body)
}

func (h *harness) page(t *testing.T, path string) (int, string) {
	t.Helper()
	status, body := h.call(t, http.MethodGet, path, h.session, nil)
	return status, string(body)
}

func (h *harness) loginChallenge(t *testing.T) []byte {
	t.Helper()
	status, body := h.call(t, http.MethodPost, "/login/options", "", nil)
	if status != http.StatusOK {
		t.Fatalf("login options: %d %s", status, body)
	}
	var opts requestOptions
	if err := json.Unmarshal(body, &opts); err != nil {
		t.Fatal(err)
	}
	c, err := base64.RawURLEncoding.DecodeString(opts.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (h *harness) loginAssertion(t *testing.T, key *approvaltest.Authenticator) json.RawMessage {
	t.Helper()
	return key.Assert(h.loginChallenge(t))
}

func (h *harness) login(t *testing.T, key *approvaltest.Authenticator) string {
	t.Helper()
	return h.signIn(t, loginRequest{Assertion: h.loginAssertion(t, key)})
}

func (h *harness) signIn(t *testing.T, req loginRequest) string {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/login", "", req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: %d %s", resp.StatusCode, b)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("login set no session cookie")
	return ""
}

func (h *harness) challenge(t *testing.T, id string, edits map[string]string) (string, []byte, challengeResponse) {
	t.Helper()
	status, body := h.post(t, "/r/"+id+"/challenge", challengeRequest{Edits: edits})
	if status != http.StatusOK {
		t.Fatalf("challenge: %d %s", status, body)
	}
	var res challengeResponse
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	opts := res.Options
	if opts.RPID != rpID || opts.UserVerification != "required" || len(opts.AllowCredentials) != 1 ||
		opts.AllowCredentials[0].ID != h.key.Key("").CredentialID {
		t.Fatalf("request options = %+v", opts)
	}
	c, err := base64.RawURLEncoding.DecodeString(opts.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	return res.Nonce, c, res
}

func (h *harness) approve(t *testing.T, id, nonce string, assertion json.RawMessage) (int, proposal.Proposal) {
	t.Helper()
	status, body := h.post(t, "/r/"+id+"/approve", approveRequest{Nonce: nonce, Assertion: assertion})
	var p proposal.Proposal
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
	}
	return status, p
}

func TestApproveExecutesOnce(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	if p.Status != proposal.Pending || p.Display[0].Value != "Farid" || p.Note != "she asked" {
		t.Fatalf("submitted = %+v", p)
	}

	nonce, c, _ := h.challenge(t, p.ID, nil)
	assertion := h.key.Assert(c)
	status, done := h.approve(t, p.ID, nonce, assertion)
	if status != http.StatusOK || done.Status != proposal.Done || done.Result == nil || done.Result.Ref != "m1" {
		t.Fatalf("approve = %d %+v", status, done)
	}
	sent := h.fake.sent()
	if len(sent) != 1 {
		t.Fatalf("executed %d times", len(sent))
	}
	if got := sent[0]; got.ID != p.ID || got.Args["to"] != "id-farid" || got.Args["text"] != "running late" || got.Note != "she asked" {
		t.Fatalf("executed %+v", got)
	}

	// Replaying the same approval, or asking to approve again, does nothing.
	if status, _ := h.approve(t, p.ID, nonce, assertion); status != http.StatusConflict {
		t.Fatalf("replayed approve = %d, want 409", status)
	}
	if status, _ := h.post(t, "/r/"+p.ID+"/challenge", challengeRequest{}); status != http.StatusConflict {
		t.Fatalf("challenge after done = %d, want 409", status)
	}
	if n := len(h.fake.sent()); n != 1 {
		t.Fatalf("executed %d times", n)
	}
	if got := h.get(t, p.ID); got.Status != proposal.Done || got.Result.Ref != "m1" {
		t.Fatalf("get = %+v", got)
	}

	// The stored proof verifies offline against the pinned key and is the
	// record that ran.
	var (
		pf     approval.Approval
		issued int64
		signed string
	)
	if err := h.srv.db.sql.QueryRow(`SELECT record, nonce, issued_at, assertion FROM proposals WHERE id = ?`, p.ID).
		Scan(&pf.Record, &pf.Nonce, &issued, &signed); err != nil {
		t.Fatal(err)
	}
	pf.IssuedAt, pf.Assertion = time.Unix(0, issued).UTC(), json.RawMessage(signed)
	v := approval.Verifier{Keys: h.srv.keys, Now: func() time.Time { return pf.IssuedAt }}
	rec, err := v.Verify(pf)
	if err != nil {
		t.Fatalf("stored proof doesn't verify: %v", err)
	}
	if rec.ID != p.ID || rec.Args["text"] != sent[0].Args["text"] {
		t.Fatalf("proof record %+v", rec)
	}
	stored, err := h.srv.db.get(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DecidedBy != "phone" || stored.DecidedAt.IsZero() {
		t.Fatalf("decision not recorded: %+v", stored)
	}
}

func TestConcurrentApproveExecutesOnce(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	nonce, c, _ := h.challenge(t, p.ID, nil)
	body := approveRequest{Nonce: nonce, Assertion: h.key.Assert(c)}

	var wg sync.WaitGroup
	statuses := make([]int, 8)
	for i := range statuses {
		wg.Go(func() {
			resp := h.do(t, http.MethodPost, "/r/"+p.ID+"/approve", h.session, body)
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

func TestEarlierChallengeStillApproves(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")

	// Someone (or a second tab) asks for another challenge; the first must
	// keep working.
	firstNonce, first, _ := h.challenge(t, p.ID, nil)
	secondNonce, second, _ := h.challenge(t, p.ID, map[string]string{"text": "on my way"})

	status, done := h.approve(t, p.ID, firstNonce, h.key.Assert(first))
	if status != http.StatusOK || done.Status != proposal.Done {
		t.Fatalf("approve with first challenge = %d %+v", status, done)
	}
	if sent := h.fake.sent(); len(sent) != 1 || sent[0].Args["text"] != "running late" {
		t.Fatalf("executed %+v", sent)
	}
	// Approving spent every other challenge of the request.
	if status, _ := h.approve(t, p.ID, secondNonce, h.key.Assert(second)); status != http.StatusConflict {
		t.Fatalf("approve with second challenge = %d, want 409", status)
	}
	if n := len(h.fake.sent()); n != 1 {
		t.Fatalf("executed %d times", n)
	}
}

func TestChallengeShowsWhatWillBeSigned(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")

	nonce, c, res := h.challenge(t, p.ID, map[string]string{"text": "on my way, 10 min"})
	// The connector re-rendered the display from the edit, including the
	// field derived from it.
	want := []approval.Field{
		{Label: "To", Value: "Farid"},
		{Label: "Text", Value: "on my way, 10 min", Param: "text"},
		{Label: "Length", Value: strings.Repeat("#", len("on my way, 10 min"))},
	}
	if !slices.Equal(res.Display, want) {
		t.Fatalf("display = %+v, want %+v", res.Display, want)
	}
	if !strings.Contains(res.Preview, "on my way, 10 min") || !strings.Contains(res.Preview, want[2].Value) ||
		strings.Contains(res.Preview, "running late") {
		t.Fatalf("preview doesn't show the signed display:\n%s", res.Preview)
	}

	status, done := h.approve(t, p.ID, nonce, h.key.Assert(c))
	if status != http.StatusOK || done.Status != proposal.Done || !slices.Equal(done.Display, want) {
		t.Fatalf("approve = %d %+v", status, done)
	}
	if sent := h.fake.sent(); len(sent) != 1 || !slices.Equal(sent[0].Display, want) {
		t.Fatalf("executed %+v", sent)
	}
}

func TestEditNonEditableRefused(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	for _, edits := range []map[string]string{
		{"text": "fine", "to": "Mallory"},
		{"bcc": "Mallory"},
	} {
		if status, body := h.post(t, "/r/"+p.ID+"/challenge", challengeRequest{Edits: edits}); status != http.StatusBadRequest {
			t.Errorf("edits %v = %d %s, want 400", edits, status, body)
		}
	}
	var n int
	if err := h.srv.db.sql.QueryRow(`SELECT count(*) FROM challenges`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d challenges stored (%v)", n, err)
	}
}

func TestApprovalRefused(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	nonce, c, _ := h.challenge(t, p.ID, nil)

	stranger := approvaltest.New(t, rpID, origin)
	noUV := approvaltest.New(t, rpID, origin)
	noUV.Flags = approvaltest.FlagUP
	phishing := approvaltest.New(t, rpID, "https://evil.example")
	h.trust(t, noUV, "no-uv")
	h.trust(t, phishing, "phished")

	for name, assertion := range map[string]json.RawMessage{
		"unknown passkey":     stranger.Assert(c),
		"other challenge":     h.key.Assert([]byte("some other challenge")),
		"no user verified":    noUV.Assert(c),
		"wrong origin":        phishing.Assert(c),
		"not a webauthn blob": json.RawMessage(`{"id":"x"}`),
	} {
		if status, _ := h.approve(t, p.ID, nonce, assertion); status != http.StatusForbidden {
			t.Errorf("%s: approve = %d, want 403", name, status)
		}
	}

	// A challenge is bound to the session that asked for it.
	other := h.login(t, h.key)
	if status, _ := h.call(t, http.MethodPost, "/r/"+p.ID+"/approve", other,
		approveRequest{Nonce: nonce, Assertion: h.key.Assert(c)}); status != http.StatusForbidden {
		t.Errorf("approve from another session = %d, want 403", status)
	}
	// Nonces that were never issued, or belong to another request.
	q := h.submit(t, "another")
	if status, _ := h.approve(t, q.ID, nonce, h.key.Assert(c)); status != http.StatusConflict {
		t.Errorf("nonce of another request = %d, want 409", status)
	}
	if status, _ := h.approve(t, p.ID, b64(make([]byte, 16)), h.key.Assert(c)); status != http.StatusConflict {
		t.Errorf("unknown nonce = %d, want 409", status)
	}

	if n := len(h.fake.sent()); n != 0 {
		t.Fatalf("executed %d times", n)
	}
	if got := h.get(t, p.ID); got.Status != proposal.Pending {
		t.Fatalf("status = %s", got.Status)
	}
	// Refusals spent nothing: the owner can still approve.
	if status, _ := h.approve(t, p.ID, nonce, h.key.Assert(c)); status != http.StatusOK {
		t.Fatalf("approve after refusals = %d", status)
	}
}

func TestExpiry(t *testing.T) {
	h := start(t)

	t.Run("stale challenge", func(t *testing.T) {
		p := h.submit(t, "running late")
		nonce, c, _ := h.challenge(t, p.ID, nil)
		assertion := h.key.Assert(c)
		h.clock.advance(approval.MaxAge + 2*time.Minute)
		if status, _ := h.approve(t, p.ID, nonce, assertion); status != http.StatusForbidden {
			t.Fatalf("stale approve = %d, want 403", status)
		}
	})

	t.Run("proposal", func(t *testing.T) {
		h.session = h.login(t, h.key)
		p := h.submit(t, "running late")
		nonce, c, _ := h.challenge(t, p.ID, nil)
		assertion := h.key.Assert(c)
		h.clock.advance(24 * time.Hour)
		h.session = h.login(t, h.key)
		if got := h.get(t, p.ID); got.Status != proposal.Expired {
			t.Fatalf("status = %s, want expired", got.Status)
		}
		if status, _ := h.approve(t, p.ID, nonce, assertion); status != http.StatusConflict && status != http.StatusForbidden {
			t.Fatalf("approve = %d, want refused", status)
		}
		if status, _ := h.post(t, "/r/"+p.ID+"/challenge", challengeRequest{}); status != http.StatusConflict {
			t.Fatalf("challenge = %d, want 409", status)
		}
	})

	if n := len(h.fake.sent()); n != 0 {
		t.Fatalf("executed %d times", n)
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
	if status, _ := h.post(t, "/r/"+first.ID+"/deny", nil); status != http.StatusSeeOther {
		t.Fatalf("deny = %d", status)
	}
	h.submit(t, "now there's room")

	// Expired proposals free their slot too.
	h.clock.advance(25 * time.Hour)
	h.submit(t, "a day later")
}

func TestDeny(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	nonce, c, _ := h.challenge(t, p.ID, nil)

	if status, _ := h.post(t, "/r/"+p.ID+"/deny", nil); status != http.StatusSeeOther {
		t.Fatalf("deny = %d, want 303", status)
	}
	if got := h.get(t, p.ID); got.Status != proposal.Rejected {
		t.Fatalf("status = %s", got.Status)
	}
	if status, _ := h.approve(t, p.ID, nonce, h.key.Assert(c)); status != http.StatusConflict {
		t.Fatalf("approve after deny = %d, want 409", status)
	}
	if status, _ := h.post(t, "/r/"+p.ID+"/deny", nil); status != http.StatusConflict {
		t.Fatalf("second deny = %d, want 409", status)
	}
	if status, _ := h.post(t, "/r/nope/deny", nil); status != http.StatusNotFound {
		t.Fatalf("deny unknown = %d, want 404", status)
	}
	if n := len(h.fake.sent()); n != 0 {
		t.Fatalf("executed %d times", n)
	}
}

func TestDelete(t *testing.T) {
	h := start(t)
	p := h.submit(t, "running late")
	if status, _ := h.post(t, "/r/"+p.ID+"/delete", nil); status != http.StatusConflict {
		t.Fatalf("delete pending = %d, want 409", status)
	}
	h.post(t, "/r/"+p.ID+"/deny", nil)
	if status, _ := h.post(t, "/r/"+p.ID+"/delete", nil); status != http.StatusSeeOther {
		t.Fatalf("delete = %d, want 303", status)
	}
	if status, _ := h.page(t, "/r/"+p.ID); status != http.StatusNotFound {
		t.Fatalf("deleted request page = %d, want 404", status)
	}
	if status, _ := h.post(t, "/r/"+p.ID+"/delete", nil); status != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", status)
	}
}

func TestConnectorFailures(t *testing.T) {
	t.Run("connection lost mid-execute is unknown", func(t *testing.T) {
		h := start(t)
		h.fake.abort.Store(true)
		p := h.submit(t, "running late")
		nonce, c, _ := h.challenge(t, p.ID, nil)
		status, got := h.approve(t, p.ID, nonce, h.key.Assert(c))
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
		nonce, c, _ := h.challenge(t, p.ID, nil)
		h.stopConnector()
		status, got := h.approve(t, p.ID, nonce, h.key.Assert(c))
		if status != http.StatusOK || got.Status != proposal.Failed {
			t.Fatalf("approve = %d %+v, want failed", status, got)
		}
	})

	t.Run("interrupted execution is unknown after restart", func(t *testing.T) {
		h := start(t)
		p := h.submit(t, "running late")
		nonce, _, _ := h.challenge(t, p.ID, nil)
		raw, _ := base64.RawURLEncoding.DecodeString(nonce)
		ch, err := h.srv.db.challenge(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := approval.Decode(ch.Record)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.srv.db.claim(context.Background(), ch, rec, json.RawMessage(`{}`), "phone", h.srv.now()); err != nil {
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

func TestSubmit(t *testing.T) {
	h := start(t)
	p := h.submit(t, "hi")
	if p.URL != origin+"/r/"+p.ID {
		t.Fatalf("url = %q", p.URL)
	}
	if got := h.get(t, p.ID); got.URL != p.URL {
		t.Fatalf("get url = %q", got.URL)
	}

	for _, tt := range []struct {
		name   string
		req    proposal.Request
		status int
		msg    string
	}{
		{"unknown connector", proposal.Request{Connector: "telegram", Op: "send"}, http.StatusNotFound, ""},
		{"read op", proposal.Request{Connector: "fake", Op: "fetch"}, http.StatusNotFound, ""},
		{"connector refuses", proposal.Request{Connector: "fake", Op: "send", Args: map[string]string{"to": "Eve", "text": "x"}}, http.StatusBadRequest, `"Eve"`},
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
}

// Open pages reload when the version moves, so everything another device or
// the clock can change must move it.
func TestVersionMoves(t *testing.T) {
	h := start(t)
	last := h.srv.version(context.Background())
	moved := func(what string) {
		t.Helper()
		v := h.srv.version(context.Background())
		if v == last {
			t.Fatalf("version unchanged after %s", what)
		}
		last = v
	}

	p := h.submit(t, "one")
	moved("a proposal arrived")
	if status, _ := h.post(t, "/r/"+p.ID+"/deny", nil); status != http.StatusSeeOther {
		t.Fatalf("deny = %d", status)
	}
	moved("a denial")
	p = h.submit(t, "two")
	moved("a proposal arrived")
	nonce, c, _ := h.challenge(t, p.ID, nil)
	if status, _ := h.approve(t, p.ID, nonce, h.key.Assert(c)); status != http.StatusOK {
		t.Fatalf("approve = %d", status)
	}
	moved("an approval")
	h.submit(t, "three")
	moved("a proposal arrived")

	laptop := approvaltest.New(t, rpID, origin)
	if h.addKey(t, h.session, laptop, "laptop") {
		t.Fatal("second key trusted without approval")
	}
	moved("a key change was requested")
	if status := h.approveChange(t, h.session, h.waiting(t), h.key); status != http.StatusOK {
		t.Fatalf("approve key = %d", status)
	}
	moved("a key change was approved")
	if err := h.confirm(fingerprint(laptop.Key(""))); err != nil {
		t.Fatal(err)
	}
	moved("a key addition was confirmed")
	h.clock.advance(proposalTTL + time.Minute)
	moved("a proposal expired")
}
