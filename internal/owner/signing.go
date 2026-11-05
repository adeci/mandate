package owner

import (
	"bytes"
	"cmp"
	"net/http"
	"slices"
	"strings"
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
	// signer, if set, is the only key that may sign it.
	signer string
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
		if c.signer != "" && env.Key != c.signer {
			fail(w, http.StatusForbidden, "only the asking device's key may sign this")
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
// log is unchanged; signer, if set, is the only key that may sign it. The
// caller holds s.mu.
func (s *Server) keyChallenge(w http.ResponseWriter, op string, k record.Key, signer string, holds func() bool, apply func(env record.Envelope) error) {
	n, head := s.keys.Head()
	entry := record.KeyEntry{Seq: n, Op: op, Key: k, Prev: head, At: record.Time{Time: s.now()}}
	s.issue(w, entry, &challenge{
		signer: signer,
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

func (s *Server) getKeys(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	retired := []string{}
	names := map[string]string{}
	for _, k := range s.keys.Trusted() {
		if s.keys.Retired(k.ID) {
			retired = append(retired, k.ID)
		}
		names[k.ID] = cmp.Or(s.st.KeyNames[k.ID], k.Name)
	}
	reply(w, http.StatusOK, map[string]any{"log": s.keys.Entries(), "trusted": s.keys.Trusted(), "retired": retired, "names": names})
}

// renameKey sets a device's display name, which signs nothing.
func (s *Server) renameKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 100 {
		fail(w, http.StatusBadRequest, "name is 1 to 100 bytes")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	if s.trusted(id) == nil {
		fail(w, http.StatusNotFound, "no such trusted key")
		return
	}
	if s.st.KeyNames == nil {
		s.st.KeyNames = map[string]string{}
	}
	s.st.KeyNames[id] = name
	if err := s.commit("keys"); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct{}{})
}

// trusted is the trusted key id, or nil. The caller holds s.mu.
func (s *Server) trusted(id string) *record.Key {
	for _, k := range s.keys.Trusted() {
		if k.ID == id {
			return &k
		}
	}
	return nil
}

// signers counts the trusted keys that are not retired, besides id. The
// caller holds s.mu.
func (s *Server) signers(id string) int {
	n := 0
	for _, k := range s.keys.Trusted() {
		if k.ID != id && !s.keys.Retired(k.ID) {
			n++
		}
	}
	return n
}

// callerKey is the ID of the trusted key whose device token r carries, and
// the token's SHA-256. The caller holds s.mu.
func (s *Server) callerKey(r *http.Request) (id, sum string) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	sum = tokenSHA256(token)
	for _, k := range s.keys.Trusted() {
		if s.st.Devices[k.ID] == sum {
			return k.ID, sum
		}
	}
	return "", sum
}

// addKeyChallenge hands the calling device an add entry for a new key of
// its own, to be signed by its current key; once signed, the device token
// belongs to the new key.
func (s *Server) addKeyChallenge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key record.Key `json:"key"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, sum := s.callerKey(r)
	k := body.Key
	if old == "" {
		fail(w, http.StatusUnauthorized, "missing or unknown device token")
		return
	}
	if slices.ContainsFunc(s.keys.Trusted(), func(t record.Key) bool { return t.ID == k.ID }) {
		fail(w, http.StatusConflict, "the key is trusted already")
		return
	}
	holds := func() bool { return s.st.Devices[old] == sum }
	s.keyChallenge(w, "add", k, old, holds, func(env record.Envelope) error {
		delete(s.st.Devices, old)
		s.st.Devices[k.ID] = sum
		if err := s.commit("keys"); err != nil {
			return err
		}
		s.note(activity{Kind: "key.added", Detail: k.Name}, env.Key)
		return nil
	})
}

// retireKeyChallenge hands out the retire entry for a key, to be signed by
// another key that signs: in a rotation, the device's new key. What the
// retired key signed stays valid; its device token, if it still has one,
// ends.
func (s *Server) retireKeyChallenge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.trusted(id)
	switch {
	case key == nil:
		fail(w, http.StatusNotFound, "no such trusted key")
		return
	case s.keys.Retired(id):
		fail(w, http.StatusConflict, "the key is retired already")
		return
	case s.signers(id) == 0:
		fail(w, http.StatusConflict, "the last key that signs can't be retired")
		return
	}
	name := cmp.Or(s.st.KeyNames[id], key.Name)
	s.keyChallenge(w, "retire", *key, "", func() bool { return true }, func(env record.Envelope) error {
		delete(s.st.Devices, id)
		if err := s.commit("keys"); err != nil {
			return err
		}
		s.note(activity{Kind: "key.retired", Detail: name}, env.Key)
		return nil
	})
}

func (s *Server) removeKeyChallenge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.trusted(id)
	switch {
	case key == nil:
		fail(w, http.StatusNotFound, "no such trusted key")
		return
	case s.signers(id) == 0:
		fail(w, http.StatusConflict, "the last key can't be removed")
		return
	}
	name := cmp.Or(s.st.KeyNames[id], key.Name)
	s.keyChallenge(w, "remove", *key, "", func() bool { return true }, func(env record.Envelope) error {
		if err := s.removeKey(id); err != nil {
			return err
		}
		s.note(activity{Kind: "key.removed", Detail: name}, env.Key)
		return nil
	})
}

// removeKey revokes the key's device token and drops what it signed. The
// caller holds s.mu and has appended the remove entry.
func (s *Server) removeKey(id string) error {
	delete(s.st.Devices, id)
	delete(s.st.KeyNames, id)
	// Open streams reconnect, and the removed device's token no longer works.
	for ch := range s.subs {
		delete(s.subs, ch)
		close(ch)
	}
	now := s.now()
	var deleted []activity
	for iid, i := range s.st.Identities {
		if i.Envelope.Key == id {
			s.st.deleteIdentity(iid, now)
			s.cancel(func(c *challenge) bool { return c.identity == iid })
			deleted = append(deleted, activity{Kind: "agent.deleted", Identity: iid, Detail: i.Name})
		}
	}
	for gid, g := range s.st.Grants {
		if g.Envelope.Key == id {
			delete(s.st.Grants, gid)
		}
	}
	if err := s.commit("keys", "identities", "now"); err != nil {
		return err
	}
	for _, a := range deleted {
		s.note(a, "")
	}
	return nil
}
