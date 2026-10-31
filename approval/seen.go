package approval

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrReplay = errors.New("approval was already used")

// Seen records used nonces on disk so an approval runs at most once, across
// restarts too. A nonce is recorded before execution starts.
type Seen struct {
	path string
	now  func() time.Time

	mu   sync.Mutex
	used map[string]time.Time
}

// OpenSeen loads the nonce log at path, creating it on first use.
func OpenSeen(path string) (*Seen, error) {
	s := &Seen{path: path, now: time.Now, used: map[string]time.Time{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.used); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// Use marks nonce as spent until the approval it belongs to could no longer
// verify anyway. It fails if the nonce was already spent.
func (s *Seen) Use(nonce []byte, issuedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for k, until := range s.used {
		if now.After(until) {
			delete(s.used, k)
		}
	}
	key := hex.EncodeToString(nonce)
	if _, ok := s.used[key]; ok {
		return ErrReplay
	}
	s.used[key] = issuedAt.Add(MaxAge + skew)
	if err := s.save(); err != nil {
		delete(s.used, key)
		return err
	}
	return nil
}

func (s *Seen) save() error {
	b, err := json.Marshal(s.used)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".seen-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
