package approval

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
// the time it was issued.
func Challenge(record, nonce []byte, issuedAt time.Time) []byte {
	return hashParts([]byte("mandate-approval-v1"), record, nonce, []byte(issuedAt.UTC().Format(time.RFC3339Nano)))
}

// hashParts length-prefixes each part so no two inputs hash alike.
func hashParts(parts ...[]byte) []byte {
	h := sha256.New()
	for _, part := range parts {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return h.Sum(nil)
}

// Approval is a stored record plus the owner's say-so over its challenge:
// either a passkey Assertion, the PublicKeyCredential JSON that
// navigator.credentials.get returns, or a Delegation and the session key's
// Signature.
type Approval struct {
	Record     []byte          `json:"record"`
	Nonce      []byte          `json:"nonce"`
	IssuedAt   time.Time       `json:"issued_at"`
	Assertion  json.RawMessage `json:"assertion,omitempty"`
	Delegation *Delegation     `json:"delegation,omitempty"`
	Signature  []byte          `json:"signature,omitempty"`
}

var (
	ErrStale     = errors.New("approval is outside its time window")
	ErrUnknown   = errors.New("approval is from an unknown passkey")
	ErrSignature = errors.New("approval signature is invalid")
	ErrExpired   = errors.New("proposal has expired")
)

// Verifier checks approvals against the keys Keys trusts at the moment of
// each call.
type Verifier struct {
	Keys *Keys
	Now  func() time.Time
}

// Verify checks the approval and returns the approved record. It does not
// check nonce reuse; callers record nonces with Seen.
func (v *Verifier) Verify(a Approval) (Record, error) {
	now := v.now()
	if a.IssuedAt.After(now.Add(skew)) || now.Sub(a.IssuedAt) > MaxAge {
		return Record{}, ErrStale
	}
	if len(a.Nonce) < 16 {
		return Record{}, errors.New("approval nonce is too short")
	}

	challenge := Challenge(a.Record, a.Nonce, a.IssuedAt)
	switch {
	case a.Delegation == nil:
		if _, err := v.passkey(a.Assertion, challenge); err != nil {
			return Record{}, err
		}
	case len(a.Assertion) != 0:
		return Record{}, errors.New("approval has both an assertion and a delegation")
	default:
		_, pub, err := v.delegation(*a.Delegation, now)
		if err != nil {
			return Record{}, err
		}
		if !sessionSigned(pub, challenge, a.Signature) {
			return Record{}, fmt.Errorf("%w: session key", ErrSignature)
		}
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

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// passkey checks that a key trusted now signed challenge.
func (v *Verifier) passkey(assertion json.RawMessage, challenge []byte) (Key, error) {
	if v.Keys == nil || v.Keys.empty() {
		return Key{}, ErrNoKeys
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertion)
	if err != nil {
		return Key{}, fmt.Errorf("parse assertion: %w", err)
	}
	key, ok := v.Keys.Find(base64.RawURLEncoding.EncodeToString(parsed.RawID))
	if !ok {
		return Key{}, ErrUnknown
	}
	return key, verifyAssertion(parsed, challenge, v.Keys.rpID, v.Keys.origin, key)
}
