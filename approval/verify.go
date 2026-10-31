package approval

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
)

const (
	// MaxAge bounds the time between issuing a challenge and executing it.
	MaxAge = 5 * time.Minute
	// skew tolerates small clock differences between the issuer and verifier.
	skew = time.Minute
)

// Challenge binds an approval to exact record bytes, a single-use nonce, and
// the time it was issued. Fields are length-prefixed so no two inputs hash
// alike.
func Challenge(record, nonce []byte, issuedAt time.Time) []byte {
	h := sha256.New()
	for _, part := range [][]byte{
		[]byte("mandate-approval-v1"),
		record,
		nonce,
		[]byte(issuedAt.UTC().Format(time.RFC3339Nano)),
	} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return h.Sum(nil)
}

// Approval is a stored record plus the owner's passkey assertion over its
// challenge. Assertion is the PublicKeyCredential JSON that
// navigator.credentials.get returns.
type Approval struct {
	Record    []byte          `json:"record"`
	Nonce     []byte          `json:"nonce"`
	IssuedAt  time.Time       `json:"issued_at"`
	Assertion json.RawMessage `json:"assertion"`
}

// Approver is an enrolled passkey. Both fields are base64url without padding;
// PublicKey is the COSE key from registration.
type Approver struct {
	Name         string `json:"name"`
	CredentialID string `json:"credential_id"`
	PublicKey    string `json:"public_key"`
}

// LoadApprovers reads a JSON array of approvers.
func LoadApprovers(path string) ([]Approver, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var as []Approver
	if err := json.Unmarshal(b, &as); err != nil {
		return nil, fmt.Errorf("parse approvers %s: %w", path, err)
	}
	for _, a := range as {
		if _, err := base64.RawURLEncoding.DecodeString(a.CredentialID); err != nil {
			return nil, fmt.Errorf("approver %q: credential_id: %w", a.Name, err)
		}
		if _, err := base64.RawURLEncoding.DecodeString(a.PublicKey); err != nil {
			return nil, fmt.Errorf("approver %q: public_key: %w", a.Name, err)
		}
	}
	return as, nil
}

var (
	ErrStale     = errors.New("approval is outside its time window")
	ErrUnknown   = errors.New("approval is from an unknown passkey")
	ErrSignature = errors.New("approval signature is invalid")
	ErrExpired   = errors.New("proposal has expired")
)

// Verifier checks approvals against enrolled passkeys for one relying party.
type Verifier struct {
	RPID      string
	Origin    string
	Approvers []Approver
	Now       func() time.Time
}

// Verify checks the assertion and returns the approved record. It does not
// check nonce reuse; callers record nonces with Seen.
func (v *Verifier) Verify(a Approval) (Record, error) {
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	if a.IssuedAt.After(now.Add(skew)) || now.Sub(a.IssuedAt) > MaxAge {
		return Record{}, ErrStale
	}
	if len(a.Nonce) < 16 {
		return Record{}, errors.New("approval nonce is too short")
	}

	parsed, err := protocol.ParseCredentialRequestResponseBytes(a.Assertion)
	if err != nil {
		return Record{}, fmt.Errorf("parse assertion: %w", err)
	}
	approver, key, ok := v.find(parsed.RawID)
	if !ok {
		return Record{}, ErrUnknown
	}
	challenge := base64.RawURLEncoding.EncodeToString(Challenge(a.Record, a.Nonce, a.IssuedAt))
	err = parsed.Verify(challenge, v.RPID, "", []string{v.Origin}, nil, nil,
		protocol.TopOriginExplicitVerificationMode, false, true, true, key, protocol.SignaturePolicy{})
	if err != nil {
		return Record{}, fmt.Errorf("%w (%s): %w", ErrSignature, approver.Name, err)
	}

	rec, err := Decode(a.Record)
	if err != nil {
		return Record{}, err
	}
	if !now.Before(rec.ExpiresAt) {
		return Record{}, ErrExpired
	}
	return rec, nil
}

func (v *Verifier) find(rawID []byte) (Approver, []byte, bool) {
	for _, a := range v.Approvers {
		id, err := base64.RawURLEncoding.DecodeString(a.CredentialID)
		if err != nil || len(id) != len(rawID) || subtle.ConstantTimeCompare(id, rawID) != 1 {
			continue
		}
		key, err := base64.RawURLEncoding.DecodeString(a.PublicKey)
		if err != nil {
			return Approver{}, nil, false
		}
		return a, key, true
	}
	return Approver{}, nil, false
}
