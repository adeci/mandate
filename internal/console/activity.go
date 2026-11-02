package console

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/adeci/mandate/internal/proposal"
)

// sessionGap splits a run of reads of one chat into sessions.
const sessionGap = 10 * time.Minute

// event is one line of the activity timeline, newest first. Actor is who
// acted: "you", "agent", "system" for a connector's outcome, or empty for
// what simply ran out.
type event struct {
	At      time.Time `json:"at"`
	End     time.Time `json:"end,omitzero"`
	Kind    string    `json:"kind"` // read, send, grant, request, stop
	Actor   string    `json:"actor"`
	Text    string    `json:"text"`
	Dropped int       `json:"dropped,omitempty"`
	URL     string    `json:"url,omitempty"`
}

// activity says what happened in [from, to) in the owner's terms, one line
// per event: a request and the grant answering it are one line, a Stop all
// is one line, and reads are grouped into sessions per agent, connector,
// chat and kind of read.
func activity(reads []proposal.ReadEvent, decided []row, asked []accessRequest, grants []grantRow, stops []stop,
	names map[string]string, fo map[string]bool, from, to time.Time,
) []event {
	in := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	agent := func(id string) string { return cmpOr(names[id], "a deleted agent") }
	byID := map[string]grantRow{}
	replaced := map[string]grantRow{}
	for _, g := range grants {
		byID[g.ID] = g
		if g.ReplacedBy != "" {
			replaced[g.ReplacedBy] = g
		}
	}
	answered := map[string]bool{}
	var out []event
	add := func(at time.Time, kind, actor, text string) {
		out = append(out, event{At: at, Kind: kind, Actor: actor, Text: text})
	}

	for _, ar := range asked {
		who := ar.AgentName
		conn := connectorTitle(ar.Connector)
		switch ar.Status {
		case proposal.AccessGranted:
			g, ok := byID[ar.GrantID]
			if !ok {
				add(ar.DecidedAt, "grant", "you", fmt.Sprintf("You let %s read %s", who, conn))
				continue
			}
			answered[g.ID] = true
			switch {
			case ar.Extends != "":
				add(ar.DecidedAt, "grant", "you", extensionText(who, g, replaced[g.ID]))
			case g.Schedule != nil:
				add(ar.DecidedAt, "grant", "you", fmt.Sprintf("You approved %s's routine: %s", who, grantText(g, fo[g.Connector])))
			default:
				add(ar.DecidedAt, "grant", "you", fmt.Sprintf("You let %s read %s", who, grantText(g, fo[g.Connector])))
			}
		case proposal.AccessDenied:
			if ar.DecidedBy == "owner" || ar.DecidedBy == "" {
				add(ar.DecidedAt, "request", "you", fmt.Sprintf("You denied %s's request to read %s", who, conn))
			}
		case proposal.AccessExpired:
			add(ar.DecidedAt, "request", "", fmt.Sprintf("%s's request to read %s expired", who, conn))
		}
	}

	for _, g := range grants {
		who := agent(g.Agent)
		conn := connectorTitle(g.Connector)
		if in(g.CreatedAt) && !answered[g.ID] {
			old, extends := replaced[g.ID]
			switch {
			case extends && old.Schedule == nil:
				add(g.CreatedAt, "grant", "you", extensionText(who, g, old))
			case extends:
				add(g.CreatedAt, "grant", "you", fmt.Sprintf("You resumed %s's %s routine", who, conn))
			case g.Schedule != nil:
				add(g.CreatedAt, "grant", "you", fmt.Sprintf("You set up %s's routine: %s", who, grantText(g, fo[g.Connector])))
			default:
				add(g.CreatedAt, "grant", "you", fmt.Sprintf("You opened %s's access to %s", who, grantText(g, fo[g.Connector])))
			}
		}
		switch {
		case !g.RevokedAt.IsZero() && in(g.RevokedAt) && g.EndedBy == "close" && g.Schedule != nil:
			add(g.RevokedAt, "grant", "you", fmt.Sprintf("You paused %s's %s routine", who, conn))
		case !g.RevokedAt.IsZero() && in(g.RevokedAt) && g.EndedBy == "close":
			text := fmt.Sprintf("You closed %s's %s access", who, conn)
			if chats := grantChats(g); chats != "" {
				text += " (" + chats + ")"
			}
			add(g.RevokedAt, "grant", "you", text)
		case g.Schedule == nil && in(g.Until) && (g.RevokedAt.IsZero() || g.RevokedAt.After(g.Until)):
			add(g.Until, "grant", "", fmt.Sprintf("%s's %s access ran out", who, conn))
		}
	}

	for _, st := range stops {
		add(st.At, "stop", "you", fmt.Sprintf("You stopped all access (%s, %s)", count(st.Grants, "grant"), count(st.Requests, "request")))
	}

	for _, pr := range decided {
		e := event{At: pr.DecidedAt, Kind: "send", URL: "/r/" + pr.ID}
		what := sendText(pr)
		switch pr.Status {
		case proposal.Done:
			e.Actor, e.Text = "you", "You sent "+what
		case proposal.Rejected:
			if pr.DecidedBy == "agent deleted" {
				e.Text = upperFirst(what) + " was dropped with its agent"
			} else {
				e.Actor, e.Text = "you", "You denied "+what
			}
		case proposal.Failed:
			e.Actor, e.Text = "system", upperFirst(what)+" failed"
			if pr.Result != nil && pr.Result.Detail != "" {
				e.Text += ": " + cut(pr.Result.Detail, 60)
			}
		case proposal.Unknown:
			e.Actor, e.Text = "system", upperFirst(what)+" may or may not have gone out"
		case proposal.Expired:
			e.Text = upperFirst(what) + " expired"
		default:
			continue
		}
		out = append(out, e)
	}

	out = append(out, sessions(reads, agent)...)
	slices.SortStableFunc(out, func(a, b event) int { return b.At.Compare(a.At) })
	return out
}

// readKind groups the verbs a read event may carry.
func readKind(verb string) string {
	switch verb {
	case "chats", "sync":
		return "list"
	case "search", "guild-search":
		return "search"
	case "media":
		return "media"
	}
	return "read"
}

// sessions groups reads into runs per agent, connector, chat and kind with
// gaps under sessionGap.
func sessions(reads []proposal.ReadEvent, agent func(string) string) []event {
	slices.SortStableFunc(reads, func(a, b proposal.ReadEvent) int {
		return cmp.Or(cmp.Compare(a.AgentID, b.AgentID), cmp.Compare(a.Connector, b.Connector),
			cmp.Compare(a.Chat, b.Chat), cmp.Compare(readKind(a.Verb), readKind(b.Verb)), a.At.Compare(b.At))
	})
	var out []event
	var first proposal.ReadEvent
	var n, items, dropped int
	var last time.Time
	flush := func() {
		if n == 0 {
			return
		}
		who, conn := agent(first.AgentID), connectorTitle(first.Connector)
		chat := cmpOr(first.ChatName, "a hidden chat")
		var text string
		switch readKind(first.Verb) {
		case "list":
			text = fmt.Sprintf("%s listed %s chats", who, conn)
		case "search":
			text = fmt.Sprintf("%s searched %s · %s", who, conn, count(items, "result"))
		case "media":
			text = fmt.Sprintf("%s opened %s in %s", who, count(n, "file"), chat)
			if n == 1 {
				text = fmt.Sprintf("%s opened a file in %s", who, chat)
			}
		default:
			if first.Chat == "" {
				text = fmt.Sprintf("%s read %s · %s", who, conn, count(items, "item"))
			} else {
				text = fmt.Sprintf("%s read %s on %s · %s", who, chat, conn, count(items, "message"))
			}
		}
		e := event{At: first.At, Kind: "read", Actor: "agent", Text: text, Dropped: dropped}
		if n > 1 && last.Sub(first.At) >= time.Minute {
			e.End = last
			e.Text += fmt.Sprintf(" · %s–%s", first.At.Local().Format("15:04"), last.Local().Format("15:04"))
		}
		out = append(out, e)
	}
	for i, e := range reads {
		same := i > 0 && e.AgentID == first.AgentID && e.Connector == first.Connector && e.Chat == first.Chat &&
			readKind(e.Verb) == readKind(first.Verb) && e.At.Sub(last) < sessionGap
		if !same {
			flush()
			first, n, items, dropped = e, 0, 0, 0
		}
		n++
		items += e.Count
		dropped += e.Dropped
		last = e.At
	}
	flush()
	return out
}

// sendText names a proposal in sentences: "Grok Bot's WhatsApp message to
// ~Lena".
func sendText(pr row) string {
	noun := pr.Op
	switch pr.Op {
	case "send":
		noun = "message"
		if pr.Connector == "fastmail" {
			noun = "email"
		}
	case "reply":
		noun = "reply"
	}
	text := fmt.Sprintf("%s's %s %s", agentName(pr.AgentName), connectorTitle(pr.Connector), noun)
	if to, _, _ := sendParts(pr); to != "" {
		text += " to " + cut(to, 60)
	}
	return text
}

// grantText says what a grant opens: "Signal · Family, Priya · 1 hour ·
// last 7 days", or for a routine "Fastmail · Inbox · weekdays 07:00 for 20
// min".
func grantText(g grantRow, fo bool) string {
	parts := []string{connectorTitle(g.Connector)}
	if chats := grantChats(g); chats != "" {
		parts = append(parts, chats)
	}
	switch sc := g.Schedule; {
	case sc != nil:
		parts = append(parts, daysText(sc.Days)+" "+sc.Start+" for "+span(sc.Length))
	case fo:
		parts = append(parts, span(g.Until.Sub(g.From)))
	default:
		parts = append(parts, span(g.Until.Sub(g.From)), horizonText(g.Horizon))
	}
	return strings.Join(parts, " · ")
}

// grantChats names a grant's chats, or nothing for every chat.
func grantChats(g grantRow) string {
	names := make([]string, len(g.Chats))
	for i, id := range g.Chats {
		names[i] = cmpOr(g.Names[id], shortID(id))
	}
	return strings.Join(names, ", ")
}

// extensionText says how much longer an extension made old run.
func extensionText(who string, g grantRow, old grantRow) string {
	more := g.Until.Sub(old.Until)
	if old.ID == "" || more <= 0 {
		more = g.Until.Sub(g.From)
	}
	return fmt.Sprintf("You gave %s %s on %s (until %s)", who, moreTime(more), connectorTitle(g.Connector), g.Until.Local().Format("15:04"))
}

// moreTime says an extension's length: "30 more minutes", "1 more hour".
func moreTime(d time.Duration) string {
	switch {
	case d == time.Hour:
		return "1 more hour"
	case d%time.Hour == 0:
		return fmt.Sprintf("%d more hours", d/time.Hour)
	case d == time.Minute:
		return "1 more minute"
	}
	return fmt.Sprintf("%d more minutes", d/time.Minute)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
