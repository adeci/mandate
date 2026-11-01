package approval_test

import (
	"errors"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
)

func TestSessionKeyApproves(t *testing.T) {
	t.Parallel()
	key := approvaltest.New(t, rpID, origin)
	rec := record(t)
	s := key.Unlock(now.Add(approval.UnlockWindow), []byte("login challenge"))
	got, err := verifier(t, key).Verify(s.Approve(rec, now))
	if err != nil {
		t.Fatal(err)
	}
	if got.Args["text"] != "running late" {
		t.Fatalf("record = %+v", got)
	}
}

func TestSessionKeyRejects(t *testing.T) {
	t.Parallel()
	key := approvaltest.New(t, rpID, origin)
	other := approvaltest.New(t, rpID, origin)
	v := verifier(t, key)
	rec := record(t)
	until := now.Add(approval.UnlockWindow)
	s := key.Unlock(until, nil)

	otherRecord := s.Approve(rec, now)
	otherRecord.Record, _ = approval.Encode(approval.Record{ID: "p2", Connector: "whatsapp", Op: "send", ExpiresAt: now.Add(time.Hour)})

	// A passkey assertion over anything but an unlock challenge.
	asDelegation := func(assertion []byte) approval.Approval {
		a := s.Approve(rec, now)
		d := *a.Delegation
		d.Assertion = assertion
		a.Delegation = &d
		return a
	}
	both := s.Approve(rec, now)
	both.Assertion = key.Approve(rec, now).Assertion

	for name, tc := range map[string]struct {
		a    approval.Approval
		want error
	}{
		"window closed":                 {key.Unlock(now, nil).Approve(rec, now), approval.ErrLocked},
		"window too long":               {key.Unlock(now.Add(approval.UnlockWindow+2*time.Minute), nil).Approve(rec, now), nil},
		"unenrolled passkey":            {other.Unlock(until, nil).Approve(rec, now), approval.ErrUnknown},
		"signature over another record": {otherRecord, approval.ErrSignature},
		"approval assertion":            {asDelegation(key.Approve(rec, now).Assertion), approval.ErrSignature},
		"key change assertion":          {asDelegation(key.Assert(approval.KeyChallenge([]byte("change")))), approval.ErrSignature},
		"assertion and delegation":      {both, nil},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(tc.a)
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRemovingKeyEndsItsWindows(t *testing.T) {
	t.Parallel()
	first := approvaltest.New(t, rpID, origin)
	second := approvaltest.New(t, rpID, origin)
	k := approvaltest.Keys(t, first, second)
	v := &approval.Verifier{Keys: k, Now: func() time.Time { return now }}
	s := second.Unlock(now.Add(approval.UnlockWindow), nil)
	if _, err := v.Verify(s.Approve(record(t), now)); err != nil {
		t.Fatal(err)
	}
	mustApply(t, k, first.Sign(change(t, k, approval.OpRemove, second.Key("key2"))))
	if _, err := v.Verify(s.Approve(record(t), now)); !errors.Is(err, approval.ErrUnknown) {
		t.Fatalf("after removing the key: %v", err)
	}
}

func TestDelegationCannotChangeKeys(t *testing.T) {
	t.Parallel()
	key := approvaltest.New(t, rpID, origin)
	newKey := approvaltest.New(t, rpID, origin)
	k, _ := trusting(t, key)
	c := change(t, k, approval.OpAdd, newKey.Key("laptop"))
	s := key.Unlock(now.Add(approval.UnlockWindow), approval.KeyChallenge(c))
	sc := approval.SignedChange{Change: c, Signer: key.Key("").CredentialID, Assertion: s.Delegation.Assertion}
	if err := k.Apply(sc); !errors.Is(err, approval.ErrSignature) {
		t.Fatalf("err = %v", err)
	}
}
