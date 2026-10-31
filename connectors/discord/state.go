package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// state never holds the token or message content.
type state struct {
	// Browser identity, stable across restarts like one long-lived profile.
	LaunchID           string    `json:"launch_id"`
	LaunchSignature    string    `json:"launch_signature"`
	HeartbeatSessionID string    `json:"heartbeat_session_id,omitempty"`
	HeartbeatUsedAt    time.Time `json:"heartbeat_used_at"`
	BuildNumber        int       `json:"build_number,omitempty"`
	BuildCheckedAt     time.Time `json:"build_checked_at"`
	Cookies            []cookie  `json:"cookies,omitempty"`

	SelfID        string    `json:"self_id,omitempty"`
	SelfCheckedAt time.Time `json:"self_checked_at"`

	Requests      []time.Time `json:"requests"`    // upstream requests in the last 24 h
	RateLimits    []time.Time `json:"rate_limits"` // 429s in the last hour
	CooldownUntil time.Time   `json:"cooldown_until"`
	BreakUntil    time.Time   `json:"break_until"`
	SessionStart  time.Time   `json:"session_start"`
	SessionCount  int         `json:"session_count"`
	LastRequest   time.Time   `json:"last_request"`

	// Denied maps "channel:<id>" / "guild:<id>" to a cached 403/404.
	Denied map[string]denial `json:"denied,omitempty"`
}

type denial struct {
	Status int       `json:"status"`
	Until  time.Time `json:"until"`
}

// Session cookies are not kept: a browser drops them on restart.
type cookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Domain   string    `json:"domain,omitempty"`
	Path     string    `json:"path,omitempty"`
	Expires  time.Time `json:"expires"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
}

func (s *state) buildNumber() int {
	if s.BuildNumber > 0 {
		return s.BuildNumber
	}
	return defaultBuildNumber
}

// A corrupt file is an error rather than a silent reset, which would forget a cooldown.
func loadState(path string) (*state, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &state{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	return &s, nil
}

func (s *state) ensureIdentity() {
	if s.LaunchID == "" {
		s.LaunchID = newUUID()
	}
	if s.LaunchSignature == "" {
		s.LaunchSignature = newLaunchSignature()
	}
}

func (s *state) prune(now time.Time) {
	s.Requests = since(s.Requests, now.Add(-budgetWindow))
	s.RateLimits = since(s.RateLimits, now.Add(-rateLimitWindow))
	for k, d := range s.Denied {
		if !d.Until.After(now) {
			delete(s.Denied, k)
		}
	}
	kept := s.Cookies[:0]
	for _, c := range s.Cookies {
		if c.Expires.After(now) {
			kept = append(kept, c)
		}
	}
	s.Cookies = kept
}

// since returns the suffix of ascending ts strictly after cutoff.
func since(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}

func (s *state) save(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("save state: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("save state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("save state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
