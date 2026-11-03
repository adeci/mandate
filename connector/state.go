package connector

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/adeci/mandate/record"
)

// keyLogFile, in private/, holds the key log as a JSON array of envelopes.
// `mandate reset` deletes it.
const keyLogFile = "keylog.json"

// loadKeys reads the key log at path; none is an empty log.
func loadKeys(path string) (*record.Log, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return new(record.Log), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read key log: %w", err)
	}
	var entries []record.Envelope
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("decode key log %s: %w", path, err)
	}
	l, err := record.NewLog(entries)
	if err != nil {
		return nil, fmt.Errorf("verify key log %s: %w", path, err)
	}
	return l, nil
}

func saveKeys(path string, l *record.Log) error {
	b, err := json.Marshal(l.Entries())
	if err != nil {
		return err
	}
	return writeFile(path, b)
}

var errSpent = errors.New("this approval was already used")

// spent is the append-only file of approvals run: a line of record ID and
// nonce each, in base64url so that neither can break a line.
type spent struct {
	mu     sync.Mutex
	f      *os.File
	ids    map[string]bool
	nonces map[string]bool
}

func openSpent(path string) (*spent, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open spent approvals: %w", err)
	}
	s := &spent{f: f, ids: map[string]bool{}, nonces: map[string]bool{}}
	if err := s.load(); err != nil {
		f.Close()
		return nil, fmt.Errorf("load spent approvals %s: %w", path, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func (s *spent) load() error {
	b, err := io.ReadAll(s.f)
	if err != nil {
		return err
	}
	// A crash mid-append leaves a torn last line, whose write never ran.
	n := bytes.LastIndexByte(b, '\n') + 1
	if n < len(b) {
		if err := s.f.Truncate(int64(n)); err != nil {
			return err
		}
	}
	for line := range bytes.Lines(b[:n]) {
		id, nonce, ok := bytes.Cut(bytes.TrimSuffix(line, []byte("\n")), []byte(" "))
		if !ok {
			return fmt.Errorf("bad line %q", line)
		}
		s.ids[string(id)] = true
		s.nonces[string(nonce)] = true
	}
	return nil
}

// spend durably records id and nonce, refusing either if seen before.
func (s *spent) spend(id string, nonce []byte) error {
	i := base64.RawURLEncoding.EncodeToString([]byte(id))
	n := base64.RawURLEncoding.EncodeToString(nonce)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[i] || s.nonces[n] {
		return errSpent
	}
	// Marked before writing: a failed write runs nothing, and no retry runs
	// it either while this process lives.
	s.ids[i], s.nonces[n] = true, true
	if _, err := s.f.WriteString(i + " " + n + "\n"); err != nil {
		return fmt.Errorf("record spent approval: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("record spent approval: %w", err)
	}
	return nil
}

func (s *spent) close() error { return s.f.Close() }

// writeFile replaces path with b, so a crash leaves the old or the new
// content and never a mix.
func writeFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
