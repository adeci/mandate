package owner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/exchange"
	"github.com/adeci/mandate/internal/peer"
	"github.com/adeci/mandate/record"
)

const (
	testPassword = "ABCD-EFGH-IJKL-MNOP"
	testSPKI     = "f910152c80be10ee2280a04cb011ce9131b993408adb19cf47f1495c331bcdbc"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t     *testing.T
	s     *Server
	http  *http.Client
	clock *clock
	lives chan live
}

// sockDir is short: t.TempDir's paths can pass the socket path limit.
func sockDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "owner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

// start runs the owner side with a fake gateway that records each live set.
// The test process is both front and gateway.
func start(t *testing.T, conns map[string]*connector.Client) *harness {
	t.Helper()
	socks := sockDir(t)
	lives := make(chan live, 100)
	gwLn, err := net.Listen("unix", filepath.Join(socks, "gw"))
	if err != nil {
		t.Fatal(err)
	}
	gw := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var l live
		if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case lives <- l:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go gw.Serve(gwLn)
	t.Cleanup(func() { gw.Close() })

	s, err := newServer(t.TempDir(), conns, filepath.Join(socks, "gw"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Now().UTC()}
	s.now = c.now
	ln, err := peer.Listen(filepath.Join(socks, "owner"))
	if err != nil {
		t.Fatal(err)
	}
	gid := uint32(os.Getgid())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.serve(ctx, ln, gid, gid) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return &harness{t, s, unixClient(filepath.Join(socks, "owner")), c, lives}
}

// nowhere is a connector that never answers.
func nowhere() map[string]*connector.Client {
	return map[string]*connector.Client{"test": connector.Dial("test", "/nonexistent/test.sock")}
}

// do calls the owner side and decodes the answer into out. Headers come in
// pairs.
func (h *harness) do(method, path, token string, body, out any, headers ...string) int {
	var rd io.Reader
	if b, ok := body.([]byte); ok {
		rd = bytes.NewReader(b)
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Error(err)
			return 0
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://owner"+path, rd)
	if err != nil {
		h.t.Error(err)
		return 0
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Error(err)
		return 0
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			h.t.Errorf("%s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

type device struct {
	priv  *ecdsa.PrivateKey
	token string
	key   record.Key
}

func newDevice(t *testing.T, name string) device {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := record.P256Key(&priv.PublicKey, name)
	if err != nil {
		t.Fatal(err)
	}
	return device{priv, exchange.NewToken(), k}
}

func (h *harness) challenge(d device, path string, body any) record.Envelope {
	h.t.Helper()
	var env record.Envelope
	if st := h.do("POST", path, d.token, body, &env); st != http.StatusOK {
		h.t.Fatalf("POST %s = %d", path, st)
	}
	return env
}

// sign signs env with d and posts it, answering the status and the ID.
func (h *harness) sign(d device, env record.Envelope) (int, string) {
	if err := record.Sign(&env, d.priv); err != nil {
		h.t.Fatal(err)
	}
	var out struct{ ID string }
	st := h.do("POST", "/owner/v1/signed", d.token, env, &out)
	return st, out.ID
}

func (h *harness) openSetup() {
	b, _ := json.Marshal(setupHash{SHA256: hashPassword(testPassword)})
	if err := writeFile(setupFile(h.s.dir), b, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

var secretParam = regexp.MustCompile(`secret=([A-Za-z0-9_-]+)`)

// setupSecret submits the setup password and returns the pairing secret
// the page links to.
func (h *harness) setupSecret(password string) (int, string) {
	req, _ := http.NewRequest(http.MethodPost, "http://box.example:8443/setup", strings.NewReader(url.Values{"password": {password}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Mandate-Spki-Sha256", testSPKI)
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	m := secretParam.FindSubmatch(page)
	if m == nil {
		return resp.StatusCode, ""
	}
	if !bytes.Contains(page, []byte(testSPKI)) || !bytes.Contains(page, []byte("<svg")) {
		h.t.Error("the setup page lacks the fingerprint or the QR code")
	}
	return resp.StatusCode, string(m[1])
}

func (h *harness) pairWith(secret string, d device) int {
	now := h.clock.now()
	env, err := record.New(record.KeyEntry{Op: "add", Key: d.key, At: rtime(now)}, now)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := record.Sign(&env, d.priv); err != nil {
		h.t.Fatal(err)
	}
	body := map[string]any{"secret": secret, "token_sha256": exchange.TokenSHA256(d.token), "entry": env}
	return h.do("POST", "/owner/v1/pair", "", body, nil)
}

func (h *harness) pair() device {
	h.t.Helper()
	h.openSetup()
	_, secret := h.setupSecret(testPassword)
	d := newDevice(h.t, "Pixel 9")
	if st := h.pairWith(secret, d); st != http.StatusOK {
		h.t.Fatalf("pair = %d", st)
	}
	return d
}

// join runs a code exchange as the new party, with the owner typing code,
// and returns the exchange ID. base is /internal for an agent, through the
// gateway, and /owner/v1 for a device.
func (h *harness) join(base, kind, name, code, typed string, p exchange.Payload, owner device) (string, int, map[string]any) {
	h.t.Helper()
	var started struct{ ID string }
	if st := h.do("POST", base+"/join", "", map[string]string{"kind": kind, "name": name}, &started, "X-Forwarded-For", "192.0.2.7"); st != http.StatusCreated {
		h.t.Fatalf("start exchange = %d", st)
	}
	var waiting []map[string]any
	h.do("GET", "/owner/v1/exchanges", owner.token, nil, &waiting)
	if !slices.ContainsFunc(waiting, func(j map[string]any) bool { return j["id"] == started.ID && j["address"] == "192.0.2.7" }) {
		h.t.Fatalf("exchanges = %v", waiting)
	}

	type outcome struct {
		st  int
		out map[string]any
	}
	typedc := make(chan outcome, 1)
	go func() {
		var out map[string]any
		st := h.do("POST", "/owner/v1/exchanges/"+started.ID+"/code", owner.token, map[string]string{"code": typed}, &out)
		typedc <- outcome{st, out}
	}()

	var poll struct{ Msg string }
	h.do("GET", base+"/join/"+started.ID+"?wait=10", "", nil, &poll)
	msg, _ := hex.DecodeString(poll.Msg)
	id, _ := hex.DecodeString(started.ID)
	r, err := exchange.Respond(kind, id, code, msg, p)
	if err != nil {
		h.t.Fatal(err)
	}
	answer := map[string]string{"msg": hex.EncodeToString(r.Msg), "confirm": hex.EncodeToString(r.Confirm), "sealed": hex.EncodeToString(r.Sealed)}
	var confirmed struct{ Confirm string }
	st := h.do("POST", base+"/join/"+started.ID, "", answer, &confirmed, "Mandate-Spki-Sha256", testSPKI)
	if st == http.StatusOK {
		confirm, _ := hex.DecodeString(confirmed.Confirm)
		spki, _ := hex.DecodeString(testSPKI)
		if err := r.Check(confirm, spki); err != nil {
			h.t.Errorf("server_confirm: %v", err)
		}
	}
	o := <-typedc
	return started.ID, o.st, o.out
}

// agent lets in an agent identity and returns its ID.
func (h *harness) agent(owner device) string {
	h.t.Helper()
	p := exchange.Payload{TokenSHA256: exchange.TokenSHA256(exchange.NewToken())}
	id, st, _ := h.join("/internal", exchange.Agent, "laptop", "123456", "123456", p, owner)
	if st != http.StatusOK {
		h.t.Fatalf("code = %d", st)
	}
	env := h.challenge(owner, "/owner/v1/exchanges/"+id+"/challenge", nil)
	st, ident := h.sign(owner, env)
	if st != http.StatusOK {
		h.t.Fatalf("sign identity = %d", st)
	}
	var poll struct{ Status string }
	h.do("GET", "/internal/join/"+id, "", nil, &poll)
	if poll.Status != "approved" {
		h.t.Fatalf("agent's poll = %q", poll.Status)
	}
	return ident
}

func requestBody(identity string) map[string]any {
	return map[string]any{
		"identity": identity, "connector": "test", "kind": "once", "for": 1800, "horizon": 86400, "reason": "summary",
		"terms": []term{{Term: "fam", Chats: []chat{{"c1", "Family"}, {"c2", "~Fam"}}}},
	}
}

func (h *harness) request(identity string) string {
	h.t.Helper()
	var out struct{ ID string }
	if st := h.do("POST", "/internal/requests", "", requestBody(identity), &out); st != http.StatusCreated {
		h.t.Fatalf("request = %d", st)
	}
	return out.ID
}

func (h *harness) grant(owner device, identity string) string {
	h.t.Helper()
	env := h.challenge(owner, "/owner/v1/grants/challenge", map[string]any{"identity": identity, "connector": "test", "for": 600, "horizon": 0})
	st, id := h.sign(owner, env)
	if st != http.StatusOK {
		h.t.Fatalf("sign grant = %d", st)
	}
	return id
}

// waitLive waits for a pushed live set that satisfies ok.
func (h *harness) waitLive(what string, ok func(live) bool) {
	h.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case l := <-h.lives:
			if ok(l) {
				return
			}
		case <-timeout:
			h.t.Fatalf("no live set %s", what)
		}
	}
}

func hasGrant(l live, id string) bool {
	return slices.ContainsFunc(l.Grants, func(e record.Envelope) bool { return bytes.Contains(e.Record, []byte(`"id":"`+id+`"`)) })
}

func TestFirstPairing(t *testing.T) {
	h := start(t, nil)
	if st := h.do("GET", "/setup", "", nil, nil); st != http.StatusNotFound {
		t.Fatalf("setup page without a password = %d", st)
	}
	h.openSetup()
	if st := h.do("GET", "/setup", "", nil, nil); st != http.StatusOK {
		t.Fatalf("setup page = %d", st)
	}
	if st, secret := h.setupSecret("WRONG-WRONG-WRONG"); st != http.StatusForbidden || secret != "" {
		t.Fatalf("wrong password = %d, secret %q", st, secret)
	}
	st, secret := h.setupSecret(strings.ToLower(strings.ReplaceAll(testPassword, "-", "")))
	if st != http.StatusOK || secret == "" {
		t.Fatalf("password = %d, secret %q", st, secret)
	}

	phone := newDevice(t, "Pixel 9")
	if st := h.pairWith("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", phone); st != http.StatusForbidden {
		t.Fatalf("pair with a wrong secret = %d", st)
	}
	if st := h.pairWith(secret, phone); st != http.StatusOK {
		t.Fatalf("pair = %d", st)
	}
	if _, err := os.Stat(setupFile(h.s.dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("setup password not spent: %v", err)
	}
	if st := h.do("GET", "/setup", "", nil, nil); st != http.StatusNotFound {
		t.Errorf("setup page after pairing = %d", st)
	}
	if st := h.pairWith(secret, newDevice(t, "thief")); st != http.StatusConflict {
		t.Errorf("second pairing = %d", st)
	}
	if st := h.do("GET", "/owner/v1/keys", phone.token, nil, nil); st != http.StatusOK {
		t.Errorf("paired device's token = %d", st)
	}
	if st := h.do("GET", "/owner/v1/keys", exchange.NewToken(), nil, nil); st != http.StatusUnauthorized {
		t.Errorf("unknown token = %d", st)
	}
}

func TestChallengeRules(t *testing.T) {
	h := start(t, nowhere())
	owner := h.pair()
	agent := h.agent(owner)

	req := h.request(agent)
	if st := h.do("POST", "/owner/v1/requests/"+req+"/challenge", owner.token, map[string]any{"chats": []string{"c3"}}, nil); st != http.StatusBadRequest {
		t.Errorf("narrowing to a chat not asked for = %d", st)
	}
	first := h.challenge(owner, "/owner/v1/requests/"+req+"/challenge", map[string]any{"chats": []string{"c1"}})
	other := h.challenge(owner, "/owner/v1/requests/"+req+"/challenge", nil)
	st, grant := h.sign(owner, first)
	if st != http.StatusOK {
		t.Fatalf("sign = %d", st)
	}
	if st, _ := h.sign(owner, first); st != http.StatusConflict {
		t.Errorf("challenge used twice = %d", st)
	}
	if st, _ := h.sign(owner, other); st != http.StatusConflict {
		t.Errorf("challenge for a request decided meanwhile = %d", st)
	}
	var status struct {
		Status  string
		GrantID string `json:"grant_id"`
	}
	h.do("GET", "/internal/requests/"+req+"?identity="+agent+"&wait=5", "", nil, &status)
	if status.Status != "granted" || status.GrantID != grant {
		t.Errorf("request status = %+v, want granted %s", status, grant)
	}
	if st := h.do("GET", "/internal/requests/"+req+"?identity="+h.agent(owner), "", nil, nil); st != http.StatusNotFound {
		t.Errorf("another identity's request = %d", st)
	}

	late := h.challenge(owner, "/owner/v1/requests/"+h.request(agent)+"/challenge", nil)
	h.clock.add(challengeTTL + time.Second)
	if st, _ := h.sign(owner, late); st != http.StatusGone {
		t.Errorf("expired challenge = %d", st)
	}

	extend := h.challenge(owner, "/owner/v1/grants/"+grant+"/extend/challenge", map[string]int{"for": 600})
	if st := h.do("POST", "/owner/v1/grants/"+grant+"/close", owner.token, nil, nil); st != http.StatusOK {
		t.Fatalf("close = %d", st)
	}
	if st, _ := h.sign(owner, extend); st != http.StatusConflict {
		t.Errorf("challenge after Close = %d", st)
	}

	h.request(agent)
	h.request(agent) // three wait now
	var limited struct {
		RetryAfter int `json:"retry_after_seconds"`
	}
	if st := h.do("POST", "/internal/requests", "", requestBody(agent), &limited); st != http.StatusTooManyRequests || limited.RetryAfter < 1 {
		t.Errorf("fourth waiting request = %d, retry after %d", st, limited.RetryAfter)
	}
}

func TestDeviceJoinAndRevocation(t *testing.T) {
	h := start(t, nil)
	phone := h.pair()
	laptop := newDevice(t, "MacBook")
	p := exchange.Payload{Key: &laptop.key, TokenSHA256: exchange.TokenSHA256(laptop.token)}

	id, st, out := h.join("/owner/v1", exchange.Device, "MacBook", "654321", "111111", p, phone)
	if st != http.StatusForbidden || out["attempts_left"] != 2.0 {
		t.Fatalf("wrong code = %d %v", st, out)
	}
	var poll struct{ Msg string }
	if st := h.do("GET", "/owner/v1/join/"+id, "", nil, &poll); st != http.StatusOK || poll.Msg != "" {
		t.Errorf("poll after a wrong code = %d %+v", st, poll)
	}
	if st := h.do("GET", "/internal/join/"+id, "", nil, nil); st != http.StatusNotFound {
		t.Errorf("device exchange through the agent route = %d", st)
	}

	id, st, out = h.join("/owner/v1", exchange.Device, "MacBook", "654321", "654321", p, phone)
	if st != http.StatusOK || out["payload"] == nil {
		t.Fatalf("code = %d %v", st, out)
	}
	if st := h.do("GET", "/owner/v1/keys", laptop.token, nil, nil); st != http.StatusUnauthorized {
		t.Errorf("token before approval = %d", st)
	}
	add := h.challenge(phone, "/owner/v1/exchanges/"+id+"/challenge", nil)
	if st, _ := h.sign(phone, add); st != http.StatusOK {
		t.Fatalf("sign add = %d", st)
	}
	var approved struct{ Status string }
	h.do("GET", "/owner/v1/join/"+id, "", nil, &approved)
	if approved.Status != "approved" {
		t.Errorf("new device's poll = %q", approved.Status)
	}
	if st := h.do("GET", "/owner/v1/keys", laptop.token, nil, nil); st != http.StatusOK {
		t.Fatalf("joined device's token = %d", st)
	}

	remove := h.challenge(laptop, "/owner/v1/keys/"+laptop.key.ID+"/remove/challenge", nil)
	if st, _ := h.sign(phone, remove); st != http.StatusOK {
		t.Fatalf("sign remove = %d", st)
	}
	if st := h.do("GET", "/owner/v1/keys", laptop.token, nil, nil); st != http.StatusUnauthorized {
		t.Errorf("removed device's token = %d", st)
	}
	if st := h.do("POST", "/owner/v1/keys/"+phone.key.ID+"/remove/challenge", phone.token, nil, nil); st != http.StatusConflict {
		t.Errorf("removing the last key = %d", st)
	}
}

func TestLiveSet(t *testing.T) {
	h := start(t, nowhere())
	owner := h.pair()
	agent := h.agent(owner)
	g := h.grant(owner, agent)
	h.waitLive("with the grant", func(l live) bool { return hasGrant(l, g) && len(l.Identities) == 1 && len(l.Keys) == 1 })
	h.do("POST", "/owner/v1/grants/"+g+"/close", owner.token, nil, nil)
	h.waitLive("without the closed grant", func(l live) bool { return !hasGrant(l, g) && len(l.Identities) == 1 })
}

func TestStopAll(t *testing.T) {
	h := start(t, nowhere())
	owner := h.pair()
	agent := h.agent(owner)
	h.grant(owner, agent)
	h.grant(owner, agent)
	req := h.request(agent)
	pending := h.challenge(owner, "/owner/v1/grants/challenge", map[string]any{"identity": agent, "connector": "test", "for": 600, "horizon": 0})

	var stopped struct{ Grants, Requests int }
	if st := h.do("POST", "/owner/v1/stop-all", owner.token, nil, &stopped); st != http.StatusOK || stopped.Grants != 2 || stopped.Requests != 1 {
		t.Fatalf("stop all = %d %+v", st, stopped)
	}
	h.waitLive("without grants", func(l live) bool { return len(l.Grants) == 0 })
	if st, _ := h.sign(owner, pending); st != http.StatusConflict {
		t.Errorf("challenge after Stop all = %d", st)
	}
	var status struct{ Status string }
	h.do("GET", "/internal/requests/"+req+"?identity="+agent, "", nil, &status)
	if status.Status != "denied" {
		t.Errorf("waiting request after Stop all = %q", status.Status)
	}
}

type fakeConnector struct {
	runs atomic.Int32
	sent atomic.Value
}

func (*fakeConnector) Spec() connector.Spec {
	return connector.Spec{Ops: []connector.Op{{Name: "send", Kind: connector.Write, Params: []connector.Param{
		{Name: "chat", Required: true, Container: true},
		{Name: "text", Required: true, Editable: true},
	}}}}
}

func (*fakeConnector) Health(context.Context) connector.Health {
	return connector.Health{State: connector.Healthy}
}

func (*fakeConnector) Setup(context.Context, string, map[string]string) (connector.Step, error) {
	return connector.Step{Done: true}, nil
}

func (*fakeConnector) Prepare(_ context.Context, _ string, args map[string]string) (connector.Prepared, error) {
	return connector.Prepared{
		Args:      args,
		Display:   []record.Field{{Label: "To", Value: args["chat"]}, {Label: "Text", Value: args["text"], Param: "text"}},
		Container: args["chat"],
	}, nil
}

func (f *fakeConnector) Execute(_ context.Context, w record.Write) connector.Result {
	f.runs.Add(1)
	f.sent.Store(w.Args["text"])
	return connector.Result{Status: connector.Done, Ref: "sent"}
}

func TestWriteExecutedOnce(t *testing.T) {
	f := &fakeConnector{}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	gid := uint32(os.Getgid())
	srv, err := connector.NewServer(f, "test", dir, gid, gid, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(sockDir(t), "test")
	ln, err := peer.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })

	h := start(t, map[string]*connector.Client{"test": connector.Dial("test", sock)})
	owner := h.pair()
	agent := h.agent(owner)
	prepared, _ := f.Prepare(t.Context(), "send", map[string]string{"chat": "c1", "text": "on my way"})
	var created struct{ ID string }
	body := map[string]any{"identity": agent, "connector": "test", "op": "send", "prepared": prepared, "note": "asked to reply"}
	if st := h.do("POST", "/internal/proposals", "", body, nil); st != http.StatusNotFound {
		t.Errorf("proposal without a grant = %d", st)
	}
	h.grant(owner, agent)
	if st := h.do("POST", "/internal/proposals", "", body, &created); st != http.StatusCreated {
		t.Fatalf("proposal = %d", st)
	}
	var view struct{ Editable []string }
	h.do("GET", "/owner/v1/proposals/"+created.ID, owner.token, nil, &view)
	if !slices.Equal(view.Editable, []string{"text"}) {
		t.Errorf("editable = %v", view.Editable)
	}
	if st := h.do("POST", "/owner/v1/proposals/"+created.ID+"/challenge", owner.token, map[string]any{"edits": map[string]string{"chat": "c9"}}, nil); st != http.StatusBadRequest {
		t.Errorf("editing the chat = %d", st)
	}

	env := h.challenge(owner, "/owner/v1/proposals/"+created.ID+"/challenge", map[string]any{"edits": map[string]string{"text": "on my way, 10 min"}})
	if st, _ := h.sign(owner, env); st != http.StatusOK {
		t.Fatalf("sign write = %d", st)
	}
	if st, _ := h.sign(owner, env); st != http.StatusConflict {
		t.Errorf("signed write posted again = %d", st)
	}
	var status struct {
		Status string
		Result struct{ Ref string }
	}
	h.do("GET", "/internal/proposals/"+created.ID+"?identity="+agent+"&wait=10", "", nil, &status)
	if status.Status != "done" || status.Result.Ref != "sent" {
		t.Fatalf("proposal status = %+v", status)
	}
	if n := f.runs.Load(); n != 1 {
		t.Errorf("write ran %d times", n)
	}
	if got := f.sent.Load(); got != "on my way, 10 min" {
		t.Errorf("sent %q, not the owner's edit", got)
	}
}
