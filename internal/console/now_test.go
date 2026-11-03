package console

import (
	"context"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

// TestNowRendersEveryCard checks the markup the page's script relies on,
// card by card: which cards carry a chat choice, a picker, Adjust and an
// Approve button naming its window.
func TestNowRendersEveryCard(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	agent, other, third := h.agent(t, "bot"), h.agent(t, "other"), h.agent(t, "third")
	h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Agent: other, Connector: "fake", For: "1h"})
	h.requestAccess(t, named(agent))
	h.requestAccess(t, proposal.AccessRequest{AgentID: agent, Connector: "fake", For: time.Hour, Reason: "every"})
	h.requestAccess(t, proposal.AccessRequest{
		AgentID: agent, Connector: "fake", Terms: []string{"Dad"}, Chats: []proposal.Chat{{ID: "d2", Name: "Dad"}}, Reason: "routine",
		Schedule: &proposal.RequestSchedule{Days: []time.Weekday{1, 2, 3, 4, 5}, Start: "07:00", Length: 20 * time.Minute},
	})
	h.requestAccess(t, proposal.AccessRequest{AgentID: other, Connector: "fake", Extend: true, For: 30 * time.Minute, Reason: "more"})
	h.requestAccess(t, proposal.AccessRequest{
		AgentID: third, Connector: "fake", Terms: []string{"lawyer"}, Unmatched: []string{"lawyer"}, For: time.Hour, Reason: "nothing",
	})
	if _, err := h.api.Submit(ctx, proposal.Request{
		Connector: "fake", Op: "send", Args: map[string]string{"to": "Farid", "text": "hi"}, AgentID: third, AgentName: "third",
	}); err != nil {
		t.Fatal(err)
	}

	_, page := h.page(t, "/")
	open := regexp.MustCompile(`<section class="panel card"[^>]*>`)
	attr := func(tag, name string) (string, bool) {
		m := regexp.MustCompile(` ` + name + `(?:="([^"]*)")?[ >]`).FindStringSubmatch(tag)
		if m == nil {
			return "", false
		}
		return m[1], true
	}
	kinds := map[string]bool{}
	for _, loc := range open.FindAllStringIndex(page, -1) {
		tag := page[loc[0]:loc[1]]
		body, _, _ := strings.Cut(page[loc[1]:], "</section>")
		kind, _ := attr(tag, "data-card")
		chats, hasChats := attr(tag, "data-chats")
		kinds[kind+"/"+chats] = true
		if _, ok := attr(tag, "data-id"); !ok {
			t.Errorf("%s card has no data-id", kind)
		}
		if _, ok := attr(tag, "data-access"); !ok {
			t.Errorf("%s card has no data-access", kind)
		}
		picker := strings.Contains(body, "data-chat-picker")
		switch kind {
		case "read", "routine":
			if !hasChats || picker != (chats == "every" || chats == "pick") {
				t.Errorf("%s card: data-chats %q, picker %v", kind, chats, picker)
			}
			if !strings.Contains(body, "data-adjust") || !regexp.MustCompile(`data-approve-access data-span="[^"]+"`).MatchString(body) {
				t.Errorf("%s card lacks Adjust or an Approve naming its window", kind)
			}
		case "extend":
			if hasChats || picker || strings.Contains(body, "data-adjust") || !strings.Contains(body, "data-approve-access") {
				t.Errorf("extend card: data-chats %v, picker %v, or no Approve", hasChats, picker)
			}
		case "send":
			if hasChats || !strings.Contains(body, `href="/r/`) || !strings.Contains(body, "data-approve-send=") {
				t.Errorf("send card: data-chats %v, or no Review link or Approve", hasChats)
			}
		default:
			t.Errorf("card of kind %q", kind)
		}
	}
	for _, want := range []string{"read/named", "read/every", "read/pick", "routine/named", "extend/", "send/"} {
		if !kinds[want] {
			t.Errorf("no %s card among %v", want, kinds)
		}
	}
	if !strings.Contains(html.UnescapeString(page), `Nothing matched "lawyer" on fake.`) {
		t.Error("a request that matched nothing doesn't say so")
	}
	// Stop all asks first without any script, at the bottom.
	stop := strings.Index(page, `<section class="panel stop-all">`)
	if stop < strings.Index(page, `id="today"`) || !strings.Contains(page[stop:], `Waiting sends stay.`) {
		t.Error("Stop all isn't below Today with a confirmation step")
	}
}

func TestActivityInOwnersTerms(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.Local)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	names := map[string]string{"a": "Grok Bot", "b": "Morning brief"}
	window := grantRow{
		Grant: approval.Grant{
			ID: "g1", Agent: "a", Connector: "signal", Chats: []string{"fam"},
			Horizon: 7 * 24 * time.Hour, From: at(21, 0), Until: at(22, 0), CreatedAt: at(21, 0),
		},
		Names: map[string]string{"fam": "Family"}, RevokedAt: at(21, 30), ReplacedBy: "g2", EndedBy: "replaced",
	}
	longer := window
	longer.ID, longer.CreatedAt, longer.From, longer.Until = "g2", at(21, 30), at(21, 30), at(22, 30)
	longer.RevokedAt, longer.ReplacedBy, longer.EndedBy = at(23, 0), "", "close"
	routine := grantRow{Grant: approval.Grant{
		ID: "g3", Agent: "b", Connector: "fastmail", Chats: []string{"in"},
		Schedule:  &approval.Schedule{Days: []time.Weekday{1, 2, 3, 4, 5}, Start: "07:00", Length: 20 * time.Minute, Zone: "UTC"},
		CreatedAt: at(8, 0),
	}, Names: map[string]string{"in": "Inbox"}, RevokedAt: at(9, 0), EndedBy: "close"}
	opened := grantRow{Grant: approval.Grant{
		ID: "g4", Agent: "a", Connector: "signal", Chats: []string{"fam", "pr"},
		Horizon: 7 * 24 * time.Hour, From: at(20, 0), Until: at(24, 0), CreatedAt: at(20, 0),
	}, Names: map[string]string{"fam": "Family", "pr": "Priya Raman"}}
	asked := []accessRequest{
		{AccessRequest: proposal.AccessRequest{AgentName: "Grok Bot", Connector: "signal"}, Status: proposal.AccessGranted, GrantID: "g1", DecidedAt: at(21, 0)},
		{AccessRequest: proposal.AccessRequest{AgentName: "Grok Bot", Connector: "signal", Extend: true}, Extends: "g1", Status: proposal.AccessGranted, GrantID: "g2", DecidedAt: at(21, 30)},
		{AccessRequest: proposal.AccessRequest{AgentName: "Grok Bot", Connector: "discord"}, Status: proposal.AccessExpired, DecidedAt: at(10, 0)},
		{AccessRequest: proposal.AccessRequest{AgentName: "Grok Bot", Connector: "discord"}, Status: proposal.AccessDenied, DecidedBy: "owner", DecidedAt: at(11, 0)},
		{AccessRequest: proposal.AccessRequest{AgentName: "Grok Bot", Connector: "discord"}, Status: proposal.AccessDenied, DecidedBy: "stop", DecidedAt: at(12, 0)},
		{
			AccessRequest: proposal.AccessRequest{AgentName: "Morning brief", Connector: "fastmail", Schedule: &proposal.RequestSchedule{}},
			Status:        proposal.AccessGranted, GrantID: "g3", DecidedAt: at(8, 0),
		},
	}
	reads := []readSession{
		{AgentID: "a", Connector: "signal", Chat: "fam", ChatName: "Family", Kind: "read", First: at(22, 3), Last: at(22, 30), Reads: 4, Items: 140, Dropped: 2},
		{AgentID: "a", Connector: "signal", Kind: "list", First: at(21, 5), Last: at(21, 5), Reads: 1, Items: 9},
		{AgentID: "a", Connector: "signal", Kind: "search", First: at(21, 6), Last: at(21, 6), Reads: 1, Items: 3},
		{AgentID: "a", Connector: "signal", Chat: "fam", ChatName: "Family", Kind: "media", First: at(21, 7), Last: at(21, 7), Reads: 1, Items: 1},
	}
	decided := []row{
		{
			Proposal: proposal.Proposal{
				ID: "p1", Connector: "whatsapp", Op: "send", Status: proposal.Done,
				Display: []approval.Field{{Label: "To", Value: "~Lena"}, {Label: "Text", Value: "hi", Param: "text"}},
			},
			AgentName: "Grok Bot", DecidedAt: at(13, 0),
		},
		{Proposal: proposal.Proposal{
			ID: "p2", Connector: "whatsapp", Op: "send", Status: proposal.Rejected,
			Display: []approval.Field{{Label: "To", Value: "~Lena"}},
		}, AgentName: "Grok Bot", DecidedAt: at(14, 0)},
		{
			Proposal: proposal.Proposal{
				ID: "p3", Connector: "whatsapp", Op: "send", Status: proposal.Failed,
				Display: []approval.Field{{Label: "To", Value: "~Lena"}}, Result: &connector.Result{Detail: "not on WhatsApp"},
			},
			AgentName: "Grok Bot", DecidedAt: at(15, 0),
		},
	}
	stops := []stop{{At: at(12, 0), Grants: 3, Requests: 1}}
	got := activity(reads, decided, asked, []grantRow{window, longer, routine, opened}, stops, names, map[string]bool{}, day, day.Add(24*time.Hour))
	var texts []string
	for _, e := range got {
		texts = append(texts, e.Text)
	}
	want := []string{
		"You closed Grok Bot's Signal access (Family)",
		"Grok Bot read Family on Signal · 140 messages · 22:03–22:30",
		"You gave Grok Bot 30 more minutes on Signal (until 22:30)",
		"Grok Bot opened a file in Family",
		"Grok Bot searched Signal · 3 results",
		"Grok Bot listed Signal chats",
		"You let Grok Bot read Signal · Family · 1 hour · last 7 days",
		"You opened Grok Bot's access to Signal · Family, Priya Raman · 4 hours · last 7 days",
		"Grok Bot's WhatsApp message to ~Lena failed: not on WhatsApp",
		"You denied Grok Bot's WhatsApp message to ~Lena",
		"You sent Grok Bot's WhatsApp message to ~Lena",
		"You stopped all access (3 grants, 1 request)",
		"You denied Grok Bot's request to read Discord",
		"Grok Bot's request to read Discord expired",
		"You paused Morning brief's Fastmail routine",
		"You approved Morning brief's routine: Fastmail · Inbox · weekdays 07:00 for 20 min",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Errorf("activity:\n%s\nwant:\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}
	for _, e := range got {
		if e.Text == want[1] && e.Dropped != 2 {
			t.Errorf("session dropped = %d", e.Dropped)
		}
		if strings.HasSuffix(e.Text, "failed: not on WhatsApp") && e.Actor != "system" {
			t.Errorf("a failed send is marked %q", e.Actor)
		}
	}
}

func TestWords(t *testing.T) {
	for id, want := range map[string]string{
		"4915123456789@s.whatsapp.net": "+49 151 2345 6789",
		"15551234567@s.whatsapp.net":   "+1 555 123 4567",
		"+4915123456789":               "+49 151 2345 6789",
		"120363041234567890@g.us":      "…567890",
		"1234567890123456789":          "…456789",
		"AbCdEfGhIjKlAxOQ==":           "…KlAxOQ",
		"c1":                           "c1",
	} {
		if got := shortID(id); got != want {
			t.Errorf("shortID(%q) = %q, want %q", id, got, want)
		}
	}
	email := row{Proposal: proposal.Proposal{Display: []approval.Field{
		{Label: "From", Value: "me@example.com"},
		{Label: "To", Value: "Dana <dana@example.com>"},
		{Label: "Subject", Value: "Re: Lease renewal", Param: "subject"},
		{Label: "Text", Value: "Hi Dana,\n\nThanks for sending over the renewal.\nBest", Param: "body"},
	}}}
	if c := sendCard(email); c.To != "Dana <dana@example.com>" || c.Text != "Re: Lease renewal — Thanks for sending over the renewal." {
		t.Errorf("email card: to %q, text %q", c.To, c.Text)
	}
}
