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
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

// Challenge kinds that sign a grant from an existing one.
const (
	kindExtend = "extend"
	kindResume = "resume"
)

// accessChallengeRequest describes the record to sign:
//   - agent: a new agent by Name and the SHA-256 of the token the browser
//     made;
//   - grant: a grant to Agent on Connector for exactly Chats (every chat
//     when nil), now For a duration or on Schedule; or one answering
//     Request, narrowed by any of Chats, For and Horizon, with Zone for a
//     routine;
//   - extend: Grant again for For more, from now;
//   - resume: Grant, a closed routine, again.
//
// Durations are Go duration strings.
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
	Zone        string           `json:"zone,omitempty"`
	Grant       string           `json:"grant,omitempty"`
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

// signing is what a challenge kind prepares: the record, what it says, and
// the grant it replaces and chat names, if any.
type signing struct {
	record   []byte
	summary  template.HTML
	replaces string
	names    map[string]string
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
	var (
		sg   signing
		err  error
		kind = approval.KindGrant
	)
	switch req.Kind {
	case approval.KindAgent:
		kind = approval.KindAgent
		sg, err = s.agentRecord(ctx, req, now)
	case approval.KindGrant:
		if req.Request != "" {
			sg, err = s.requestGrant(ctx, req, now)
		} else {
			sg, err = s.newGrant(ctx, req, now)
		}
	case kindExtend:
		sg, err = s.extendGrant(ctx, req, now)
	case kindResume:
		sg, err = s.resumeGrant(ctx, req, now)
	default:
		err = connector.Invalid("kind must be one of agent, grant, extend, resume")
	}
	ch := accessChallenge{
		Nonce: make([]byte, 16), Kind: kind, Record: sg.record, IssuedAt: s.cfg.Now().UTC(), Session: sess.hash,
		Request: req.Request, Replaces: sg.replaces, Names: sg.names,
	}
	rand.Read(ch.Nonce)
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
		Summary: sg.summary,
	})
}

// agentRecord signs only the token's hash: the browser makes the token and
// shows it, so the console never has it.
func (s *Server) agentRecord(ctx context.Context, req accessChallengeRequest, now time.Time) (signing, error) {
	name := strings.TrimSpace(req.Name)
	if !validName(name) {
		return signing{}, connector.Invalid("name must be 1-64 letters, digits, spaces, or -_.")
	}
	if h, err := hex.DecodeString(req.TokenSHA256); err != nil || len(h) != 32 || hex.EncodeToString(h) != req.TokenSHA256 {
		return signing{}, connector.Invalid("token_sha256 must be the token's SHA-256 in lowercase hex")
	}
	agents, err := s.db.agents(ctx)
	if err != nil {
		return signing{}, err
	}
	if slices.ContainsFunc(agents, func(a agentRow) bool { return strings.EqualFold(a.Name, name) }) {
		return signing{}, &connector.Error{Status: http.StatusConflict, Message: "an agent by that name exists"}
	}
	rec, err := approval.EncodeAgent(approval.Agent{ID: rand.Text(), Name: name, TokenSHA256: req.TokenSHA256, CreatedAt: now})
	if err != nil {
		return signing{}, connector.Invalid("%v", err)
	}
	return signing{record: rec, summary: "New agent " + visible(name)}, nil
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

func (s *Server) newGrant(ctx context.Context, req accessChallengeRequest, now time.Time) (signing, error) {
	a, err := s.db.agent(ctx, req.Agent)
	if err != nil {
		return signing{}, notFound(err)
	}
	if _, ok := s.conns[req.Connector]; !ok {
		return signing{}, connector.Invalid("no connector %q", req.Connector)
	}
	if len(req.Chats) > maxGrantChats || utf8.RuneCountInString(req.Note) > maxNote {
		return signing{}, connector.Invalid("too many chats or too long a note")
	}
	horizon, err := parseSpan("horizon", req.Horizon)
	if err != nil {
		return signing{}, err
	}
	fo, err := s.forwardOnly(ctx, req.Connector)
	if err != nil {
		return signing{}, err
	}
	if fo && (len(req.Chats) > 0 || horizon != 0) {
		return signing{}, connector.Invalid("%s keeps no chats: grant every allowlisted read, without chats or horizon", req.Connector)
	}
	g := approval.Grant{
		ID: rand.Text(), Agent: a.ID, Connector: req.Connector, Chats: req.Chats,
		Horizon: horizon, Note: strings.TrimSpace(req.Note), CreatedAt: now,
	}
	switch {
	case req.Schedule != nil && req.For != "":
		return signing{}, connector.Invalid("give a duration or a schedule, not both")
	case req.Schedule != nil:
		length, err := parseSpan("length", req.Schedule.Length)
		if err != nil {
			return signing{}, err
		}
		g.Schedule = &approval.Schedule{Days: req.Schedule.Days, Start: req.Schedule.Start, Length: length, Zone: req.Schedule.Zone}
	default:
		d, err := parseSpan("for", req.For)
		if err != nil {
			return signing{}, err
		}
		g.From, g.Until = now, now.Add(d)
	}
	return s.grantSigning(ctx, a.Name, g, fo, s.chatNames(ctx, g.Connector, g.Chats, nil), "")
}

// requestGrant answers an access request with a grant, narrowed by the
// owner but never wider than asked: chats named in the request or under
// them, any chats when it asked for every chat. A routine runs as asked in
// the owner's zone; an extension lengthens the window it names.
func (s *Server) requestGrant(ctx context.Context, req accessChallengeRequest, now time.Time) (signing, error) {
	ar, err := s.db.accessRequest(ctx, req.Request, now)
	if err != nil {
		return signing{}, notFound(err)
	}
	if ar.Status != proposal.AccessPending {
		return signing{}, &connector.Error{Status: http.StatusConflict, Message: "request is " + ar.Status}
	}
	if ar.Extends != "" {
		if req.Chats != nil || req.Horizon != "" {
			return signing{}, connector.Invalid("an extension keeps the window's chats and horizon")
		}
		d, err := narrowFor(req.For, ar.For)
		if err != nil {
			return signing{}, err
		}
		g, err := s.db.grant(ctx, ar.Extends)
		if errors.Is(err, errNoGrant) {
			return signing{}, &connector.Error{Status: http.StatusConflict, Message: "the window to extend was closed"}
		}
		if err != nil {
			return signing{}, err
		}
		return s.extension(ctx, g, d, now)
	}
	fo, err := s.forwardOnly(ctx, ar.Connector)
	if err != nil {
		return signing{}, err
	}
	if req.Chats != nil && len(req.Chats) == 0 || len(req.Chats) > maxGrantChats {
		return signing{}, connector.Invalid("pick 1-%d chats, or leave the choice as asked", maxGrantChats)
	}
	known := map[string]string{}
	for _, c := range ar.Chats {
		known[c.ID] = c.Name
	}
	var chats []string
	switch {
	case fo:
		if ar.names() || req.Chats != nil {
			return signing{}, connector.Invalid("%s keeps no chats; deny a request that names some", ar.Connector)
		}
	case !ar.names():
		chats = req.Chats
	default:
		if req.Chats == nil {
			return signing{}, connector.Invalid("pick the chats to grant")
		}
		tree, err := s.requestTree(ctx, ar)
		if err != nil {
			return signing{}, err
		}
		offered := map[string]bool{}
		walkChats(tree, func(n chatNode) {
			offered[n.ID] = true
			known[n.ID] = n.Name
		})
		for _, id := range req.Chats {
			if !offered[id] {
				return signing{}, connector.Invalid("chat %q was not asked for", id)
			}
		}
		chats = req.Chats
	}
	horizon := ar.Horizon
	switch {
	case fo:
		horizon = 0
	case req.Horizon != "":
		if horizon, err = parseSpan("horizon", req.Horizon); err != nil {
			return signing{}, err
		}
		if ar.Horizon > 0 && (horizon == 0 || horizon > ar.Horizon) {
			return signing{}, connector.Invalid("horizon may only narrow the %s asked for", ar.Horizon)
		}
	}
	g := approval.Grant{
		ID: rand.Text(), Agent: ar.AgentID, Connector: ar.Connector, Chats: chats, Horizon: horizon, CreatedAt: now,
	}
	if sc := ar.Schedule; sc != nil {
		if req.For != "" {
			return signing{}, connector.Invalid("a routine runs as asked; narrow its chats or horizon")
		}
		g.Schedule = &approval.Schedule{Days: sc.Days, Start: sc.Start, Length: sc.Length, Zone: req.Zone}
	} else {
		d, err := narrowFor(req.For, ar.For)
		if err != nil {
			return signing{}, err
		}
		g.From, g.Until = now, now.Add(d)
	}
	return s.grantSigning(ctx, ar.AgentName, g, fo, s.chatNames(ctx, g.Connector, g.Chats, known), "")
}

// narrowFor is the window the owner chose, no longer than asked.
func narrowFor(chosen string, asked time.Duration) (time.Duration, error) {
	if chosen == "" {
		return asked, nil
	}
	d, err := parseSpan("for", chosen)
	if err != nil {
		return 0, err
	}
	if d <= 0 || d > asked {
		return 0, connector.Invalid("for may only shorten the %s asked for", asked)
	}
	return d, nil
}

// extendGrant signs an open window again, For longer from now.
func (s *Server) extendGrant(ctx context.Context, req accessChallengeRequest, now time.Time) (signing, error) {
	g, err := s.db.grant(ctx, req.Grant)
	if err != nil {
		return signing{}, notFound(err)
	}
	if g.Schedule != nil {
		return signing{}, connector.Invalid("a routine runs on its schedule; resume or edit it instead")
	}
	d, err := parseSpan("for", cmpOr(req.For, defaultAccessFor.String()))
	if err != nil || d <= 0 {
		return signing{}, connector.Invalid("for must be a positive duration")
	}
	return s.extension(ctx, g, d, now)
}

// extension is g with the same chats and horizon from now until d past
// whichever is later, its end or now; approving it replaces g.
func (s *Server) extension(ctx context.Context, g grantRow, d time.Duration, now time.Time) (signing, error) {
	a, err := s.db.agent(ctx, g.Agent)
	if err != nil {
		return signing{}, notFound(err)
	}
	fo, err := s.forwardOnly(ctx, g.Connector)
	if err != nil {
		return signing{}, err
	}
	next := g.Grant
	next.ID, next.CreatedAt = rand.Text(), now
	end := g.Until
	if end.Before(now) {
		end = now
	}
	next.From, next.Until = now, end.Add(d)
	return s.grantSigning(ctx, a.Name, next, fo, g.Names, g.ID)
}

// resumeGrant signs a closed routine again under a new ID; the gateway
// never takes a revoked ID back.
func (s *Server) resumeGrant(ctx context.Context, req accessChallengeRequest, now time.Time) (signing, error) {
	g, err := s.db.endedGrant(ctx, req.Grant)
	if err != nil {
		return signing{}, notFound(err)
	}
	if g.Schedule == nil {
		return signing{}, connector.Invalid("only routines resume; ask for a new window instead")
	}
	a, err := s.db.agent(ctx, g.Agent)
	if err != nil {
		return signing{}, notFound(err)
	}
	fo, err := s.forwardOnly(ctx, g.Connector)
	if err != nil {
		return signing{}, err
	}
	next := g.Grant
	next.ID, next.CreatedAt = rand.Text(), now
	return s.grantSigning(ctx, a.Name, next, fo, g.Names, g.ID)
}

func (s *Server) grantSigning(ctx context.Context, agent string, g approval.Grant, fo bool, names map[string]string, replaces string) (signing, error) {
	rec, err := approval.EncodeGrant(g)
	if err != nil {
		return signing{}, connector.Invalid("%v", err)
	}
	return signing{record: rec, summary: grantSummary(agent, g, fo, names), replaces: replaces, names: names}, nil
}

// names reports whether the request named chats, matched or not; one that
// didn't asks for every chat.
func (ar accessRequest) names() bool { return len(ar.Chats) > 0 || len(ar.Unmatched) > 0 }

func walkChats(ns []chatNode, fn func(chatNode)) {
	for _, n := range ns {
		fn(n)
		walkChats(n.Children, fn)
	}
}

// chatNames names ids, preferring known names and asking the connector
// for the rest.
func (s *Server) chatNames(ctx context.Context, conn string, ids []string, known map[string]string) map[string]string {
	out := map[string]string{}
	for _, id := range ids {
		n, ok := known[id]
		if !ok {
			c, _ := s.chat(ctx, conn, id)
			n = c.Name
		}
		out[id] = n
	}
	return out
}

// grantSummary says what g allows in plain words.
func grantSummary(agent string, g approval.Grant, fo bool, names map[string]string) template.HTML {
	if fo {
		return template.HTML(fmt.Sprintf("%s may make every allowlisted read on %s, %s",
			visible(agent), visible(g.Connector), visible(windowText(g))))
	}
	return template.HTML(fmt.Sprintf("%s may read %s on %s, %s, %s",
		visible(agent), chatsText(g, names), visible(g.Connector), visible(windowText(g)), visible(horizonText(g.Horizon))))
}

func chatsText(g approval.Grant, names map[string]string) template.HTML {
	if len(g.Chats) == 0 {
		return "every chat"
	}
	labels := make([]string, len(g.Chats))
	for i, id := range g.Chats {
		labels[i] = string(chatLabel(names[id], id))
	}
	return template.HTML(strings.Join(labels, ", ")) + " (chats created later are not included)"
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
		s.cfg.Log.Info("grant created", "id", g.ID, "agent", g.Agent, "connector", g.Connector,
			"request", ch.Request, "replaces", ch.Replaces, "by", sess.who())
		res = accessApproveResponse{Kind: ch.Kind, ID: g.ID}
	default:
		connector.WriteError(w, fmt.Errorf("stored challenge of kind %q", ch.Kind))
		return
	}
	connector.WriteJSON(w, http.StatusOK, res)
}

func accessConflict(err error) error {
	switch {
	case errors.Is(err, errNoChallenge), errors.Is(err, errNoAgent), errors.Is(err, errReplaced):
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

// closeGrant ends a window or pauses a routine; it needs only the session.
func (s *Server) closeGrant(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	s.endAccess(w, r, s.db.revokeGrant(r.Context(), id, s.now()), "grant closed", id, sess)
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

// stopAll closes every grant and denies every access request; proposals
// each still wait for a passkey.
func (s *Server) stopAll(w http.ResponseWriter, r *http.Request, sess session) {
	grants, requests, err := s.db.stopAll(r.Context(), s.now())
	if err != nil {
		s.cfg.Log.Error("stop all", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't stop everything.")
		return
	}
	s.cfg.Log.Info("stopped all access", "grants", grants, "requests", requests, "by", sess.who())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// endAccess answers a session-only change by going back to the page it
// came from.
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
	http.Redirect(w, r, localPath(r.FormValue("next")), http.StatusSeeOther)
}
