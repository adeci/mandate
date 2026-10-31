package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/internal/proposal"
)

func (h *harness) passwordLogin(t *testing.T) string {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/login", "", loginRequest{Password: password})
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && resp.StatusCode == http.StatusOK {
			return c.Value
		}
	}
	t.Fatalf("password login = %d", resp.StatusCode)
	return ""
}

// register creates key named name the way a browser does.
func (h *harness) register(t *testing.T, token string, key *approvaltest.Authenticator, name string) (int, []byte) {
	t.Helper()
	status, body := h.call(t, http.MethodPost, "/keys/options", token, registerRequest{Name: name})
	if status != http.StatusOK {
		t.Fatalf("key options: %d %s", status, body)
	}
	var o creationOptions
	if err := json.Unmarshal(body, &o); err != nil {
		t.Fatal(err)
	}
	c, err := base64.RawURLEncoding.DecodeString(o.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	return h.call(t, http.MethodPost, "/keys", token, key.Register(c))
}

// addKey registers key and reports whether it is trusted at once.
func (h *harness) addKey(t *testing.T, token string, key *approvaltest.Authenticator, name string) bool {
	t.Helper()
	status, body := h.register(t, token, key, name)
	var res addKeyResponse
	if status != http.StatusOK || json.Unmarshal(body, &res) != nil {
		t.Fatalf("add key: %d %s", status, body)
	}
	return res.Trusted
}

// trust adds key to the log as approved by h.key, skipping the pages.
func (h *harness) trust(t *testing.T, key *approvaltest.Authenticator, name string) {
	t.Helper()
	change, err := h.srv.keys.Next(approval.OpAdd, key.Key(name), h.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.keys.Apply(h.key.Sign(change)); err != nil {
		t.Fatal(err)
	}
	h.srv.syncKeys(context.Background())
}

func (h *harness) waiting(t *testing.T) string {
	t.Helper()
	cs, err := h.srv.db.keyChanges(context.Background(), h.srv.now())
	if err != nil || len(cs) != 1 {
		t.Fatalf("waiting key changes = %+v, %v", cs, err)
	}
	return cs[0].ID
}

// approveChange signs waiting change id with signer.
func (h *harness) approveChange(t *testing.T, token, id string, signer *approvaltest.Authenticator) int {
	t.Helper()
	status, body := h.call(t, http.MethodPost, "/keys/changes/"+id+"/challenge", token, nil)
	if status != http.StatusOK {
		return status
	}
	var res keyChallengeResponse
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	c, err := base64.RawURLEncoding.DecodeString(res.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	status, _ = h.call(t, http.MethodPost, "/keys/changes/"+id+"/approve", token, keyApproveRequest{Change: res.Change, Assertion: signer.Assert(c)})
	return status
}

func (h *harness) postForm(t *testing.T, path, token string, form url.Values) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.pages.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (h *harness) loginStatus(t *testing.T, key *approvaltest.Authenticator) int {
	t.Helper()
	status, _ := h.call(t, http.MethodPost, "/login", "", loginRequest{Assertion: h.loginAssertion(t, key)})
	return status
}

func (h *harness) synced(t *testing.T) {
	t.Helper()
	n, head := h.srv.keys.Head()
	if cn, chead := h.connKeys.Head(); cn != n || chead != head {
		t.Fatalf("connector key log at %d %s, console at %d %s", cn, chead, n, head)
	}
}

func TestPasswordSignIn(t *testing.T) {
	h := bare(t, nil)
	if status, _ := h.call(t, http.MethodGet, "/session", h.session, nil); status != http.StatusNoContent {
		t.Fatalf("password session = %d", status)
	}
	if status, _ := h.call(t, http.MethodPost, "/login/options", "", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("passkey login options without keys = %d", status)
	}

	try := func(pw string) int {
		status, _ := h.call(t, http.MethodPost, "/login", "", loginRequest{Password: pw})
		return status
	}
	for range freeTries {
		if status := try("wrong"); status != http.StatusForbidden {
			t.Fatalf("wrong password = %d", status)
		}
	}
	if status := try(password); status != http.StatusTooManyRequests {
		t.Fatalf("right password while throttled = %d", status)
	}
	h.clock.advance(2 * time.Second)
	if status := try(password); status != http.StatusOK {
		t.Fatalf("right password after waiting = %d", status)
	}
}

func TestPasswordSessionCannotApprove(t *testing.T) {
	h := start(t)
	pw := h.passwordLogin(t)
	stranger := approvaltest.New(t, rpID, origin)

	p := h.submit(t, "running late")
	status, body := h.call(t, http.MethodPost, "/r/"+p.ID+"/challenge", pw, challengeRequest{})
	var ch challengeResponse
	if status != http.StatusOK || json.Unmarshal(body, &ch) != nil {
		t.Fatalf("challenge = %d %s", status, body)
	}
	c, _ := base64.RawURLEncoding.DecodeString(ch.Options.Challenge)
	for name, assertion := range map[string]json.RawMessage{"none": nil, "untrusted key": stranger.Assert(c)} {
		if status, _ := h.call(t, http.MethodPost, "/r/"+p.ID+"/approve", pw, approveRequest{Nonce: ch.Nonce, Assertion: assertion}); status != http.StatusForbidden {
			t.Errorf("approve with %s = %d, want 403", name, status)
		}
	}

	laptop := approvaltest.New(t, rpID, origin)
	if h.addKey(t, pw, laptop, "laptop") {
		t.Fatal("second key trusted without approval")
	}
	id := h.waiting(t)
	if status := h.approveChange(t, pw, id, stranger); status != http.StatusForbidden {
		t.Fatalf("key change approved by an untrusted key = %d", status)
	}
	if _, ok := h.srv.keys.Find(laptop.Key("").CredentialID); ok || len(h.fake.sent()) != 0 {
		t.Fatal("password session changed keys or ran a write")
	}

	// A trusted key's assertion is what counts, whatever the session.
	if status := h.approveChange(t, pw, id, h.key); status != http.StatusOK {
		t.Fatalf("key change approved by the trusted key = %d", status)
	}
	if status, _ := h.call(t, http.MethodPost, "/r/"+p.ID+"/approve", pw, approveRequest{Nonce: ch.Nonce, Assertion: h.key.Assert(c)}); status != http.StatusOK {
		t.Fatalf("approve with the trusted key = %d", status)
	}
}

func TestKeyChanges(t *testing.T) {
	h := start(t)
	h.synced(t)

	laptop := approvaltest.New(t, rpID, origin)
	laptop.Flags |= approvaltest.FlagBE
	if h.addKey(t, h.session, laptop, "laptop") {
		t.Fatal("second key trusted without approval")
	}
	if status := h.loginStatus(t, laptop); status != http.StatusForbidden {
		t.Fatalf("waiting key signs in = %d", status)
	}
	id := h.waiting(t)
	if status := h.approveChange(t, h.session, id, laptop); status != http.StatusForbidden {
		t.Fatalf("key approved its own addition = %d", status)
	}
	if status := h.approveChange(t, h.session, id, h.key); status != http.StatusOK {
		t.Fatalf("approve = %d", status)
	}
	if k, ok := h.srv.keys.Find(laptop.Key("").CredentialID); !ok || !k.Synced || k.Name != "laptop" {
		t.Fatalf("laptop = %+v, %v", k, ok)
	}
	h.synced(t)
	if status := h.approveChange(t, h.session, id, h.key); status != http.StatusNotFound {
		t.Fatalf("approving again = %d", status)
	}
	laptopSession := h.login(t, laptop)

	// Removing the first key, approved from the laptop, ends its sessions.
	if status := h.postForm(t, "/keys/remove", h.session, url.Values{"credential_id": {h.key.Key("").CredentialID}}); status != http.StatusSeeOther {
		t.Fatalf("remove = %d", status)
	}
	if status := h.approveChange(t, laptopSession, h.waiting(t), laptop); status != http.StatusOK {
		t.Fatalf("approve removal = %d", status)
	}
	h.synced(t)
	if status, _ := h.call(t, http.MethodGet, "/session", h.session, nil); status != http.StatusUnauthorized {
		t.Fatalf("removed key's session = %d", status)
	}
	if status := h.postForm(t, "/keys/remove", laptopSession, url.Values{"credential_id": {laptop.Key("").CredentialID}}); status != http.StatusConflict {
		t.Fatalf("removing the last key = %d", status)
	}

	// Waiting changes expire.
	if h.addKey(t, laptopSession, approvaltest.New(t, rpID, origin), "late") {
		t.Fatal("third key trusted without approval")
	}
	id = h.waiting(t)
	h.clock.advance(keyChangeTTL + time.Minute)
	if status := h.approveChange(t, h.passwordLogin(t), id, laptop); status != http.StatusNotFound {
		t.Fatalf("approving an expired change = %d", status)
	}
}

func TestConnectorKeyLogs(t *testing.T) {
	var rogue *fake
	h := startWith(t, func(cfg *Config, dir string) {
		sock := dir + "/rogue.sock"
		rogue, _ = serveFake(t, sock, "rogue", approvaltest.Keys(t, approvaltest.New(t, rpID, origin)))
		cfg.Connectors["rogue"] = sock
	})
	h.synced(t)
	for _, path := range []string{"/keys", "/connections"} {
		if _, body := h.page(t, path); !strings.Contains(body, "rogue") || !strings.Contains(body, "key log refused") {
			t.Errorf("%s doesn't warn about rogue:\n%s", path, body)
		}
	}

	p := h.submitTo(t, "rogue", "hi")
	nonce, c, _ := h.challenge(t, p.ID, nil)
	status, got := h.approve(t, p.ID, nonce, h.key.Assert(c))
	if status != http.StatusOK || got.Status != proposal.Failed || !strings.Contains(got.Result.Detail, "unknown passkey") {
		t.Fatalf("approve on rogue = %d %+v", status, got)
	}
	if n := len(rogue.sent()); n != 0 {
		t.Fatalf("rogue executed %d times", n)
	}
}
