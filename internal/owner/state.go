package owner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/record"
)

// state is everything the owner side decides on, kept in state.json and
// rewritten whole on every change. Activity is appended to a file per day
// instead, since it only grows.
type state struct {
	Keys       []record.Envelope    `json:"keys"`
	Identities map[string]*identity `json:"identities"`
	Grants     map[string]*grant    `json:"grants"`
	Requests   map[string]*request  `json:"requests"`
	Proposals  map[string]*proposal `json:"proposals"`
	NeverShare []neverShare         `json:"never_share"`
	// Devices are the paired devices by ID (devices.go).
	Devices map[string]*pairedDevice `json:"paired_devices"`
	// Before a device could have several keys: a token per key ID, and
	// display names by key ID. Read once, into Devices.
	OldDevices  map[string]string `json:"devices,omitempty"`
	OldKeyNames map[string]string `json:"key_names,omitempty"`
}

type identity struct {
	Envelope  record.Envelope `json:"envelope"`
	Record    record.Identity `json:"record"`
	Name      string          `json:"name"`
	FirstSeen time.Time       `json:"first_seen,omitzero"`
	LastSeen  time.Time       `json:"last_seen,omitzero"`
}

type grant struct {
	Envelope record.Envelope `json:"envelope"`
	Record   record.Grant    `json:"record"`
	// ChatNames are the names the agent's request matched, for display.
	ChatNames map[string]string `json:"chat_names,omitempty"`
	// ClosedAt ends a window early or pauses a routine.
	ClosedAt time.Time `json:"closed_at,omitzero"`
}

type chat struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is stored for a request term's chats; responses fill it, Icon
	// and Parents from the connector.
	Kind    string             `json:"kind,omitempty"`
	Icon    string             `json:"icon,omitempty"`
	Parents []connector.Parent `json:"parents,omitempty"`
}

type term struct {
	Term  string `json:"term"`
	Chats []chat `json:"chats"`
	// More says the term matched more chats than it lists.
	More bool `json:"more,omitempty"`
}

type schedule struct {
	Days   []int  `json:"days,omitempty"`
	Start  string `json:"start"`
	Length int    `json:"length"`
}

type request struct {
	ID        string    `json:"id"`
	Identity  string    `json:"identity"`
	Connector string    `json:"connector"`
	Kind      string    `json:"kind"` // once, routine or extend
	Terms     []term    `json:"terms,omitempty"`
	For       int       `json:"for,omitempty"`
	Horizon   int       `json:"horizon"`
	Schedule  *schedule `json:"schedule,omitempty"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"` // pending, granted, denied or expired
	GrantID   string    `json:"grant_id,omitempty"`
	Until     time.Time `json:"until,omitzero"`
	DecidedAt time.Time `json:"decided_at,omitzero"`
}

type proposal struct {
	ID        string             `json:"id"`
	Identity  string             `json:"identity"`
	Connector string             `json:"connector"`
	Op        string             `json:"op"`
	Prepared  connector.Prepared `json:"prepared"`
	Note      string             `json:"note,omitempty"`
	CreatedAt time.Time          `json:"created_at"`
	ExpiresAt time.Time          `json:"expires_at"`
	// Status is pending, executing, done, failed, unknown, rejected or
	// expired.
	Status    string            `json:"status"`
	Approval  *record.Envelope  `json:"approval,omitempty"`
	Result    *connector.Result `json:"result,omitempty"`
	DecidedAt time.Time         `json:"decided_at,omitzero"`
}

type neverShare struct {
	Connector string `json:"connector"`
	Kind      string `json:"kind"` // chat or person
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
}

const (
	requestTTL  = time.Hour
	proposalTTL = 24 * time.Hour
	// A window that ended stays live this long, for proposals answering it.
	afterEnd = 24 * time.Hour
	// Decided requests and proposals are kept this long for the agent's poll.
	keepDecided = 24 * time.Hour
	// Per identity, counted from the requests kept here.
	requestsWaiting = 3
	requestsPerHour = 10
)

func newState() state {
	return state{
		Devices:    map[string]*pairedDevice{},
		Identities: map[string]*identity{},
		Grants:     map[string]*grant{},
		Requests:   map[string]*request{},
		Proposals:  map[string]*proposal{},
	}
}

func stateFile(dir string) string { return filepath.Join(dir, "state.json") }

func loadState(dir string) (state, error) {
	st := newState()
	b, err := os.ReadFile(stateFile(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read owner state: %w", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("decode owner state: %w", err)
	}
	if st.Devices == nil {
		st.Devices = map[string]*pairedDevice{}
	}
	// Each device of before is a device of one key.
	for id, token := range st.OldDevices {
		d := newPaired(token, id)
		d.Name = st.OldKeyNames[id]
		st.Devices[id] = d
	}
	st.OldDevices, st.OldKeyNames = nil, nil
	return st, nil
}

func saveState(dir string, st *state) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode owner state: %w", err)
	}
	return writeFile(stateFile(dir), b, 0o600)
}

// writeFile replaces path atomically: a crash leaves the old file or the new.
func writeFile(path string, b []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
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
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func (g *grant) end() time.Time {
	if !g.ClosedAt.IsZero() && (g.Record.Schedule != nil || g.ClosedAt.Before(g.Record.Until.Time)) {
		return g.ClosedAt
	}
	return g.Record.Until.Time
}

// live is whether the gateway gets the grant: not closed, not a paused
// routine, not ended more than a day ago.
func (g *grant) live(now time.Time) bool {
	if !g.ClosedAt.IsZero() {
		return false
	}
	return g.Record.Schedule != nil || now.Before(g.Record.Until.Add(afterEnd))
}

func (g *grant) open(now time.Time) bool {
	return g.Record.Schedule == nil && g.ClosedAt.IsZero() &&
		!now.Before(g.Record.From.Time) && now.Before(g.Record.Until.Time)
}

// extended is where extending g by seconds from now ends: that far past
// its end, or past now if it ended already.
func extended(g *grant, now time.Time, seconds int) time.Time {
	from := g.end()
	if now.After(from) {
		from = now
	}
	return from.Add(time.Duration(seconds) * time.Second)
}

// expire moves what ran out of time to its final status and drops what is
// no longer needed. It reports whether anything changed, and what expired.
func (st *state) expire(now time.Time) (bool, []activity) {
	changed := false
	var expired []activity
	for id, r := range st.Requests {
		switch {
		case r.Status == "pending" && !now.Before(r.CreatedAt.Add(requestTTL)):
			r.Status, r.DecidedAt = "expired", now
			changed = true
			expired = append(expired, requestActivity("request.expired", r))
		case r.Status != "pending" && now.Sub(r.DecidedAt) > keepDecided:
			delete(st.Requests, id)
			changed = true
		}
	}
	for id, p := range st.Proposals {
		switch {
		case p.Status == "pending" && !now.Before(p.ExpiresAt):
			p.Status, p.DecidedAt = "expired", now
			changed = true
			expired = append(expired, proposalActivity("proposal.expired", p, p.Op))
		case p.Status != "pending" && p.Status != "executing" && now.Sub(p.DecidedAt) > keepDecided:
			delete(st.Proposals, id)
			changed = true
		}
	}
	for id, g := range st.Grants {
		if g.Record.Schedule == nil && now.Sub(g.end()) > afterEnd {
			delete(st.Grants, id)
			changed = true
		}
	}
	return changed, expired
}

func (st *state) deleteIdentity(id string, now time.Time) {
	delete(st.Identities, id)
	for gid, g := range st.Grants {
		if g.Record.Identity == id {
			delete(st.Grants, gid)
		}
	}
	for _, r := range st.Requests {
		if r.Identity == id && r.Status == "pending" {
			r.Status, r.DecidedAt = "denied", now
		}
	}
	for _, p := range st.Proposals {
		if p.Identity == id && p.Status == "pending" {
			p.Status, p.DecidedAt = "rejected", now
		}
	}
}

type live struct {
	Keys       []record.Envelope `json:"keys"`
	Identities []record.Envelope `json:"identities"`
	Grants     []record.Envelope `json:"grants"`
	NeverShare []liveNeverShare  `json:"never_share"`
}

type liveNeverShare struct {
	Connector string `json:"connector"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
}

func (st *state) live(now time.Time) live {
	l := live{
		Keys:       append([]record.Envelope{}, st.Keys...),
		Identities: []record.Envelope{},
		Grants:     []record.Envelope{},
		NeverShare: []liveNeverShare{},
	}
	for _, i := range st.Identities {
		l.Identities = append(l.Identities, i.Envelope)
	}
	for _, g := range st.Grants {
		if g.live(now) && st.Identities[g.Record.Identity] != nil {
			l.Grants = append(l.Grants, g.Envelope)
		}
	}
	for _, n := range st.NeverShare {
		l.NeverShare = append(l.NeverShare, liveNeverShare{n.Connector, n.Kind, n.ID})
	}
	return l
}
