package console

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const (
	accessRequestTTL = time.Hour
	defaultAccessFor = 30 * time.Minute
	maxAccessFor     = 12 * time.Hour
	maxAccessWait    = 900 * time.Second
	accessPoll       = time.Second
	maxReason        = 1000
	maxGrantChats    = 100
	maxTerms         = 10
	maxTerm          = 200
	// chatsPerTerm is how many chats the gateway resolves a term to.
	chatsPerTerm = 5
	maxNote      = 500
	// One agent may have this many access requests waiting, and make this
	// many in an hour.
	maxWaitingRequests = 3
	maxHourlyRequests  = 10
	// extendAfter is how long after a window ends an agent may still ask to
	// extend it.
	extendAfter = time.Hour
)

func agentName(name string) string {
	if name == "" {
		return "An agent"
	}
	return name
}

func (s *Server) requestURL(id string) string { return s.cfg.Origin + "/#request-" + id }

// keyLog serves the key log for the gateway to verify against its own copy.
func (s *Server) keyLog(w http.ResponseWriter, _ *http.Request) {
	log := s.keys.Log()
	if log == nil {
		log = []approval.SignedChange{}
	}
	connector.WriteJSON(w, http.StatusOK, log)
}

type accessRecordsResponse struct {
	Records []json.RawMessage `json:"records"`
}

func (s *Server) accessRecords(w http.ResponseWriter, r *http.Request) {
	recs, err := s.db.liveRecords(r.Context())
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	connector.WriteJSON(w, http.StatusOK, accessRecordsResponse{Records: recs})
}

// requestAccess stores an agent's request for the owner. What the agent
// learns from the answer depends only on its own limits and windows, never
// on chats.
func (s *Server) requestAccess(w http.ResponseWriter, r *http.Request) {
	var req proposal.AccessRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	if req.For == 0 && req.Schedule == nil {
		req.For = defaultAccessFor
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if err := checkAccessRequest(req); err != nil {
		connector.WriteError(w, err)
		return
	}
	if _, ok := s.conns[req.Connector]; !ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusNotFound, Message: fmt.Sprintf("no connector %q", req.Connector)})
		return
	}
	ctx := r.Context()
	if len(req.Chats) > 0 || len(req.Unmatched) > 0 {
		if fo, err := s.forwardOnly(ctx, req.Connector); err == nil && fo {
			connector.WriteError(w, connector.Invalid("%s keeps no chats; ask without naming any", req.Connector))
			return
		}
	}
	a, err := s.db.agent(ctx, req.AgentID)
	if err != nil {
		connector.WriteError(w, notFound(err))
		return
	}
	req.AgentName = a.Name
	now := s.now()
	ar := accessRequest{
		AccessRequest: req, ID: rand.Text(), Status: proposal.AccessPending,
		CreatedAt: now, ExpiresAt: now.Add(accessRequestTTL),
	}
	if req.Extend {
		g, err := s.extendable(ctx, a.ID, req.Connector, now)
		if err != nil {
			connector.WriteError(w, err)
			return
		}
		ar.Extends = g.ID
	}
	if err := s.db.addAccessRequest(ctx, ar, maxWaitingRequests, maxHourlyRequests); err != nil {
		connector.WriteError(w, quotaError(err))
		return
	}
	s.cfg.Log.Info("access requested", "id", ar.ID, "agent", a.Name, "connector", req.Connector,
		"chats", len(req.Chats), "unmatched", len(req.Unmatched), "for", req.For,
		"routine", req.Schedule != nil, "extend", req.Extend)
	ref := proposal.RequestRef{ID: ar.ID, URL: s.requestURL(ar.ID)}
	s.notify.notify(a.Name, fmt.Sprintf("%s asks to read %s", a.Name, req.Connector), ref.URL, true)
	connector.WriteJSON(w, http.StatusCreated, ref)
}

// extendable is the agent's one-off grant on connector that is open or ended
// within extendAfter and not revoked, the latest ending if several are.
func (s *Server) extendable(ctx context.Context, agent, conn string, now time.Time) (grantRow, error) {
	grants, err := s.db.grants(ctx)
	if err != nil {
		return grantRow{}, err
	}
	var best grantRow
	for _, g := range grants {
		if g.Agent == agent && g.Connector == conn && g.Schedule == nil && !g.From.After(now) &&
			now.Sub(g.Until) < extendAfter && g.Until.After(best.Until) {
			best = g
		}
	}
	if best.ID == "" {
		return grantRow{}, &connector.Error{Status: http.StatusConflict, Message: fmt.Sprintf("no window to extend on %s", conn)}
	}
	return best, nil
}

// checkAccessRequest applies the input rules to the terms as the agent gave
// them, so whether a term matched never decides whether it is accepted.
func checkAccessRequest(req proposal.AccessRequest) error {
	switch {
	case req.Schedule == nil && (req.For <= 0 || req.For > maxAccessFor):
		return connector.Invalid("for must be over 0 and at most %s", maxAccessFor)
	case req.Horizon < 0:
		return connector.Invalid("horizon must not be negative")
	case req.Reason == "":
		return connector.Invalid("say why in the reason")
	case utf8.RuneCountInString(req.Reason) > maxReason:
		return connector.Invalid("reason is longer than %d characters", maxReason)
	case req.Extend && (req.Schedule != nil || len(req.Terms) > 0 || req.Horizon != 0):
		return connector.Invalid("an extension keeps the window's chats and horizon")
	}
	if sc := req.Schedule; sc != nil {
		// A grant on this schedule must encode; the approver adds the zone.
		_, err := approval.EncodeGrant(approval.Grant{
			ID: "x", Agent: "x", Connector: "x",
			Schedule: &approval.Schedule{Days: sc.Days, Start: sc.Start, Length: sc.Length, Zone: "UTC"},
		})
		if err != nil || req.For != 0 {
			return connector.Invalid("a routine needs days, a start HH:MM and a length up to 24h, and no for")
		}
	}
	if len(req.Terms) > maxTerms {
		return connector.Invalid("at most %d chat terms", maxTerms)
	}
	seen := map[string]bool{}
	for _, t := range req.Terms {
		if t != strings.TrimSpace(t) || t == "" || utf8.RuneCountInString(t) > maxTerm || seen[t] {
			return connector.Invalid("chat terms must be distinct, trimmed and 1-%d characters", maxTerm)
		}
		seen[t] = true
	}
	switch {
	case slices.ContainsFunc(req.Unmatched, func(t string) bool { return !seen[t] }):
		return connector.Invalid("an unmatched term is not among the terms")
	case len(req.Terms) > 0 && len(req.Chats) == 0 && len(req.Unmatched) == 0,
		len(req.Terms) == 0 && (len(req.Chats) > 0 || len(req.Unmatched) > 0):
		return connector.Invalid("chats and unmatched terms must come from the terms")
	case len(req.Chats) > len(req.Terms)*chatsPerTerm:
		return connector.Invalid("more than %d chats per term", chatsPerTerm)
	case slices.ContainsFunc(req.Chats, func(c proposal.Chat) bool { return c.ID == "" }):
		return connector.Invalid("a chat has no id")
	}
	return nil
}

func quotaError(err error) error {
	if errors.Is(err, errTooMany) {
		return &connector.Error{Status: http.StatusTooManyRequests, Message: fmt.Sprintf(
			"an agent may have %d access requests waiting and make %d an hour; wait for the owner", maxWaitingRequests, maxHourlyRequests)}
	}
	return err
}

// accessQuota tells the gateway whether an agent may make a request now,
// before it resolves any chat terms.
func (s *Server) accessQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.db.agent(ctx, r.URL.Query().Get("agent"))
	if err == nil {
		err = s.db.accessQuota(ctx, a.ID, s.now(), maxWaitingRequests, maxHourlyRequests)
	}
	if err != nil {
		connector.WriteError(w, quotaError(notFound(err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// accessStatus answers a request's state; with wait=SECONDS it waits until
// the request is decided or the wait is over.
func (s *Server) accessStatus(w http.ResponseWriter, r *http.Request) {
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			connector.WriteError(w, connector.Invalid("wait must be whole seconds"))
			return
		}
		wait = min(time.Duration(n)*time.Second, maxAccessWait)
	}
	ctx := r.Context()
	deadline := time.Now().Add(wait)
	for {
		ar, err := s.db.accessRequest(ctx, r.PathValue("id"), s.now())
		if err != nil {
			connector.WriteError(w, notFound(err))
			return
		}
		if ar.Status != proposal.AccessPending || !time.Now().Before(deadline) {
			st, err := s.statusOf(ctx, ar)
			if err != nil {
				connector.WriteError(w, err)
				return
			}
			connector.WriteJSON(w, http.StatusOK, st)
			return
		}
		t := time.NewTimer(min(accessPoll, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// statusOf leaves Until out once the grant is revoked.
func (s *Server) statusOf(ctx context.Context, ar accessRequest) (proposal.AccessStatus, error) {
	st := proposal.AccessStatus{Status: ar.Status, AgentID: ar.AgentID, GrantID: ar.GrantID}
	if ar.GrantID == "" {
		return st, nil
	}
	g, err := s.db.grant(ctx, ar.GrantID)
	switch {
	case errors.Is(err, errNoGrant):
		return st, nil
	case err != nil:
		return proposal.AccessStatus{}, err
	}
	st.Until = g.Until
	return st, nil
}

// recordSeen stores when an agent last used the gateway, which posts this
// at most once a minute per agent.
func (s *Server) recordSeen(w http.ResponseWriter, r *http.Request) {
	var seen proposal.Seen
	if !connector.DecodeJSON(w, r, &seen, maxBody) {
		return
	}
	if seen.At.IsZero() {
		seen.At = s.now()
	}
	if err := s.db.seen(r.Context(), seen.AgentID, seen.At); err != nil {
		connector.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) recordRead(w http.ResponseWriter, r *http.Request) {
	var e proposal.ReadEvent
	if !connector.DecodeJSON(w, r, &e, maxBody) {
		return
	}
	if e.At.IsZero() {
		e.At = s.now()
	}
	if err := s.db.addRead(r.Context(), e); err != nil {
		connector.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
