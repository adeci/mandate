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

// Seen records used nonces and request IDs on disk so an approval, and the
// request it approves, runs at most once, across restarts too. Both are
// recorded before execution starts.
type Seen struct {
	path string
	now  func() time.Time

	mu sync.Mutex
	// used holds hex nonces and "id:"-prefixed request IDs, each until it
	// could no longer verify anyway.
	used map[string]time.Time
}

// OpenSeen loads the log at path, creating it on first use.
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

// Use spends a's nonce and the ID of rec, the record a approves. It fails
// if either was spent already, so a request approved twice runs once.
func (s *Seen) Use(a Approval, rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for k, until := range s.used {
		if now.After(until) {
			delete(s.used, k)
		}
	}
	nonce, id := hex.EncodeToString(a.Nonce), "id:"+rec.ID
	if _, ok := s.used[nonce]; ok {
		return ErrReplay
	}
	if _, ok := s.used[id]; ok {
		return fmt.Errorf("%w: request %s already ran", ErrReplay, rec.ID)
	}
	s.used[nonce] = a.IssuedAt.Add(MaxAge + skew)
	s.used[id] = rec.ExpiresAt.Add(skew)
	if err := s.save(); err != nil {
		delete(s.used, nonce)
		delete(s.used, id)
		return err
	}
	return nil
}

func (s *Seen) save() error {
	b, err := json.Marshal(s.used)
	if err != nil {
		return err
	}
	return writeAtomic(s.path, b)
}

// writeAtomic replaces path with b, mode 0600, so a crash leaves the old or
// the new contents and never a mix.
func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes a rename into dir survive a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	return errors.Join(err, d.Close())
}
