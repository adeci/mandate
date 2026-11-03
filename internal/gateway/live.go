package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/adeci/mandate/record"
)

// liveSet is what PUT /internal/live carries and what live.json keeps: the
// owner side's key log, and the identities, grants and never-share list
// that are live. The file holds only what verified.
type liveSet struct {
	Keys       []record.Envelope `json:"keys"`
	Identities []record.Envelope `json:"identities"`
	Grants     []record.Envelope `json:"grants"`
	NeverShare []neverShare      `json:"never_share"`
}

type neverShare struct {
	Connector string `json:"connector"`
	Kind      string `json:"kind"` // chat or person
	ID        string `json:"id"`
}

var errKeyLog = errors.New("key log refused")

type neverShared struct{ chats, people []string }

// live is the verified live set, as the gateway serves it.
type live struct {
	set    liveSet
	keys   *record.Log
	tokens map[string]string         // token_sha256 → identity ID
	grants map[string][]record.Grant // identity ID → its grants
	hidden map[string]neverShared    // connector → never-share
}

// verify builds the live set from set, whose keys must extend held. What
// fails to verify is left out and logged.
func verify(held *record.Log, set liveSet, now time.Time, log *slog.Logger) (*live, error) {
	keys := new(record.Log)
	*keys = *held
	if err := keys.Extend(set.Keys); err != nil {
		return nil, fmt.Errorf("%w: %w", errKeyLog, err)
	}
	l := &live{
		set:    liveSet{Keys: keys.Entries(), Identities: []record.Envelope{}, Grants: []record.Envelope{}, NeverShare: []neverShare{}},
		keys:   keys,
		tokens: map[string]string{},
		grants: map[string][]record.Grant{},
		hidden: map[string]neverShared{},
	}
	identities := map[string]bool{}
	for _, e := range set.Identities {
		r, err := verifyKind(keys, e, "identity", now)
		if err != nil {
			log.Warn("identity not served", "err", err)
			continue
		}
		id := r.(record.Identity)
		identities[id.ID] = true
		l.tokens[id.TokenSHA256] = id.ID
		l.set.Identities = append(l.set.Identities, e)
	}
	for _, e := range set.Grants {
		r, err := verifyKind(keys, e, "grant", now)
		if err != nil {
			log.Warn("grant not served", "err", err)
			continue
		}
		g := r.(record.Grant)
		if !identities[g.Identity] {
			log.Warn("grant not served", "grant", g.ID, "err", "its identity is not live")
			continue
		}
		l.grants[g.Identity] = append(l.grants[g.Identity], g)
		l.set.Grants = append(l.set.Grants, e)
	}
	for _, n := range set.NeverShare {
		h := l.hidden[n.Connector]
		switch {
		case n.Connector == "" || n.ID == "":
			return nil, errors.New("never_share entry without connector or id")
		case n.Kind == "chat":
			h.chats = append(h.chats, n.ID)
		case n.Kind == "person":
			h.people = append(h.people, n.ID)
		default:
			return nil, fmt.Errorf("never_share kind %q is not chat or person", n.Kind)
		}
		l.hidden[n.Connector] = h
		l.set.NeverShare = append(l.set.NeverShare, n)
	}
	return l, nil
}

func verifyKind(keys *record.Log, e record.Envelope, kind string, now time.Time) (record.Record, error) {
	if e.Kind != kind {
		return nil, fmt.Errorf("%s record among the %ss", e.Kind, kind)
	}
	return keys.Verify(e, now)
}

// identity is the identity whose token is token, "" for none.
func (l *live) identity(token string) string {
	h := sha256.Sum256([]byte(token))
	return l.tokens[hex.EncodeToString(h[:])]
}

// grantsOn are identity's grants on connector.
func (l *live) grantsOn(identity, connector string) []record.Grant {
	var out []record.Grant
	for _, g := range l.grants[identity] {
		if g.Connector == connector {
			out = append(out, g)
		}
	}
	return out
}

// loadLive reads what the last push left; none is an empty set, which
// serves nobody until the owner side pushes.
func loadLive(path string, log *slog.Logger) (*live, error) {
	var set liveSet
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read live set: %w", err)
	default:
		if err := json.Unmarshal(b, &set); err != nil {
			return nil, fmt.Errorf("decode live set %s: %w", path, err)
		}
	}
	l, err := verify(new(record.Log), set, time.Now(), log)
	if err != nil {
		return nil, fmt.Errorf("live set %s: %w", path, err)
	}
	return l, nil
}

const maxLive = 16 << 20

func (g *gateway) putLive(w http.ResponseWriter, r *http.Request) {
	var set liveSet
	if !decode(w, r, &set, maxLive) {
		return
	}
	g.liveMu.Lock()
	defer g.liveMu.Unlock()
	l, err := verify(g.live.Load().keys, set, time.Now(), g.log)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errKeyLog) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	b, err := json.Marshal(l.set)
	if err == nil {
		err = writeFile(filepath.Join(g.stateDir, "live.json"), b)
	}
	if err != nil {
		g.log.Error("save live set", "err", err)
		writeError(w, http.StatusInternalServerError, "could not save the live set")
		return
	}
	g.live.Store(l)
	g.log.Info("live set", "keys", len(l.set.Keys), "identities", len(l.set.Identities), "grants", len(l.set.Grants),
		"never_share", len(l.set.NeverShare))
	w.WriteHeader(http.StatusNoContent)
}

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
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
