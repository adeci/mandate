package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/adeci/mandate/store/storetest"
)

// The chats a connection stores, kept in testdata/chats.json for the
// gateway's tests of agents' terms: contacts by address-book and push name,
// the owner's own chat, groups, one with the owner among its members, and a
// number nobody named.
func TestChatsFixture(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := newIngester(whatsmeow.NewClient(dev, nil), f.st, testQueue(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	in.storeContacts(ctx)
	in.nameDMs(ctx)
	in.storeGroups(ctx, []*types.GroupInfo{{
		JID: types.NewJID("120363000000000999", types.GroupServer), GroupName: types.GroupName{Name: "Book club"},
		Participants: []types.GroupParticipant{{JID: samLee}, {JID: stranger}, {JID: owner}},
	}})
	// Written from the phone to a number in no address book.
	in.handle(ctx, &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("15550004444", types.DefaultUserServer), Sender: owner, IsFromMe: true},
			ID:            "3EB0F1", Timestamp: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		},
		Message: &waE2E.Message{Conversation: proto.String("the sink again")},
	})
	storetest.Check(t, f.st.DB(), filepath.Join("testdata", "chats.json"))
}
