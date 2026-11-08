package main

import (
	"path/filepath"
	"testing"

	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"

	"github.com/adeci/mandate/store/storetest"
)

// The chats a sync stores, kept in testdata/chats.json for the gateway's
// tests of agents' terms: contacts named by the address book, a profile or
// a number, the owner's own chat, and groups.
func TestChatsFixture(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	f.groups[bouldering] = &signalmeow.Group{GroupIdentifier: bouldering, Title: "Bouldering", Members: []*signalmeow.GroupMember{{ACI: owner}, {ACI: sam}}}
	for _, gid := range []types.GroupIdentifier{climbing, bouldering} {
		if err := f.c.dev.GroupStore.StoreMasterKey(ctx, gid, "key"); err != nil {
			t.Fatal(err)
		}
	}
	newIngester(f.cli(), f.c.dev, f.st, f.sess, testQueue(), discard).syncChats(ctx)
	storetest.Check(t, f.st.DB(), filepath.Join("testdata", "chats.json"))
}
