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

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/record"
)

type connectorInfo struct {
	Name   string           `json:"name"`
	Doc    string           `json:"doc"`
	Media  bool             `json:"media"`
	Ops    []connector.Op   `json:"ops"`
	Health connector.Health `json:"health"`
	Access access           `json:"access"`
}

func (g *gateway) listConnectors(w http.ResponseWriter, r *http.Request, cl *caller) {
	now := time.Now()
	out := []connectorInfo{}
	for _, c := range g.sortedConns() {
		info := connectorInfo{
			Name: c.name, Ops: []connector.Op{},
			Health: connector.Health{State: connector.Broken, Detail: "not answering"},
			Access: accessNow(cl.live.grantsOn(cl.identity, c.name), now),
		}
		if s, err := c.getSpec(r.Context()); err != nil {
			cl.log.Warn("connector spec", "connector", c.name, "err", err)
		} else if h, err := c.health(r.Context()); err != nil {
			cl.log.Warn("connector health", "connector", c.name, "err", err)
		} else {
			info.Doc, info.Media, info.Ops, info.Health = s.Doc, s.Media, s.Ops, h
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

type grantInfo struct {
	ID        string           `json:"id"`
	Connector string           `json:"connector"`
	Chats     []chatRef        `json:"chats,omitempty"`
	From      record.Time      `json:"from,omitzero"`
	Until     record.Time      `json:"until,omitzero"`
	Schedule  *record.Schedule `json:"schedule,omitempty"`
	Horizon   int              `json:"horizon"`
	Now       access           `json:"now"`
}

// listAccess lists the identity's grants that are open or will open again,
// naming their chats as the grants show them; a chat since hidden keeps
// only its ID.
func (g *gateway) listAccess(w http.ResponseWriter, r *http.Request, cl *caller) {
	now := time.Now()
	out := []grantInfo{}
	byConn := map[string][]record.Grant{}
	for _, gr := range cl.live.grants[cl.identity] {
		a := accessNow([]record.Grant{gr}, now)
		if !a.Open && a.Next.IsZero() {
			continue
		}
		out = append(out, grantInfo{
			ID: gr.ID, Connector: gr.Connector, From: gr.From, Until: gr.Until, Schedule: gr.Schedule,
			Horizon: gr.Horizon, Now: a,
		})
		byConn[gr.Connector] = append(byConn[gr.Connector], gr)
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	names := map[string][]chatRef{}
	for name, grants := range byConn {
		var ids []string
		var scope []rule
		for _, gr := range grants {
			ids = append(ids, gr.Chats...)
			scope = append(scope, rule{Chats: gr.Chats})
		}
		c, ok := g.conns[name]
		if !ok || len(ids) == 0 {
			continue
		}
		v, err := g.view(ctx, cl, c, scope)
		if err != nil {
			unavailable(w, cl, "open store", err)
			return
		}
		names[name] = g.chatNames(ctx, cl, v, ids)
	}
	for i := range out {
		gr := byConn[out[i].Connector]
		idx := slices.IndexFunc(gr, func(x record.Grant) bool { return x.ID == out[i].ID })
		for _, id := range gr[idx].Chats {
			ref := chatRef{ID: id}
			if j := slices.IndexFunc(names[out[i].Connector], func(c chatRef) bool { return c.ID == id }); j >= 0 {
				ref = names[out[i].Connector][j]
			}
			out[i].Chats = append(out[i].Chats, ref)
		}
	}
	slices.SortFunc(out, func(a, b grantInfo) int {
		return cmp.Or(cmp.Compare(a.Connector, b.Connector), a.Now.change().Compare(b.Now.change()), cmp.Compare(a.ID, b.ID))
	})
	writeJSON(w, http.StatusOK, out)
}

// change is when the access changes: it closes if open, else opens.
func (a access) change() time.Time {
	if a.Open {
		return a.Until
	}
	return a.Next
}

// Limits of an access request.
const (
	defaultFor     = 1800
	maxFor         = 43200
	defaultHorizon = 604800
	maxTerms       = 10
	maxTerm        = 200
	matchesPerTerm = 5
	maxReason      = 1000
)

type requestSchedule struct {
	Days   []int  `json:"days,omitempty"`
	Start  string `json:"start"`
	Length int    `json:"length"`
}

type accessRequest struct {
	Connector string           `json:"connector"`
	Chats     []string         `json:"chats"`
	For       *int             `json:"for"`
	Horizon   *int             `json:"horizon"`
	Schedule  *requestSchedule `json:"schedule"`
	Extend    bool             `json:"extend"`
	Reason    string           `json:"reason"`
}

type term struct {
	Term  string    `json:"term"`
	Chats []chatRef `json:"chats"`
}

// ownerRequest is an access request as the owner side takes it.
type ownerRequest struct {
	Identity  string           `json:"identity"`
	Connector string           `json:"connector"`
	Kind      string           `json:"kind"` // once, routine or extend
	Terms     []term           `json:"terms,omitempty"`
	For       int              `json:"for,omitempty"`
	Horizon   *int             `json:"horizon,omitempty"`
	Schedule  *requestSchedule `json:"schedule,omitempty"`
	Reason    string           `json:"reason"`
}

// check refuses what the owner side could not grant, and fills in the
// defaults.
func (req *accessRequest) check() (ownerRequest, error) {
	o := ownerRequest{Connector: req.Connector, Kind: "once", Reason: strings.TrimSpace(req.Reason)}
	switch n := utf8.RuneCountInString(o.Reason); {
	case n == 0:
		return o, fmt.Errorf("reason is required: say what the task is and why it needs this")
	case n > maxReason:
		return o, fmt.Errorf("reason is over %d characters", maxReason)
	}
	if req.Extend {
		if req.Chats != nil || req.Horizon != nil || req.Schedule != nil {
			return o, fmt.Errorf("extend keeps the chats and horizon of the window it extends; leave out chats, horizon and schedule")
		}
		o.Kind = "extend"
	} else {
		h := defaultHorizon
		if req.Horizon != nil {
			h = *req.Horizon
		}
		if h < 0 {
			return o, fmt.Errorf("horizon must not be negative")
		}
		o.Horizon = &h
		terms, err := cleanTerms(req.Chats)
		if err != nil {
			return o, err
		}
		for _, t := range terms {
			o.Terms = append(o.Terms, term{Term: t, Chats: []chatRef{}})
		}
	}
	if s := req.Schedule; s != nil {
		if req.For != nil {
			return o, fmt.Errorf("a routine's windows last its schedule's length; leave out for")
		}
		if t, err := time.Parse("15:04", s.Start); err != nil || t.Format("15:04") != s.Start {
			return o, fmt.Errorf("schedule start must be HH:MM, e.g. 07:00")
		}
		if s.Length < 1 || s.Length > 86400 {
			return o, fmt.Errorf("schedule length must be 1 to 86400 seconds")
		}
		for _, d := range s.Days {
			if d < 0 || d > 6 {
				return o, fmt.Errorf("schedule day %d is not 0 (Sunday) to 6", d)
			}
		}
		slices.Sort(s.Days)
		s.Days = slices.Compact(s.Days)
		o.Kind, o.Schedule = "routine", s
		return o, nil
	}
	o.For = defaultFor
	if req.For != nil {
		o.For = *req.For
	}
	if o.For < 1 || o.For > maxFor {
		return o, fmt.Errorf("for must be 1 to %d seconds", maxFor)
	}
	return o, nil
}

// cleanTerms trims and deduplicates chat terms, ignoring case as matching
// does.
func cleanTerms(raw []string) ([]string, error) {
	if raw != nil && len(raw) == 0 {
		return nil, fmt.Errorf("chats must hold 1 to %d terms, or be left out for every chat", maxTerms)
	}
	var terms []string
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if n := utf8.RuneCountInString(t); n == 0 || n > maxTerm {
			return nil, fmt.Errorf("a chat term must be 1 to %d characters", maxTerm)
		}
		if !slices.ContainsFunc(terms, func(x string) bool { return strings.EqualFold(x, t) }) {
			terms = append(terms, t)
		}
	}
	if len(terms) > maxTerms {
		return nil, fmt.Errorf("at most %d chat terms", maxTerms)
	}
	return terms, nil
}

// requestAccess passes a request to the owner side with the visible chats
// each term matched, terms that matched nothing included. The agent gets the
// same answer whatever matched, so a request is no probe of the chats.
func (g *gateway) requestAccess(w http.ResponseWriter, r *http.Request, cl *caller) {
	var req accessRequest
	if !decode(w, r, &req, maxBody) {
		return
	}
	c, ok := g.conn(w, req.Connector)
	if !ok {
		return
	}
	o, err := req.check()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	o.Identity = cl.identity
	if o.Kind == "extend" && len(writeScope(cl.live.grantsOn(cl.identity, c.name), time.Now())) == 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("no window on %s is open or ended in the last day, so there is nothing to extend", c.name))
		return
	}
	if !g.matchTerms(r.Context(), w, cl, c, o.Terms) {
		return
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := g.toOwner.do(r.Context(), http.MethodPost, "/internal/requests", o, &created); err != nil {
		relay(w, cl, "submit request", err)
		return
	}
	cl.log.Info("access requested", "connector", c.name, "id", created.ID, "kind", o.Kind)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": created.ID})
}

// matchTerms fills in the visible chats each term finds, the most recently
// active first. Grants do not limit the search: the agent asks for chats it
// may not read yet.
func (g *gateway) matchTerms(ctx context.Context, w http.ResponseWriter, cl *caller, c *conn, terms []term) bool {
	if len(terms) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	v, err := g.view(ctx, cl, c, everything)
	if err != nil {
		unavailable(w, cl, "open store", err)
		return false
	}
	words := make([]string, len(terms))
	for i, t := range terms {
		words[i] = t.Term
	}
	rows, err := v.matches(ctx, words, matchesPerTerm)
	if err != nil {
		unavailable(w, cl, "match chats", err)
		return false
	}
	for _, row := range rows {
		k, _ := row[0].(int64)
		id, _ := row[1].(string)
		name, _ := row[2].(string)
		terms[k].Chats = append(terms[k].Chats, chatRef{id, name})
	}
	return true
}

type requestStatus struct {
	Status  string      `json:"status"`
	GrantID string      `json:"grant_id,omitempty"`
	Until   record.Time `json:"until,omitzero"`
}

func (g *gateway) requestStatus(w http.ResponseWriter, r *http.Request, cl *caller) {
	id := r.PathValue("id")
	wait, ok := waitParam(w, r)
	if !ok {
		return
	}
	if !recordID.MatchString(id) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no request %q of yours", id))
		return
	}
	var st requestStatus
	if err := g.toOwner.do(r.Context(), http.MethodGet, "/internal/requests/"+id+query(cl.identity, wait), nil, &st); err != nil {
		relay(w, cl, "request status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
