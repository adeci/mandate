package gateway

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

// Limits of an access request; the console enforces them too.
const (
	defaultFor     = 30 * time.Minute
	maxFor         = 12 * time.Hour
	defaultHorizon = 7 * 24 * time.Hour
	maxTerms       = 10
	maxTerm        = 200
	matchesPerTerm = 5
	maxReason      = 1000
)

// readScope is what the request's agent may read of a connector now.
type readScope struct {
	grants []approval.Grant
	sql    string
}

type noAccess struct {
	Error   string `json:"error"`
	Request string `json:"request"`
}

// readable answers the agent's open grants on name in the current snapshot,
// or refuses with how to ask for one.
func (h *handler) readable(w http.ResponseWriter, r *http.Request, name string) (readScope, bool) {
	return h.scoped(w, r, name, func(approval.Grant) bool { return true }, "")
}

// forwardable is readable for raw reads, which neither chats nor a horizon
// can limit, so only a grant on every chat with whole history opens them.
func (h *handler) forwardable(w http.ResponseWriter, r *http.Request, name string) (readScope, bool) {
	return h.scoped(w, r, name, func(g approval.Grant) bool { return len(g.Chats) == 0 && g.Horizon == 0 },
		"; raw reads need a grant on every chat with whole history")
}

func (h *handler) scoped(w http.ResponseWriter, r *http.Request, name string, usable func(approval.Grant) bool, why string) (readScope, bool) {
	now := time.Now()
	ag := h.access.byID(agentOf(r.Context()).ID)
	var grants []approval.Grant
	if ag != nil {
		grants = slices.DeleteFunc(ag.open(name, now), func(g approval.Grant) bool { return !usable(g) })
	}
	if len(grants) == 0 {
		cmd := fmt.Sprintf("mandate request %s --reason WHY", name)
		msg := fmt.Sprintf("no read access to %s now%s", name, why)
		if ag != nil {
			if next := ag.window(name, now).Next; !next.IsZero() {
				msg += "; a grant opens " + next.UTC().Format("2006-01-02 15:04 UTC")
			}
		}
		msg += "; if the task needs it, ask the owner with: " + cmd
		logFor(r.Context()).Info("no access", "remote", r.RemoteAddr, "connector", name, "path", r.URL.Path)
		connector.WriteJSON(w, http.StatusForbidden, noAccess{Error: msg, Request: cmd})
		return readScope{}, false
	}
	sql, err := scope(grants, now)
	if err != nil {
		logFor(r.Context()).Error("scope", "connector", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable; tell the owner")
		return readScope{}, false
	}
	return readScope{grants: grants, sql: sql}, true
}

type chatRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type grantInfo struct {
	ID        string             `json:"id"`
	Connector string             `json:"connector"`
	Chats     []chatRef          `json:"chats,omitempty"`
	From      time.Time          `json:"from,omitzero"`
	Until     time.Time          `json:"until,omitzero"`
	Schedule  *approval.Schedule `json:"schedule,omitempty"`
	Horizon   time.Duration      `json:"horizon"`
	Note      string             `json:"note,omitempty"`
	Now       window             `json:"now"`
}

// grants lists the agent's grants that are open or will open again, naming
// their chats as the agent would see them; a chat since hidden keeps only its
// ID.
func (h *handler) grants(w http.ResponseWriter, r *http.Request) {
	ag, now := agentOf(r.Context()), time.Now()
	out := []grantInfo{}
	chats := map[string][]string{}
	for _, g := range ag.grants {
		var win window
		win.add(g, now)
		if !win.Open && win.Next.IsZero() {
			continue
		}
		info := grantInfo{
			ID: g.ID, Connector: g.Connector, From: g.From, Until: g.Until, Schedule: g.Schedule,
			Horizon: g.Horizon, Note: g.Note, Now: win,
		}
		for _, id := range g.Chats {
			info.Chats = append(info.Chats, chatRef{ID: id})
		}
		chats[g.Connector] = append(chats[g.Connector], g.Chats...)
		out = append(out, info)
	}
	names := map[string]map[string]string{}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	for name, ids := range chats {
		if _, ok := h.cfg.Sources[name]; !ok || len(ids) == 0 {
			continue
		}
		s, ok := h.source(ctx, w, name, everything)
		if !ok {
			return
		}
		m, err := s.names(ids)
		s.close()
		if err != nil {
			logFor(ctx).Error("chat names", "source", name, "err", err)
			writeError(w, http.StatusServiceUnavailable, "gateway unavailable; tell the owner")
			return
		}
		names[name] = m
	}
	for i := range out {
		for j, c := range out[i].Chats {
			out[i].Chats[j].Name = names[out[i].Connector][c.ID]
		}
	}
	slices.SortFunc(out, func(a, b grantInfo) int {
		return cmp.Or(cmp.Compare(a.Connector, b.Connector), a.Now.next().Compare(b.Now.next()), cmp.Compare(a.ID, b.ID))
	})
	connector.WriteJSON(w, http.StatusOK, out)
}

// next is when the window changes: it closes if open, else opens.
func (w window) next() time.Time {
	if w.Open {
		return w.Until
	}
	return w.Next
}

// accessRequest is what an agent asks for. Chats are search terms; Horizon
// nil asks for the default.
type accessRequest struct {
	Connector string         `json:"connector"`
	Chats     []string       `json:"chats,omitempty"`
	For       time.Duration  `json:"for,omitempty"`
	Horizon   *time.Duration `json:"horizon,omitempty"`
	Reason    string         `json:"reason"`
}

// requestAccess resolves the request's chat terms and passes it to the
// console, terms that matched nothing included. The agent gets the same
// answer whatever matched, so a request is no probe of the chats.
func (h *handler) requestAccess(w http.ResponseWriter, r *http.Request) {
	var req accessRequest
	if !connector.DecodeJSON(w, r, &req, maxRequestBytes) {
		return
	}
	name := req.Connector
	logFor(r.Context()).Info("access request", "remote", r.RemoteAddr, "connector", name, "terms", len(req.Chats))
	_, source := h.cfg.Sources[name]
	if _, ok := h.connectors.calls[name]; !ok && !source {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no connector %q; see mandate --help", name))
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	horizon := defaultHorizon
	if req.Horizon != nil {
		horizon = *req.Horizon
	}
	switch {
	case req.Reason == "":
		writeError(w, http.StatusBadRequest, "--reason is required: say what the task is and why it needs these chats")
		return
	case len([]rune(req.Reason)) > maxReason:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("--reason is over %d characters", maxReason))
		return
	case req.For < 0 || req.For > maxFor:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("--for must be positive and at most %s", maxFor))
		return
	case horizon < 0:
		writeError(w, http.StatusBadRequest, "--since must not be negative")
		return
	}
	terms, err := cleanTerms(req.Chats)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Raw reads are limited by neither chats nor a horizon; the console signs
	// such grants without either, as it too goes by Spec.Forward.
	forward := false
	if _, ok := h.connectors.calls[name]; ok {
		spec, ok := h.spec(w, r, name)
		if !ok {
			return
		}
		forward = spec.Forward
	}
	switch {
	case len(terms) > 0 && forward:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s serves raw reads, which no chat limits; ask without --chat", name))
		return
	case len(terms) > 0 && !source:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s keeps no chats; ask without --chat", name))
		return
	}
	if forward || !source {
		horizon = 0
	}
	// Resolving costs time that depends on what matches, so it runs only for
	// a request the console will take.
	ag := agentOf(r.Context())
	if err := h.console.AccessQuota(r.Context(), ag.ID); err != nil {
		relay(w, r, "console", err)
		return
	}
	chats, unmatched, ok := h.resolve(w, r, name, terms)
	if !ok {
		return
	}
	ref, err := h.console.RequestAccess(r.Context(), proposal.AccessRequest{
		AgentID: ag.ID, AgentName: ag.Name, Connector: name, Terms: terms, Chats: chats, Unmatched: unmatched,
		For: cmp.Or(req.For, defaultFor), Horizon: horizon, Reason: req.Reason,
	})
	if err != nil {
		relay(w, r, "console", err)
		return
	}
	logFor(r.Context()).Info("access requested", "connector", name, "id", ref.ID, "chats", len(chats), "unmatched", len(unmatched))
	connector.WriteJSON(w, http.StatusAccepted, ref)
}

// cleanTerms trims and deduplicates chat terms, ignoring case as matching
// does, and checks them before anything is looked up.
func cleanTerms(raw []string) ([]string, error) {
	var terms []string
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if n := utf8.RuneCountInString(t); n == 0 || n > maxTerm {
			return nil, fmt.Errorf("--chat must be 1 to %d characters", maxTerm)
		}
		if !slices.ContainsFunc(terms, func(x string) bool { return strings.EqualFold(x, t) }) {
			terms = append(terms, t)
		}
	}
	if len(terms) > maxTerms {
		return nil, fmt.Errorf("at most %d --chat terms", maxTerms)
	}
	return terms, nil
}

// resolve finds the visible chats each term names, the most recently active
// first, and the terms that name none. Grants do not limit the search: the
// agent asks for chats it may not read yet.
func (h *handler) resolve(w http.ResponseWriter, r *http.Request, name string, terms []string) (chats []proposal.Chat, unmatched []string, ok bool) {
	if len(terms) == 0 {
		return nil, nil, true
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	s, ok := h.source(ctx, w, name, everything)
	if !ok {
		return nil, nil, false
	}
	defer s.close()
	rows, err := s.matches(terms, matchesPerTerm)
	if err != nil {
		s.log.Error("resolve chats", "source", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable; tell the owner")
		return nil, nil, false
	}
	matched := make([]bool, len(terms))
	for _, row := range rows {
		k, _ := row[0].(int64)
		matched[k] = true
		c := proposal.Chat{}
		c.ID, _ = row[1].(string)
		c.Name, _ = row[2].(string)
		if !slices.ContainsFunc(chats, func(x proposal.Chat) bool { return x.ID == c.ID }) {
			chats = append(chats, c)
		}
	}
	for k, term := range terms {
		if !matched[k] {
			unmatched = append(unmatched, term)
		}
	}
	return chats, unmatched, true
}

// requestStatus answers the state of one of the agent's own requests, after
// waiting up to wait=SECONDS for the owner to decide.
func (h *handler) requestStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !proposalID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid request id")
		return
	}
	wait, ok := waitParam(w, r)
	if !ok {
		return
	}
	st, err := h.console.AccessStatus(r.Context(), id, wait)
	if err != nil {
		relay(w, r, "console", err)
		return
	}
	if st.AgentID != agentOf(r.Context()).ID {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no access request %q of yours", id))
		return
	}
	connector.WriteJSON(w, http.StatusOK, st)
}
