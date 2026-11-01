package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/internal/proposal"
)

func TestProtectedRoutesNeedSession(t *testing.T) {
	h := start(t)
	p := h.submit(t, "the code is 4711")
	expired := h.login(t, h.key)
	h.clock.advance(sessionTTL + time.Second)
	h.session = h.login(t, h.key)
	loggedOut := h.login(t, h.key)
	if status, _ := h.call(t, http.MethodPost, "/logout", loggedOut, nil); status != http.StatusSeeOther {
		t.Fatalf("logout = %d", status)
	}

	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", "/", nil},
		{"GET", "/r/" + p.ID, nil},
		{"POST", "/r/" + p.ID + "/challenge", challengeRequest{}},
		{"POST", "/r/" + p.ID + "/approve", approveRequest{Nonce: "AAAA", Assertion: json.RawMessage(`{}`)}},
		{"POST", "/r/" + p.ID + "/deny", nil},
		{"POST", "/r/" + p.ID + "/delete", nil},
		{"GET", "/connections", nil},
		{"GET", "/connections/fake/health", nil},
		{"POST", "/connections/fake/setup", setupRequest{}},
		{"POST", "/connections/fake/unlink", nil},
		{"GET", "/keys", nil},
		{"POST", "/keys/options", registerRequest{Name: "mallory"}},
		{"POST", "/keys", json.RawMessage(`{}`)},
		{"POST", "/keys/remove", nil},
		{"POST", "/keys/changes/x/challenge", nil},
		{"POST", "/keys/changes/x/approve", keyApproveRequest{}},
		{"POST", "/keys/changes/x/discard", nil},
		{"GET", "/session", nil},
		{"POST", "/logout", nil},
	}
	for _, token := range []struct{ name, value string }{
		{"no cookie", ""},
		{"forged cookie", "not-a-session"},
		{"expired session", expired},
		{"logged out session", loggedOut},
	} {
		for _, rt := range routes {
			status, body := h.call(t, rt.method, rt.path, token.value, rt.body)
			want := http.StatusUnauthorized
			if rt.method == "GET" && rt.path != "/session" && !strings.HasPrefix(rt.path, "/connections/") {
				want = http.StatusSeeOther
			}
			if status != want {
				t.Errorf("%s: %s %s = %d, want %d", token.name, rt.method, rt.path, status, want)
			}
			if strings.Contains(string(body), "4711") || strings.Contains(string(body), p.ID) && status != http.StatusSeeOther {
				t.Errorf("%s: %s %s leaks the request: %s", token.name, rt.method, rt.path, body)
			}
		}
	}
	resp := h.do(t, "GET", "/r/"+p.ID, "", nil)
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/login?next=%2Fr%2F"+p.ID {
		t.Fatalf("redirect = %q", loc)
	}
	if got := h.get(t, p.ID); got.Status != proposal.Pending {
		t.Fatalf("status = %s after unauthenticated requests", got.Status)
	}
	if h.fake.linked || h.fake.setupCalls != 0 || h.fake.unlinks != 0 {
		t.Fatal("unauthenticated setup or unlink reached the connector")
	}
	h.srv.regMu.Lock()
	n := len(h.srv.regs)
	h.srv.regMu.Unlock()
	if n != 0 {
		t.Fatalf("%d key registrations started without a session", n)
	}

	for _, path := range []string{"/login", "/static/app.js"} {
		if status, _ := h.call(t, "GET", path, "", nil); status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, status)
		}
	}
}

func TestLoginSession(t *testing.T) {
	h := start(t)

	resp := h.do(t, http.MethodPost, "/login", "", loginRequest{Assertion: h.loginAssertion(t, h.key), Next: "/r/abc"})
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if resp.StatusCode != http.StatusOK || cookie == nil {
		t.Fatalf("login = %d, cookie %v", resp.StatusCode, cookie)
	}
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" ||
		cookie.Domain != "" || cookie.MaxAge != int((12*time.Hour).Seconds()) {
		t.Fatalf("cookie = %+v", cookie)
	}
	token := cookie.Value
	if status, _ := h.call(t, "GET", "/session", token, nil); status != http.StatusNoContent {
		t.Fatalf("session = %d", status)
	}
	// Only the hash is stored.
	var stored int
	if err := h.srv.db.sql.QueryRow(`SELECT count(*) FROM sessions WHERE hash = ?`, []byte(token)).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("token stored in the clear (%v)", err)
	}

	if status, _ := h.call(t, http.MethodPost, "/logout", token, nil); status != http.StatusSeeOther {
		t.Fatalf("logout = %d", status)
	}
	if status, _ := h.call(t, "GET", "/session", token, nil); status != http.StatusUnauthorized {
		t.Fatalf("session after logout = %d", status)
	}

	token = h.login(t, h.key)
	h.clock.advance(sessionTTL - time.Minute)
	if status, _ := h.call(t, "GET", "/session", token, nil); status != http.StatusNoContent {
		t.Fatalf("session before expiry = %d", status)
	}
	h.clock.advance(2 * time.Minute)
	if status, _ := h.call(t, "GET", "/session", token, nil); status != http.StatusUnauthorized {
		t.Fatalf("session after 12h = %d", status)
	}
}

func TestLoginRefused(t *testing.T) {
	h := start(t)
	stranger := approvaltest.New(t, rpID, origin)
	noUV := approvaltest.New(t, rpID, origin)
	noUV.Flags = approvaltest.FlagUP
	phishing := approvaltest.New(t, rpID, "https://evil.example")
	h.trust(t, noUV, "no-uv")
	h.trust(t, phishing, "phished")

	replayed := h.loginAssertion(t, h.key)
	if status, _ := h.call(t, http.MethodPost, "/login", "", loginRequest{Assertion: replayed}); status != http.StatusOK {
		t.Fatalf("first login = %d", status)
	}
	stale := h.loginAssertion(t, h.key)
	h.clock.advance(loginTTL + time.Second)

	for name, assertion := range map[string]json.RawMessage{
		"unknown passkey":     h.loginAssertion(t, stranger),
		"no user verified":    h.loginAssertion(t, noUV),
		"wrong origin":        h.loginAssertion(t, phishing),
		"replayed":            replayed,
		"stale challenge":     stale,
		"forged challenge":    h.key.Assert(make([]byte, loginChallengeSize)),
		"approval challenge":  h.key.Assert([]byte("an approval challenge is a sha256")),
		"not a webauthn blob": json.RawMessage(`{"id":"x"}`),
	} {
		resp := h.do(t, http.MethodPost, "/login", "", loginRequest{Assertion: assertion})
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: login = %d, want refused", name, resp.StatusCode)
		}
		if len(resp.Cookies()) != 0 {
			t.Errorf("%s: login set a cookie", name)
		}
	}
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{
		"/connections?x=1":     "/connections?x=1",
		"//evil.example/":      "/",
		`/\evil.example`:       "/",
		"https://evil.example": "/",
		"r/abc":                "/",
		"/login?next=/":        "/",
	} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q, want %q", in, got, want)
		}
	}
}
