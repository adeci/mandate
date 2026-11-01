package console

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/internal/proposal"
)

// wire is d as a page sends it.
func wire(d approval.Delegation) *delegation {
	return &delegation{PublicKey: b64(d.PublicKey), ExpiresAt: d.ExpiresAt, Covers: b64(d.Covers), Assertion: d.Assertion}
}

// approveIn approves id with s's session key signing challenge c.
func (h *harness) approveIn(t *testing.T, id, nonce string, s *approvaltest.Session, c []byte) int {
	t.Helper()
	status, _ := h.post(t, "/r/"+id+"/approve", approveRequest{Nonce: nonce, Delegation: wire(s.Delegation), Signature: b64(s.Sign(c))})
	return status
}

func TestUnlockWindowApprovesWithoutPasskey(t *testing.T) {
	h := start(t)
	first, second := h.submit(t, "one"), h.submit(t, "two")

	// One assertion approves the first request and opens the window.
	nonce, c, _ := h.challenge(t, first.ID, nil)
	s := h.key.Unlock(h.clock.now().Add(approval.UnlockWindow), c)
	if status := h.approveIn(t, first.ID, nonce, s, c); status != http.StatusOK {
		t.Fatalf("approve with passkey = %d", status)
	}

	// The second needs only the session key, once.
	nonce, c, _ = h.challenge(t, second.ID, nil)
	if status := h.approveIn(t, second.ID, nonce, s, c); status != http.StatusOK {
		t.Fatalf("approve with session key = %d", status)
	}
	if status := h.approveIn(t, second.ID, nonce, s, c); status != http.StatusConflict {
		t.Fatalf("replay = %d, want 409", status)
	}
	sent := h.fake.sent()
	if len(sent) != 2 || sent[0].Args["text"] != "one" || sent[1].Args["text"] != "two" {
		t.Fatalf("executed %+v", sent)
	}
	if got := h.get(t, second.ID); got.Status != proposal.Done {
		t.Fatalf("get = %+v", got)
	}

	// The stored proof verifies offline.
	var (
		pf     approval.Approval
		issued int64
		proof  string
		by     string
	)
	if err := h.srv.db.sql.QueryRow(`SELECT record, nonce, issued_at, assertion, decided_by FROM proposals WHERE id = ?`, second.ID).
		Scan(&pf.Record, &pf.Nonce, &issued, &proof, &by); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(proof), &pf); err != nil {
		t.Fatal(err)
	}
	pf.IssuedAt = time.Unix(0, issued).UTC()
	v := approval.Verifier{Keys: h.srv.keys, Now: func() time.Time { return pf.IssuedAt }}
	if rec, err := v.Verify(pf); err != nil || rec.ID != second.ID || by != "phone" {
		t.Fatalf("stored proof: %v, %q, by %q", err, rec.ID, by)
	}
}

func TestPasswordSessionCannotUnlock(t *testing.T) {
	h := start(t)
	p := h.submit(t, "one")
	h.session = h.passwordLogin(t)
	nonce, c, _ := h.challenge(t, p.ID, nil)
	s := h.key.Unlock(h.clock.now().Add(approval.UnlockWindow), c)
	if status := h.approveIn(t, p.ID, nonce, s, c); status != http.StatusForbidden {
		t.Fatalf("approve in a password session = %d, want 403", status)
	}
	if n := len(h.fake.sent()); n != 0 {
		t.Fatalf("executed %d times", n)
	}
	if status, _ := h.approve(t, p.ID, nonce, h.key.Assert(c)); status != http.StatusOK {
		t.Fatalf("approve with passkey = %d", status)
	}
}

func TestSigningInOpensWindow(t *testing.T) {
	h := start(t)
	s := h.key.Unlock(h.clock.now().Add(approval.UnlockWindow), h.loginChallenge(t))
	h.session = h.signIn(t, loginRequest{Assertion: s.Delegation.Assertion, Unlock: wire(s.Delegation)})
	p := h.submit(t, "one")
	nonce, c, _ := h.challenge(t, p.ID, nil)
	if status := h.approveIn(t, p.ID, nonce, s, c); status != http.StatusOK {
		t.Fatalf("approve with session key = %d", status)
	}

	long := h.key.Unlock(h.clock.now().Add(approval.UnlockWindow+2*time.Minute), h.loginChallenge(t))
	for name, d := range map[string]approval.Delegation{
		"login challenge reused": s.Delegation,
		"window too long":        long.Delegation,
	} {
		resp := h.do(t, http.MethodPost, "/login", "", loginRequest{Assertion: d.Assertion, Unlock: wire(d)})
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: login = %d, want 403", name, resp.StatusCode)
		}
	}
}
