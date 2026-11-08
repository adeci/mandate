package gateway

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

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
			"main general":    {"1100000000000000001"},
			"main / general":  {"1100000000000000001"},
			"main / #general": {"1100000000000000001"},
			"main #general":   {"1100000000000000001"},
			"general":         {"1100000000000000001", "1200000000000000011"},
			"main":            {"1100000000000000001", "1100000000000000012"}, // its channels, not the guild
			"other":           {"1200000000000000011"},
			"rollout":         {"1100000000000000013"},
			"sam":             {"1300000000000000001"},
			"climbing":        {"1300000000000000002"},
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
			"Climbing crew": {"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		},
		"whatsapp": {
			"Sam":           {"111222333@lid", "15550001111@s.whatsapp.net", "447700900123@s.whatsapp.net"},
			"Sam Lee":       {"15550001111@s.whatsapp.net"},
			"Book club":     {"120363000000000999@g.us"},
			"Climbing crew": {"120363000000000777@g.us", "120363000000000888@g.us"},
			"+15550004444":  {"15550004444@s.whatsapp.net"},
		},
		"matrix": {
			"Club / general": {"!general:example.org"},
			"general":        {"!general:example.org", "!lobby:example.org"},
			"Team":           {"!team:example.org"},
			"Club":           {"!general:example.org", "!team:example.org"}, // its rooms, not the space
		},
		"gmessages": {
			"Farid":  {"1"},
			"Family": {"3"},
			"999":    {"2"},
			"Ana":    {"4"},
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
			v, err := newView(db, neverShared{}, everything)
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
				// The owner's picker finds what the request did.
				found, err := picker.Containers(t.Context(), term, "", matchesPerTerm)
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
					if f.ParentName != "" {
						f.Name = f.ParentName + " / " + f.Name
					}
					pickerNames = append(pickerNames, f.Name)
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
	v, err := newView(db, neverShared{}, scope)
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
			"1200000000000000011 other / #general", "1300000000000000001 ~Sam Park", "1300000000000000002 climbing",
		}},
		// Search finds what a request's term would.
		{"discord", nil, "main general", []string{"1100000000000000001 main / #general"}},
		{"discord", nil, "main", []string{"1100000000000000001 main / #general", "1100000000000000012 main / #releases"}},
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
