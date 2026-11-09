package connector

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/adeci/mandate/store"
)

// The picker's listing: a level, its shelves before its chats, the top
// level by default; search at any depth with headings, and by a DM's
// handle; lookup by ID.
func TestContainers(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Second)
	day := func(n int) time.Time { return now.Add(-time.Duration(8-n) * 24 * time.Hour) }
	for _, c := range []store.Container{
		{ID: "guild:1", Kind: "guild", Name: "main", Icon: "icons/1/a"},
		{ID: "guild:2", Kind: "guild", Name: "other"},
		{ID: "10", Kind: "channel", ParentID: "guild:1", Name: "#general", LastMessageAt: day(5)},
		{ID: "11", Kind: "channel", ParentID: "guild:1", Name: "#releases", LastMessageAt: day(3)},
		{ID: "12", Kind: "thread", ParentID: "11", Name: "v2 rollout", LastMessageAt: day(6)},
		{ID: "13", Kind: "forum", ParentID: "guild:1", Name: "help"},
		{ID: "14", Kind: "thread", ParentID: "13", Name: "general question", LastMessageAt: day(1)},
		{ID: "20", Kind: "channel", ParentID: "guild:2", Name: "#general", LastMessageAt: day(2)},
		{ID: "30", Kind: "dm", Name: "~Sam", Participants: []store.Participant{{ID: "u1", Name: "~Sam", Handle: "@sam_p"}}, LastMessageAt: day(4)},
		{ID: "31", Kind: "group", IsGroup: true, Name: "climbing", Participants: []store.Participant{{ID: "u1"}, {ID: "u2"}}, LastMessageAt: day(7)},
		{ID: "32", Kind: "dm", Name: "~Kai"},
	} {
		if err := st.UpsertContainer(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// A chat dated only by its messages, as a connector's first sync leaves it.
	if err := st.UpsertMessage(ctx, store.Message{ID: "m", ContainerID: "32", SentAt: now.Add(-time.Hour), Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	self := uint32(os.Getgid())
	c, _ := serve(t, &fake{}, dir, self, self)
	ids := func(q ContainerQuery) []string {
		t.Helper()
		cs, err := c.Containers(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	main := "guild:1"
	for name, tc := range map[string]struct {
		q    ContainerQuery
		want []string
	}{
		"top level":        {ContainerQuery{Limit: 50}, []string{"guild:1", "guild:2", "32", "31", "30"}},
		"top level, cut":   {ContainerQuery{Limit: 1}, []string{"guild:1", "guild:2", "32"}},
		"in main":          {ContainerQuery{Parent: main, Limit: 50}, []string{"13", "10", "11"}},
		"search":           {ContainerQuery{Q: "main", Limit: 50}, []string{"guild:1"}}, // all of main
		"search in main":   {ContainerQuery{Q: "general", Parent: main, Limit: 50}, []string{"10", "14"}},
		"search, headings": {ContainerQuery{Q: "help", Limit: 50}, []string{"13"}},
		"search, path":     {ContainerQuery{Q: "main general", Limit: 50}, []string{"10", "14"}},
		"search, handle":   {ContainerQuery{Q: "@sam_p", Limit: 50}, []string{"30"}},
		"by id":            {ContainerQuery{IDs: []string{"31", "12", "nosuch"}}, []string{"31", "12"}},
	} {
		if got := ids(tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}

	cs, err := c.Containers(ctx, ContainerQuery{IDs: []string{"guild:1", "12", "31", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	icon := iconVersion("icons/1/a")
	want := []Container{
		{ID: "guild:1", Kind: "guild", Name: "main", Parents: []Parent{}, LastActivity: day(6), Opens: true, Icon: icon},
		{ID: "12", Kind: "thread", Name: "v2 rollout", Parents: []Parent{{"guild:1", "main", "guild", icon}, {"11", "#releases", "channel", ""}}, LastActivity: day(6)},
		{
			ID: "31", Kind: "group", Name: "climbing", Parents: []Parent{}, LastActivity: day(7), Members: 3,
			Participants: []store.Participant{{ID: "u1"}, {ID: "u2"}},
		},
		{
			ID: "30", Kind: "dm", Name: "~Sam", Parents: []Parent{}, LastActivity: day(4), Handle: "@sam_p",
			Participants: []store.Participant{{ID: "u1", Name: "~Sam", Handle: "@sam_p"}},
		},
	}
	if !reflect.DeepEqual(cs, want) {
		t.Errorf("rows\n got %+v\nwant %+v", cs, want)
	}
}
