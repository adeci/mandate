package gateway

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
	"github.com/adeci/mandate/store/storetest"
)

// Agents' terms resolve against the chats each connector really stores:
// the fixtures are written by the connectors' own code and checked by
// their tests.
func TestMatchesConnectorChats(t *testing.T) {
	for name, terms := range map[string]map[string][]string{
		"discord": { // main's #general has main's own ID, as in old servers
			"main general":    {"1100000000000000001", "1100000000000000016"}, // then the voice channel's chat
			"main / general":  {"1100000000000000001", "1100000000000000016"},
			"main / #general": {"1100000000000000001", "1100000000000000016"},
			"main #general":   {"1100000000000000001", "1100000000000000016"},
			"general":         {"1100000000000000001", "1100000000000000016", "1200000000000000011"},
			// A holder's own name asks for all of it.
			"main":     {"guild:1100000000000000001"},
			"other":    {"guild:1200000000000000001"},
			"rollout":  {"1100000000000000013"},
			"help":     {"1100000000000000018"},
			"crash":    {"1100000000000000019"},
			"lounge":   nil, // a voice channel never used for text isn't stored
			"sam":      {"1300000000000000001"},
			"sam_p":    {"1300000000000000001"}, // by his @handle, not the group he is in
			"@sam_p":   {"1300000000000000001"},
			"climbing": {"1300000000000000002"},
		},
		"fastmail": {
			"Inbox":            {"mailbox:mb-inbox"},
			"archive":          {"mailbox:mb-archive"},
			"Receipts":         {"mailbox:mb-rec"},
			"Inbox / Receipts": {"mailbox:mb-rec"},
			"lunch":            {"t1"},
			"shipped":          {"t9"},
		},
		"signal": {
			"Farid":         {"00000000-0000-4000-8000-000000000002"},
			"mom":           {"00000000-0000-4000-8000-000000000004"},
			"+15557654321":  {"00000000-0000-4000-8000-000000000005"},
			"+15551234567":  {"00000000-0000-4000-8000-000000000002"}, // Farid Haddad, by his number
			"Climbing crew": {"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		},
		"whatsapp": {
			"Sam":           {"111222333@lid", "15550001111@s.whatsapp.net", "447700900123@s.whatsapp.net"},
			"Sam Lee":       {"15550001111@s.whatsapp.net"},
			"Book club":     {"120363000000000999@g.us"},
			"Climbing crew": {"120363000000000777@g.us", "120363000000000888@g.us"},
			"+15550004444":  {"15550004444@s.whatsapp.net"},
			"+15551234567":  {"15551234567@s.whatsapp.net"}, // Farid Haddad, by the number in the ID
			"555 123 4567":  {"15551234567@s.whatsapp.net"}, // without the country code
			"555":           {"15550004444@s.whatsapp.net"}, // by its name, not inside IDs or numbers
			"whatsapp":      nil,
			"+447700900123": {"447700900123@s.whatsapp.net", "111222333@lid"}, // then the LID chat, by its person's number
		},
		"matrix": {
			"Club / general":    {"!general:example.org"},
			"general":           {"!general:example.org", "!lobby:example.org"},
			"Team":              {"!team:example.org"},
			"Club":              {"!space:example.org"},
			"!team:example.org": {"!team:example.org"},
			"@bob:example.org":  {"!dm:example.org"},
		},
		"gmessages": {
			"Farid":           {"1"},
			"Family":          {"3"},
			"999":             {"2"},
			"Ana":             {"4"},
			"555 999 0002":    {"2"}, // named (555) 999-0002
			"555-999-0002":    {"2"},
			"+1 555 999 0002": {"2"},
			"+15551230001":    {"1"}, // Farid Haddad, by his number
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "data.db")
			st, err := store.Open(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			storetest.Load(t, st, filepath.Join("..", "..", "connectors", name, "testdata", "chats.json"))
			c := &conn{store: path}
			defer c.close()
			db, err := c.storeDB(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			v, err := newView(db, neverShared{}, everything, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			picker := servePicker(t, &fakeConn{st: st}, dir)
			for term, want := range terms {
				rows, err := v.matches(t.Context(), []string{term}, matchesPerTerm)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, r := range rows {
					got = append(got, r[1].(string))
				}
				if !eq(got, want) {
					t.Errorf("%q matches %v, want %v", term, got, want)
				}
				// The owner's picker finds what the request did, holders
				// among the headings.
				found, err := picker.Containers(t.Context(), connector.ContainerQuery{Q: term, Limit: matchesPerTerm})
				if err != nil {
					t.Fatal(err)
				}
				var names, pickerNames []string
				for _, r := range rows {
					n, _ := r[2].(string)
					names = append(names, n)
				}
				got = nil
				for _, f := range found {
					got = append(got, f.ID)
					path := []string{}
					for _, p := range f.Parents {
						path = append(path, p.Name)
					}
					pickerNames = append(pickerNames, strings.Join(append(path, f.Name), " / "))
				}
				if !eq(got, want) {
					t.Errorf("picker search %q finds %v, want %v", term, got, want)
				}
				if !eq(pickerNames, names) {
					t.Errorf("picker names %q as %q, the request as %q", term, pickerNames, names)
				}
			}
		})
	}
}

// fixtureView is a connector's fixture store as scope shows it.
func fixtureView(t *testing.T, name string, scope []rule) *view {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	storetest.Load(t, st, filepath.Join("..", "..", "connectors", name, "testdata", "chats.json"))
	c := &conn{store: path}
	t.Cleanup(c.close)
	db, err := c.storeDB(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	v, err := newView(db, neverShared{}, scope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Agents see the names the picker shows: full paths, parents included
// when only the child is granted, and never a guild or space.
func TestAgentChatNames(t *testing.T) {
	listing := func(t *testing.T, v *view, search string) []string {
		t.Helper()
		tab, err := v.chats(t.Context(), search, 50)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range tab.Rows {
			out = append(out, r[0].(string)+" "+r[2].(string))
		}
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		connector string
		chats     []string // nil: every chat
		search    string
		want      []string
	}{
		{"discord", []string{"1100000000000000001", "1200000000000000011", "1100000000000000013"}, "", []string{
			"1100000000000000001 main / #general", "1100000000000000013 main / #releases / v2 rollout", "1200000000000000011 other / #general",
		}},
		{"discord", nil, "", []string{
			"1100000000000000001 main / #general", "1100000000000000012 main / #releases", "1100000000000000013 main / #releases / v2 rollout",
			"1100000000000000015 main / #announcements", "1100000000000000016 main / General", "1100000000000000019 main / help / crash on start",
			"1200000000000000011 other / #general", "1300000000000000001 ~Sam Park", "1300000000000000002 climbing",
		}},
		// Search finds what a request's term would.
		{"discord", nil, "main general", []string{"1100000000000000001 main / #general", "1100000000000000016 main / General"}},
		{"discord", nil, "main", []string{
			"1100000000000000001 main / #general", "1100000000000000012 main / #releases",
			"1100000000000000015 main / #announcements", "1100000000000000016 main / General",
			"1100000000000000019 main / help / crash on start",
		}},
		// A grant on a server covers all of it, threads and forum posts too.
		{"discord", []string{"guild:1100000000000000001"}, "", []string{
			"1100000000000000001 main / #general", "1100000000000000012 main / #releases", "1100000000000000013 main / #releases / v2 rollout",
			"1100000000000000015 main / #announcements", "1100000000000000016 main / General", "1100000000000000019 main / help / crash on start",
		}},
		{"discord", nil, "rollout", []string{"1100000000000000013 main / #releases / v2 rollout"}},
		{"matrix", []string{"!general:example.org", "!lobby:example.org"}, "", []string{
			"!general:example.org Club / general", "!lobby:example.org general",
		}},
		{"matrix", nil, "Club / general", []string{"!general:example.org Club / general"}},
		{"fastmail", []string{"mailbox:mb-rec"}, "", []string{"mailbox:mb-rec Inbox / Receipts", "t9 Inbox / Receipts / Your order has shipped"}},
	} {
		v := fixtureView(t, tc.connector, []rule{{Chats: tc.chats}})
		if got := listing(t, v, tc.search); !slices.Equal(got, tc.want) {
			t.Errorf("%s %v chats %q:\n got %q\nwant %q", tc.connector, tc.chats, tc.search, got, tc.want)
		}
	}
	// mandate access names granted chats the same way.
	v := fixtureView(t, "discord", []rule{{Chats: []string{"1100000000000000013"}}})
	names, err := v.names(t.Context(), []string{"1100000000000000013"})
	if err != nil || names["1100000000000000013"] != "main / #releases / v2 rollout" {
		t.Errorf("names = %v, %v", names, err)
	}
}

// A term's chats rank those idle for a year after every active one; then
// the chat it names by ID, then those it names by their own name, then the
// newest, a mailbox dated by its newest thread.
func TestMatchRanking(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "data.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, c := range []store.Container{
		{ID: "guild:1", Kind: "guild", Name: "rust-lang"},
		{ID: "11", Kind: "channel", ParentID: "guild:1", Name: "#general", LastMessageAt: now.Add(-time.Hour)},
		{ID: "guild:2", Kind: "guild", Name: "old"},
		{ID: "21", Kind: "channel", ParentID: "guild:2", Name: "#rust", LastMessageAt: now.AddDate(-2, 0, 0)},
		{ID: "22", Kind: "channel", ParentID: "guild:2", Name: "#rust-help"},
		{ID: "rust", Kind: "dm", Name: "~Ferris", LastMessageAt: now.AddDate(0, 0, -2)},
		{ID: "mailbox:r", Kind: "mailbox", Name: "Rustaceans"},
		{ID: "t1", Kind: "thread", ParentID: "mailbox:r", Name: "weekly", LastMessageAt: now.AddDate(0, -11, 0)},
	} {
		if err := st.UpsertContainer(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	c := &conn{store: path}
	defer c.close()
	db, err := c.storeDB(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	v, err := newView(db, neverShared{}, everything, now)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := v.matches(t.Context(), []string{"rust"}, matchesPerTerm)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r[1].(string))
	}
	if want := []string{"rust", "mailbox:r", "11", "21", "22"}; !slices.Equal(got, want) {
		t.Errorf("rust matches %v, want %v", got, want)
	}
}

// servePicker serves the owner's routes of a connector over dir's store.
func servePicker(t *testing.T, c connector.Connector, dir string) *connector.Client {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv, err := connector.NewServer(c, "chat", dir, self(), self(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "mt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	serveUntilCleanup(t, sock, func(ctx context.Context, ln net.Listener) error { return srv.Serve(ctx, ln) })
	return connector.Dial("chat", sock)
}
