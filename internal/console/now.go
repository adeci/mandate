package console

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const (
	// todayWindow is how far back one page of Today goes.
	todayWindow = 24 * time.Hour
	// sessionGap splits a run of reads of one chat into sessions.
	sessionGap = 10 * time.Minute
	// stoppedKept is how long a closed routine stays offered for resuming.
	stoppedKept = 30 * 24 * time.Hour
)

// nowData is the Now screen: what waits for the owner, what is open, and
// what happened. The page renders it and /now.json serves it.
type nowData struct {
	Waiting  []card      `json:"waiting"`
	Open     []grantView `json:"open"`
	Routines []grantView `json:"routines"`
	Stopped  []grantView `json:"stopped"`
	Today    []event     `json:"today"`
	// Older is the link to the day before Today.
	Older   string `json:"older"`
	CanSign bool   `json:"can_sign"`
}

// card is something waiting for the owner. Kind is read, routine or extend
// for an access request and send for a proposal.
type card struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Agent     string    `json:"agent"`
	Connector string    `json:"connector"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Reason is the agent's own words.
	Reason string `json:"reason,omitempty"`

	// Access requests: the chats named and those under them (Every when it
	// named none), the terms that matched nothing, and the window.
	Chats       []chatNode                `json:"chats,omitempty"`
	Unmatched   []string                  `json:"unmatched,omitempty"`
	Every       bool                      `json:"every,omitempty"`
	ForwardOnly bool                      `json:"forward_only,omitempty"`
	For         time.Duration             `json:"for,omitempty"`
	Horizon     time.Duration             `json:"horizon,omitempty"`
	Schedule    *proposal.RequestSchedule `json:"schedule,omitempty"`
	Extends     *grantView                `json:"extends,omitempty"`
	When        string                    `json:"when,omitempty"`
	HorizonText string                    `json:"horizon_text,omitempty"`
	// Window is the window in the Approve button's short words.
	Window string `json:"window,omitempty"`
	// Approve names what the passkey signs as the card first shows.
	Approve        string   `json:"approve,omitempty"`
	CanApprove     bool     `json:"can_approve"`
	ForOptions     []option `json:"for_options,omitempty"`
	HorizonOptions []option `json:"horizon_options,omitempty"`

	// Proposals: the op, its recipient and the first line it sends.
	Op   string `json:"op,omitempty"`
	To   string `json:"to,omitempty"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
}

// grantView is a grant as screens show it: its terms, its chats by name,
// and its state now.
type grantView struct {
	ID          string             `json:"id"`
	AgentID     string             `json:"agent_id"`
	Agent       string             `json:"agent"`
	Connector   string             `json:"connector"`
	Chats       []proposal.Chat    `json:"chats,omitempty"`
	ForwardOnly bool               `json:"forward_only,omitempty"`
	Horizon     time.Duration      `json:"horizon"`
	From        time.Time          `json:"from,omitzero"`
	Until       time.Time          `json:"until,omitzero"`
	Schedule    *approval.Schedule `json:"schedule,omitempty"`
	Note        string             `json:"note,omitempty"`
	Open        bool               `json:"open"`
	// Closes is when an open window closes, Next when a closed one opens.
	Closes    time.Time `json:"closes,omitzero"`
	Next      time.Time `json:"next,omitzero"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`
	// When, HorizonText and State say the above in words.
	When        string `json:"when"`
	HorizonText string `json:"horizon_text"`
	State       string `json:"state"`
	Tone        string `json:"tone"`
}

// event is one line of Today, newest first.
type event struct {
	At      time.Time `json:"at"`
	End     time.Time `json:"end,omitzero"`
	Kind    string    `json:"kind"` // read, send, grant, request
	Agent   string    `json:"agent"`
	Text    string    `json:"text"`
	Dropped int       `json:"dropped,omitempty"`
	URL     string    `json:"url,omitempty"`
}

func (s *Server) nowPage(w http.ResponseWriter, r *http.Request, sess session) {
	d, err := s.nowData(r.Context(), before(r))
	if err != nil {
		s.cfg.Log.Error("load now", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load what's going on.")
		return
	}
	s.owner(w, r, sess, "now", "Now", "now", d)
}

func (s *Server) nowJSON(w http.ResponseWriter, r *http.Request, _ session) {
	d, err := s.nowData(r.Context(), before(r))
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	connector.WriteJSON(w, http.StatusOK, d)
}

// before is where Today ends: now, or the ?before= of an older page.
func before(r *http.Request) time.Time {
	if v, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); err == nil {
		return time.UnixMilli(v).UTC()
	}
	return time.Time{}
}

func (s *Server) nowData(ctx context.Context, until time.Time) (nowData, error) {
	now := s.now()
	if until.IsZero() || until.After(now) {
		until = now
	}
	d := nowData{CanSign: len(s.keys.Trusted()) > 0}
	agents, err := s.db.agents(ctx)
	if err != nil {
		return d, err
	}
	names := map[string]string{}
	for _, a := range agents {
		names[a.ID] = a.Name
	}
	fo := s.forwardOnlyAll(ctx)

	grants, err := s.db.grantsSince(ctx, now.Add(-max(stoppedKept, now.Sub(until)+todayWindow)))
	if err != nil {
		return d, err
	}
	byID := map[string]grantRow{}
	for _, g := range grants {
		byID[g.ID] = g
		agent, ok := names[g.Agent]
		if !ok {
			continue
		}
		v := grantViewOf(g, agent, fo[g.Connector], now)
		switch {
		case g.RevokedAt.IsZero() && g.Schedule != nil:
			d.Routines = append(d.Routines, v)
		case g.RevokedAt.IsZero() && v.Open:
			d.Open = append(d.Open, v)
		case !g.RevokedAt.IsZero() && g.Schedule != nil && g.ReplacedBy == "" && now.Sub(g.RevokedAt) < stoppedKept:
			d.Stopped = append(d.Stopped, v)
		}
	}

	requests, err := s.db.pendingAccessRequests(ctx, now)
	if err != nil {
		return d, err
	}
	for _, ar := range requests {
		d.Waiting = append(d.Waiting, s.requestCard(ctx, ar, byID, fo[ar.Connector], d.CanSign, now))
	}
	pending, err := s.db.pending(ctx)
	if err != nil {
		return d, err
	}
	for _, pr := range pending {
		d.Waiting = append(d.Waiting, sendCard(pr))
	}
	slices.SortStableFunc(d.Waiting, func(a, b card) int { return b.CreatedAt.Compare(a.CreatedAt) })

	from := until.Add(-todayWindow)
	reads, err := s.db.reads(ctx, from, until)
	if err != nil {
		return d, err
	}
	decided, err := s.db.decidedBetween(ctx, from, until)
	if err != nil {
		return d, err
	}
	asked, err := s.db.accessRequestsBetween(ctx, from, until, now)
	if err != nil {
		return d, err
	}
	d.Today = today(reads, decided, asked, grants, names, from, until)
	d.Older = "/?before=" + strconv.FormatInt(from.UnixMilli(), 10)
	return d, nil
}

// forwardOnlyAll says which connectors keep no chats; one whose spec can't
// be read now counts as keeping chats.
func (s *Server) forwardOnlyAll(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	for name := range s.conns {
		fo, err := s.forwardOnly(ctx, name)
		if err != nil {
			s.cfg.Log.Warn("connector spec", "connector", name, "err", err)
		}
		out[name] = fo
	}
	return out
}

func grantViewOf(g grantRow, agent string, fo bool, now time.Time) grantView {
	v := grantView{
		ID: g.ID, AgentID: g.Agent, Agent: agent, Connector: g.Connector, ForwardOnly: fo,
		Horizon: g.Horizon, From: g.From, Until: g.Until, Schedule: g.Schedule, Note: g.Note,
		RevokedAt: g.RevokedAt, When: windowText(g.Grant), HorizonText: horizonText(g.Horizon),
	}
	for _, id := range g.Chats {
		v.Chats = append(v.Chats, proposal.Chat{ID: id, Name: g.Names[id]})
	}
	if fo {
		v.HorizonText = "every allowlisted read"
	}
	open, at := g.Open(now)
	switch {
	case !g.RevokedAt.IsZero():
		v.State, v.Tone = "closed "+g.RevokedAt.Local().Format(stamp), "muted"
	case open:
		v.Open, v.Closes = true, at
		v.State, v.Tone = "open until "+at.Local().Format(stamp), "ok"
	case !at.IsZero():
		v.Next = at
		v.State, v.Tone = "next "+at.Local().Format(stamp), "wait"
	default:
		v.State, v.Tone = "ended", "muted"
	}
	return v
}

// requestCard shows an access request. Only an unambiguous request starts
// with its chat ticked: exactly one chat matched, a DM, under a name the
// owner gave it.
func (s *Server) requestCard(ctx context.Context, ar accessRequest, grants map[string]grantRow, fo, canSign bool, now time.Time) card {
	c := card{
		Kind: "read", ID: ar.ID, AgentID: ar.AgentID, Agent: ar.AgentName, Connector: ar.Connector,
		CreatedAt: ar.CreatedAt, ExpiresAt: ar.ExpiresAt, Reason: ar.Reason,
		Unmatched: ar.Unmatched, Every: !ar.names(), ForwardOnly: fo,
		For: ar.For, Horizon: ar.Horizon, Schedule: ar.Schedule,
		When: "for " + span(ar.For), HorizonText: horizonText(ar.Horizon), Window: short(ar.For),
		ForOptions:     spanOptions(forChoices, ar.For, ar.For, span),
		HorizonOptions: spanOptions(horizonChoices, ar.Horizon, ar.Horizon, horizonText),
	}
	switch {
	case ar.Extends != "":
		c.Kind, c.When, c.ForOptions, c.HorizonOptions = "extend", span(ar.For)+" more", nil, nil
		if g, ok := grants[ar.Extends]; ok {
			v := grantViewOf(g, ar.AgentName, fo, now)
			c.Extends, c.HorizonText = &v, v.HorizonText
		}
		c.Approve = "Approve +" + short(ar.For)
		c.CanApprove = canSign && c.Extends != nil && c.Extends.RevokedAt.IsZero()
		return c
	case ar.Schedule != nil:
		sc := ar.Schedule
		c.Kind, c.When, c.ForOptions = "routine", routineText(*sc), nil
		c.Window = daysText(sc.Days) + " " + sc.Start + " · " + short(sc.Length)
	}
	if fo {
		c.HorizonText, c.HorizonOptions = "every allowlisted read", nil
	}
	if ar.names() && !fo {
		tree, err := s.requestTree(ctx, ar)
		if err != nil {
			s.cfg.Log.Warn("chats under requested chats", "connector", ar.Connector, "err", err)
			for _, ch := range ar.Chats {
				tree = append(tree, chatNode{ID: ch.ID, Name: ch.Name, Label: visible(cmpOr(ch.Name, ch.ID))})
			}
		}
		if len(ar.Chats) == 1 && len(ar.Unmatched) == 0 && len(tree) == 1 &&
			tree[0].Kind == "dm" && !strings.HasPrefix(tree[0].Name, "~") {
			tree[0].Ticked = true
		}
		c.Chats = tree
	}
	c.Approve = approveLabel(c)
	c.CanApprove = canSign && (c.Every || len(c.Chats) > 0) && (!fo || !ar.names())
	return c
}

// approveLabel names what approving signs before any adjusting: the window,
// the chats ticked, and the horizon.
func approveLabel(c card) string {
	parts := []string{"Approve", c.Window}
	switch {
	case c.ForwardOnly:
		return strings.Join(append(parts, "allowlisted reads"), " · ")
	case c.Every:
		parts = append(parts, "every chat")
	default:
		var ticked []string
		walkChats(c.Chats, func(n chatNode) {
			if n.Ticked {
				ticked = append(ticked, cmpOr(n.Name, n.ID))
			}
		})
		switch len(ticked) {
		case 0:
			parts = append(parts, "no chats yet")
		case 1:
			parts = append(parts, ticked[0])
		default:
			parts = append(parts, fmt.Sprintf("%d chats", len(ticked)))
		}
	}
	return strings.Join(append(parts, short(c.Horizon)), " · ")
}

// sendCard shows a proposal by its recipient, the first field not tied to
// a param, and the first line of the first field that is.
func sendCard(pr row) card {
	c := card{
		Kind: "send", ID: pr.ID, AgentID: pr.AgentID, Agent: agentName(pr.AgentName), Connector: pr.Connector,
		CreatedAt: pr.CreatedAt, ExpiresAt: pr.ExpiresAt, Reason: pr.Note, Op: pr.Op, URL: "/r/" + pr.ID,
	}
	for _, f := range pr.Display {
		switch {
		case f.Param == "" && c.To == "":
			c.To = f.Value
		case f.Param != "" && c.Text == "":
			c.Text, _, _ = strings.Cut(f.Value, "\n")
		}
	}
	return c
}

// today is what happened in [from, to), newest first: reads grouped into
// sessions per agent, connector and chat, proposals decided, grants opened,
// closed and run out, and requests made.
func today(reads []proposal.ReadEvent, decided []row, asked []accessRequest, grants []grantRow,
	names map[string]string, from, to time.Time,
) []event {
	var out []event
	agent := func(id string) string { return cmpOr(names[id], "a deleted agent") }

	slices.SortStableFunc(reads, func(a, b proposal.ReadEvent) int {
		return cmp.Or(cmp.Compare(a.AgentID, b.AgentID), cmp.Compare(a.Connector, b.Connector),
			cmp.Compare(a.Chat, b.Chat), a.At.Compare(b.At))
	})
	var cur *event
	var count int
	var chat string
	flush := func() {
		if cur == nil {
			return
		}
		what := "items"
		if chat != "" {
			what = "messages"
		}
		cur.Text = fmt.Sprintf("%s · %d %s · %s–%s", cur.Text, count, what,
			cur.At.Local().Format("15:04"), cur.End.Local().Format("15:04"))
		out = append(out, *cur)
	}
	var prev proposal.ReadEvent
	for i, e := range reads {
		same := i > 0 && e.AgentID == prev.AgentID && e.Connector == prev.Connector && e.Chat == prev.Chat &&
			e.At.Sub(prev.At) < sessionGap
		if !same {
			flush()
			subject := "chats"
			if e.Chat != "" {
				subject = cmpOr(e.ChatName, "a hidden chat")
			}
			cur = &event{
				At: e.At, Kind: "read", Agent: agent(e.AgentID),
				Text: fmt.Sprintf("%s read %s on %s", agent(e.AgentID), subject, e.Connector),
			}
			count, chat = 0, e.Chat
		}
		cur.End = e.At
		cur.Dropped += e.Dropped
		count += e.Count
		prev = e
	}
	flush()

	for _, pr := range decided {
		out = append(out, event{
			At: pr.DecidedAt, Kind: "send", Agent: agentName(pr.AgentName), URL: "/r/" + pr.ID,
			Text: fmt.Sprintf("%s's %s %s: %s", agentName(pr.AgentName), pr.Connector, pr.Op, pr.Status),
		})
	}
	for _, ar := range asked {
		out = append(out, event{
			At: ar.CreatedAt, Kind: "request", Agent: ar.AgentName,
			Text: fmt.Sprintf("%s asked to read %s: %s", ar.AgentName, ar.Connector, ar.Status),
		})
	}
	in := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	for _, g := range grants {
		who := agent(g.Agent)
		if in(g.CreatedAt) {
			out = append(out, event{
				At: g.CreatedAt, Kind: "grant", Agent: who,
				Text: fmt.Sprintf("%s may read %s, %s", who, g.Connector, windowText(g.Grant)),
			})
		}
		switch {
		case !g.RevokedAt.IsZero() && in(g.RevokedAt) && g.ReplacedBy == "" && (g.Schedule != nil || g.RevokedAt.Before(g.Until)):
			out = append(out, event{
				At: g.RevokedAt, Kind: "grant", Agent: who,
				Text: fmt.Sprintf("Closed %s's access to %s", who, g.Connector),
			})
		case g.Schedule == nil && in(g.Until) && (g.RevokedAt.IsZero() || g.RevokedAt.After(g.Until)):
			out = append(out, event{
				At: g.Until, Kind: "grant", Agent: who,
				Text: fmt.Sprintf("%s's access to %s ran out", who, g.Connector),
			})
		}
	}
	slices.SortStableFunc(out, func(a, b event) int { return b.At.Compare(a.At) })
	return out
}
