package gateway

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const syncInterval = 15 * time.Second

// agent is an agent the owner signed, with its signed grants.
type agent struct {
	approval.Agent
	sum    []byte
	grants []approval.Grant
}

// access holds the agents and grants the console last served that verify
// against the key log. IDs once served and later missing, or no longer
// verifying, stay revoked, so a console cannot bring a deleted agent or
// revoked grant back.
type access struct {
	console  *proposal.Client
	peers    map[string]*connector.Client // connectors, which pin the key log too
	verifier *approval.Verifier
	path     string

	mu      sync.Mutex // serializes sync
	synced  time.Time  // when the last successful sync started
	forced  time.Time  // when refresh last synced
	seen    map[string]bool
	revoked map[string]bool
	dirty   bool // revocations not yet written to path
	records []record
	cur     atomic.Pointer[[]*agent]
}

// knownIDs is access.json.
type knownIDs struct {
	Seen    []string `json:"seen"`
	Revoked []string `json:"revoked"`
}

// record is a verified access record under its ID prefixed with its kind.
type record struct {
	id     string
	signed approval.Signed
	agent  *agent
	grant  approval.Grant
}

func openAccess(dir, rpID, origin string, console *proposal.Client, sockets map[string]string) (*access, error) {
	keys, err := approval.OpenKeys(filepath.Join(dir, "keys.json"), rpID, origin)
	if err != nil {
		return nil, fmt.Errorf("open key log: %w", err)
	}
	a := &access{
		console: console, peers: map[string]*connector.Client{}, verifier: &approval.Verifier{Keys: keys},
		path: filepath.Join(dir, "access.json"), seen: map[string]bool{}, revoked: map[string]bool{},
	}
	for name, socket := range sockets {
		a.peers[name] = connector.Dial(socket, specTimeout)
	}
	b, err := os.ReadFile(a.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var known knownIDs
		if err := json.Unmarshal(b, &known); err != nil {
			return nil, fmt.Errorf("parse %s: %w", a.path, err)
		}
		for _, id := range known.Seen {
			a.seen[id] = true
		}
		for _, id := range known.Revoked {
			a.revoked[id] = true
		}
	}
	a.cur.Store(&[]*agent{})
	return a, nil
}

func (a *access) agents() []*agent { return *a.cur.Load() }

// find matches token against every agent, so the time taken does not tell
// which one it is.
func (a *access) find(token string) *agent {
	sum := sha256.Sum256([]byte(token))
	var found *agent
	for _, ag := range a.agents() {
		if subtle.ConstantTimeCompare(sum[:], ag.sum) == 1 {
			found = ag
		}
	}
	return found
}

// byID is the agent as the current snapshot has it; nil once it is gone.
func (a *access) byID(id string) *agent {
	for _, ag := range a.agents() {
		if ag.ID == id {
			return ag
		}
	}
	return nil
}

func (a *access) run(ctx context.Context) {
	t := time.NewTicker(syncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.sync(ctx); err != nil {
			slog.Warn("access sync", "err", err)
		}
	}
}

// sync takes the console's key log and access records. A failed write of
// revocations is retried on every sync until it succeeds.
func (a *access) sync(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.syncLocked(ctx)
}

// refreshGap spaces the syncs refresh forces, so agents can't drive the
// console with them.
const refreshGap = 2 * time.Second

// refresh makes sure a sync that started after since has succeeded, so a
// grant the console reported by then is served. A caller sooner than
// refreshGap after the last forced sync waits for the gap.
func (a *access) refresh(ctx context.Context, since time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.synced.After(since) {
		return nil
	}
	if wait := time.Until(a.forced.Add(refreshGap)); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	a.forced = time.Now()
	return a.syncLocked(ctx)
}

func (a *access) syncLocked(ctx context.Context) error {
	start := time.Now()
	err := a.update(ctx)
	if err == nil {
		a.synced = start
	}
	if a.dirty {
		if serr := a.save(); serr != nil {
			return errors.Join(err, fmt.Errorf("save revocations: %w", serr))
		}
		a.dirty = false
	}
	return err
}

// update extends the key log, re-verifies the records held when it changed,
// and then replaces them with the console's. An unreachable console leaves
// the last snapshot in place. A key log that no longer extends the one held
// here is refused, yet records missing from the console still count as
// revoked, since that only takes access away.
func (a *access) update(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	log, err := a.console.Keys(ctx)
	if err != nil {
		return fmt.Errorf("fetch key log: %w", err)
	}
	keys := a.verifier.Keys
	n, head := keys.Head()
	keyErr := a.extend(ctx, log)
	if m, h := keys.Head(); m != n || h != head {
		var kept []record
		for _, r := range a.records {
			if v, err := a.verify(r.signed); err != nil {
				slog.Warn("access record no longer verifies", "id", r.id, "err", err)
				a.revoke(r.id)
			} else {
				kept = append(kept, v)
			}
		}
		a.publish(kept)
	}
	// Nothing verifies without a trusted key; revoking everything seen
	// because a connector was down at bootstrap would be permanent.
	if n, _ := keys.Head(); n == 0 {
		return errors.Join(keyErr, errors.New("no trusted keys yet"))
	}

	signed, err := a.console.Access(ctx)
	if err != nil {
		return errors.Join(keyErr, fmt.Errorf("fetch access records: %w", err))
	}
	present := map[string]bool{}
	var records []record
	for _, s := range signed {
		r, err := a.verify(s)
		if err != nil {
			slog.Warn("access record refused", "kind", s.Kind, "err", err)
			continue
		}
		present[r.id] = true
		records = append(records, r)
	}
	for id := range a.seen {
		if !present[id] {
			a.revoke(id)
		}
	}
	for id := range present {
		if !a.seen[id] && !a.revoked[id] {
			a.seen[id], a.dirty = true, true
		}
	}
	a.publish(records)
	return keyErr
}

// extend takes the console's key log. A gateway without one first takes the
// log every connector has pinned, so a console that turned hostile cannot
// introduce its own key; the console's log must then extend it. Connectors
// that disagree or don't answer leave it without keys.
func (a *access) extend(ctx context.Context, log []approval.SignedChange) error {
	keys := a.verifier.Keys
	if n, _ := keys.Head(); n == 0 && len(a.peers) > 0 {
		n, head, err := a.pinned(ctx)
		if err != nil {
			return fmt.Errorf("key log bootstrap refused: %w", err)
		}
		if n > 0 {
			if len(log) < n || approval.ChangeHash(log[n-1].Change) != head {
				return errors.New("key log bootstrap refused: the console's log does not extend the connectors'")
			}
			if err := keys.Extend(log[:n]); err != nil {
				return fmt.Errorf("key log bootstrap refused: %w", err)
			}
		}
	}
	if err := keys.Extend(log); err != nil {
		return fmt.Errorf("key log refused: %w", err)
	}
	return nil
}

// pinned is the key log length and head every connector holds.
func (a *access) pinned(ctx context.Context) (int, string, error) {
	var first connector.KeysHead
	var firstName string
	for _, name := range slices.Sorted(maps.Keys(a.peers)) {
		h, err := a.peers[name].Keys(ctx)
		if err != nil {
			return 0, "", fmt.Errorf("key log of connector %s: %w", name, err)
		}
		if firstName == "" {
			first, firstName = h, name
		} else if h != first {
			return 0, "", fmt.Errorf("connectors %s and %s hold different key logs", firstName, name)
		}
	}
	return first.Length, first.Head, nil
}

func (a *access) verify(s approval.Signed) (record, error) {
	r := record{signed: s}
	switch s.Kind {
	case approval.KindAgent:
		ag, err := a.verifier.VerifyAgent(s)
		if err != nil {
			return r, err
		}
		sum, err := hex.DecodeString(ag.TokenSHA256)
		if err != nil || len(sum) != sha256.Size {
			return r, fmt.Errorf("agent %s: token_sha256 is not a hex SHA-256", ag.ID)
		}
		r.id, r.agent = "agent:"+ag.ID, &agent{Agent: ag, sum: sum}
	case approval.KindGrant:
		g, err := a.verifier.VerifyGrant(s)
		if err != nil {
			return r, err
		}
		r.id, r.grant = "grant:"+g.ID, g
	default:
		return r, fmt.Errorf("unknown kind %q", s.Kind)
	}
	return r, nil
}

func (a *access) revoke(id string) {
	if a.revoked[id] {
		return
	}
	slog.Info("access revoked", "id", id)
	delete(a.seen, id)
	a.revoked[id], a.dirty = true, true
}

// publish keeps records, less the revoked ones, and serves them.
func (a *access) publish(records []record) {
	a.records = slices.DeleteFunc(records, func(r record) bool { return a.revoked[r.id] })
	agents := map[string]*agent{}
	for _, r := range a.records {
		if r.agent != nil {
			ag := *r.agent
			agents[ag.ID] = &ag
		}
	}
	for _, r := range a.records {
		if r.agent == nil {
			if ag := agents[r.grant.Agent]; ag != nil {
				ag.grants = append(ag.grants, r.grant)
			}
		}
	}
	list := slices.SortedFunc(maps.Values(agents), func(x, y *agent) int { return cmp.Compare(x.ID, y.ID) })
	a.cur.Store(&list)
}

func (a *access) save() error {
	b, err := json.Marshal(knownIDs{Seen: slices.Sorted(maps.Keys(a.seen)), Revoked: slices.Sorted(maps.Keys(a.revoked))})
	if err != nil {
		return err
	}
	return approval.WriteAtomic(a.path, b)
}

// window is an agent's access to a connector now: whether it is open and
// until when, or else when it next opens.
type window struct {
	Open  bool      `json:"open"`
	Until time.Time `json:"until,omitzero"`
	Next  time.Time `json:"next,omitzero"`
}

func (w *window) add(g approval.Grant, now time.Time) {
	open, at := g.Open(now)
	switch {
	case open:
		w.Open = true
		if at.After(w.Until) {
			w.Until = at
		}
	case !at.IsZero() && (w.Next.IsZero() || at.Before(w.Next)):
		w.Next = at
	}
}

func (ag *agent) window(connector string, now time.Time) window {
	var w window
	for _, g := range ag.grants {
		if g.Connector == connector {
			w.add(g, now)
		}
	}
	if w.Open {
		w.Next = time.Time{}
	}
	return w
}

// scope is the SQL filter's ?3: one rule per grant. A rule without chats
// covers every chat; cutoff is the earliest sent_at it shows, "" for all,
// and until the latest, "" for no bound.
type rule struct {
	Chats  []string `json:"chats,omitempty"`
	Cutoff string   `json:"cutoff"`
	Until  string   `json:"until,omitempty"`
}

// everything shows every chat the hide rules leave visible, for finding the
// chats an agent asks for.
const everything = `[{"cutoff":""}]`

// scope shows what grants show at now. A grant whose window ended, at
// ends[i], shows what it showed then; a zero end is a grant open now.
func scope(grants []approval.Grant, ends []time.Time, now time.Time) (string, error) {
	rules := make([]rule, len(grants))
	for i, g := range grants {
		rules[i].Chats = g.Chats
		at := now
		if end := ends[i]; !end.IsZero() {
			at, rules[i].Until = end, end.UTC().Format(sentAtFormat)
		}
		if c := g.Cutoff(at); !c.IsZero() {
			rules[i].Cutoff = c.UTC().Format(sentAtFormat)
		}
	}
	b, err := json.Marshal(rules)
	return string(b), err
}

// sentAtFormat is how store writes times, so they compare as text.
const sentAtFormat = "2006-01-02T15:04:05.000Z"

type agentKey struct{}

func withAgent(ctx context.Context, ag *agent) context.Context {
	return context.WithValue(ctx, agentKey{}, ag)
}

func agentOf(ctx context.Context) *agent {
	ag, _ := ctx.Value(agentKey{}).(*agent)
	return ag
}

// logFor names the request's agent in every line.
func logFor(ctx context.Context) *slog.Logger {
	if ag := agentOf(ctx); ag != nil {
		return slog.With("agent", ag.Name)
	}
	return slog.Default()
}

const readQueue = 256

// reportReads sends read events to the console one at a time; report drops
// what does not fit, so a slow console never holds up reads.
func (h *handler) reportReads(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-h.reads:
			if err := h.console.Read(ctx, ev); err != nil {
				slog.Warn("report read", "agent_id", ev.AgentID, "connector", ev.Connector, "err", err)
				h.dropMu.Lock()
				h.dropped[ev.AgentID] += 1 + ev.Dropped
				h.dropMu.Unlock()
			}
		}
	}
}

func (h *handler) report(ctx context.Context, name, verb, chat, chatName string, grants []approval.Grant, count int) {
	ag := agentOf(ctx)
	ids := make([]string, len(grants))
	for i, g := range grants {
		ids[i] = g.ID
	}
	ev := proposal.ReadEvent{
		AgentID: ag.ID, Connector: name, Verb: verb, Chat: chat, ChatName: chatName, Grants: ids, Count: count, At: time.Now(),
	}
	// What was dropped rides on the agent's next event, so a flood of cheap
	// reads cannot hide the others without a trace.
	h.dropMu.Lock()
	defer h.dropMu.Unlock()
	ev.Dropped = h.dropped[ag.ID]
	select {
	case h.reads <- ev:
		delete(h.dropped, ag.ID)
	default:
		h.dropped[ag.ID]++
		logFor(ctx).Warn("read log full; dropped", "connector", name, "verb", verb)
	}
}

const seenEvery = time.Minute

// markSeen tells the console the agent is using the gateway, at most once
// per seenEvery, without holding up the request.
func (h *handler) markSeen(id string) {
	now := time.Now()
	h.seenMu.Lock()
	due := now.Sub(h.seen[id]) >= seenEvery
	if due {
		h.seen[id] = now
	}
	h.seenMu.Unlock()
	if !due {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		if err := h.console.Seen(ctx, proposal.Seen{AgentID: id, At: now}); err != nil {
			slog.Warn("report seen", "agent_id", id, "err", err)
		}
	}()
}
