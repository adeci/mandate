package main

import (
	"path/filepath"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/store"
	"github.com/adeci/mandate/store/storetest"
)

// The chats a sync stores, kept in testdata/chats.json for the gateway's
// tests of agents' terms: a contact, a stranger named by number, a group.
func TestChatsFixture(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, conv := range []*gmproto.Conversation{faridDM(), strangerDM(), family()} {
		if err := st.UpsertContainer(t.Context(), container(conv)); err != nil {
			t.Fatal(err)
		}
	}
	storetest.Check(t, st.DB(), filepath.Join("testdata", "chats.json"))
}
