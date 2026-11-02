package console

import (
	"context"
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

// stoppedKept is how long a closed routine stays offered for resuming.
const stoppedKept = 30 * 24 * time.Hour

// nowData is the Now screen: what waits for the owner, what is open, and
// what happened on Day. The page renders it and /now.json serves it.
type nowData struct {
	// Notice reports what the owner just did, e.g. Stop all.
	Notice   string      `json:"notice,omitempty"`
	Waiting  []card      `json:"waiting"`
	Open     []grantView `json:"open"`
	Routines []grantView `json:"routines"`
	Stopped  []grantView `json:"stopped"`
	// Day names the day Today shows ("Today", "Yesterday", "Sun 1 Nov");
	// Older and Newer link to the days around it.
	Day     string  `json:"day"`
	Today   []event `json:"today"`
	Older   string  `json:"older"`
	Newer   string  `json:"newer,omitempty"`
	CanSign bool    `json:"can_sign"`
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

	// Access requests: the chats named and those under them (Every when a
	// read or routine named none), the terms that matched nothing, and the
	// window.
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
	// Window is the window as the Approve button says it.
	Window string `json:"window,omitempty"`
	// Approve names what the passkey signs as the card first shows; Ready
	// is whether a chat is ticked yet.
	Approve        string   `json:"approve,omitempty"`
	Ready          bool     `json:"ready"`
	CanApprove     bool     `json:"can_approve"`
	ForOptions     []option `json:"for_options,omitempty"`
	HorizonOptions []option `json:"horizon_options,omitempty"`

	// Proposals: the op, its recipient, and its subject and first line.
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

func (s *Server) nowPage(w http.ResponseWriter, r *http.Request, sess session) {
	d, err := s.nowData(r.Context(), r)
	if err != nil {
		s.cfg.Log.Error("load now", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load what's going on.")
		return
	}
	s.owner(w, r, sess, "now", "Now", "now", d)
}

func (s *Server) nowJSON(w http.ResponseWriter, r *http.Request, _ session) {
	d, err := s.nowData(r.Context(), r)
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	connector.WriteJSON(w, http.StatusOK, d)
}

// nowData answers ?day=YYYY-MM-DD with that day's activity, and
// ?stopped=GRANTS.REQUESTS or ?denied=PROPOSAL with a notice of what the
// owner just did.
func (s *Server) nowData(ctx context.Context, r *http.Request) (nowData, error) {
	now := s.now()
	q := r.URL.Query()
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

	today := startOfDay(now)
	day := today
	if t, err := time.ParseInLocation(time.DateOnly, q.Get("day"), time.Local); err == nil && t.Before(today) {
		day = t
	}
	next := day.AddDate(0, 0, 1)
	d.Day = dayName(day, today)
	d.Older = "/?day=" + day.AddDate(0, 0, -1).Format(time.DateOnly) + "#today"
	switch {
	case next.Equal(today):
		d.Newer = "/#today"
	case next.Before(today):
		d.Newer = "/?day=" + next.Format(time.DateOnly) + "#today"
	}
	to := earlier(next, now)

	grants, err := s.db.grantsSince(ctx, earlier(now.Add(-stoppedKept), day))
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

	reads, err := s.db.reads(ctx, day, to)
	if err != nil {
		return d, err
	}
	decided, err := s.db.decidedBetween(ctx, day, to)
	if err != nil {
		return d, err
	}
	asked, err := s.db.accessRequestsDecided(ctx, day, to, now)
	if err != nil {
		return d, err
	}
	stops, err := s.db.stopsBetween(ctx, day, to)
	if err != nil {
		return d, err
	}
	d.Today = activity(reads, decided, asked, grants, stops, names, fo, day, to)
	d.Notice = s.notice(ctx, q)
	return d, nil
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Local().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

func dayName(day, today time.Time) string {
	switch {
	case day.Equal(today):
		return "Today"
	case day.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	}
	return day.Format("Mon 2 Jan")
}

// notice says what the owner just did, from what the redirect after it
// carries; it reads only numbers and IDs from the URL, never text.
func (s *Server) notice(ctx context.Context, q map[string][]string) string {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if v := get("stopped"); v != "" {
		g, r, _ := strings.Cut(v, ".")
		grants, err1 := strconv.Atoi(g)
		requests, err2 := strconv.Atoi(r)
		if err1 == nil && err2 == nil {
			return fmt.Sprintf("Stopped: %s closed, %s denied", count(grants, "grant"), count(requests, "request"))
		}
	}
	if id := get("denied"); id != "" {
		if pr, err := s.db.get(ctx, id); err == nil {
			return "You denied " + sendText(pr) + "."
		}
	}
	return ""
}

// count is n things in words: "1 grant", "3 grants".
func count(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return strconv.Itoa(n) + " " + thing + "s"
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
		v.State, v.Tone = endedState(g), "muted"
	case open:
		v.Open, v.Closes = true, at
		v.State, v.Tone = "until "+moment(at, now), "ok"
	case !at.IsZero():
		v.Next = at
		v.State, v.Tone = "next "+moment(at, now), "wait"
	default:
		v.State, v.Tone = "ended", "muted"
	}
	return v
}

// endedState says how a revoked grant ended.
func endedState(g grantRow) string {
	switch {
	case g.EndedBy == "stop":
		return "stopped by Stop all"
	case g.EndedBy == "replaced" && g.Schedule == nil:
		return "extended"
	case g.EndedBy == "agent deleted":
		return "agent deleted"
	case g.Schedule != nil:
		return "paused"
	}
	return "closed"
}

// moment is a time as a clock today and with its date otherwise.
func moment(t, now time.Time) string {
	if startOfDay(t).Equal(startOfDay(now)) {
		return t.Local().Format("15:04")
	}
	return t.Local().Format(stamp)
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
		When: "for " + span(ar.For), HorizonText: horizonText(ar.Horizon), Window: span(ar.For),
		ForOptions:     spanOptions(forChoices, ar.For, ar.For, span),
		HorizonOptions: spanOptions(horizonChoices, ar.Horizon, ar.Horizon, horizonText),
	}
	switch {
	case ar.Extends != "":
		c.Kind, c.Every, c.When = "extend", false, span(ar.For)+" more"
		c.ForOptions, c.HorizonOptions = nil, nil
		if g, ok := grants[ar.Extends]; ok {
			v := grantViewOf(g, ar.AgentName, fo, now)
			c.Extends, c.HorizonText = &v, v.HorizonText
		}
		c.Approve, c.Ready = "Approve · "+span(ar.For)+" more", true
		c.CanApprove = canSign && c.Extends != nil && c.Extends.RevokedAt.IsZero()
		return c
	case ar.Schedule != nil:
		c.Kind, c.When, c.ForOptions = "routine", routineText(*ar.Schedule), nil
		c.Window = scheduleWords(*ar.Schedule)
	}
	if fo {
		c.HorizonText, c.HorizonOptions = "every allowlisted read", nil
	}
	if ar.names() && !fo {
		tree, err := s.requestTree(ctx, ar)
		if err != nil {
			s.cfg.Log.Warn("chats under requested chats", "connector", ar.Connector, "err", err)
			for _, ch := range ar.Chats {
				tree = append(tree, chatNodeOf(connector.Container{ID: ch.ID, Name: ch.Name}))
			}
		}
		if len(ar.Chats) == 1 && len(ar.Unmatched) == 0 && len(tree) == 1 &&
			tree[0].Kind == "dm" && !strings.HasPrefix(tree[0].Name, "~") {
			tree[0].Ticked = true
		}
		c.Chats = tree
	}
	c.Approve, c.Ready = approveLabel(c)
	c.CanApprove = canSign && (c.Every || len(c.Chats) > 0) && (!fo || !ar.names())
	return c
}

// approveLabel names what approving signs before any adjusting: the window,
// the chats ticked, and the horizon. ready is false while no chat is.
func approveLabel(c card) (label string, ready bool) {
	parts := []string{"Approve", c.Window}
	switch {
	case c.ForwardOnly:
		return strings.Join(append(parts, "allowlisted reads"), " · "), true
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
			return "Approve", false
		case 1:
			parts = append(parts, ticked[0])
		default:
			parts = append(parts, fmt.Sprintf("%d chats", len(ticked)))
		}
	}
	return strings.Join(append(parts, horizonText(c.Horizon)), " · "), true
}

// sendCard shows a proposal by its recipient and, for email, its subject,
// then its first line past any greeting.
func sendCard(pr row) card {
	to, subject, text := sendParts(pr)
	if subject != "" && text != "" {
		text = subject + " — " + text
	} else if subject != "" {
		text = subject
	}
	return card{
		Kind: "send", ID: pr.ID, AgentID: pr.AgentID, Agent: agentName(pr.AgentName), Connector: pr.Connector,
		CreatedAt: pr.CreatedAt, ExpiresAt: pr.ExpiresAt, Reason: pr.Note, Op: pr.Op, URL: "/r/" + pr.ID,
		To: to, Text: text,
	}
}

// sendParts reads a proposal's display: whom it goes to (the field naming
// the recipient, else the first not tied to a param), its subject, and the
// first line of its text that isn't a greeting.
func sendParts(pr row) (to, subject, text string) {
	var first string
	for _, f := range pr.Display {
		switch {
		case f.Label == "To" || f.Label == "Group" || f.Label == "Room" || f.Label == "Chat":
			if to == "" {
				to = f.Value
			}
		case f.Label == "Subject":
			subject = f.Value
		case f.Param != "" && text == "":
			text = firstLine(f.Value)
		case f.Param == "" && first == "":
			first = f.Value
		}
	}
	return cmpOr(to, first), subject, text
}

// greetings start the lines firstLine skips, with a short line ending in a
// comma or colon.
var greetings = []string{"hi", "hello", "hey", "dear", "hallo", "liebe", "lieber", "moin", "guten", "good morning", "good afternoon", "good evening"}

// firstLine is the first line of text that isn't blank or a greeting, cut
// to a phrase.
func firstLine(text string) string {
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		greeting := len(strings.Fields(line)) <= 4 && (strings.HasSuffix(line, ",") || strings.HasSuffix(line, ":")) ||
			slices.ContainsFunc(greetings, func(g string) bool { return lower == g || strings.HasPrefix(lower, g+" ") && len(line) < 30 })
		if line == "" || greeting {
			continue
		}
		if utf8.RuneCountInString(line) > 80 {
			line = string([]rune(line)[:80]) + "…"
		}
		return line
	}
	return ""
}
