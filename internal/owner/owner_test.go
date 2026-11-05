package owner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
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
	"github.com/adeci/mandate/internal/config"
	"github.com/adeci/mandate/internal/exchange"
	"github.com/adeci/mandate/internal/peer"
	"github.com/adeci/mandate/record"
	"github.com/adeci/mandate/store"
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
	stop  func() // shuts the owner side down and waits
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
// opts adjust the server before it serves.
func start(t *testing.T, conns map[string]*connector.Client, opts ...func(*Server)) *harness {
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
	c := &clock{t: time.Now().UTC().Truncate(time.Second)}
	s.now = c.now
	for _, o := range opts {
		o(s)
	}
	ln, err := peer.Listen(filepath.Join(socks, "owner"))
	if err != nil {
		t.Fatal(err)
	}
	gid := uint32(os.Getgid())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, ln, gid, gid) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	return &harness{t, s, unixClient(filepath.Join(socks, "owner")), c, lives, stop}
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

// setupPageText fetches the setup page.
func (h *harness) setupPageText() (int, string) {
	return h.page("/setup")
}

// page fetches a page as front forwards it.
func (h *harness) page(path string) (int, string) {
	req, _ := http.NewRequest(http.MethodGet, "http://box.example:8443"+path, nil)
	req.Header.Set("Mandate-Spki-Sha256", testSPKI)
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(page)
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
	// People compare the key only when no CA vouches for the certificate.
	key := strings.Contains(string(page), `<p class="key">Key f910 152c 80be 10ee 2280</p>`)
	if key == h.s.certVerified || !bytes.Contains(page, []byte("<svg")) {
		h.t.Errorf("the pairing page (certificate verified %v) shows the key %v, or lacks the QR code:\n%s", h.s.certVerified, key, page)
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

// open opens an exchange as the owner and returns its ID and code.
func (h *harness) open(owner device, kind string) (string, string) {
	h.t.Helper()
	var ex struct{ ID, Code string }
	if st := h.do("POST", "/owner/v1/exchanges", owner.token, map[string]string{"kind": kind}, &ex); st != http.StatusCreated {
		h.t.Fatalf("open exchange = %d", st)
	}
	return ex.ID, ex.Code
}

// join tries code as the new party and returns the status and body of its
// answer. base is /internal for an agent, through the gateway, and
// /owner/v1 for a device.
func (h *harness) join(base, kind, code string, p exchange.Payload) (int, map[string]any) {
	h.t.Helper()
	var started struct{ ID, Msg string }
	if st := h.do("POST", base+"/join", "", map[string]string{"kind": kind, "name": "laptop"}, &started, "X-Forwarded-For", "192.0.2.7"); st != http.StatusCreated {
		h.t.Fatalf("join = %d", st)
	}
	msg, _ := hex.DecodeString(started.Msg)
	id, _ := hex.DecodeString(started.ID)
	r, err := exchange.Respond(kind, id, code, msg, p)
	if err != nil {
		h.t.Fatal(err)
	}
	answer := map[string]string{"msg": hex.EncodeToString(r.Msg), "confirm": hex.EncodeToString(r.Confirm), "sealed": hex.EncodeToString(r.Sealed)}
	var out map[string]any
	st := h.do("POST", base+"/join/"+started.ID, "", answer, &out, "Mandate-Spki-Sha256", testSPKI)
	if st == http.StatusOK {
		confirm, _ := hex.DecodeString(out["confirm"].(string))
		spki, _ := hex.DecodeString(testSPKI)
		if err := r.Check(confirm, spki); err != nil {
			h.t.Errorf("server_confirm: %v", err)
		}
	}
	return st, out
}

// confirmed checks that exchange id waits for the owner with p.
func (h *harness) confirmed(owner device, id string, p exchange.Payload) {
	h.t.Helper()
	var waiting []struct {
		ID, State, Name, Address string
		Payload                  exchange.Payload
	}
	h.do("GET", "/owner/v1/exchanges", owner.token, nil, &waiting)
	if len(waiting) != 1 || waiting[0].ID != id || waiting[0].State != "confirmed" || waiting[0].Name != "laptop" ||
		waiting[0].Address != "192.0.2.7" || waiting[0].Payload.TokenSHA256 != p.TokenSHA256 {
		h.t.Fatalf("exchanges = %+v", waiting)
	}
}

// agent lets in an agent identity and returns its ID.
func (h *harness) agent(owner device) string {
	h.t.Helper()
	id, code := h.open(owner, exchange.Agent)
	p := exchange.Payload{TokenSHA256: exchange.TokenSHA256(exchange.NewToken())}
	if st, out := h.join("/internal", exchange.Agent, code, p); st != http.StatusOK {
		h.t.Fatalf("join = %d %v", st, out)
	}
	h.confirmed(owner, id, p)
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

func TestHome(t *testing.T) {
	h := start(t, nil)
	home := func() (int, string, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://box.example:8443/", nil)
		resp, err := h.http.Transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("/ headers = %v", resp.Header)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Location"), string(b)
	}
	if st, to, _ := home(); st != http.StatusFound || to != "/setup" {
		t.Errorf("/ before pairing = %d to %q", st, to)
	}
	h.pair()
	if st, _, page := home(); st != http.StatusOK || !strings.Contains(page, "<h1>mandate</h1>\n<p class=\"center\">Set up.</p>\n</main>") {
		t.Errorf("/ once paired = %d %q", st, page)
	}
	if st := h.do("GET", "/index.html", "", nil, nil); st != http.StatusNotFound {
		t.Errorf("/index.html = %d", st)
	}
}

func TestFirstPairing(t *testing.T) {
	h := start(t, nil)
	if st, page := h.setupPageText(); st != http.StatusOK || !strings.Contains(page, "waiting to be set up") || strings.Contains(page, "<form") {
		t.Fatalf("setup page without a password = %d %q", st, page)
	}
	h.openSetup()
	if st, page := h.setupPageText(); st != http.StatusOK || !strings.Contains(page, "<form") {
		t.Fatalf("setup page = %d %q", st, page)
	}
	// The link mandate setup prints opens the pairing page; a wrong one
	// gets the form.
	if st, page := h.page("/setup/WRONG-WRONG-WRONG"); st != http.StatusOK || !strings.Contains(page, "<form") || strings.Contains(page, "<svg") {
		t.Fatalf("a wrong setup link = %d %q", st, page)
	}
	if st, page := h.page("/setup/" + testPassword); st != http.StatusOK || !strings.Contains(page, "<svg") || !strings.Contains(page, "Key f910 152c 80be 10ee 2280") {
		t.Fatalf("the setup link = %d %q", st, page)
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
	// An open pairing page moves on to / by itself, by script or reload.
	if st, page := h.page("/setup/" + testPassword); st != http.StatusOK || !strings.Contains(page, "Set up.") {
		t.Errorf("the spent setup link = %d %q", st, page)
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

func TestWiderThanAsked(t *testing.T) {
	h := start(t, nowhere())
	owner := h.pair()
	agent := h.agent(owner)
	req := h.request(agent) // for 1800, horizon 86400
	env := h.challenge(owner, "/owner/v1/requests/"+req+"/challenge", map[string]any{"for": 7200, "horizon": 0})
	var g record.Grant
	if err := json.Unmarshal(env.Record, &g); err != nil {
		t.Fatal(err)
	}
	if g.Until.Sub(g.From.Time) != 2*time.Hour || g.Horizon != 0 {
		t.Errorf("grant = %s", env.Record)
	}
	if st, _ := h.sign(owner, env); st != http.StatusOK {
		t.Errorf("sign = %d", st)
	}
}

type activityPage struct {
	Entries []activity
	Next    string
}

func TestDecisionInActivity(t *testing.T) {
	h := start(t, nowhere())
	owner := h.pair()
	agent := h.agent(owner)
	req := h.request(agent)
	if st := h.do("POST", "/owner/v1/requests/"+req+"/deny", owner.token, nil, nil); st != http.StatusOK {
		t.Fatalf("deny = %d", st)
	}
	var page activityPage
	if st := h.do("GET", "/owner/v1/activity", owner.token, nil, &page); st != http.StatusOK {
		t.Fatalf("activity = %d", st)
	}
	var kinds []string
	for _, e := range page.Entries {
		kinds = append(kinds, e.Kind)
	}
	if want := []string{"request.denied", "request.asked", "agent.admitted", "device.joined"}; !slices.Equal(kinds, want) || page.Next != "" {
		t.Fatalf("kinds = %v, next %q, want %v", kinds, page.Next, want)
	}
	denied := page.Entries[0]
	if denied.Identity != agent || denied.Connector != "test" || len(denied.Chats) != 2 || denied.Chats[0] != (chat{"c1", "Family"}) {
		t.Errorf("denied = %+v", denied)
	}
}

func TestActivityPaging(t *testing.T) {
	h := start(t, nil)
	owner := h.pair() // device.joined, now
	at := func(day, sec int) time.Time { return time.Date(2026, 1, day, 12, 0, sec, 0, time.UTC) }
	var reads []map[string]any
	for _, e := range []struct {
		at   time.Time
		verb string
	}{{at(3, 0), "d3a"}, {at(1, 1), "d1b"}, {at(2, 0), "d2"}, {at(1, 0), "d1a"}, {at(3, 0), "d3b"}} {
		reads = append(reads, map[string]any{"at": e.at, "identity": "x", "connector": "test", "verb": e.verb, "chats": []chat{}, "count": 1})
	}
	if st := h.do("POST", "/internal/activity", "", reads, nil); st != http.StatusOK {
		t.Fatalf("post activity = %d", st)
	}
	var got []string
	pages := 0
	for before := ""; ; {
		var page activityPage
		path := "/owner/v1/activity?limit=2"
		if before != "" {
			path += "&before=" + url.QueryEscape(before)
		}
		if st := h.do("GET", path, owner.token, nil, &page); st != http.StatusOK {
			t.Fatalf("activity = %d", st)
		}
		pages++
		for _, e := range page.Entries {
			got = append(got, e.Kind+" "+e.Detail)
		}
		if before = page.Next; before == "" || pages > 5 {
			break
		}
	}
	// Same second: the later line is newer.
	want := []string{"device.joined Pixel 9", "read d3b", "read d3a", "read d2", "read d1b", "read d1a"}
	if !slices.Equal(got, want) || pages != 3 {
		t.Errorf("pages = %d, entries = %v, want %v", pages, got, want)
	}
	if st := h.do("GET", "/owner/v1/activity?before=yesterday", owner.token, nil, nil); st != http.StatusBadRequest {
		t.Errorf("bad cursor = %d", st)
	}
}

func TestShutdownEndsStreams(t *testing.T) {
	h := start(t, nowhere())
	owner := h.pair()
	agent := h.agent(owner)
	req := h.request(agent)
	streams := make(chan error, 2)
	go func() {
		r, _ := http.NewRequest(http.MethodGet, "http://owner/owner/v1/stream", nil)
		r.Header.Set("Authorization", "Bearer "+owner.token)
		resp, err := h.http.Do(r)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		streams <- err
	}()
	go func() {
		resp, err := h.http.Get("http://owner/internal/requests/" + req + "?identity=" + agent + "&wait=900")
		if err == nil {
			resp.Body.Close()
		}
		streams <- err
	}()
	time.Sleep(200 * time.Millisecond) // both are open
	began := time.Now()
	h.stop()
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("shutdown took %v with a stream and a long poll open", d)
	}
	for range 2 {
		<-streams
	}
}

func TestSetupOutput(t *testing.T) {
	var b strings.Builder
	writeSetup(&b, setupURL(&config.Config{Listen: ":8443"}, "box.example", testPassword), "6ffd 8c34 742e fa9a 1c18")
	want := "Initial setup  https://box.example:8443/setup/" + testPassword + "\n" +
		"Key            6ffd 8c34 742e fa9a 1c18\n"
	if b.String() != want {
		t.Errorf("own key:\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	writeSetup(&b, setupURL(&config.Config{Listen: ":443"}, "mandate.example", testPassword), "")
	if want := "Initial setup  https://mandate.example/setup/" + testPassword + "\n"; b.String() != want {
		t.Errorf("certificate:\n%s\nwant\n%s", b.String(), want)
	}
}

func TestCopyLinkAllowed(t *testing.T) {
	w := httptest.NewRecorder()
	renderSetup(w, http.StatusOK, setupPage{Href: "mandate://pair?x", QR: "<svg></svg>"})
	m := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("no script on the pairing page:\n%s", w.Body)
	}
	if !strings.Contains(w.Body.String(), `<noscript><meta http-equiv="refresh" content="5"></noscript>`) {
		t.Error("without JavaScript, the pairing page does not reload")
	}
	sum := sha256.Sum256([]byte(m[1]))
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Errorf("the policy %q does not allow the script", csp)
	}
}

func TestCertModeSetupHost(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, c x509.Certificate) string {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		c.SerialNumber = big.NewInt(1)
		der, err := x509.CreateCertificate(rand.Reader, &c, &c, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tt := range []struct {
		cert x509.Certificate
		want string
	}{
		{x509.Certificate{Subject: pkix.Name{CommonName: "cn.example"}, DNSNames: []string{"mandate.example", "other.example"}}, "mandate.example"},
		{x509.Certificate{Subject: pkix.Name{CommonName: "cn.example"}}, "cn.example"},
	} {
		cfg := &config.Config{Listen: ":443", TLS: config.TLS{Mode: "cert", Cert: write(tt.want+".pem", tt.cert)}}
		var b strings.Builder
		printSetup(t.Context(), cfg, &b, testPassword)
		if want := "Initial setup  https://" + tt.want + "/setup/" + testPassword + "\n"; b.String() != want {
			t.Errorf("printed %q, want %q", b.String(), want)
		}
	}
}

func TestCertifiedServer(t *testing.T) {
	h := start(t, nil, func(s *Server) { s.certVerified, s.agentPort = true, "8444" })
	owner := h.pair()
	var info struct {
		AgentURL string `json:"agent_url"`
	}
	if st := h.do("GET", "/owner/v1/server", owner.token, nil, &info); st != http.StatusOK || info.AgentURL != "https://owner:8444" {
		t.Errorf("server = %d %+v", st, info)
	}
}

func TestChallengeRules(t *testing.T) {
	h := start(t, nowhere())
	owner := h.pair()
	agent := h.agent(owner)

	req := h.request(agent)
	if picked := h.challenge(owner, "/owner/v1/requests/"+req+"/challenge", map[string]any{"chats": []string{"c3"}}); !bytes.Contains(picked.Record, []byte(`"chats":["c3"]`)) {
		t.Errorf("a chat the owner picked = %s", picked.Record)
	}
	if st := h.do("POST", "/owner/v1/requests/"+req+"/challenge", owner.token, map[string]any{"horizon": -1}, nil); st != http.StatusBadRequest {
		t.Errorf("a negative history = %d", st)
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

	ask := map[string]string{"kind": exchange.Device, "name": "laptop"}
	if st := h.do("POST", "/owner/v1/join", "", ask, nil); st != http.StatusNotFound {
		t.Errorf("join with nothing open = %d", st)
	}
	first, _ := h.open(phone, exchange.Device)
	id, code := h.open(phone, exchange.Device)
	var poll struct{ Status string }
	if h.do("GET", "/owner/v1/join/"+first, "", nil, &poll); poll.Status != "ended" {
		t.Errorf("replaced exchange = %q", poll.Status)
	}
	if st := h.do("POST", "/internal/join", "", map[string]string{"kind": exchange.Agent, "name": "laptop"}, nil); st != http.StatusNotFound {
		t.Errorf("agent join with a device exchange open = %d", st)
	}

	if st, out := h.join("/owner/v1", exchange.Device, wrong(code), p); st != http.StatusForbidden || out["attempts_left"] != 2.0 {
		t.Fatalf("wrong code = %d %v", st, out)
	}
	if st := h.do("POST", "/owner/v1/exchanges/"+id+"/challenge", phone.token, nil, nil); st != http.StatusConflict {
		t.Errorf("challenge before the code = %d", st)
	}
	if st, out := h.join("/owner/v1", exchange.Device, code, p); st != http.StatusOK {
		t.Fatalf("right code = %d %v", st, out)
	}
	h.confirmed(phone, id, p)
	if st := h.do("POST", "/owner/v1/join", "", ask, nil); st != http.StatusNotFound {
		t.Errorf("join once confirmed = %d", st)
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

// wrong is another code than code.
func wrong(code string) string { return code[:5] + string('0'+(code[5]-'0'+1)%10) }

func TestExchangeEnds(t *testing.T) {
	h := start(t, nil)
	owner := h.pair()
	p := exchange.Payload{TokenSHA256: exchange.TokenSHA256(exchange.NewToken())}
	ask := map[string]string{"kind": exchange.Agent, "name": "laptop"}
	poll := func(id string) string {
		var out struct{ Status string }
		h.do("GET", "/internal/join/"+id, "", nil, &out)
		return out.Status
	}

	id, code := h.open(owner, exchange.Agent)
	h.do("POST", "/internal/join", "", ask, nil) // an attempt nobody answers
	if st := h.do("POST", "/internal/join", "", ask, nil); st != http.StatusConflict {
		t.Errorf("a second attempt at once = %d", st)
	}
	h.clock.add(attemptTTL)
	for left := 2.0; left >= 0; left-- {
		if st, out := h.join("/internal", exchange.Agent, wrong(code), p); st != http.StatusForbidden || out["attempts_left"] != left {
			t.Fatalf("wrong code = %d %v, want %v left", st, out, left)
		}
	}
	if s := poll(id); s != "ended" {
		t.Errorf("after three wrong codes = %q", s)
	}
	if st := h.do("POST", "/internal/join", "", ask, nil); st != http.StatusNotFound {
		t.Errorf("join after three wrong codes = %d", st)
	}

	id, _ = h.open(owner, exchange.Agent)
	if st := h.do("DELETE", "/owner/v1/exchanges/"+id, owner.token, nil, nil); st != http.StatusOK || poll(id) != "ended" {
		t.Errorf("cancel = %d %q", st, poll(id))
	}
	id, _ = h.open(owner, exchange.Agent)
	h.clock.add(joinTTL)
	var waiting []any
	if h.do("GET", "/owner/v1/exchanges", owner.token, nil, &waiting); len(waiting) != 0 || poll(id) != "ended" {
		t.Errorf("after 10 minutes: %v %q", waiting, poll(id))
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

// serveConnector serves f with state in dir and dials it as "test".
func serveConnector(t *testing.T, f *fakeConnector, dir string) map[string]*connector.Client {
	t.Helper()
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
	return map[string]*connector.Client{"test": connector.Dial("test", sock)}
}

// storeChats writes a data.db in dir with a group and a DM.
func storeChats(t *testing.T, dir string) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertContainers(t.Context(), []store.Container{
		{ID: "c3", Kind: "group", Name: "Book club", IsGroup: true, Participants: []store.Participant{{ID: "u1", Name: "Ana"}, {ID: "u2", Name: "Bo"}}},
		{ID: "d1", Kind: "dm", Name: "Ana", Participants: []store.Participant{{ID: "u1", Name: "Ana"}}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPickedChatNamed(t *testing.T) {
	dir := t.TempDir()
	storeChats(t, dir)
	h := start(t, serveConnector(t, &fakeConnector{}, dir))
	owner := h.pair()
	agent := h.agent(owner)
	req := h.request(agent)
	if st, _ := h.sign(owner, h.challenge(owner, "/owner/v1/requests/"+req+"/challenge", map[string]any{"chats": []string{"c1", "c3", "c9"}})); st != http.StatusOK {
		t.Fatalf("sign = %d", st)
	}
	var now struct {
		Grants []struct {
			ChatNames map[string]string `json:"chat_names"`
		}
	}
	h.do("GET", "/owner/v1/now", owner.token, nil, &now)
	want := map[string]string{"c1": "Family", "c3": "Book club"}
	if len(now.Grants) != 1 || !maps.Equal(now.Grants[0].ChatNames, want) {
		t.Errorf("grants = %+v, want names %v", now.Grants, want)
	}
}

func TestChatsListDMParticipants(t *testing.T) {
	dir := t.TempDir()
	storeChats(t, dir)
	h := start(t, serveConnector(t, &fakeConnector{}, dir))
	owner := h.pair()
	var chats []connector.Container
	if st := h.do("GET", "/owner/v1/connectors/test/chats", owner.token, nil, &chats); st != http.StatusOK {
		t.Fatalf("chats = %d", st)
	}
	got := map[string][]store.Participant{}
	for _, c := range chats {
		got[c.ID] = c.Participants
	}
	if len(chats) != 2 || got["c3"] != nil || !slices.Equal(got["d1"], []store.Participant{{ID: "u1", Name: "Ana"}}) {
		t.Errorf("chats = %+v", chats)
	}
}

func TestWriteExecutedOnce(t *testing.T) {
	f := &fakeConnector{}
	h := start(t, serveConnector(t, f, t.TempDir()))
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
