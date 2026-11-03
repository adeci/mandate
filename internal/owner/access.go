package owner

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/adeci/mandate/record"
)

func rtime(t time.Time) record.Time { return record.Time{Time: t.UTC().Truncate(time.Second)} }

type newRequest struct {
	Identity  string    `json:"identity"`
	Connector string    `json:"connector"`
	Kind      string    `json:"kind"`
	Terms     []term    `json:"terms"`
	For       int       `json:"for"`
	Horizon   int       `json:"horizon"`
	Schedule  *schedule `json:"schedule"`
	Reason    string    `json:"reason"`
}

func (s *Server) postRequest(w http.ResponseWriter, r *http.Request) {
	var nr newRequest
	if !decode(w, r, &nr) {
		return
	}
	switch {
	case nr.Kind != "once" && nr.Kind != "routine" && nr.Kind != "extend":
		fail(w, http.StatusBadRequest, "kind is once, routine or extend")
		return
	case (nr.Kind == "routine") != (nr.Schedule != nil):
		fail(w, http.StatusBadRequest, "a routine, and only a routine, has a schedule")
		return
	case nr.Kind != "routine" && nr.For < 1:
		fail(w, http.StatusBadRequest, "for must be positive")
		return
	case nr.Horizon < 0:
		fail(w, http.StatusBadRequest, "horizon must not be negative")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Identities[nr.Identity] == nil || s.conns[nr.Connector] == nil {
		fail(w, http.StatusNotFound, "unknown identity or connector")
		return
	}
	now := s.now()
	// A waiting request expires within the hour, so either limit lifts by
	// the time the oldest request it counts is an hour old.
	var waiting, recent []time.Time
	for _, req := range s.st.Requests {
		if req.Identity != nr.Identity {
			continue
		}
		if req.Status == "pending" {
			waiting = append(waiting, req.CreatedAt)
		}
		if now.Sub(req.CreatedAt) < time.Hour {
			recent = append(recent, req.CreatedAt)
		}
	}
	limited := waiting
	if len(waiting) < requestsWaiting {
		limited = nil
		if len(recent) >= requestsPerHour {
			limited = recent
		}
	}
	if limited != nil {
		lifts := slices.MinFunc(limited, time.Time.Compare).Add(time.Hour)
		reply(w, http.StatusTooManyRequests, apiError{
			Error: "at most 3 waiting requests and 10 an hour; wait for the owner",
			Code:  codes[http.StatusTooManyRequests], RetryAfter: int(lifts.Sub(now).Seconds()) + 1,
		})
		return
	}
	req := &request{
		ID: record.NewID(), Identity: nr.Identity, Connector: nr.Connector, Kind: nr.Kind,
		Terms: nr.Terms, For: nr.For, Horizon: nr.Horizon, Schedule: nr.Schedule, Reason: nr.Reason,
		CreatedAt: now, Status: "pending",
	}
	s.st.Requests[req.ID] = req
	s.seen(nr.Identity, now)
	if err := s.commit("now"); err != nil {
		failErr(w, err)
		return
	}
	s.publish("waiting", map[string]string{"kind": "request", "id": req.ID, "identity": req.Identity, "connector": req.Connector})
	reply(w, http.StatusCreated, map[string]string{"id": req.ID})
}

// The caller holds s.mu and commits.
func (s *Server) seen(id string, at time.Time) {
	if i := s.st.Identities[id]; i != nil {
		if i.FirstSeen.IsZero() {
			i.FirstSeen = at
		}
		if at.After(i.LastSeen) {
			i.LastSeen = at
		}
	}
}

func (s *Server) requestStatus(w http.ResponseWriter, r *http.Request) {
	wait, ok := waitParam(r)
	if !ok {
		fail(w, http.StatusBadRequest, "wait is a number of seconds")
		return
	}
	id, ident := r.PathValue("id"), r.URL.Query().Get("identity")
	var view map[string]any
	s.await(r.Context(), wait, func() bool {
		req := s.st.Requests[id]
		if req == nil || req.Identity != ident {
			view = nil
			return true
		}
		view = map[string]any{"status": req.Status}
		if req.Status == "granted" {
			view["grant_id"] = req.GrantID
			if !req.Until.IsZero() {
				view["until"] = rtime(req.Until)
			}
		}
		return req.Status != "pending"
	})
	if view == nil {
		fail(w, http.StatusNotFound, "no such request")
		return
	}
	reply(w, http.StatusOK, view)
}

// The caller holds s.mu.
func (s *Server) waiting(req *request) bool {
	return s.st.Requests[req.ID] == req && req.Status == "pending" && req.GrantID == "" &&
		s.now().Before(req.CreatedAt.Add(requestTTL)) && s.st.Identities[req.Identity] != nil
}

type narrowing struct {
	Chats   []string `json:"chats"`
	For     int      `json:"for"`
	Horizon *int     `json:"horizon"`
	Zone    string   `json:"zone"`
}

func (s *Server) requestChallenge(w http.ResponseWriter, r *http.Request) {
	var n narrowing
	if !decode(w, r, &n) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req := s.st.Requests[r.PathValue("id")]
	switch {
	case req == nil:
		fail(w, http.StatusNotFound, "no such request")
		return
	case req.Status == "expired" || req.Status == "pending" && !s.now().Before(req.CreatedAt.Add(requestTTL)):
		fail(w, http.StatusGone, "the request expired")
		return
	case !s.waiting(req):
		fail(w, http.StatusConflict, "the request was decided meanwhile")
		return
	}
	g, names, old, err := s.grantFor(req, n)
	if err != nil {
		failErr(w, err)
		return
	}
	c := &challenge{identity: req.Identity}
	if old != nil {
		c.grant = old.Record.ID
	}
	c.holds = func() bool {
		return s.waiting(req) && (old == nil || s.st.Grants[old.Record.ID] == old && old.live(s.now()))
	}
	c.apply = func(env record.Envelope, _ record.Record) error {
		if old != nil {
			delete(s.st.Grants, old.Record.ID)
		}
		s.st.Grants[g.ID] = &grant{Envelope: env, Record: g, ChatNames: names}
		// The request says granted once the gateway has the grant.
		req.GrantID, req.Until = g.ID, g.Until.Time
		return s.commit("now")
	}
	s.issue(w, g, c)
}

// grantFor is the grant answering req, narrowed by n, with the names of its
// chats and, for extend, the grant it replaces. The caller holds s.mu.
func (s *Server) grantFor(req *request, n narrowing) (record.Grant, map[string]string, *grant, error) {
	now := s.now()
	// offered nil means every chat.
	var offered []string
	names := map[string]string{}
	horizon, length := req.Horizon, req.For
	var old *grant
	if req.Kind == "extend" {
		if old = s.extendable(req.Identity, req.Connector); old == nil {
			return record.Grant{}, nil, nil, errorf(http.StatusConflict, "no window to extend")
		}
		offered, horizon, names = old.Record.Chats, old.Record.Horizon, old.ChatNames
	} else if len(req.Terms) > 0 {
		offered = []string{}
		for _, t := range req.Terms {
			for _, c := range t.Chats {
				if _, ok := names[c.ID]; !ok {
					offered = append(offered, c.ID)
				}
				names[c.ID] = c.Name
			}
		}
	}
	chats := offered
	if n.Chats != nil {
		for _, c := range n.Chats {
			if offered != nil && !slices.Contains(offered, c) || c == "" {
				return record.Grant{}, nil, nil, errorf(http.StatusBadRequest, "chat %q was not asked for", c)
			}
		}
		chats = n.Chats
	}
	if chats != nil && len(chats) == 0 {
		return record.Grant{}, nil, nil, errorf(http.StatusBadRequest, "no chat to grant")
	}
	if n.Horizon != nil {
		if *n.Horizon < 0 || horizon != 0 && (*n.Horizon == 0 || *n.Horizon > horizon) {
			return record.Grant{}, nil, nil, errorf(http.StatusBadRequest, "the horizon widens the request")
		}
		horizon = *n.Horizon
	}
	if n.For != 0 {
		if n.For < 0 || n.For > length {
			return record.Grant{}, nil, nil, errorf(http.StatusBadRequest, "for widens the request")
		}
		length = n.For
	}
	g := record.Grant{
		ID: record.NewID(), Identity: req.Identity, Connector: req.Connector,
		Chats: chats, Horizon: horizon, CreatedAt: rtime(now),
	}
	switch req.Kind {
	case "once":
		g.From, g.Until = rtime(now), rtime(now.Add(time.Duration(length)*time.Second))
	case "extend":
		g.From, g.Until, g.Note = rtime(now), rtime(extended(old, now, length)), old.Record.Note
	case "routine":
		if n.Zone == "" {
			return record.Grant{}, nil, nil, errorf(http.StatusBadRequest, "a routine needs the device's zone")
		}
		g.Schedule = &record.Schedule{Days: req.Schedule.Days, Start: req.Schedule.Start, Length: req.Schedule.Length, Zone: n.Zone}
	}
	kept := map[string]string{}
	for _, c := range chats {
		if names[c] != "" {
			kept[c] = names[c]
		}
	}
	return g, kept, old, nil
}

// The caller holds s.mu.
func (s *Server) extendable(identity, conn string) *grant {
	var best *grant
	for _, g := range s.st.Grants {
		if g.Record.Identity == identity && g.Record.Connector == conn && g.Record.Schedule == nil &&
			g.live(s.now()) && (best == nil || g.Record.Until.After(best.Record.Until.Time)) {
			best = g
		}
	}
	return best
}

func (s *Server) denyRequest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := s.st.Requests[r.PathValue("id")]
	switch {
	case req == nil:
		fail(w, http.StatusNotFound, "no such request")
	case !s.waiting(req):
		fail(w, http.StatusConflict, "the request was decided or expired meanwhile")
	default:
		req.Status, req.DecidedAt = "denied", s.now()
		if err := s.commit("now"); err != nil {
			failErr(w, err)
			return
		}
		reply(w, http.StatusOK, struct{}{})
	}
}

type newGrant struct {
	Identity  string           `json:"identity"`
	Connector string           `json:"connector"`
	Chats     []string         `json:"chats"`
	For       int              `json:"for"`
	Schedule  *record.Schedule `json:"schedule"`
	Horizon   int              `json:"horizon"`
	Note      string           `json:"note"`
}

func (s *Server) grantChallenge(w http.ResponseWriter, r *http.Request) {
	var ng newGrant
	if !decode(w, r, &ng) {
		return
	}
	if (ng.For > 0) == (ng.Schedule != nil) {
		fail(w, http.StatusBadRequest, "give either for or schedule")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Identities[ng.Identity] == nil || s.conns[ng.Connector] == nil {
		fail(w, http.StatusNotFound, "unknown identity or connector")
		return
	}
	now := s.now()
	g := record.Grant{
		ID: record.NewID(), Identity: ng.Identity, Connector: ng.Connector, Chats: ng.Chats,
		Horizon: ng.Horizon, Schedule: ng.Schedule, Note: ng.Note, CreatedAt: rtime(now),
	}
	if ng.Schedule == nil {
		g.From, g.Until = rtime(now), rtime(now.Add(time.Duration(ng.For)*time.Second))
	}
	s.issue(w, g, &challenge{
		identity: ng.Identity,
		holds:    func() bool { return s.st.Identities[ng.Identity] != nil },
		apply: func(env record.Envelope, _ record.Record) error {
			s.st.Grants[g.ID] = &grant{Envelope: env, Record: g}
			return s.commit("now")
		},
	})
}

func (s *Server) extendChallenge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		For int `json:"for"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.For < 1 {
		fail(w, http.StatusBadRequest, "for must be positive")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.st.Grants[r.PathValue("id")]
	now := s.now()
	switch {
	case old == nil:
		fail(w, http.StatusNotFound, "no such grant")
		return
	case old.Record.Schedule != nil || !old.live(now):
		fail(w, http.StatusConflict, "only a live window can be extended")
		return
	}
	g := old.Record
	g.ID, g.CreatedAt = record.NewID(), rtime(now)
	g.From, g.Until = rtime(now), rtime(extended(old, now, body.For))
	s.replaceGrant(w, old, g, func() bool { return old.live(s.now()) })
}

func (s *Server) resumeChallenge(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.st.Grants[r.PathValue("id")]
	switch {
	case old == nil:
		fail(w, http.StatusNotFound, "no such grant")
		return
	case old.Record.Schedule == nil || old.ClosedAt.IsZero():
		fail(w, http.StatusConflict, "only a paused routine can be resumed")
		return
	}
	g := old.Record
	g.ID, g.CreatedAt = record.NewID(), rtime(s.now())
	s.replaceGrant(w, old, g, func() bool { return !old.ClosedAt.IsZero() })
}

// The caller holds s.mu.
func (s *Server) replaceGrant(w http.ResponseWriter, old *grant, g record.Grant, holds func() bool) {
	s.issue(w, g, &challenge{
		identity: g.Identity,
		grant:    old.Record.ID,
		holds: func() bool {
			return s.st.Grants[old.Record.ID] == old && s.st.Identities[g.Identity] != nil && holds()
		},
		apply: func(env record.Envelope, _ record.Record) error {
			delete(s.st.Grants, old.Record.ID)
			s.st.Grants[g.ID] = &grant{Envelope: env, Record: g, ChatNames: old.ChatNames}
			return s.commit("now")
		},
	})
}

func (s *Server) closeGrant(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	g := s.st.Grants[id]
	switch {
	case g == nil:
		fail(w, http.StatusNotFound, "no such grant")
		return
	case !g.ClosedAt.IsZero():
		fail(w, http.StatusConflict, "the grant is closed already")
		return
	}
	g.ClosedAt = s.now()
	s.cancel(func(c *challenge) bool { return c.grant == id })
	if err := s.commit("now", "identities"); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct{}{})
}

func (s *Server) stopAll(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var grants, requests int
	for _, g := range s.st.Grants {
		if g.ClosedAt.IsZero() {
			g.ClosedAt = now
			grants++
		}
	}
	for _, req := range s.st.Requests {
		if req.Status == "pending" {
			req.Status, req.DecidedAt = "denied", now
			requests++
		}
	}
	s.cancel(func(*challenge) bool { return true })
	if err := s.commit("now", "identities"); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]int{"grants": grants, "requests": requests})
}

type grantView struct {
	record.Grant
	IdentityName string            `json:"identity_name"`
	ChatNames    map[string]string `json:"chat_names,omitempty"`
	ClosedAt     time.Time         `json:"closed_at,omitzero"`
}

func (s *Server) grantView(g *grant) grantView {
	name := ""
	if i := s.st.Identities[g.Record.Identity]; i != nil {
		name = i.Name
	}
	return grantView{g.Record, name, g.ChatNames, g.ClosedAt}
}

type requestView struct {
	*request
	IdentityName string `json:"identity_name"`
}

func (s *Server) getNow(w http.ResponseWriter, r *http.Request) {
	day, before := s.now().UTC(), time.Time{}
	if d := r.URL.Query().Get("day"); d != "" {
		t, err := time.Parse(time.DateOnly, d)
		if err != nil {
			fail(w, http.StatusBadRequest, "day is YYYY-MM-DD")
			return
		}
		day = t
	}
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			fail(w, http.StatusBadRequest, "before is an RFC 3339 time")
			return
		}
		before = t
	}
	acts, more, err := readActivity(s.dir, day, before)
	if err != nil {
		s.log.Error("read activity", "err", err)
		failErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := struct {
		Requests  []requestView  `json:"requests"`
		Proposals []proposalView `json:"proposals"`
		Grants    []grantView    `json:"grants"`
		Routines  []grantView    `json:"routines"`
		Stopped   []grantView    `json:"stopped"`
		Activity  []activity     `json:"activity"`
		More      bool           `json:"more"`
	}{
		Requests: []requestView{}, Proposals: []proposalView{},
		Grants: []grantView{}, Routines: []grantView{}, Stopped: []grantView{},
		Activity: acts, More: more,
	}
	for _, req := range s.st.Requests {
		if s.waiting(req) {
			out.Requests = append(out.Requests, requestView{req, s.st.Identities[req.Identity].Name})
		}
	}
	for _, p := range s.st.Proposals {
		if p.Status == "pending" && now.Before(p.ExpiresAt) {
			out.Proposals = append(out.Proposals, s.proposalView(p))
		}
	}
	for _, g := range s.st.Grants {
		switch {
		case g.Record.Schedule == nil && g.open(now):
			out.Grants = append(out.Grants, s.grantView(g))
		case g.Record.Schedule != nil && g.ClosedAt.IsZero():
			out.Routines = append(out.Routines, s.grantView(g))
		case g.Record.Schedule != nil:
			out.Stopped = append(out.Stopped, s.grantView(g))
		}
	}
	reply(w, http.StatusOK, out)
}

const activityPage = 500

// readActivity is the day's activity before before (if set), newest first.
func readActivity(dir string, day, before time.Time) ([]activity, bool, error) {
	f, err := os.Open(activityFile(dir, day))
	if errors.Is(err, fs.ErrNotExist) {
		return []activity{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var all []activity
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, maxBody)
	for sc.Scan() {
		var a activity
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			continue // a line torn by a crash
		}
		if before.IsZero() || a.At.Before(before) {
			all = append(all, a)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	slices.SortStableFunc(all, func(a, b activity) int { return b.At.Compare(a.At) })
	if len(all) > activityPage {
		return all[:activityPage], true, nil
	}
	if all == nil {
		all = []activity{}
	}
	return all, false, nil
}

func (s *Server) postActivity(w http.ResponseWriter, r *http.Request) {
	var entries []activity
	if !decode(w, r, &entries) {
		return
	}
	if err := appendActivity(s.dir, entries); err != nil {
		s.log.Error("append activity", "err", err)
		failErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range entries {
		s.seen(a.Identity, a.At)
	}
	if err := s.commit("now"); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct{}{})
}

func (s *Server) listIdentities(w http.ResponseWriter, _ *http.Request) {
	type identityView struct {
		ID        string      `json:"id"`
		Name      string      `json:"name"`
		CreatedAt record.Time `json:"created_at"`
		FirstSeen time.Time   `json:"first_seen,omitzero"`
		LastSeen  time.Time   `json:"last_seen,omitzero"`
		Grants    []grantView `json:"grants"`
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []identityView{}
	for _, i := range s.st.Identities {
		v := identityView{i.Record.ID, i.Name, i.Record.CreatedAt, i.FirstSeen, i.LastSeen, []grantView{}}
		for _, g := range s.st.Grants {
			if g.Record.Identity == i.Record.ID {
				v.Grants = append(v.Grants, s.grantView(g))
			}
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b identityView) int { return strings.Compare(a.Name, b.Name) })
	reply(w, http.StatusOK, out)
}

func (s *Server) renameIdentity(w http.ResponseWriter, r *http.Request) {
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
	i := s.st.Identities[r.PathValue("id")]
	if i == nil {
		fail(w, http.StatusNotFound, "no such identity")
		return
	}
	i.Name = name
	if err := s.commit("identities", "now"); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct{}{})
}

func (s *Server) deleteIdentity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Identities[id] == nil {
		fail(w, http.StatusNotFound, "no such identity")
		return
	}
	s.st.deleteIdentity(id, s.now())
	s.cancel(func(c *challenge) bool { return c.identity == id })
	if err := s.commit("identities", "now"); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct{}{})
}
