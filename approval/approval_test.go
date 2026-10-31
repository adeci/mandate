package approval_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
)

const (
	rpID   = "lachesis.example.ts.net"
	origin = "https://lachesis.example.ts.net:8781"
)

var now = time.Date(2026, 10, 30, 22, 0, 0, 0, time.UTC)

func record(t *testing.T) []byte {
	t.Helper()
	b, err := approval.Encode(approval.Record{
		ID: "p1", Connector: "whatsapp", Op: "send",
		Args:      map[string]string{"to": "15551234567@s.whatsapp.net", "text": "running late"},
		Display:   []approval.Field{{Label: "To", Value: "Farid (+1 555 123 4567)"}, {Label: "Text", Value: "running late", Param: "text"}},
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(23 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func verifier(approvers ...approval.Approver) *approval.Verifier {
	return &approval.Verifier{RPID: rpID, Origin: origin, Approvers: approvers, Now: func() time.Time { return now }}
}

func TestVerifyAcceptsGenuineApproval(t *testing.T) {
	t.Parallel()
	key := approvaltest.New(t, rpID, origin)
	rec := record(t)
	got, err := verifier(key.Approver("phone")).Verify(key.Approve(rec, now.Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Args["text"] != "running late" || got.Connector != "whatsapp" {
		t.Fatalf("record = %+v", got)
	}
}

func TestVerifyRejects(t *testing.T) {
	t.Parallel()
	key := approvaltest.New(t, rpID, origin)
	other := approvaltest.New(t, rpID, origin)
	wrongOrigin := approvaltest.New(t, rpID, "https://evil.example:8781")
	wrongRP := approvaltest.New(t, "evil.example", origin)
	noUV := approvaltest.New(t, rpID, origin)
	noUV.Flags = approvaltest.FlagUP
	v := verifier(key.Approver("phone"), wrongOrigin.Approver("o"), wrongRP.Approver("r"), noUV.Approver("u"))
	rec := record(t)

	swapped := key.Approve(rec, now)
	tampered := append([]byte{}, rec...)
	tampered[len(tampered)-3] ^= 1 // flips a byte of the record after signing
	swapped.Record = tampered

	expired, _ := approval.Encode(approval.Record{ID: "p2", Connector: "whatsapp", Op: "send", CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)})

	for name, tc := range map[string]struct {
		a    approval.Approval
		want error
	}{
		"unenrolled passkey":           {other.Approve(rec, now), approval.ErrUnknown},
		"record changed after signing": {swapped, approval.ErrSignature},
		"wrong origin":                 {wrongOrigin.Approve(rec, now), approval.ErrSignature},
		"wrong relying party":          {wrongRP.Approve(rec, now), approval.ErrSignature},
		"no user verification":         {noUV.Approve(rec, now), approval.ErrSignature},
		"challenge too old":            {key.Approve(rec, now.Add(-approval.MaxAge-time.Second)), approval.ErrStale},
		"challenge from the future":    {key.Approve(rec, now.Add(2*time.Minute)), approval.ErrStale},
		"proposal expired":             {key.Approve(expired, now), approval.ErrExpired},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(tc.a); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestChallengeBindsEveryInput(t *testing.T) {
	t.Parallel()
	base := approval.Challenge([]byte("ab"), []byte("c"), now)
	for name, c := range map[string][]byte{
		"record":    approval.Challenge([]byte("abc"), []byte("c"), now),
		"nonce":     approval.Challenge([]byte("ab"), []byte("d"), now),
		"issued at": approval.Challenge([]byte("ab"), []byte("c"), now.Add(time.Nanosecond)),
		"boundary":  approval.Challenge([]byte("a"), []byte("bc"), now),
	} {
		if string(c) == string(base) {
			t.Errorf("changing %s did not change the challenge", name)
		}
	}
}

func TestSeenRejectsReplayAcrossRestarts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "seen.json")
	s, err := approval.OpenSeen(path)
	if err != nil {
		t.Fatal(err)
	}
	nonce := []byte("0123456789abcdef")
	if err := s.Use(nonce, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Use(nonce, time.Now()); !errors.Is(err, approval.ErrReplay) {
		t.Fatalf("second use: %v", err)
	}
	reopened, err := approval.OpenSeen(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Use(nonce, time.Now()); !errors.Is(err, approval.ErrReplay) {
		t.Fatalf("use after restart: %v", err)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	if _, err := approval.Decode([]byte(`{"v":1,"id":"p","connector":"c","op":"o","extra":1}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := approval.Decode([]byte(`{"v":2,"id":"p","connector":"c","op":"o"}`)); err == nil {
		t.Fatal("unknown version accepted")
	}
}
