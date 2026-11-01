package main

import (
	"context"
	"reflect"
	"testing"

	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"

	"github.com/adeci/mandate/store"
)

func TestReceive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	in := newIngester(f.cli(), f.c.dev, f.st, f.sess, discard)
	const t1, t2, t3, t4, t5, t6 = 1798000000000, 1798000001000, 1798000002000, 1798000003000, 1798000004000, 1798000005000
	dm, group := farid.String(), string(climbing)
	for _, e := range []*events.ChatEvent{
		{Info: events.MessageInfo{Sender: farid, ChatID: dm}, Event: &signalpb.DataMessage{Body: new("running late"), Timestamp: new(uint64(t1))}},
		{Info: events.MessageInfo{Sender: mallory, ChatID: group, GroupRevision: 3}, Event: &signalpb.DataMessage{
			Body:        new("look \uFFFC 🧗"),
			BodyRanges:  []*signalpb.BodyRange{{Start: new(uint32(5)), Length: new(uint32(1)), AssociatedValue: &signalpb.BodyRange_MentionAci{MentionAci: farid.String()}}},
			Attachments: []*signalpb.AttachmentPointer{{ContentType: new("image/jpeg")}},
			Quote:       &signalpb.DataMessage_Quote{Id: new(uint64(t1)), AuthorAci: new(farid.String())},
			Timestamp:   new(uint64(t2)),
		}},
		// Sent from the owner's phone to a number.
		{Info: events.MessageInfo{Sender: owner, ChatID: "PNI:" + strangerPNI.String()}, Event: &signalpb.DataMessage{Body: new("who is this?"), Timestamp: new(uint64(t3))}},
		{Info: events.MessageInfo{Sender: farid, ChatID: dm}, Event: &signalpb.EditMessage{
			TargetSentTimestamp: new(uint64(t1)),
			DataMessage:         &signalpb.DataMessage{Body: new("running very late"), Timestamp: new(uint64(t4))},
		}},
		{Info: events.MessageInfo{Sender: mallory, ChatID: group, GroupRevision: 3}, Event: &signalpb.DataMessage{
			Delete: &signalpb.DataMessage_Delete{TargetSentTimestamp: new(uint64(t2))}, Timestamp: new(uint64(t5)),
		}},
		// Neither someone else's delete nor an admin delete applies.
		{Info: events.MessageInfo{Sender: farid, ChatID: stranger.String()}, Event: &signalpb.DataMessage{
			Delete: &signalpb.DataMessage_Delete{TargetSentTimestamp: new(uint64(t3))}, Timestamp: new(uint64(t5)),
		}},
		{Info: events.MessageInfo{Sender: stranger, ChatID: stranger.String()}, Event: &signalpb.DataMessage{
			AdminDelete: &signalpb.DataMessage_AdminDelete{TargetAuthorAciBinary: owner[:], TargetSentTimestamp: new(uint64(t3))},
			Timestamp:   new(uint64(t5)),
		}},
		{Info: events.MessageInfo{Sender: farid, ChatID: dm}, Event: &signalpb.DataMessage{
			Reaction: &signalpb.DataMessage_Reaction{Emoji: new("👍"), TargetSentTimestamp: new(uint64(t3))}, Timestamp: new(uint64(t6)),
		}},
		{Info: events.MessageInfo{Sender: farid, ChatID: dm}, Event: &signalpb.TypingMessage{}},
	} {
		if !in.handle(ctx, e) {
			t.Fatalf("handle %T failed", e.Event)
		}
	}

	want := []store.Message{
		{ID: messageID(dm, farid, t1), ContainerID: dm, SenderID: farid.String(), SenderName: "Farid Haddad", Body: "running very late"},
		{
			ID: messageID(group, mallory, t2), ContainerID: group, SenderID: mallory.String(), SenderName: "~Mom",
			MediaType: "image", ReplyToID: messageID(group, farid, t1),
		},
		{ID: messageID(stranger.String(), owner, t3), ContainerID: stranger.String(), SenderID: owner.String(), IsFromMe: true, Body: "who is this?"},
	}
	if got := messages(t, f.st); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages\n got %+v\nwant %+v", got, want)
	}
	var deleted bool
	if err := f.st.DB().QueryRow(`SELECT deleted FROM messages WHERE id = ?`, messageID(group, mallory, t2)).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("delete not applied: %v", err)
	}

	type chat struct {
		name, last   string
		participants string
	}
	for id, wantChat := range map[string]chat{
		dm:                {name: "Farid Haddad", last: "2026-12-23T04:26:40.000Z", participants: "[]"},
		group:             {name: "Climbing crew", last: "2026-12-23T04:26:41.000Z", participants: `[{"id":"` + owner.String() + `","name":"Owner"},{"id":"` + farid.String() + `","name":"Farid Haddad"},{"id":"` + mallory.String() + `","name":"~Mom"}]`},
		stranger.String(): {last: "2026-12-23T04:26:42.000Z", participants: "[]"},
	} {
		var got chat
		if err := f.st.DB().QueryRow(`SELECT COALESCE(name, ''), COALESCE(last_message_at, ''), participants_json FROM containers WHERE id = ?`, id).
			Scan(&got.name, &got.last, &got.participants); err != nil {
			t.Fatal(err)
		}
		if got != wantChat {
			t.Errorf("%s: %+v, want %+v", id, got, wantChat)
		}
	}
}

// Contacts synced from the owner's devices name stored DMs; only the
// address book or a nickname gives an unmarked name.
func TestContactListNamesDMs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	in := newIngester(f.cli(), f.c.dev, f.st, f.sess, discard)
	in.handle(ctx, &events.ContactList{Contacts: []*types.Recipient{
		{ACI: farid, ContactName: "Farid Haddad", Profile: types.Profile{Name: "Bank Support"}},
		{ACI: mallory, Profile: types.Profile{Name: "Mom"}},
		{ACI: sam, Nickname: "Sam"},
	}})
	for id, want := range map[string]string{
		farid.String():   "Farid Haddad",
		mallory.String(): "~Mom",
	} {
		var got string
		if err := f.st.DB().QueryRow(`SELECT COALESCE(name, '') FROM containers WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s named %q, want %q", id, got, want)
		}
	}
	var n int
	if err := f.st.DB().QueryRow(`SELECT count(*) FROM containers WHERE id = ?`, sam.String()).Scan(&n); err != nil || n != 0 {
		t.Fatalf("contact without a chat became a chat: %d, %v", n, err)
	}
}
