package gmessages

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/adeci/mandate/internal/exitcode"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

var errNotPaired = fmt.Errorf(`not paired: run "mandate messages pair": %w`, exitcode.ErrNeedsOwner)

// sessionFile persists libgm.AuthData as JSON. The library mutates AuthData in
// place (token refreshes), so saves serialize on mu.
type sessionFile struct {
	path string
	mu   sync.Mutex
	auth *libgm.AuthData
}

func loadSession(path string) (*sessionFile, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotPaired
	} else if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	auth := &libgm.AuthData{}
	if err := json.Unmarshal(raw, auth); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", path, err)
	}
	if auth.Browser == nil || auth.TachyonAuthToken == nil || auth.RequestCrypto == nil || auth.RefreshKey == nil {
		return nil, errNotPaired
	}
	return &sessionFile{path: path, auth: auth}, nil
}

func (s *sessionFile) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth.CookiesLock.RLock()
	raw, err := json.Marshal(s.auth)
	s.auth.CookiesLock.RUnlock()
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	return writeFileAtomic(s.path, raw)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	// CreateTemp creates the file with mode 0600.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	return nil
}
