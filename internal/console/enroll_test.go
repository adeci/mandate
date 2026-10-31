package console

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
)

func (h *harness) adminEnroll(t *testing.T, name string) string {
	t.Helper()
	b, _ := json.Marshal(enrollRequest{Name: name})
	resp, err := http.Post(h.admin.URL+"/enroll", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out enrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll: %d %v", resp.StatusCode, err)
	}
	token, ok := strings.CutPrefix(out.URL, origin+"/enroll/")
	if !ok || token == "" || token != out.Token {
		t.Fatalf("enroll URL = %q", out.URL)
	}
	return token
}

func (h *harness) creationOptions(t *testing.T, token string) creationOptions {
	t.Helper()
	status, body := h.call(t, "GET", "/enroll/"+token+"/options", "", nil)
	if status != http.StatusOK {
		t.Fatalf("options: %d %s", status, body)
	}
	var o creationOptions
	if err := json.Unmarshal(body, &o); err != nil {
		t.Fatal(err)
	}
	return o
}

func (h *harness) register(t *testing.T, token string, key *approvaltest.Authenticator) (int, []byte) {
	t.Helper()
	o := h.creationOptions(t, token)
	c, err := base64.RawURLEncoding.DecodeString(o.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	return h.call(t, http.MethodPost, "/enroll/"+token, "", key.Register(c))
}

func TestEnrollmentLink(t *testing.T) {
	h := start(t)
	token := h.adminEnroll(t, "yubikey")

	if status, body := h.call(t, "GET", "/enroll/"+token, "", nil); status != http.StatusOK || !bytes.Contains(body, []byte("yubikey")) {
		t.Fatalf("enroll page: %d", status)
	}
	o := h.creationOptions(t, token)
	if o.RP.ID != rpID || o.AuthenticatorSelection.UserVerification != "required" || len(o.ExcludeCredentials) != 1 {
		t.Fatalf("creation options = %+v", o)
	}

	key := approvaltest.New(t, rpID, origin)
	status, body := h.register(t, token, key)
	if status != http.StatusOK {
		t.Fatalf("register: %d %s", status, body)
	}
	var got enrolled
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := key.Approver("yubikey")
	if got.Approver != want || got.Synced ||
		got.Nix != `{ name = "yubikey"; credential_id = "`+want.CredentialID+`"; public_key = "`+want.PublicKey+`"; }` {
		t.Fatalf("enrolled %+v, want %+v", got, want)
	}

	// The enrolled entry verifies real assertions once it is pinned.
	v := approval.Verifier{RPID: rpID, Origin: origin, Approvers: []approval.Approver{got.Approver}}
	rec, _ := approval.Encode(approval.Record{ID: "x", Connector: "fake", Op: "send", ExpiresAt: time.Now().Add(time.Hour)})
	if _, err := v.Verify(key.Approve(rec, time.Now())); err != nil {
		t.Fatalf("enrolled key doesn't verify: %v", err)
	}

	// But enrolling alone trusts nothing: no login, no approval.
	resp := h.do(t, http.MethodPost, "/login", "", loginRequest{Assertion: h.loginAssertion(t, key)})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("login with enrolled, unpinned key = %d, want 403", resp.StatusCode)
	}
	p := h.submit(t, "running late")
	nonce, c, _ := h.challenge(t, p.ID, nil)
	if status, _ := h.approve(t, p.ID, nonce, key.Assert(c)); status != http.StatusForbidden {
		t.Fatalf("approve with enrolled, unpinned key = %d, want 403", status)
	}

	// The link works once.
	if status, _ := h.call(t, http.MethodPost, "/enroll/"+token, "", key.Register([]byte("again"))); status != http.StatusNotFound {
		t.Fatalf("second registration = %d, want 404", status)
	}
	if status, _ := h.call(t, "GET", "/enroll/"+token, "", nil); status != http.StatusNotFound {
		t.Fatalf("page after use = %d, want 404", status)
	}

	b, err := os.ReadFile(filepath.Join(h.cfg.StateDir, "enrolled.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved []approval.Approver
	if err := json.Unmarshal(b, &saved); err != nil || len(saved) != 1 || saved[0] != want {
		t.Fatalf("enrolled.json = %s (%v)", b, err)
	}
	if loaded, err := loadApprovers(filepath.Join(h.cfg.StateDir, "enrolled.json")); err != nil || len(loaded) != 1 {
		t.Fatalf("enrolled.json isn't a valid approvers file: %v", err)
	}

	_, page := h.page(t, "/passkeys")
	if !strings.Contains(page, "yubikey") || !strings.Contains(page, want.CredentialID) {
		t.Fatalf("passkeys page doesn't list the unpinned key:\n%s", page)
	}
}

func TestEnrollmentLinkRefusals(t *testing.T) {
	t.Run("expired link", func(t *testing.T) {
		h := start(t)
		token := h.adminEnroll(t, "phone")
		h.clock.advance(10*time.Minute + time.Second)
		if status, _ := h.call(t, "GET", "/enroll/"+token, "", nil); status != http.StatusNotFound {
			t.Fatalf("page = %d, want 404", status)
		}
		if status, _ := h.call(t, "GET", "/enroll/"+token+"/options", "", nil); status != http.StatusNotFound {
			t.Fatalf("options = %d, want 404", status)
		}
		if status, _ := h.call(t, http.MethodPost, "/enroll/"+token, "", approvaltest.New(t, rpID, origin).Register([]byte("x"))); status != http.StatusNotFound {
			t.Fatalf("register = %d, want 404", status)
		}
	})

	for _, tt := range []struct {
		name string
		key  func(t *testing.T) *approvaltest.Authenticator
	}{
		{"wrong origin", func(t *testing.T) *approvaltest.Authenticator {
			return approvaltest.New(t, rpID, "https://evil.example")
		}},
		{"wrong RP ID", func(t *testing.T) *approvaltest.Authenticator {
			return approvaltest.New(t, "evil.example", origin)
		}},
		{"no user verification", func(t *testing.T) *approvaltest.Authenticator {
			k := approvaltest.New(t, rpID, origin)
			k.Flags = approvaltest.FlagUP
			return k
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t)
			token := h.adminEnroll(t, "phone")
			if status, body := h.register(t, token, tt.key(t)); status != http.StatusBadRequest {
				t.Fatalf("register = %d %s, want 400", status, body)
			}
			// A failed attempt spends the link too.
			if status, _ := h.call(t, "GET", "/enroll/"+token, "", nil); status != http.StatusNotFound {
				t.Fatalf("page after failed attempt = %d, want 404", status)
			}
			if _, err := os.Stat(filepath.Join(h.cfg.StateDir, "enrolled.json")); err == nil {
				t.Fatal("failed registration was saved")
			}
		})
	}

	t.Run("challenge from another link", func(t *testing.T) {
		h := start(t)
		first, second := h.adminEnroll(t, "a"), h.adminEnroll(t, "b")
		c, _ := base64.RawURLEncoding.DecodeString(h.creationOptions(t, first).Challenge)
		if status, _ := h.call(t, http.MethodPost, "/enroll/"+second, "", approvaltest.New(t, rpID, origin).Register(c)); status != http.StatusBadRequest {
			t.Fatalf("register = %d, want 400", status)
		}
	})

	t.Run("bad name", func(t *testing.T) {
		h := start(t)
		resp, err := http.Post(h.admin.URL+"/enroll", "application/json", strings.NewReader(`{"name":"\"; x = \"${y}"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("enroll = %d, want 400", resp.StatusCode)
		}
	})
}

func TestAddPasskeyFromConsole(t *testing.T) {
	h := start(t)
	status, body := h.post(t, "/passkeys", enrollRequest{Name: "pixel"})
	if status != http.StatusOK {
		t.Fatalf("add = %d %s", status, body)
	}
	var res enrollResponse
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}

	synced := approvaltest.New(t, rpID, origin)
	synced.Flags |= approvaltest.FlagBE
	status, body = h.register(t, res.Token, synced)
	if status != http.StatusOK {
		t.Fatalf("register = %d %s", status, body)
	}
	var got enrolled
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Synced {
		t.Fatalf("enrolled = %+v, want a synced passkey", got)
	}
	keys, err := h.srv.db.passkeys(t.Context())
	if err != nil || len(keys) != 1 || !keys[0].Synced || keys[0].Name != "pixel" {
		t.Fatalf("stored passkeys = %+v (%v)", keys, err)
	}
}
