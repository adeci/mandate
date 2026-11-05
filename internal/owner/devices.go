package owner

import (
	"net/http"
	"slices"
	"strings"

	"github.com/adeci/mandate/record"
)

// pairedDevice is a paired device: a group of keys, one per way it approves,
// sharing one device token. Its ID is its first key's ID, kept after that
// key is retired. A retired key leaves its device; the device ends with
// its last key removed.
type pairedDevice struct {
	TokenSHA256 string   `json:"token_sha256"`
	Name        string   `json:"name,omitempty"` // display name, unsigned; "" is its first key's name
	Keys        []string `json:"keys"`           // key IDs, oldest first
	// Methods label how each key approves, unsigned.
	Methods map[string]string `json:"methods,omitempty"`
}

// methods are the labels a device may give its keys.
var methods = []string{"fingerprint", "screen lock", "security key", "token"}

// newPaired is a device of one key; the app labels its method later.
func newPaired(token, key string) *pairedDevice {
	return &pairedDevice{TokenSHA256: token, Keys: []string{key}}
}

// deviceOf is the device holding key id.
func (st *state) deviceOf(id string) (string, *pairedDevice) {
	for did, d := range st.Devices {
		if slices.Contains(d.Keys, id) {
			return did, d
		}
	}
	return "", nil
}

// deviceByToken is the device whose token's SHA-256 is sum. The caller
// holds s.mu.
func (s *Server) deviceByToken(sum string) (string, *pairedDevice) {
	for id, d := range s.st.Devices {
		if d.TokenSHA256 == sum {
			return id, d
		}
	}
	return "", nil
}

// callerKey is a key of the device whose token r carries, and the token's
// SHA-256. The caller holds s.mu.
func (s *Server) callerKey(r *http.Request) (id, sum string) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	sum = tokenSHA256(token)
	if _, d := s.deviceByToken(sum); d != nil {
		return d.Keys[0], sum
	}
	return "", sum
}

// deviceName is d's display name. The caller holds s.mu.
func (s *Server) deviceName(d *pairedDevice) string {
	if d.Name != "" {
		return d.Name
	}
	for _, id := range d.Keys {
		if k := s.trusted(id); k != nil {
			return k.Name
		}
	}
	return ""
}

// methodName is how the key approves, for Activity.
func (d *pairedDevice) methodName(id string) string {
	if m := d.Methods[id]; m != "" {
		return m
	}
	return "a key"
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

func (s *Server) getKeys(w http.ResponseWriter, r *http.Request) {
	type keyView struct {
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	type deviceView struct {
		ID      string    `json:"id"`
		Name    string    `json:"name"`
		Current bool      `json:"current,omitempty"`
		Keys    []keyView `json:"keys"`
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	retired := []string{}
	for _, k := range s.keys.Trusted() {
		if s.keys.Retired(k.ID) {
			retired = append(retired, k.ID)
		}
	}
	_, sum := s.callerKey(r)
	devices := []deviceView{}
	for id, d := range s.st.Devices {
		v := deviceView{ID: id, Name: s.deviceName(d), Current: d.TokenSHA256 == sum, Keys: []keyView{}}
		for _, k := range d.Keys {
			v.Keys = append(v.Keys, keyView{k, d.Methods[k]})
		}
		devices = append(devices, v)
	}
	slices.SortFunc(devices, func(a, b deviceView) int { return strings.Compare(a.Name+a.ID, b.Name+b.ID) })
	reply(w, http.StatusOK, map[string]any{"log": s.keys.Entries(), "trusted": s.keys.Trusted(), "retired": retired, "devices": devices})
}

// patchKey renames the key's device or labels how the key approves; both
// sign nothing.
func (s *Server) patchKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   *string `json:"name"`
		Method *string `json:"method"`
	}
	if !decode(w, r, &body) {
		return
	}
	var name string
	if body.Name != nil {
		if name = strings.TrimSpace(*body.Name); name == "" || len(name) > 100 {
			fail(w, http.StatusBadRequest, "name is 1 to 100 bytes")
			return
		}
	}
	if body.Method != nil && !slices.Contains(methods, *body.Method) {
		fail(w, http.StatusBadRequest, "method is fingerprint, screen lock, security key or token")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	_, d := s.st.deviceOf(id)
	if d == nil {
		fail(w, http.StatusNotFound, "no such key of a device")
		return
	}
	if body.Name != nil {
		d.Name = name
	}
	if body.Method != nil {
		if d.Methods == nil {
			d.Methods = map[string]string{}
		}
		d.Methods[id] = *body.Method
	}
	if err := s.commit("keys"); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct{}{})
}

// addKeyChallenge hands the calling device an add entry for another key of
// its own, to be signed by one of its keys that is not retired. Its token
// works with every key it has.
func (s *Server) addKeyChallenge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key    record.Key `json:"key"`
		Method string     `json:"method"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !slices.Contains(methods, body.Method) {
		fail(w, http.StatusBadRequest, "method is fingerprint, screen lock, security key or token")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, sum := s.callerKey(r)
	did, d := s.deviceByToken(sum)
	k := body.Key
	if d == nil {
		fail(w, http.StatusUnauthorized, "missing or unknown device token")
		return
	}
	if s.trusted(k.ID) != nil {
		fail(w, http.StatusConflict, "the key is trusted already")
		return
	}
	var signers []string
	for _, id := range d.Keys {
		if !s.keys.Retired(id) {
			signers = append(signers, id)
		}
	}
	holds := func() bool { return s.st.Devices[did] == d }
	s.keyChallenge(w, "add", k, signers, holds, func(env record.Envelope) error {
		d.Keys = append(d.Keys, k.ID)
		if d.Methods == nil {
			d.Methods = map[string]string{}
		}
		d.Methods[k.ID] = body.Method
		if err := s.commit("keys"); err != nil {
			return err
		}
		s.note(activity{Kind: "key.added", Detail: s.deviceName(d) + " added " + body.Method}, env.Key)
		return nil
	})
}

// retireKeyChallenge hands out the retire entry for a key, to be signed by
// another key that signs. What the retired key signed stays valid, and it
// leaves its device, which must keep a key.
func (s *Server) retireKeyChallenge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.trusted(id)
	_, d := s.st.deviceOf(id)
	switch {
	case key == nil:
		fail(w, http.StatusNotFound, "no such trusted key")
		return
	case s.keys.Retired(id):
		fail(w, http.StatusConflict, "the key is retired already")
		return
	case d != nil && len(d.Keys) == 1:
		fail(w, http.StatusConflict, "a device's last key can't be retired; remove the device")
		return
	case s.signers(id) == 0:
		fail(w, http.StatusConflict, "the last key that signs can't be retired")
		return
	}
	s.keyChallenge(w, "retire", *key, nil, func() bool { return true }, func(env record.Envelope) error {
		detail := key.Name + " removed a key"
		if _, d := s.st.deviceOf(id); d != nil {
			detail = s.deviceName(d) + " removed " + d.methodName(id)
			d.Keys = slices.DeleteFunc(d.Keys, func(k string) bool { return k == id })
			delete(d.Methods, id)
		}
		if err := s.commit("keys"); err != nil {
			return err
		}
		s.note(activity{Kind: "key.retired", Detail: detail}, env.Key)
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
	name := key.Name
	if _, d := s.st.deviceOf(id); d != nil {
		name = s.deviceName(d)
	}
	s.keyChallenge(w, "remove", *key, nil, func() bool { return true }, func(env record.Envelope) error {
		if err := s.removeKey(id); err != nil {
			return err
		}
		s.note(activity{Kind: "key.removed", Detail: name}, env.Key)
		return nil
	})
}

// removeKey drops the key from its device, ending the device with its last
// key, and drops what the key signed. The caller holds s.mu and has
// appended the remove entry.
func (s *Server) removeKey(id string) error {
	if did, d := s.st.deviceOf(id); d != nil {
		d.Keys = slices.DeleteFunc(d.Keys, func(k string) bool { return k == id })
		delete(d.Methods, id)
		if len(d.Keys) == 0 {
			delete(s.st.Devices, did)
			// Open streams reconnect, and the device's token no longer works.
			for ch := range s.subs {
				delete(s.subs, ch)
				close(ch)
			}
		}
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
