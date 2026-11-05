package owner

import (
	"bytes"
	"net/http"
	"slices"
	"time"

	"github.com/adeci/mandate/record"
)

const challengeTTL = 5 * time.Minute

// challenge is an unsigned envelope the server handed out, with what must
// still hold to accept it and what accepting it does.
type challenge struct {
	env record.Envelope
	// identity and grant name what a Close, Stop all or identity deletion
	// cancels.
	identity, grant string
	used, cancelled bool
	// signers, if set, are the only keys that may sign it.
	signers []string
	// holds reports whether the state the challenge was made for still
	// holds; apply makes the change. Both run with s.mu held.
	holds func() bool
	apply func(env record.Envelope, r record.Record) error
}

// The caller holds s.mu.
func (s *Server) issue(w http.ResponseWriter, r record.Record, c *challenge) {
	env, err := record.New(r, s.now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	c.env = env
	for n, old := range s.challenges {
		if s.now().Sub(old.env.IssuedAt.Time) > time.Hour {
			delete(s.challenges, n)
		}
	}
	s.challenges[string(env.Nonce)] = c
	reply(w, http.StatusOK, env)
}

// The caller holds s.mu.
func (s *Server) cancel(match func(c *challenge) bool) {
	for _, c := range s.challenges {
		if match(c) {
			c.cancelled = true
		}
	}
}

func (s *Server) signed(w http.ResponseWriter, r *http.Request) {
	var env record.Envelope
	if !decode(w, r, &env) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.challenges[string(env.Nonce)]
	switch {
	case c == nil || c.env.Kind != env.Kind || !bytes.Equal(c.env.Record, env.Record) ||
		!c.env.IssuedAt.Equal(env.IssuedAt.Time):
		fail(w, http.StatusNotFound, "no such challenge")
	case c.used:
		fail(w, http.StatusConflict, "the challenge was already used")
	case c.cancelled:
		fail(w, http.StatusConflict, "the challenge was cancelled")
	case s.now().Sub(c.env.IssuedAt.Time) > challengeTTL:
		fail(w, http.StatusGone, "the challenge is older than 5 minutes")
	case !c.holds():
		fail(w, http.StatusConflict, "this was decided, closed or replaced meanwhile")
	default:
		rec, err := s.keys.Verify(env, s.now())
		if err != nil {
			fail(w, http.StatusForbidden, err.Error())
			return
		}
		// A retired key's earlier identities and grants stay valid, but it
		// signs nothing new.
		if s.keys.Retired(env.Key) {
			fail(w, http.StatusForbidden, record.ErrRetired.Error())
			return
		}
		if c.signers != nil && !slices.Contains(c.signers, env.Key) {
			fail(w, http.StatusForbidden, "only the asking device's keys may sign this")
			return
		}
		c.used = true
		if err := c.apply(env, rec); err != nil {
			failErr(w, err)
			return
		}
		reply(w, http.StatusOK, map[string]string{"kind": env.Kind, "id": recordID(rec)})
	}
}

func recordID(r record.Record) string {
	switch r := r.(type) {
	case record.KeyEntry:
		return r.Key.ID
	case record.Identity:
		return r.ID
	case record.Grant:
		return r.ID
	case record.Write:
		return r.ID
	}
	return ""
}

// keyChallenge hands out the next key log entry, which holds only while the
// log is unchanged; signers, if set, are the only keys that may sign it.
// The caller holds s.mu.
func (s *Server) keyChallenge(w http.ResponseWriter, op string, k record.Key, signers []string, holds func() bool, apply func(env record.Envelope) error) {
	n, head := s.keys.Head()
	entry := record.KeyEntry{Seq: n, Op: op, Key: k, Prev: head, At: record.Time{Time: s.now()}}
	s.issue(w, entry, &challenge{
		signers: signers,
		holds: func() bool {
			m, h := s.keys.Head()
			return m == n && h == head && holds()
		},
		apply: func(env record.Envelope, _ record.Record) error {
			if err := s.appendKey(env); err != nil {
				return err
			}
			return apply(env)
		},
	})
}
