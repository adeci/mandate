package approvals

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/approval/approvaltest"
)

// startEnroll asks the admin socket for a link and returns its token.
func (h *harness) startEnroll(t *testing.T, name string) string {
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
	if !ok || token == "" {
		t.Fatalf("enroll URL = %q", out.URL)
	}
	if d := out.ExpiresAt.Sub(h.clock.now()); d > 10*time.Minute || d < 9*time.Minute {
		t.Fatalf("link valid for %v", d)
	}
	return token
}

func (h *harness) enrollGet(t *testing.T, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(h.pages.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (h *harness) creationOptions(t *testing.T, token string) creationOptions {
	t.Helper()
	status, body := h.enrollGet(t, "/enroll/"+token+"/options")
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
	return h.post(t, "/enroll/"+token, key.Register(c))
}

func TestEnrollment(t *testing.T) {
	h := start(t)
	token := h.startEnroll(t, "yubikey")

	if status, body := h.enrollGet(t, "/enroll/"+token); status != http.StatusOK || !bytes.Contains(body, []byte("yubikey")) {
		t.Fatalf("enroll page: %d", status)
	}
	o := h.creationOptions(t, token)
	var algs []int
	for _, p := range o.PubKeyCredParams {
		algs = append(algs, int(p.Algorithm))
	}
	if o.RP != (rpEntity{ID: rpID, Name: "mandate"}) || !slices.Equal(algs, []int{-7, -8, -257}) ||
		o.AuthenticatorSelection != (authenticatorSelection{ResidentKey: "preferred", UserVerification: "required"}) ||
		o.Attestation != "none" || len(o.ExcludeCredentials) != 1 || o.User.ID == "" {
		t.Fatalf("creation options = %+v", o)
	}

	key := approvaltest.New(t, rpID, origin)
	status, body := h.register(t, token, key)
	if status != http.StatusOK {
		t.Fatalf("register: %d %s", status, body)
	}
	var got approval.Approver
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if want := key.Approver("yubikey"); got != want {
		t.Fatalf("enrolled %+v, want %+v", got, want)
	}

	// The enrolled entry verifies real assertions once it is trusted.
	v := approval.Verifier{RPID: rpID, Origin: origin, Approvers: []approval.Approver{got}}
	rec, _ := approval.Encode(approval.Record{ID: "x", Connector: "fake", Op: "send", ExpiresAt: time.Now().Add(time.Hour)})
	if _, err := v.Verify(key.Approve(rec, time.Now())); err != nil {
		t.Fatalf("enrolled key doesn't verify: %v", err)
	}

	// But enrolling alone trusts nothing.
	p := h.submit(t, "running late")
	if status, _ := h.approve(t, p.ID, key.Assert(h.challenge(t, p.ID, nil))); status != http.StatusForbidden {
		t.Fatalf("approve with freshly enrolled key = %d, want 403", status)
	}

	if status, _ := h.post(t, "/enroll/"+token, key.Register([]byte("again"))); status != http.StatusNotFound {
		t.Fatalf("second registration = %d, want 404", status)
	}
	if status, _ := h.enrollGet(t, "/enroll/"+token); status != http.StatusNotFound {
		t.Fatalf("page after use = %d, want 404", status)
	}

	b, err := os.ReadFile(filepath.Join(h.cfg.StateDir, "enrolled.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved []approval.Approver
	if err := json.Unmarshal(b, &saved); err != nil || len(saved) != 1 || saved[0] != got {
		t.Fatalf("enrolled.json = %s (%v)", b, err)
	}
	if loaded, err := loadApprovers(filepath.Join(h.cfg.StateDir, "enrolled.json")); err != nil || len(loaded) != 1 {
		t.Fatalf("enrolled.json isn't a valid approvers file: %v", err)
	}

	var pushed bool
	for _, m := range h.ntfy.all() {
		pushed = pushed || m.msg.Message == "new passkey enrolled: yubikey"
	}
	if !pushed {
		t.Fatalf("no enrollment notification in %+v", h.ntfy.all())
	}
	if got := h.kinds(t); slices.Index(got, evEnrolled) < 0 {
		t.Fatalf("audit = %v, want an enrolled event", got)
	}
}

func TestEnrollmentRefusals(t *testing.T) {
	t.Run("expired link", func(t *testing.T) {
		h := start(t)
		token := h.startEnroll(t, "phone")
		h.clock.advance(10*time.Minute + time.Second)
		if status, _ := h.enrollGet(t, "/enroll/"+token); status != http.StatusNotFound {
			t.Fatalf("page = %d, want 404", status)
		}
		if status, _ := h.enrollGet(t, "/enroll/"+token+"/options"); status != http.StatusNotFound {
			t.Fatalf("options = %d, want 404", status)
		}
		if status, _ := h.post(t, "/enroll/"+token, approvaltest.New(t, rpID, origin).Register([]byte("x"))); status != http.StatusNotFound {
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
			token := h.startEnroll(t, "phone")
			if status, body := h.register(t, token, tt.key(t)); status != http.StatusBadRequest {
				t.Fatalf("register = %d %s, want 400", status, body)
			}
			// A failed attempt spends the link too.
			if status, _ := h.enrollGet(t, "/enroll/"+token); status != http.StatusNotFound {
				t.Fatalf("page after failed attempt = %d, want 404", status)
			}
			if _, err := os.Stat(filepath.Join(h.cfg.StateDir, "enrolled.json")); err == nil {
				t.Fatal("failed registration was saved")
			}
		})
	}

	t.Run("challenge from another link", func(t *testing.T) {
		h := start(t)
		first, second := h.startEnroll(t, "a"), h.startEnroll(t, "b")
		c, _ := base64.RawURLEncoding.DecodeString(h.creationOptions(t, first).Challenge)
		if status, _ := h.post(t, "/enroll/"+second, approvaltest.New(t, rpID, origin).Register(c)); status != http.StatusBadRequest {
			t.Fatalf("register = %d, want 400", status)
		}
	})

	t.Run("bad name", func(t *testing.T) {
		h := start(t)
		resp, err := http.Post(h.admin.URL+"/enroll", "application/json", strings.NewReader(`{"name":"<b>x</b>"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("enroll = %d, want 400", resp.StatusCode)
		}
	})
}
