package approval

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// UnlockWindow is the longest a delegation may run. Connectors refuse one
// that ends later than this from now.
const UnlockWindow = 10 * time.Minute

var ErrLocked = errors.New("unlock window has closed")

// Delegation is the owner's passkey letting a page's session key approve
// records until ExpiresAt. The same assertion approves Covers, the login or
// approval challenge it was made for, so opening a window needs no extra
// touch.
type Delegation struct {
	// PublicKey is the session's P-256 key as an uncompressed point.
	PublicKey []byte          `json:"public_key"`
	ExpiresAt time.Time       `json:"expires_at"`
	Covers    []byte          `json:"covers"`
	Assertion json.RawMessage `json:"assertion"`
}

// UnlockChallenge is what a passkey signs to open a window. It is
// domain-separated from Challenge and KeyChallenge. ExpiresAt counts in whole
// seconds, which is what pages can format the same way.
func UnlockChallenge(publicKey []byte, expiresAt time.Time, covers []byte) []byte {
	return hashParts([]byte("mandate-unlock-v1"), publicKey, []byte(expiresAt.UTC().Format(time.RFC3339)), covers)
}

// VerifyDelegation checks that a key trusted now signed d and that its window
// is open, and returns that key.
func (v *Verifier) VerifyDelegation(d Delegation) (Key, error) {
	k, _, err := v.delegation(d, v.now())
	return k, err
}

func (v *Verifier) delegation(d Delegation, now time.Time) (Key, *ecdsa.PublicKey, error) {
	expires := d.ExpiresAt.Truncate(time.Second)
	if !now.Before(expires) {
		return Key{}, nil, ErrLocked
	}
	if expires.After(now.Add(UnlockWindow + skew)) {
		return Key{}, nil, fmt.Errorf("unlock window is longer than %s", UnlockWindow)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), d.PublicKey)
	if err != nil {
		return Key{}, nil, fmt.Errorf("session key: %w", err)
	}
	k, err := v.passkey(d.Assertion, UnlockChallenge(d.PublicKey, expires, d.Covers))
	if err != nil {
		return Key{}, nil, err
	}
	return k, pub, nil
}

// sessionSigned reports whether sig is pub's signature over challenge in the
// form WebCrypto's ECDSA with SHA-256 produces: r and s, 32 bytes each.
func sessionSigned(pub *ecdsa.PublicKey, challenge, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	h := sha256.Sum256(challenge)
	return ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}
