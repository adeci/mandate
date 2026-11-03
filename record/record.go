// Package record encodes, signs and verifies the records the owner's devices
// sign, as docs/spec/records.md describes.
package record

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Record is a KeyEntry, Identity, Grant or Write.
type Record interface {
	Kind() string
	// check holds the kind's rules that need no key log or clock.
	check() error
}

// Key is a device key.
type Key struct {
	Type         string `json:"type"` // "p256" or "fido2"
	ID           string `json:"id"`
	Name         string `json:"name"`
	PublicKey    Bytes  `json:"public_key"`
	CredentialID Bytes  `json:"credential_id,omitempty"`
}

// KeyEntry is an entry of the key log.
type KeyEntry struct {
	Seq  int    `json:"seq"`
	Op   string `json:"op"` // "add" or "remove"
	Key  Key    `json:"key"`
	Prev string `json:"prev"`
	At   Time   `json:"at"`
}

type Identity struct {
	ID          string `json:"id"`
	TokenSHA256 string `json:"token_sha256"`
	CreatedAt   Time   `json:"created_at"`
}

type Grant struct {
	ID        string    `json:"id"`
	Identity  string    `json:"identity"`
	Connector string    `json:"connector"`
	Chats     []string  `json:"chats,omitempty"`
	Horizon   int       `json:"horizon"`
	From      Time      `json:"from,omitzero"`
	Until     Time      `json:"until,omitzero"`
	Schedule  *Schedule `json:"schedule,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt Time      `json:"created_at"`
}

type Schedule struct {
	Days   []int  `json:"days,omitempty"`
	Start  string `json:"start"`
	Length int    `json:"length"`
	Zone   string `json:"zone"`
}

type Write struct {
	ID        string            `json:"id"`
	Connector string            `json:"connector"`
	Op        string            `json:"op"`
	Args      map[string]string `json:"args"`
	Display   []Field           `json:"display"`
	Note      string            `json:"note,omitempty"`
	CreatedAt Time              `json:"created_at"`
	ExpiresAt Time              `json:"expires_at"`
}

// Field is a line of a write's display.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Param string `json:"param,omitempty"`
}

func (KeyEntry) Kind() string { return "key" }
func (Identity) Kind() string { return "identity" }
func (Grant) Kind() string    { return "grant" }
func (Write) Kind() string    { return "write" }

// Bytes is base64url without padding in JSON.
type Bytes []byte

func (b Bytes) MarshalText() ([]byte, error) {
	return base64.RawURLEncoding.AppendEncode(nil, b), nil
}

func (b *Bytes) UnmarshalText(text []byte) error {
	// The decoder skips line breaks even in strict mode.
	if bytes.ContainsAny(text, "\r\n") {
		return errors.New("line break in base64url")
	}
	out, err := base64.RawURLEncoding.Strict().AppendDecode(nil, text)
	if err != nil {
		return fmt.Errorf("decoding base64url: %w", err)
	}
	*b = out
	return nil
}

// Time is a time in RFC 3339, UTC, whole seconds.
type Time struct{ time.Time }

const timeLayout = "2006-01-02T15:04:05Z"

func (t Time) format() string { return t.UTC().Format(timeLayout) }

func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.format())
}

func (t *Time) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("decoding time: %w", err)
	}
	p, err := time.Parse(timeLayout, s)
	// Parse accepts fractional seconds the layout lacks.
	if err != nil || p.Format(timeLayout) != s {
		return fmt.Errorf("time %q is not RFC 3339 UTC in whole seconds", s)
	}
	t.Time = p
	return nil
}

// Encode returns r's record bytes.
func Encode(r Record) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, fmt.Errorf("%s record: %w", r.Kind(), err)
	}
	b, err := canonical(r)
	if err != nil {
		return nil, fmt.Errorf("encoding %s record: %w", r.Kind(), err)
	}
	return b, nil
}

var errNotCanonical = errors.New("record bytes are not canonical or lack a field")

func decode(kind string, b []byte) (Record, error) {
	switch kind {
	case "key":
		return decodeAs[KeyEntry](b)
	case "identity":
		return decodeAs[Identity](b)
	case "grant":
		return decodeAs[Grant](b)
	case "write":
		return decodeAs[Write](b)
	}
	return nil, fmt.Errorf("unknown kind %q", kind)
}

// decodeAs refuses unknown fields; re-encoding refuses the rest, since a
// missing required field comes back as its zero value and an empty optional
// one not at all.
func decodeAs[R Record](b []byte) (Record, error) {
	var r R
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return nil, fmt.Errorf("decoding %s record: %w", r.Kind(), err)
	}
	c, err := canonical(r)
	if err != nil || !bytes.Equal(c, b) {
		return nil, errNotCanonical
	}
	return r, nil
}

// NewID returns a fresh record ID; rand.Text is exactly that format.
func NewID() string { return rand.Text() }

func isID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// isHash takes only lowercase hex, which hex.DecodeString does not enforce.
func isHash(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (k Key) parse() (*ecdsa.PublicKey, error) {
	switch k.Type {
	case "p256":
		if len(k.CredentialID) > 0 {
			return nil, fmt.Errorf("p256 key %s has a credential_id", k.ID)
		}
	case "fido2":
		if len(k.CredentialID) == 0 {
			return nil, fmt.Errorf("fido2 key %s lacks a credential_id", k.ID)
		}
	default:
		return nil, fmt.Errorf("key %s has unknown type %q", k.ID, k.Type)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), k.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("key %s: %w", k.ID, err)
	}
	if hashHex(k.PublicKey) != k.ID {
		return nil, fmt.Errorf("key id %s is not the SHA-256 of its public_key", k.ID)
	}
	return pub, nil
}

func (e KeyEntry) check() error {
	if e.Op != "add" && e.Op != "remove" {
		return fmt.Errorf("unknown op %q", e.Op)
	}
	_, err := e.Key.parse()
	return err
}

func (i Identity) check() error {
	if !isID(i.ID) {
		return fmt.Errorf("id %q is not a record ID", i.ID)
	}
	if !isHash(i.TokenSHA256) {
		return errors.New("token_sha256 is not a SHA-256 in lowercase hex")
	}
	return nil
}

func (g Grant) check() error {
	if !isID(g.ID) {
		return fmt.Errorf("id %q is not a record ID", g.ID)
	}
	if !isID(g.Identity) {
		return fmt.Errorf("identity %q is not a record ID", g.Identity)
	}
	if g.Connector == "" {
		return errors.New("no connector")
	}
	if slices.Contains(g.Chats, "") {
		return errors.New("empty chat ID")
	}
	if g.Horizon < 0 {
		return errors.New("negative horizon")
	}
	if g.Schedule != nil {
		if !g.From.IsZero() || !g.Until.IsZero() {
			return errors.New("both a window and a schedule")
		}
		return g.Schedule.check()
	}
	if g.From.IsZero() || !g.From.Before(g.Until.Time) {
		return errors.New("neither from before until nor a schedule")
	}
	return nil
}

func (s Schedule) check() error {
	for i, d := range s.Days {
		if d < 0 || d > 6 || i > 0 && d <= s.Days[i-1] {
			return fmt.Errorf("schedule days %v are not ascending from 0 to 6", s.Days)
		}
	}
	if t, err := time.Parse("15:04", s.Start); err != nil || t.Format("15:04") != s.Start {
		return fmt.Errorf("schedule start %q is not HH:MM", s.Start)
	}
	if s.Length < 1 || s.Length > 86400 {
		return fmt.Errorf("schedule length %d is not 1 to 86400", s.Length)
	}
	// LoadLocation takes "" and "Local" as UTC and the machine's zone.
	if s.Zone == "" || s.Zone == "Local" {
		return fmt.Errorf("schedule zone %q is not an IANA zone", s.Zone)
	}
	if _, err := time.LoadLocation(s.Zone); err != nil {
		return fmt.Errorf("schedule zone: %w", err)
	}
	return nil
}

func (w Write) check() error {
	if !isID(w.ID) {
		return fmt.Errorf("id %q is not a record ID", w.ID)
	}
	if w.Connector == "" || w.Op == "" {
		return errors.New("connector and op are required")
	}
	return nil
}
