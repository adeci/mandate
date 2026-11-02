package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"maps"
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
	readsShown   = 100
	// One agent may have this many access requests waiting, and make this
	// many in an hour.
	maxWaitingRequests = 3
	maxHourlyRequests  = 10
)

func agentName(name string) string {
	if name == "" {
		return "An agent"
	}
	return name
}

func (s *Server) accessURL(id string) string { return s.cfg.Origin + "/access#request-" + id }

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
// learns from the answer depends only on its own limits, never on chats.
func (s *Server) requestAccess(w http.ResponseWriter, r *http.Request) {
	var req proposal.AccessRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	if req.For == 0 {
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
	if err := s.db.addAccessRequest(ctx, ar, maxWaitingRequests, maxHourlyRequests); err != nil {
		connector.WriteError(w, quotaError(err))
		return
	}
	s.cfg.Log.Info("access requested", "id", ar.ID, "agent", a.Name, "connector", req.Connector,
		"chats", len(req.Chats), "unmatched", len(req.Unmatched), "for", req.For)
	ref := proposal.RequestRef{ID: ar.ID, URL: s.accessURL(ar.ID)}
	s.notify.notify(fmt.Sprintf("%s asks to read %s", a.Name, req.Connector), ref.URL)
	connector.WriteJSON(w, http.StatusCreated, ref)
}

// checkAccessRequest applies the input rules to the terms as the agent gave
// them, so whether a term matched never decides whether it is accepted.
func checkAccessRequest(req proposal.AccessRequest) error {
	switch {
	case req.For < 0 || req.For > maxAccessFor:
		return connector.Invalid("for must be over 0 and at most %s", maxAccessFor)
	case req.Horizon < 0:
		return connector.Invalid("horizon must not be negative")
	case req.Reason == "":
		return connector.Invalid("say why in the reason")
	case utf8.RuneCountInString(req.Reason) > maxReason:
		return connector.Invalid("reason is longer than %d characters", maxReason)
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

// accessChallengeRequest describes the record to sign: a new agent by Name
// and the SHA-256 of the token the browser made; a grant to Agent on
// Connector for exactly Chats (every chat when nil), now For a duration or
// on Schedule; or a grant answering Request, narrowed by any of Chats, For
// and Horizon. Durations are Go duration strings.
type accessChallengeRequest struct {
	Kind        string           `json:"kind"`
	Name        string           `json:"name,omitempty"`
	TokenSHA256 string           `json:"token_sha256,omitempty"`
	Agent       string           `json:"agent,omitempty"`
	Connector   string           `json:"connector,omitempty"`
	Chats       []string         `json:"chats"`
	For         string           `json:"for,omitempty"`
	Schedule    *scheduleRequest `json:"schedule,omitempty"`
	Horizon     string           `json:"horizon,omitempty"`
	Note        string           `json:"note,omitempty"`
	Request     string           `json:"request,omitempty"`
}

type scheduleRequest struct {
	Days   []time.Weekday `json:"days"`
	Start  string         `json:"start"`
	Length string         `json:"length"`
	Zone   string         `json:"zone"`
}

type accessChallengeResponse struct {
	Nonce   string         `json:"nonce"`
	Options requestOptions `json:"options"`
	// Summary is the record in plain words, escaped with hidden characters
	// marked, shown while the passkey asks.
	Summary template.HTML `json:"summary"`
}

// accessChallenge stores the record bytes the passkey will sign, bound to
// this session.
func (s *Server) accessChallenge(w http.ResponseWriter, r *http.Request, sess session) {
	var req accessChallengeRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	if len(s.keys.Trusted()) == 0 {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: "add a passkey under Keys first"})
		return
	}
	ctx := r.Context()
	now := s.now()
	ch := accessChallenge{Nonce: make([]byte, 16), Kind: req.Kind, IssuedAt: s.cfg.Now().UTC(), Session: sess.hash}
	rand.Read(ch.Nonce)
	var (
		summary template.HTML
		err     error
	)
	switch {
	case req.Kind == approval.KindAgent:
		ch.Record, summary, err = s.agentRecord(ctx, req, now)
	case req.Kind == approval.KindGrant && req.Request != "":
		ch.Request = req.Request
		ch.Record, summary, err = s.requestGrant(ctx, req, now)
	case req.Kind == approval.KindGrant:
		ch.Record, summary, err = s.newGrant(ctx, req, now)
	default:
		err = connector.Invalid("kind must be %q or %q", approval.KindAgent, approval.KindGrant)
	}
	if err == nil {
		err = s.db.addAccessChallenge(ctx, ch)
	}
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	connector.WriteJSON(w, http.StatusOK, accessChallengeResponse{
		Nonce:   b64(ch.Nonce),
		Options: s.requestOptions(approval.AccessChallenge(ch.Kind, ch.Record, ch.Nonce, ch.IssuedAt)),
		Summary: summary,
	})
}

// agentRecord signs only the token's hash: the browser makes the token and
// shows it, so the console never has it.
func (s *Server) agentRecord(ctx context.Context, req accessChallengeRequest, now time.Time) ([]byte, template.HTML, error) {
	name := strings.TrimSpace(req.Name)
	if !validName(name) {
		return nil, "", connector.Invalid("name must be 1-64 letters, digits, spaces, or -_.")
	}
	if h, err := hex.DecodeString(req.TokenSHA256); err != nil || len(h) != 32 || hex.EncodeToString(h) != req.TokenSHA256 {
		return nil, "", connector.Invalid("token_sha256 must be the token's SHA-256 in lowercase hex")
	}
	agents, err := s.db.agents(ctx)
	if err != nil {
		return nil, "", err
	}
	if slices.ContainsFunc(agents, func(a agentRow) bool { return strings.EqualFold(a.Name, name) }) {
		return nil, "", &connector.Error{Status: http.StatusConflict, Message: "an agent by that name exists"}
	}
	rec, err := approval.EncodeAgent(approval.Agent{ID: rand.Text(), Name: name, TokenSHA256: req.TokenSHA256, CreatedAt: now})
	if err != nil {
		return nil, "", connector.Invalid("%v", err)
	}
	return rec, "New agent " + visible(name), nil
}

// parseSpan reads a Go duration; "" and "0" are zero.
func parseSpan(what, v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, connector.Invalid("%s %q is not a duration", what, v)
	}
	return d, nil
}

func (s *Server) newGrant(ctx context.Context, req accessChallengeRequest, now time.Time) ([]byte, template.HTML, error) {
	a, err := s.db.agent(ctx, req.Agent)
	if err != nil {
		return nil, "", notFound(err)
	}
	if _, ok := s.conns[req.Connector]; !ok {
		return nil, "", connector.Invalid("no connector %q", req.Connector)
	}
	if len(req.Chats) > maxGrantChats || utf8.RuneCountInString(req.Note) > maxNote {
		return nil, "", connector.Invalid("too many chats or too long a note")
	}
	horizon, err := parseSpan("horizon", req.Horizon)
	if err != nil {
		return nil, "", err
	}
	fo, err := s.forwardOnly(ctx, req.Connector)
	if err != nil {
		return nil, "", err
	}
	if fo && (len(req.Chats) > 0 || horizon != 0) {
		return nil, "", connector.Invalid("%s keeps no chats: grant every allowlisted read, without chats or horizon", req.Connector)
	}
	g := approval.Grant{
		ID: rand.Text(), Agent: a.ID, Connector: req.Connector, Chats: req.Chats,
		Horizon: horizon, Note: strings.TrimSpace(req.Note), CreatedAt: now,
	}
	switch {
	case req.Schedule != nil && req.For != "":
		return nil, "", connector.Invalid("give a duration or a schedule, not both")
	case req.Schedule != nil:
		length, err := parseSpan("length", req.Schedule.Length)
		if err != nil {
			return nil, "", err
		}
		g.Schedule = &approval.Schedule{Days: req.Schedule.Days, Start: req.Schedule.Start, Length: length, Zone: req.Schedule.Zone}
	default:
		d, err := parseSpan("for", req.For)
		if err != nil {
			return nil, "", err
		}
		g.From, g.Until = now, now.Add(d)
	}
	rec, err := approval.EncodeGrant(g)
	if err != nil {
		return nil, "", connector.Invalid("%v", err)
	}
	return rec, s.grantSummary(ctx, a.Name, g, fo, nil), nil
}

// requestGrant answers an access request with a grant from now, narrowed
// by the owner but never wider than asked: chats named in the request or
// under them, any chats when it asked for every chat.
func (s *Server) requestGrant(ctx context.Context, req accessChallengeRequest, now time.Time) ([]byte, template.HTML, error) {
	ar, err := s.db.accessRequest(ctx, req.Request, now)
	if err != nil {
		return nil, "", notFound(err)
	}
	if ar.Status != proposal.AccessPending {
		return nil, "", &connector.Error{Status: http.StatusConflict, Message: "request is " + ar.Status}
	}
	fo, err := s.forwardOnly(ctx, ar.Connector)
	if err != nil {
		return nil, "", err
	}
	if req.Chats != nil && len(req.Chats) == 0 || len(req.Chats) > maxGrantChats {
		return nil, "", connector.Invalid("pick 1-%d chats, or leave the choice as asked", maxGrantChats)
	}
	known := map[string]string{}
	for _, c := range ar.Chats {
		known[c.ID] = c.Name
	}
	var chats []string
	switch {
	case fo:
		if ar.names() || req.Chats != nil {
			return nil, "", connector.Invalid("%s keeps no chats; deny a request that names some", ar.Connector)
		}
	case !ar.names():
		chats = req.Chats
	default:
		if req.Chats == nil {
			return nil, "", connector.Invalid("pick the chats to grant")
		}
		offered, err := s.offered(ctx, ar)
		if err != nil {
			return nil, "", err
		}
		for _, id := range req.Chats {
			if !offered[id] {
				return nil, "", connector.Invalid("chat %q was not asked for", id)
			}
		}
		chats = req.Chats
	}
	d := ar.For
	if req.For != "" {
		if d, err = parseSpan("for", req.For); err != nil {
			return nil, "", err
		}
		if d <= 0 || d > ar.For {
			return nil, "", connector.Invalid("for may only shorten the %s asked for", ar.For)
		}
	}
	horizon := ar.Horizon
	switch {
	case fo:
		horizon = 0
	case req.Horizon != "":
		if horizon, err = parseSpan("horizon", req.Horizon); err != nil {
			return nil, "", err
		}
		if ar.Horizon > 0 && (horizon == 0 || horizon > ar.Horizon) {
			return nil, "", connector.Invalid("horizon may only narrow the %s asked for", ar.Horizon)
		}
	}
	g := approval.Grant{
		ID: rand.Text(), Agent: ar.AgentID, Connector: ar.Connector, Chats: chats,
		Horizon: horizon, From: now, Until: now.Add(d), CreatedAt: now,
	}
	rec, err := approval.EncodeGrant(g)
	if err != nil {
		return nil, "", connector.Invalid("%v", err)
	}
	return rec, s.grantSummary(ctx, ar.AgentName, g, fo, known), nil
}

// names reports whether the request named chats, matched or not; one that
// didn't asks for every chat.
func (ar accessRequest) names() bool { return len(ar.Chats) > 0 || len(ar.Unmatched) > 0 }

// offered is what the owner may grant for a request that names chats: the
// chats its page shows.
func (s *Server) offered(ctx context.Context, ar accessRequest) (map[string]bool, error) {
	tree, err := s.requestTree(ctx, ar)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	var add func([]chatNode)
	add = func(ns []chatNode) {
		for _, n := range ns {
			out[n.ID] = true
			add(n.Children)
		}
	}
	add(tree)
	return out, nil
}

type accessApproveRequest struct {
	Nonce     string          `json:"nonce"`
	Assertion json.RawMessage `json:"assertion"`
}

type accessApproveResponse struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func (s *Server) accessApprove(w http.ResponseWriter, r *http.Request, sess session) {
	var req accessApproveRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	nonce, err := base64.RawURLEncoding.DecodeString(req.Nonce)
	if err != nil {
		connector.WriteError(w, connector.Invalid("invalid nonce"))
		return
	}
	ctx := r.Context()
	ch, err := s.db.accessChallenge(ctx, nonce)
	if errors.Is(err, errNoChallenge) || err == nil && s.cfg.Now().Sub(ch.IssuedAt) > approval.MaxAge {
		err = &connector.Error{Status: http.StatusConflict, Message: errNoChallenge.Error()}
	}
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	if string(ch.Session) != string(sess.hash) {
		connector.WriteError(w, &connector.Error{Status: http.StatusForbidden, Message: "this approval was started in another session"})
		return
	}
	signed := approval.Signed{Kind: ch.Kind, Record: ch.Record, Nonce: ch.Nonce, IssuedAt: ch.IssuedAt, Assertion: req.Assertion}
	stored, err := json.Marshal(signed)
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	refuse := func(err error) {
		s.cfg.Log.Warn("access approval refused", "kind", ch.Kind, "err", err)
		connector.WriteError(w, &connector.Error{Status: http.StatusForbidden, Message: err.Error()})
	}
	var res accessApproveResponse
	switch ch.Kind {
	case approval.KindAgent:
		a, err := s.verifier.VerifyAgent(signed)
		if err != nil {
			refuse(err)
			return
		}
		if err := s.db.addAgent(ctx, ch, a, stored); err != nil {
			connector.WriteError(w, accessConflict(err))
			return
		}
		s.cfg.Log.Info("agent created", "id", a.ID, "name", a.Name, "by", sess.who())
		res = accessApproveResponse{Kind: ch.Kind, ID: a.ID}
	case approval.KindGrant:
		g, err := s.verifier.VerifyGrant(signed)
		if err != nil {
			refuse(err)
			return
		}
		if err := s.db.addGrant(ctx, ch, g, stored, s.now()); err != nil {
			connector.WriteError(w, accessConflict(err))
			return
		}
		s.cfg.Log.Info("grant created", "id", g.ID, "agent", g.Agent, "connector", g.Connector, "request", ch.Request, "by", sess.who())
		res = accessApproveResponse{Kind: ch.Kind, ID: g.ID}
	default:
		connector.WriteError(w, fmt.Errorf("stored challenge of kind %q", ch.Kind))
		return
	}
	connector.WriteJSON(w, http.StatusOK, res)
}

func accessConflict(err error) error {
	switch {
	case errors.Is(err, errNoChallenge), errors.Is(err, errNoAgent):
		return &connector.Error{Status: http.StatusConflict, Message: err.Error()}
	case errors.Is(err, errNotPending):
		return &connector.Error{Status: http.StatusConflict, Message: "the request was decided or expired meanwhile"}
	}
	return err
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	s.endAccess(w, r, s.db.deleteAgent(r.Context(), id, s.now()), "agent deleted", id, sess)
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	s.endAccess(w, r, s.db.revokeGrant(r.Context(), id, s.now()), "grant revoked", id, sess)
}

func (s *Server) denyAccess(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	err := s.db.denyAccessRequest(r.Context(), id, s.now())
	if errors.Is(err, errNotPending) {
		s.errorPage(w, http.StatusConflict, "This request was already decided or has expired.")
		return
	}
	s.endAccess(w, r, err, "access denied", id, sess)
}

func (s *Server) endAccess(w http.ResponseWriter, r *http.Request, err error, what, id string, sess session) {
	switch {
	case errors.Is(err, errNoAgent), errors.Is(err, errNoGrant):
		s.errorPage(w, http.StatusNotFound, "It is already gone.")
		return
	case err != nil:
		s.cfg.Log.Error(what, "id", id, "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't save the change.")
		return
	}
	s.cfg.Log.Info(what, "id", id, "by", sess.who())
	http.Redirect(w, r, "/access", http.StatusSeeOther)
}

// grantSummary says what g allows in plain words; known names chats the
// caller already has names for.
func (s *Server) grantSummary(ctx context.Context, agent string, g approval.Grant, fo bool, known map[string]string) template.HTML {
	if fo {
		return template.HTML(fmt.Sprintf("%s may make every allowlisted read on %s, %s",
			visible(agent), visible(g.Connector), visible(windowText(g))))
	}
	return template.HTML(fmt.Sprintf("%s may read %s on %s, %s, %s",
		visible(agent), s.chatsText(ctx, g, known), visible(g.Connector), visible(windowText(g)), visible(horizonText(g.Horizon))))
}

func (s *Server) chatsText(ctx context.Context, g approval.Grant, known map[string]string) template.HTML {
	if len(g.Chats) == 0 {
		return "every chat"
	}
	return s.chatLabels(ctx, g.Connector, g.Chats, known) + " (chats created later are not included)"
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func windowText(g approval.Grant) string {
	if g.Schedule == nil {
		return "until " + g.Until.Local().Format(stamp)
	}
	sc := g.Schedule
	return fmt.Sprintf("%s at %s for %s (%s)", daysText(sc.Days), sc.Start, span(sc.Length), sc.Zone)
}

func daysText(days []time.Weekday) string {
	set := slices.Sorted(slices.Values(days))
	set = slices.Compact(set)
	switch {
	case len(set) == 0 || len(set) == 7:
		return "every day"
	case slices.Equal(set, []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}):
		return "weekdays"
	case slices.Equal(set, []time.Weekday{time.Sunday, time.Saturday}):
		return "weekends"
	}
	out := make([]string, len(set))
	for i, d := range set {
		out[i] = d.String()[:3]
	}
	return strings.Join(out, ", ")
}

func horizonText(h time.Duration) string {
	if h == 0 {
		return "all history"
	}
	return "the last " + span(h)
}

// span says d in the largest whole unit.
func span(d time.Duration) string {
	unit := func(n int64, name string) string {
		if n == 1 {
			return "1 " + name
		}
		return strconv.FormatInt(n, 10) + " " + name + "s"
	}
	switch {
	case d%(24*time.Hour) == 0:
		return unit(int64(d/(24*time.Hour)), "day")
	case d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	}
	return d.String()
}

// option is a choice in a select.
type option struct {
	Value, Label string
	Selected     bool
}

var (
	forChoices     = []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 12 * time.Hour}
	grantChoices   = append(slices.Clone(forChoices), 24*time.Hour, 7*24*time.Hour, 30*24*time.Hour)
	horizonChoices = []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour, 0}
)

// spanOptions offers choices up to most (any when 0) and chosen itself,
// selecting chosen.
func spanOptions(choices []time.Duration, most, chosen time.Duration, label func(time.Duration) string) []option {
	var out []option
	add := func(d time.Duration) {
		out = append(out, option{Value: d.String(), Label: label(d), Selected: d == chosen})
	}
	seen := false
	for _, d := range choices {
		if most > 0 && (d == 0 || d > most) {
			continue
		}
		seen = seen || d == chosen
		add(d)
	}
	if !seen {
		add(chosen)
	}
	return out
}

type accessRequestView struct {
	ID, AgentName, Connector string
	Reason                   template.HTML
	// Chats are the chats the request named, with the chats under them.
	Chats     []chatNode
	Unmatched []string
	// Every is a request for every chat; ForwardOnly one on a connector
	// without chats.
	Every, ForwardOnly bool
	CanApprove         bool
	For, Horizon       []option
	Left               string
}

type grantView struct {
	ID, Connector, When, Horizon, State, Tone string
	Chats, Note                               template.HTML
}

type agentView struct {
	ID, Name          string
	Created, LastRead time.Time
	Grants            []grantView
}

type readView struct {
	At                           time.Time
	Agent, Connector, Verb, Chat string
	Count, Dropped               int
}

type accessView struct {
	Requests   []accessRequestView
	Agents     []agentView
	Reads      []readView
	Connectors []string
	// ForwardOnly connectors keep no chats.
	ForwardOnly          map[string]bool
	CanSign              bool
	For, Horizon, Length []option
}

func (s *Server) accessPage(w http.ResponseWriter, r *http.Request, sess session) {
	v, err := s.accessView(r.Context())
	if err != nil {
		s.cfg.Log.Error("load access", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load access.")
		return
	}
	s.owner(w, r, sess, "access", "Access", "access", v)
}

func (s *Server) accessView(ctx context.Context) (accessView, error) {
	now := s.now()
	v := accessView{
		Connectors:  slices.Sorted(maps.Keys(s.conns)),
		ForwardOnly: map[string]bool{},
		CanSign:     len(s.keys.Trusted()) > 0,
		For:         spanOptions(grantChoices, 0, time.Hour, span),
		Length:      spanOptions(forChoices, 0, time.Hour, span),
		Horizon:     spanOptions(horizonChoices, 0, 7*24*time.Hour, horizonText),
	}
	for _, name := range v.Connectors {
		fo, err := s.forwardOnly(ctx, name)
		if err != nil {
			s.cfg.Log.Warn("ask for chats", "connector", name, "err", err)
		}
		v.ForwardOnly[name] = fo
	}
	requests, err := s.db.pendingAccessRequests(ctx, now)
	if err != nil {
		return v, err
	}
	for _, ar := range requests {
		rv := accessRequestView{
			ID: ar.ID, AgentName: ar.AgentName, Connector: ar.Connector, Reason: visible(ar.Reason),
			Unmatched: ar.Unmatched, Every: !ar.names(), ForwardOnly: v.ForwardOnly[ar.Connector],
			For:     spanOptions(forChoices, ar.For, ar.For, span),
			Horizon: spanOptions(horizonChoices, ar.Horizon, ar.Horizon, horizonText),
			Left:    left(ar.ExpiresAt.Sub(now)),
		}
		if ar.names() {
			tree, err := s.requestTree(ctx, ar)
			if err != nil {
				s.cfg.Log.Warn("chats under requested chats", "connector", ar.Connector, "err", err)
				for _, c := range ar.Chats {
					tree = append(tree, chatNode{ID: c.ID, Label: chatLabel(c.Name, c.ID)})
				}
			}
			rv.Chats = tree
		}
		rv.CanApprove = v.CanSign && (rv.Every || len(rv.Chats) > 0) && (!rv.ForwardOnly || !ar.names())
		v.Requests = append(v.Requests, rv)
	}
	agents, err := s.db.agents(ctx)
	if err != nil {
		return v, err
	}
	grants, err := s.db.grants(ctx)
	if err != nil {
		return v, err
	}
	names := map[string]string{}
	for _, a := range agents {
		names[a.ID] = a.Name
		av := agentView{ID: a.ID, Name: a.Name, Created: a.CreatedAt, LastRead: a.LastRead}
		for _, g := range grants {
			if g.Agent == a.ID {
				av.Grants = append(av.Grants, s.grantView(ctx, g, v.ForwardOnly[g.Connector], now))
			}
		}
		v.Agents = append(v.Agents, av)
	}
	reads, err := s.db.reads(ctx, readsShown)
	if err != nil {
		return v, err
	}
	for _, e := range reads {
		v.Reads = append(v.Reads, readView{
			At: e.At, Agent: cmpOr(names[e.AgentID], e.AgentID), Connector: e.Connector, Verb: e.Verb, Chat: e.Chat,
			Count: e.Count, Dropped: e.Dropped,
		})
	}
	return v, nil
}

func (s *Server) grantView(ctx context.Context, g approval.Grant, fo bool, now time.Time) grantView {
	gv := grantView{
		ID: g.ID, Connector: g.Connector, Chats: s.chatsText(ctx, g, nil),
		When: windowText(g), Horizon: horizonText(g.Horizon), Note: visible(g.Note),
	}
	if fo {
		gv.Chats, gv.Horizon = "every allowlisted read", "any age"
	}
	if g.Schedule == nil {
		gv.When = "from " + g.From.Local().Format(stamp) + " " + gv.When
	}
	switch open, at := g.Open(now); {
	case open:
		gv.State, gv.Tone = "open until "+at.Local().Format(stamp), "ok"
	case !at.IsZero():
		gv.State, gv.Tone = "next "+at.Local().Format(stamp), "wait"
	default:
		gv.State, gv.Tone = "ended", "muted"
	}
	return gv
}
