package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/adeci/mandate/store/storetest"
)

// The chats a connection stores, kept in testdata/chats.json for the
// gateway's tests of agents' terms: contacts by address-book and push name,
// the owner's own chat, and groups.
func TestChatsFixture(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	in.storeContacts(ctx)
	in.nameDMs(ctx)
	in.storeGroups(ctx, []*types.GroupInfo{{
		JID: types.NewJID("120363000000000999", types.GroupServer), GroupName: types.GroupName{Name: "Book club"},
		Participants: []types.GroupParticipant{{JID: samLee}, {JID: stranger}},
	}})
	storetest.Check(t, f.st.DB(), filepath.Join("testdata", "chats.json"))
}
