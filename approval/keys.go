package approval

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// Key is a trusted passkey. CredentialID and PublicKey (COSE) are base64url
// without padding.
type Key struct {
	Name         string `json:"name"`
	CredentialID string `json:"credential_id"`
	PublicKey    string `json:"public_key"`
	// Synced is the WebAuthn backup-eligible flag: a phone or password
	// manager passkey rather than a hardware key.
	Synced bool `json:"synced,omitempty"`
}

const (
	OpAdd    = "add"
	OpRemove = "remove"
)

// KeyChange is one entry of the key log. Like a Record it is stored as exact
// bytes, which are what a trusted key signs and what Prev hashes.
type KeyChange struct {
	Seq  int    `json:"seq"`
	Op   string `json:"op"`
	Key  Key    `json:"key"`
	Prev string `json:"prev"`
	// At is when the change was approved, for display only.
	At time.Time `json:"at"`
}

// SignedChange is a log entry: the change bytes and, after seq 0, the
// assertion of the trusted key that approved them.
type SignedChange struct {
	Change    []byte          `json:"change"`
	Signer    string          `json:"signer,omitempty"`
	Assertion json.RawMessage `json:"assertion,omitempty"`
}

var (
	ErrOutOfOrder = errors.New("key change is out of order")
	ErrUnsigned   = errors.New("key change is unsigned")
	ErrLastKey    = errors.New("can't remove the last trusted key")
	ErrDiverged   = errors.New("key log differs from the one held here")
	ErrNoKeys     = errors.New("no trusted passkeys; writes are refused")
)

func EncodeChange(c KeyChange) ([]byte, error) {
	c.At = c.At.UTC()
	return json.Marshal(c)
}

// DecodeChange accepts only the bytes EncodeChange would produce.
func DecodeChange(b []byte) (KeyChange, error) {
	var c KeyChange
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return KeyChange{}, fmt.Errorf("decode key change: %w", err)
	}
	if again, err := EncodeChange(c); err != nil || !bytes.Equal(again, b) {
		return KeyChange{}, errors.New("decode key change: not in canonical form")
	}
	return c, nil
}

// KeyChallenge is what a trusted key signs to approve change bytes. It is
// domain-separated from Challenge.
func KeyChallenge(change []byte) []byte {
	return hashParts([]byte("mandate-key-change-v1"), change)
}

func changeHash(change []byte) string {
	h := sha256.Sum256(change)
	return hex.EncodeToString(h[:])
}

// Keys is a key log and the trusted set it replays to, persisted at a path.
// Every entry after the first is verified as a WebAuthn assertion for RPID
// and Origin, on apply and again on load.
type Keys struct {
	rpID, origin, path string

	mu sync.RWMutex
	st keyState
}

type keyState struct {
	log     []SignedChange
	trusted []Key
}

// OpenKeys loads the log at path; a missing file is an empty log.
func OpenKeys(path, rpID, origin string) (*Keys, error) {
	k := &Keys{rpID: rpID, origin: origin, path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return k, nil
	}
	if err != nil {
		return nil, err
	}
	var log []SignedChange
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&log); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for i, sc := range log {
		if err := k.st.apply(sc, rpID, origin); err != nil {
			return nil, fmt.Errorf("%s: entry %d: %w", path, i, err)
		}
	}
	return k, nil
}

func (k *Keys) RPID() string   { return k.rpID }
func (k *Keys) Origin() string { return k.origin }

// Apply verifies sc against the current log and appends it.
func (k *Keys) Apply(sc SignedChange) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	next := k.st.clone()
	if err := next.apply(sc, k.rpID, k.origin); err != nil {
		return err
	}
	return k.commit(next)
}

// Check verifies sc as Apply would, without appending it.
func (k *Keys) Check(sc SignedChange) error {
	k.mu.RLock()
	defer k.mu.RUnlock()
	next := k.st.clone()
	return next.apply(sc, k.rpID, k.origin)
}

// Extend replaces the log with log if it begins with every entry held here
// and the rest apply. An empty log thus trusts whatever first key it is
// given.
func (k *Keys) Extend(log []SignedChange) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	held := k.st.log
	if len(log) < len(held) {
		return ErrDiverged
	}
	for i, sc := range held {
		if !bytes.Equal(sc.Change, log[i].Change) || sc.Signer != log[i].Signer {
			return ErrDiverged
		}
	}
	if len(log) == len(held) {
		return nil
	}
	next := k.st.clone()
	for i, sc := range log[len(held):] {
		if err := next.apply(sc, k.rpID, k.origin); err != nil {
			return fmt.Errorf("entry %d: %w", len(held)+i, err)
		}
	}
	return k.commit(next)
}

func (k *Keys) commit(next keyState) error {
	b, err := json.Marshal(next.log)
	if err != nil {
		return err
	}
	if err := writeAtomic(k.path, b); err != nil {
		return err
	}
	k.st = next
	return nil
}

// Next encodes the change that would follow the log now, after checking
// the op is allowed. Who may approve it is checked when it is applied.
func (k *Keys) Next(op string, key Key, at time.Time) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	c := KeyChange{Seq: len(k.st.log), Op: op, Key: key, Prev: k.st.head(), At: at}
	if err := k.st.check(c); err != nil {
		return nil, err
	}
	return EncodeChange(c)
}

func (k *Keys) Log() []SignedChange {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return slices.Clone(k.st.log)
}

// Head is the log's length and the hash of its last change, "" when empty.
func (k *Keys) Head() (int, string) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.st.log), k.st.head()
}

func (k *Keys) Trusted() []Key {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return slices.Clone(k.st.trusted)
}

func (k *Keys) empty() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.st.trusted) == 0
}

func (k *Keys) Find(credentialID string) (Key, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.st.find(credentialID)
}

func (st keyState) clone() keyState {
	return keyState{log: slices.Clone(st.log), trusted: slices.Clone(st.trusted)}
}

func (st *keyState) head() string {
	if len(st.log) == 0 {
		return ""
	}
	return changeHash(st.log[len(st.log)-1].Change)
}

func (st *keyState) find(credentialID string) (Key, bool) {
	i := slices.IndexFunc(st.trusted, func(k Key) bool { return k.CredentialID == credentialID })
	if i < 0 {
		return Key{}, false
	}
	return st.trusted[i], true
}

func (st *keyState) apply(sc SignedChange, rpID, origin string) error {
	c, err := DecodeChange(sc.Change)
	if err != nil {
		return err
	}
	if c.Seq != len(st.log) || c.Prev != st.head() {
		return ErrOutOfOrder
	}
	if c.Seq == 0 {
		if c.Op != OpAdd || sc.Signer != "" || len(sc.Assertion) != 0 {
			return errors.New("the first key change must be an unsigned add")
		}
	} else {
		if sc.Signer == "" || len(sc.Assertion) == 0 {
			return ErrUnsigned
		}
		signer, ok := st.find(sc.Signer)
		if !ok {
			return fmt.Errorf("signer: %w", ErrUnknown)
		}
		parsed, err := protocol.ParseCredentialRequestResponseBytes(sc.Assertion)
		if err != nil {
			return fmt.Errorf("parse assertion: %w", err)
		}
		if base64.RawURLEncoding.EncodeToString(parsed.RawID) != signer.CredentialID {
			return errors.New("assertion is from another key than its signer")
		}
		if err := verifyAssertion(parsed, KeyChallenge(sc.Change), rpID, origin, signer); err != nil {
			return err
		}
	}
	if err := st.check(c); err != nil {
		return err
	}
	st.log = append(st.log, SignedChange{Change: slices.Clone(sc.Change), Signer: sc.Signer, Assertion: slices.Clone(sc.Assertion)})
	if c.Op == OpAdd {
		st.trusted = append(st.trusted, c.Key)
	} else {
		st.trusted = slices.DeleteFunc(st.trusted, func(k Key) bool { return k.CredentialID == c.Key.CredentialID })
	}
	return nil
}

// check applies the op's rules to the trusted set; signatures are apply's.
func (st *keyState) check(c KeyChange) error {
	switch c.Op {
	case OpAdd:
		if err := validKey(c.Key); err != nil {
			return err
		}
		if _, ok := st.find(c.Key.CredentialID); ok {
			return fmt.Errorf("key %q is already trusted", c.Key.Name)
		}
	case OpRemove:
		if k, ok := st.find(c.Key.CredentialID); !ok || k != c.Key {
			return fmt.Errorf("key %q is not trusted", c.Key.Name)
		}
		if len(st.trusted) == 1 {
			return ErrLastKey
		}
	default:
		return fmt.Errorf("unknown key change op %q", c.Op)
	}
	return nil
}

func validKey(k Key) error {
	if k.Name == "" {
		return errors.New("key has no name")
	}
	if id, err := base64.RawURLEncoding.DecodeString(k.CredentialID); err != nil || len(id) == 0 {
		return fmt.Errorf("key %q: invalid credential_id", k.Name)
	}
	pub, err := base64.RawURLEncoding.DecodeString(k.PublicKey)
	if err == nil {
		_, err = webauthncose.ParsePublicKey(pub)
	}
	if err != nil {
		return fmt.Errorf("key %q: invalid public_key: %w", k.Name, err)
	}
	return nil
}

// verifyAssertion checks a passkey's signature over challenge with the
// relying party, origin, and user verification required.
func verifyAssertion(parsed *protocol.ParsedCredentialAssertionData, challenge []byte, rpID, origin string, key Key) error {
	pub, err := base64.RawURLEncoding.DecodeString(key.PublicKey)
	if err != nil {
		return fmt.Errorf("key %q: %w", key.Name, err)
	}
	err = parsed.Verify(base64.RawURLEncoding.EncodeToString(challenge), rpID, "", []string{origin}, nil, nil,
		protocol.TopOriginExplicitVerificationMode, false, true, true, pub, protocol.SignaturePolicy{})
	if err != nil {
		return fmt.Errorf("%w (%s): %w", ErrSignature, key.Name, err)
	}
	return nil
}
