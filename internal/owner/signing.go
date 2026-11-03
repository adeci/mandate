package owner

import (
	"bytes"
	"net/http"
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
	// holds reports whether the state the challenge was made for still
	// holds; apply makes the change. Both run with s.mu held.
	holds func() bool
	apply func(env record.Envelope, r record.Record) error
}

// issue hands out c and answers its envelope. The caller holds s.mu.
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

// cancel cancels the challenges match selects. The caller holds s.mu.
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
// log is unchanged. The caller holds s.mu.
func (s *Server) keyChallenge(w http.ResponseWriter, op string, k record.Key, holds func() bool, apply func() error) {
	n, head := s.keys.Head()
	entry := record.KeyEntry{Seq: n, Op: op, Key: k, Prev: head, At: record.Time{Time: s.now()}}
	s.issue(w, entry, &challenge{
		holds: func() bool {
			m, h := s.keys.Head()
			return m == n && h == head && holds()
		},
		apply: func(env record.Envelope, _ record.Record) error {
			if err := s.appendKey(env); err != nil {
				return err
			}
			return apply()
		},
	})
}

func (s *Server) getKeys(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply(w, http.StatusOK, map[string]any{"log": s.keys.Entries(), "trusted": s.keys.Trusted()})
}

func (s *Server) removeKeyChallenge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	trusted := s.keys.Trusted()
	var key *record.Key
	for i := range trusted {
		if trusted[i].ID == id {
			key = &trusted[i]
		}
	}
	switch {
	case key == nil:
		fail(w, http.StatusNotFound, "no such trusted key")
		return
	case len(trusted) == 1:
		fail(w, http.StatusConflict, "the last key can't be removed")
		return
	}
	s.keyChallenge(w, "remove", *key, func() bool { return true }, func() error { return s.removeKey(id) })
}

// removeKey revokes the key's device token and drops what it signed. The
// caller holds s.mu and has appended the remove entry.
func (s *Server) removeKey(id string) error {
	delete(s.st.Devices, id)
	// Open streams reconnect, and the removed device's token no longer works.
	for ch := range s.subs {
		delete(s.subs, ch)
		close(ch)
	}
	now := s.now()
	for iid, i := range s.st.Identities {
		if i.Envelope.Key == id {
			s.st.deleteIdentity(iid, now)
			s.cancel(func(c *challenge) bool { return c.identity == iid })
		}
	}
	for gid, g := range s.st.Grants {
		if g.Envelope.Key == id {
			delete(s.st.Grants, gid)
		}
	}
	return s.commit("keys", "identities", "now")
}
