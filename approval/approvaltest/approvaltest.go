// Package approvaltest is a software passkey that produces real WebAuthn
// registrations and assertions, so tests exercise the same verifiers
// production uses.
package approvaltest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/adeci/mandate/approval"
)

// Flags for authenticator data: user present, user verified, and backup
// eligible (a synced passkey).
const (
	FlagUP = 0x01
	FlagUV = 0x04
	FlagBE = 0x08
)

type Authenticator struct {
	RPID   string
	Origin string
	// Flags are set in every registration and assertion; tests clear FlagUV
	// to simulate an authenticator that skipped user verification, or set
	// FlagBE for a synced passkey.
	Flags byte

	key *ecdsa.PrivateKey
	id  []byte
}

func New(t testing.TB, rpID, origin string) *Authenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &Authenticator{RPID: rpID, Origin: origin, Flags: FlagUP | FlagUV, key: key, id: id}
}

// Key is this passkey as registration reports it.
func (a *Authenticator) Key(name string) approval.Key {
	return approval.Key{
		Name:         name,
		CredentialID: base64.RawURLEncoding.EncodeToString(a.id),
		PublicKey:    base64.RawURLEncoding.EncodeToString(a.coseKey()),
		Synced:       a.Flags&FlagBE != 0,
	}
}

// Sign approves key change bytes.
func (a *Authenticator) Sign(change []byte) approval.SignedChange {
	return approval.SignedChange{
		Change:    change,
		Signer:    base64.RawURLEncoding.EncodeToString(a.id),
		Assertion: a.Assert(approval.KeyChallenge(change)),
	}
}

// Keys returns a key log in a temporary directory that trusts keys, named
// key1, key2, and so on: the first added unsigned, the rest signed by it.
func Keys(t testing.TB, keys ...*Authenticator) *approval.Keys {
	t.Helper()
	k, err := approval.OpenKeys(filepath.Join(t.TempDir(), "keys.json"), keys[0].RPID, keys[0].Origin)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range keys {
		change, err := k.Next(approval.OpAdd, a.Key("key"+strconv.Itoa(i+1)), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		sc := approval.SignedChange{Change: change}
		if i > 0 {
			sc = keys[0].Sign(change)
		}
		if err := k.Apply(sc); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

// coseKey is the public key in canonical CTAP2 CBOR, so it encodes the same
// way every time.
func (a *Authenticator) coseKey() []byte {
	enc, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	pub, err := enc.Marshal(map[int]any{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: a.key.X.FillBytes(make([]byte, 32)),
		-3: a.key.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		panic(err)
	}
	return pub
}

// Register answers navigator.credentials.create for challenge with
// attestation "none" and returns the PublicKeyCredential JSON.
func (a *Authenticator) Register(challenge []byte) json.RawMessage {
	clientData, err := json.Marshal(map[string]any{
		"type":        "webauthn.create",
		"challenge":   base64.RawURLEncoding.EncodeToString(challenge),
		"origin":      a.Origin,
		"crossOrigin": false,
	})
	if err != nil {
		panic(err)
	}
	const flagAT = 0x40 // attested credential data included
	rpHash := sha256.Sum256([]byte(a.RPID))
	authData := append(rpHash[:], a.Flags|flagAT, 0, 0, 0, 0)
	authData = append(authData, make([]byte, 16)...) // AAGUID
	authData = append(authData, byte(len(a.id)>>8), byte(len(a.id)))
	authData = append(authData, a.id...)
	authData = append(authData, a.coseKey()...)
	enc, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	att, err := enc.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	})
	if err != nil {
		panic(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	out, err := json.Marshal(map[string]any{
		"id":    b64(a.id),
		"rawId": b64(a.id),
		"type":  "public-key",
		"response": map[string]string{
			"clientDataJSON":    b64(clientData),
			"attestationObject": b64(att),
		},
	})
	if err != nil {
		panic(err)
	}
	return out
}

// Assert signs challenge the way navigator.credentials.get does and returns
// the PublicKeyCredential JSON.
func (a *Authenticator) Assert(challenge []byte) json.RawMessage {
	clientData, err := json.Marshal(map[string]any{
		"type":        "webauthn.get",
		"challenge":   base64.RawURLEncoding.EncodeToString(challenge),
		"origin":      a.Origin,
		"crossOrigin": false,
	})
	if err != nil {
		panic(err)
	}
	rpHash := sha256.Sum256([]byte(a.RPID))
	authData := append(rpHash[:], a.Flags, 0, 0, 0, 1)
	clientHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		panic(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	out, err := json.Marshal(map[string]any{
		"id":    b64(a.id),
		"rawId": b64(a.id),
		"type":  "public-key",
		"response": map[string]string{
			"authenticatorData": b64(authData),
			"clientDataJSON":    b64(clientData),
			"signature":         b64(sig),
		},
	})
	if err != nil {
		panic(err)
	}
	return out
}

// Approve builds a complete approval for record bytes issued at issuedAt.
func (a *Authenticator) Approve(record []byte, issuedAt time.Time) approval.Approval {
	nonce := newNonce()
	return approval.Approval{
		Record:    record,
		Nonce:     nonce,
		IssuedAt:  issuedAt,
		Assertion: a.Assert(approval.Challenge(record, nonce, issuedAt)),
	}
}

func newNonce() []byte {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	return nonce
}

// Session is a page's session key in a window a passkey opened.
type Session struct {
	Delegation approval.Delegation

	key *ecdsa.PrivateKey
}

// Unlock opens a window until expiresAt with a new session key, the
// assertion also approving covers, as signing in or approving does.
func (a *Authenticator) Unlock(expiresAt time.Time, covers []byte) *Session {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	return &Session{key: key, Delegation: approval.Delegation{
		PublicKey: pub,
		ExpiresAt: expiresAt,
		Covers:    covers,
		Assertion: a.Assert(approval.UnlockChallenge(pub, expiresAt, covers)),
	}}
}

// Sign signs challenge the way WebCrypto's ECDSA with SHA-256 does.
func (s *Session) Sign(challenge []byte) []byte {
	h := sha256.Sum256(challenge)
	r, sig, err := ecdsa.Sign(rand.Reader, s.key, h[:])
	if err != nil {
		panic(err)
	}
	return append(r.FillBytes(make([]byte, 32)), sig.FillBytes(make([]byte, 32))...)
}

// Approve builds an approval for record bytes with the session key alone.
func (s *Session) Approve(record []byte, issuedAt time.Time) approval.Approval {
	nonce := newNonce()
	d := s.Delegation
	return approval.Approval{
		Record:     record,
		Nonce:      nonce,
		IssuedAt:   issuedAt,
		Delegation: &d,
		Signature:  s.Sign(approval.Challenge(record, nonce, issuedAt)),
	}
}
