package httpproxy

import (
	"bufio"
	"context"
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

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/approval/approvaltest"
	"github.com/adeci/mandate/internal/connector"
)

const (
	rpID   = "approve.example.ts.net"
	origin = "https://approve.example.ts.net"
	// Has characters that change under query and JSON escaping.
	token = "ghp_s3cr3t+T0ken/abc=0123456789"
)

// upstream records what reached it.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

type recorded struct {
	method, rawPath, rawQuery, host, body string
	header                                http.Header
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, recorded{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Host, string(body), r.Header.Clone()})
		u.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) requests() []recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recorded(nil), u.reqs...)
}

// echo reflects the token back in every way an upstream plausibly could.
func echo(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	w.Header().Set("Set-Cookie", "session=abc; HttpOnly")
	w.Header().Set("X-Echo", auth)
	w.Header().Set("Link", "<https://up/x?access_token="+url.QueryEscape(strings.TrimPrefix(auth, "Bearer "))+`>; rel="next"`)
	w.Header().Set("Etag", `"v2"`)
	w.Header().Set("X-Ratelimit-Remaining", "42")
	w.Header().Set("Ratelimit-Policy", "100;w=60")
	w.Header().Set("Retry-After", "5")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"auth":%q,"escaped":%q,"query":"%s"}`, auth, auth, url.QueryEscape(auth))
}

type harness struct {
	up     *upstream
	proxy  *Proxy
	sock   string
	client *connector.Client
	key    *approvaltest.Authenticator
}

func start(t *testing.T, h http.HandlerFunc) *harness {
	t.Helper()
	up := newUpstream(t, h)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "github"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	services, err := ParseConfig([]byte(`[{"name":"github","base_url":"` + up.URL + `",
		"token":{"header":"Authorization","prefix":"Bearer ","file":"github"},
		"reads":[{"method":"GET","path":"/repos/*/*/issues"}],
		"writes":[{"method":"POST","path":"/repos/*/*/issues"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(services, dir, log)
	if err != nil {
		t.Fatal(err)
	}
	key := approvaltest.New(t, rpID, origin)
	seen, err := approval.OpenSeen(filepath.Join(dir, "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &connector.Server{
		Connector: p,
		Verifier:  &approval.Verifier{RPID: rpID, Origin: origin, Approvers: []approval.Approver{key.Approver("phone")}},
		Seen:      seen,
		Log:       log,
	}
	sock := filepath.Join(dir, "p.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- connector.ServeUnix(ctx, sock, p.Handler(srv)) }()
	t.Cleanup(func() { cancel(); <-done })
	c := connector.Dial(sock, 5*time.Second)
	for i := 0; ; i++ {
		if _, err := c.Spec(context.Background()); err == nil {
			break
		} else if i == 100 {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &harness{up: up, proxy: p, sock: sock, client: c, key: key}
}

type reply struct {
	StatusCode int
	Header     http.Header
}

// raw sends target byte for byte, so nothing on the client side cleans the
// path before the proxy sees it.
func (h *harness) raw(t *testing.T, method, target string, header map[string]string, body string) (reply, string) {
	t.Helper()
	conn, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: proxy.local\r\nConnection: close\r\n", method, target)
	for k, v := range header {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(body), body)
	if _, err := io.WriteString(conn, b.String()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{resp.StatusCode, resp.Header}, string(out)
}

func leaksToken(s string) bool {
	return strings.Contains(s, token) || strings.Contains(s, url.QueryEscape(token))
}

func TestForwardInjectsTokenAndStripsAgentHeaders(t *testing.T) {
	t.Parallel()
	h := start(t, echo)
	resp, body := h.raw(t, "GET", "/forward/github/repos/a/b/issues?state=open&per_page=5", map[string]string{
		"Authorization":       "Bearer agent-supplied",
		"Proxy-Authorization": "Basic eA==",
		"Cookie":              "session=agent",
		"X-Forwarded-For":     "10.0.0.1",
		"X-Forwarded-Host":    "evil.example",
		"Forwarded":           "for=10.0.0.1",
		"X-Real-Ip":           "10.0.0.1",
		"Accept":              "application/vnd.github+json",
		"If-None-Match":       `"v1"`,
		"Accept-Encoding":     "br",
	}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	reqs := h.up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests", len(reqs))
	}
	got := reqs[0]
	if got.method != "GET" || got.rawPath != "/repos/a/b/issues" || got.rawQuery != "state=open&per_page=5" {
		t.Errorf("upstream got %s %s?%s", got.method, got.rawPath, got.rawQuery)
	}
	if want := strings.TrimPrefix(h.up.URL, "http://"); got.host != want {
		t.Errorf("upstream Host = %q, want %q", got.host, want)
	}
	if a := got.header.Values("Authorization"); len(a) != 1 || a[0] != "Bearer "+token {
		t.Errorf("upstream Authorization = %q", a)
	}
	for _, k := range []string{"Proxy-Authorization", "Cookie", "X-Forwarded-For", "X-Forwarded-Host", "Forwarded", "X-Real-Ip"} {
		if v := got.header.Get(k); v != "" {
			t.Errorf("upstream got %s: %s", k, v)
		}
	}
	if got.header.Get("Accept") != "application/vnd.github+json" || got.header.Get("If-None-Match") != `"v1"` {
		t.Errorf("Accept/If-None-Match not forwarded: %v", got.header)
	}
	if got.header.Get("Content-Type") != "" {
		t.Errorf("Content-Type forwarded on a read: %v", got.header)
	}
	if ae := got.header.Get("Accept-Encoding"); ae == "br" {
		t.Error("agent's Accept-Encoding was forwarded")
	}

	for _, k := range []string{"Set-Cookie", "X-Echo"} {
		if c := resp.Header.Values(k); len(c) != 0 {
			t.Errorf("%s reached the agent: %q", k, c)
		}
	}
	if resp.Header.Get("Etag") != `"v2"` || resp.Header.Get("X-Ratelimit-Remaining") != "42" ||
		resp.Header.Get("Ratelimit-Policy") != "100;w=60" || resp.Header.Get("Retry-After") != "5" {
		t.Errorf("useful headers dropped: %v", resp.Header)
	}
	if leaksToken(body) || leaksToken(fmt.Sprint(resp.Header)) {
		t.Errorf("token leaked: %v %s", resp.Header, body)
	}
	if !strings.Contains(body, redacted) || !strings.Contains(resp.Header.Get("Link"), redacted) {
		t.Errorf("echoed token not redacted: %v %s", resp.Header, body)
	}
}

func TestForwardDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	elsewhere := newUpstream(t, echo)
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
	})
	resp, body := h.raw(t, "GET", "/forward/github/repos/a/b/issues", nil, "")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != elsewhere.URL+"/steal" {
		t.Fatalf("got %d %v: %s", resp.StatusCode, resp.Header, body)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Fatalf("redirect was followed %d times", n)
	}
}

func TestForwardRefusesPathTricks(t *testing.T) {
	t.Parallel()
	h := start(t, echo)
	for _, target := range []string{
		"/forward/github/repos/a/../admin",
		"/forward/github/repos/a/b/../b/issues",
		"/forward/github/repos/a/./b/issues",
		"/forward/github/repos/%2e%2e/admin/issues",
		"/forward/github/repos/a/%2E%2e/b/issues",
		"/forward/github/repos/a%2Fb/c/issues",
		"/forward/github/repos/a%2fb/c/issues",
		"/forward/github/repos//b/issues",
		"/forward/github//repos/a/b/issues",
		"/forward/github/repos/a/b/issues/",
		`/forward/github/repos/a\b/c/issues`,
		"/forward/github/repos/a%5cb/c/issues",
		"/forward/github/repos/a/b/issues%00",
	} {
		resp, body := h.raw(t, "GET", target, nil, "")
		// The JSON error shows the proxy refused it, not net/http's parser.
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"error":"path `) {
			t.Errorf("%s: status %d: %s", target, resp.StatusCode, body)
		}
	}
	if n := len(h.up.requests()); n != 0 {
		t.Fatalf("upstream saw %d requests", n)
	}
}

func TestForwardRefusesUnknownServiceAndRoute(t *testing.T) {
	t.Parallel()
	h := start(t, echo)
	for _, tt := range []struct {
		method, target string
		status         int
	}{
		{"GET", "/forward/gitlab/repos/a/b/issues", http.StatusNotFound},
		{"GET", "/forward/github", http.StatusBadRequest},
		{"GET", "/forward/github/user", http.StatusForbidden},
		{"GET", "/forward/github/repos/a/b/issues/1", http.StatusForbidden},
		{"DELETE", "/forward/github/repos/a/b/issues", http.StatusForbidden},
		// A write rule is not a read rule.
		{"POST", "/forward/github/repos/a/b/issues", http.StatusForbidden},
		// A read rule can't be a POST, so a GraphQL query has no way in.
		{"POST", "/forward/github/graphql", http.StatusForbidden},
	} {
		resp, body := h.raw(t, tt.method, tt.target, nil, "")
		if resp.StatusCode != tt.status {
			t.Errorf("%s %s: status %d, want %d: %s", tt.method, tt.target, resp.StatusCode, tt.status, body)
		}
	}
	if n := len(h.up.requests()); n != 0 {
		t.Fatalf("upstream saw %d requests", n)
	}
}

func TestForwardCapsResponse(t *testing.T) {
	t.Parallel()
	h := start(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(make([]byte, maxResponse+1))
	})
	resp, body := h.raw(t, "GET", "/forward/github/repos/a/b/issues", nil, "")
	if resp.StatusCode != http.StatusBadGateway || len(body) > 1024 {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(body))
	}
}

// approved prepares args and signs the resulting record, as the console does.
func (h *harness) approved(t *testing.T, args map[string]string) approval.Approval {
	t.Helper()
	p, err := h.client.Prepare(context.Background(), "request", args)
	if err != nil {
		t.Fatal(err)
	}
	rec := approval.Record{
		ID: "p1", Connector: "http", Op: "request", Args: p.Args, Display: p.Display,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	b, err := approval.Encode(rec)
	if err != nil {
		t.Fatal(err)
	}
	return h.key.Approve(b, time.Now())
}

func TestApprovedWriteRunsOnce(t *testing.T) {
	t.Parallel()
	h := start(t, echo)
	a := h.approved(t, map[string]string{
		"service": "github", "method": "post", "path": "/repos/%61/b/issues", "body": `{"title":"hi"}`,
	})
	res, err := h.client.Execute(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != connector.Done || res.Ref != "201" || res.Detail != "201 Created" {
		t.Fatalf("result %+v", res)
	}
	if leaksToken(res.Output) || !strings.Contains(res.Output, `"auth":"Bearer `+redacted) {
		t.Fatalf("output not scrubbed: %s", res.Output)
	}
	if _, err := h.client.Execute(context.Background(), a); !isStatus(err, http.StatusConflict) {
		t.Fatalf("replay: %v", err)
	}
	reqs := h.up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests", len(reqs))
	}
	got := reqs[0]
	if got.method != "POST" || got.rawPath != "/repos/a/b/issues" || got.body != `{"title":"hi"}` ||
		got.header.Get("Content-Type") != "application/json" || got.header.Get("Authorization") != "Bearer "+token {
		t.Fatalf("upstream got %+v", got)
	}
}

func TestPrepare(t *testing.T) {
	t.Parallel()
	h := start(t, echo)
	ctx := context.Background()
	p, err := h.client.Prepare(ctx, "request", map[string]string{
		"service": "github", "method": "post", "path": "/repos/%61/b/issues", "body": `{"title":"hi","labels":["x"]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []approval.Field{
		{Label: "Service", Value: "github"},
		{Label: "Request", Value: "POST /repos/a/b/issues"},
		{Label: "Body", Value: "{\n  \"title\": \"hi\",\n  \"labels\": [\n    \"x\"\n  ]\n}", Param: "body"},
	}
	if fmt.Sprint(p.Display) != fmt.Sprint(want) || p.Container != "" {
		t.Fatalf("display %q", p.Display)
	}
	again, err := h.client.Prepare(ctx, "request", p.Args)
	if err != nil || fmt.Sprint(again) != fmt.Sprint(p) {
		t.Fatalf("not idempotent: %+v vs %+v (%v)", again, p, err)
	}

	for _, tt := range []struct {
		args   map[string]string
		status int
	}{
		{map[string]string{"service": "github", "method": "DELETE", "path": "/repos/a/b/issues"}, http.StatusForbidden},
		{map[string]string{"service": "github", "method": "POST", "path": "/repos/a/b/pulls"}, http.StatusForbidden},
		{map[string]string{"service": "github", "method": "POST", "path": "/graphql"}, http.StatusForbidden},
		{map[string]string{"service": "github", "method": "POST", "path": "/repos/a/../b/c/issues"}, http.StatusBadRequest},
		{map[string]string{"service": "github", "method": "POST", "path": "/repos/a%2fb/c/issues"}, http.StatusBadRequest},
		{map[string]string{"service": "github", "method": "POST", "path": "/repos/a/b/issues?x=1"}, http.StatusBadRequest},
		{map[string]string{"service": "github", "method": "POST", "path": "repos/a/b/issues"}, http.StatusBadRequest},
		{map[string]string{"service": "github", "method": "PO ST", "path": "/repos/a/b/issues"}, http.StatusBadRequest},
		{map[string]string{"service": "gitlab", "method": "POST", "path": "/repos/a/b/issues"}, http.StatusBadRequest},
		{map[string]string{"service": "github", "method": "POST", "path": "/repos/a/b/issues", "url": "https://evil"}, http.StatusBadRequest},
	} {
		if _, err := h.client.Prepare(ctx, "request", tt.args); !isStatus(err, tt.status) {
			t.Errorf("prepare %v: %v, want %d", tt.args, err, tt.status)
		}
	}
	if n := len(h.up.requests()); n != 0 {
		t.Fatalf("upstream saw %d requests", n)
	}
}

func TestExecuteClassifiesFailures(t *testing.T) {
	t.Parallel()
	// The upstream reads the request, then drops the connection without
	// answering: it may have acted, so the result is unknown and the write
	// is not resent.
	h := start(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	rec := approval.Record{ID: "p1", Connector: "http", Op: "request", Args: map[string]string{
		"service": "github", "method": "POST", "path": "/repos/a/b/issues", "body": "hello",
	}}
	if res := h.proxy.Execute(context.Background(), rec); res.Status != connector.Unknown {
		t.Fatalf("dropped connection: %+v", res)
	}
	reqs := h.up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests", len(reqs))
	}
	if ct := reqs[0].header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("non-JSON body sent as %q", ct)
	}

	// Nothing listens: nothing was sent, so it failed.
	h.up.Close()
	if res := h.proxy.Execute(context.Background(), rec); res.Status != connector.Failed || leaksToken(res.Detail) {
		t.Fatalf("refused connection: %+v", res)
	}
}

func TestExecuteMapsStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		code int
		want connector.Status
	}{
		{http.StatusCreated, connector.Done},
		{http.StatusNotFound, connector.Failed},
		{http.StatusUnprocessableEntity, connector.Failed},
		{http.StatusInternalServerError, connector.Unknown},
		{http.StatusBadGateway, connector.Unknown},
	} {
		h := start(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tt.code) })
		rec := approval.Record{ID: "p1", Connector: "http", Op: "request", Args: map[string]string{
			"service": "github", "method": "POST", "path": "/repos/a/b/issues",
		}}
		res := h.proxy.Execute(context.Background(), rec)
		if res.Status != tt.want || res.Ref != fmt.Sprint(tt.code) || !strings.HasPrefix(res.Detail, fmt.Sprint(tt.code)) {
			t.Errorf("%d: %+v", tt.code, res)
		}
		if n := len(h.up.requests()); n != 1 {
			t.Errorf("%d: upstream saw %d requests", tt.code, n)
		}
	}
}

func TestExecuteCapsOutput(t *testing.T) {
	t.Parallel()
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		// The token straddles the cap, so scrubbing after cutting would
		// leave part of it.
		io.WriteString(w, strings.Repeat("é", maxOutput/2-4))
		io.WriteString(w, r.Header.Get("Authorization"))
		io.WriteString(w, strings.Repeat("x", 1000))
	})
	rec := approval.Record{ID: "p1", Connector: "http", Op: "request", Args: map[string]string{
		"service": "github", "method": "POST", "path": "/repos/a/b/issues",
	}}
	res := h.proxy.Execute(context.Background(), rec)
	if res.Status != connector.Done || len(res.Output) > maxOutput+100 || !strings.Contains(res.Output, "truncated") {
		t.Fatalf("status %s, output %d bytes", res.Status, len(res.Output))
	}
	if strings.Contains(res.Output, "ghp_") || strings.Contains(res.Output, token[:8]) {
		t.Fatalf("partial token in output: %q", res.Output[maxOutput-16:maxOutput+8])
	}
}

func TestSpecDocListsRules(t *testing.T) {
	t.Parallel()
	h := start(t, echo)
	spec, err := h.client.Spec(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(h.up.URL, "http://")
	for _, want := range []string{"github", host, "GET /repos/*/*/issues", "POST /repos/*/*/issues", "mandate http GET"} {
		if !strings.Contains(spec.Doc, want) {
			t.Errorf("doc lacks %q:\n%s", want, spec.Doc)
		}
	}
}

func isStatus(err error, status int) bool {
	var e *connector.Error
	return errors.As(err, &e) && e.Status == status
}
