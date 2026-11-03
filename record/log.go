package record

import (
	"bytes"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Log is a verified key log and the keys it trusts. The zero Log is empty.
type Log struct {
	entries []Envelope
	trusted []trustedKey
}

type trustedKey struct {
	Key
	pub *ecdsa.PublicKey
}

// NewLog verifies entries as a whole, from seq 0.
func NewLog(entries []Envelope) (*Log, error) {
	l := new(Log)
	for i, e := range entries {
		if err := l.Append(e); err != nil {
			return nil, fmt.Errorf("key log entry %d: %w", i, err)
		}
	}
	return l, nil
}

// Extend replaces the log with entries if they verify and begin with the
// entries held. An empty log takes any log that verifies.
func (l *Log) Extend(entries []Envelope) error {
	next, err := NewLog(entries)
	if err != nil {
		return err
	}
	// Each prev hashes the entry before, so a matching head is a matching
	// prefix.
	n, head := l.Head()
	if len(entries) < n || n > 0 && hashHex(entries[n-1].Record) != head {
		return errors.New("key log does not extend the one held")
	}
	*l = *next
	return nil
}

// Append verifies e as the log's next entry and appends it.
func (l *Log) Append(e Envelope) error {
	if e.Kind != "key" {
		return fmt.Errorf("%s record in the key log", e.Kind)
	}
	r, err := l.Verify(e, time.Time{})
	if err != nil {
		return err
	}
	ke := r.(KeyEntry)
	if ke.Op == "add" {
		pub, _ := ke.Key.parse() // Verify parsed it
		l.trusted = append(l.trusted, trustedKey{ke.Key, pub})
	} else {
		l.trusted = slices.DeleteFunc(l.trusted, func(t trustedKey) bool { return t.ID == ke.Key.ID })
	}
	l.entries = append(l.entries, e)
	return nil
}

// Head is the log's length and the SHA-256 of its last entry's record
// bytes, "" when empty: what the next entry's prev names.
func (l *Log) Head() (int, string) {
	if len(l.entries) == 0 {
		return 0, ""
	}
	return len(l.entries), hashHex(l.entries[len(l.entries)-1].Record)
}

func (l *Log) Entries() []Envelope { return slices.Clone(l.entries) }

// Trusted returns the keys trusted now, in the order they were added.
func (l *Log) Trusted() []Key {
	keys := make([]Key, len(l.trusted))
	for i, t := range l.trusted {
		keys[i] = t.Key
	}
	return keys
}

// Verify checks e with the keys trusted now and returns its record. A key
// entry must be the log's next entry; Verify does not append it. A write must
// also be fresh at now. Spending the nonce and a write's ID is the caller's.
func (l *Log) Verify(e Envelope, now time.Time) (Record, error) {
	r, err := decode(e.Kind, e.Record)
	if err != nil {
		return nil, err
	}
	k, pub, err := l.signer(e.Key, r)
	if err != nil {
		return nil, err
	}
	if len(e.Nonce) < 16 {
		return nil, errors.New("nonce is shorter than 16 bytes")
	}
	if err := verifySig(k, pub, e); err != nil {
		return nil, err
	}
	if err := r.check(); err != nil {
		return nil, fmt.Errorf("%s record: %w", e.Kind, err)
	}
	switch r := r.(type) {
	case KeyEntry:
		err = l.follows(r)
	case Write:
		err = fresh(r, e.IssuedAt.Time, now)
	}
	if err != nil {
		return nil, fmt.Errorf("%s record: %w", e.Kind, err)
	}
	return r, nil
}

func (l *Log) signer(id string, r Record) (Key, *ecdsa.PublicKey, error) {
	if len(l.entries) == 0 {
		// Seq 0 is signed by the key it adds.
		if e, ok := r.(KeyEntry); ok && e.Op == "add" && e.Key.ID == id {
			pub, err := e.Key.parse()
			return e.Key, pub, err
		}
		return Key{}, nil, ErrUntrusted
	}
	for _, t := range l.trusted {
		if t.ID == id {
			return t.Key, t.pub, nil
		}
	}
	return Key{}, nil, ErrUntrusted
}

func (l *Log) follows(e KeyEntry) error {
	n, head := l.Head()
	if e.Seq != n {
		return fmt.Errorf("seq %d where %d is next", e.Seq, n)
	}
	if e.Prev != head {
		return errors.New("prev does not name the previous entry")
	}
	i := slices.IndexFunc(l.trusted, func(t trustedKey) bool { return t.ID == e.Key.ID })
	switch {
	case e.Op == "add" && i >= 0:
		return fmt.Errorf("key %s is already trusted", e.Key.ID)
	case e.Op == "remove" && (i < 0 || !sameKey(l.trusted[i].Key, e.Key)):
		return fmt.Errorf("key %s is not trusted as named", e.Key.ID)
	case e.Op == "remove" && len(l.trusted) == 1:
		return errors.New("removing the last key")
	}
	return nil
}

func sameKey(a, b Key) bool {
	return a.Type == b.Type && a.ID == b.ID && a.Name == b.Name &&
		bytes.Equal(a.PublicKey, b.PublicKey) && bytes.Equal(a.CredentialID, b.CredentialID)
}

func fresh(w Write, issued, now time.Time) error {
	switch age := now.Sub(issued); {
	case age > 5*time.Minute:
		return fmt.Errorf("issued at %s: %w", issued.Format(timeLayout), ErrExpired)
	case age < -time.Minute:
		return fmt.Errorf("issued at %s, in the future", issued.Format(timeLayout))
	case !now.Before(w.ExpiresAt.Time):
		return fmt.Errorf("%w at %s", ErrExpired, w.ExpiresAt.format())
	}
	return nil
}
