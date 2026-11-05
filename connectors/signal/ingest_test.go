package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
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
		stranger.String(): {name: "+15557654321", last: "2026-12-23T04:26:42.000Z", participants: "[]"},
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

// Contacts synced from the owner's devices name stored DMs and get one
// when they have none; only the address book or a nickname gives an
// unmarked name. A DM stored under a phone number identity lists the
// person.
func TestContactListNamesDMs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	in := newIngester(f.cli(), f.c.dev, f.st, f.sess, discard)
	pniChat := "PNI:" + strangerPNI.String()
	if err := f.st.UpsertContainer(ctx, store.Container{ID: pniChat, Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	lou, pat, blocked := uuid.New(), uuid.New(), uuid.New()
	in.handle(ctx, &events.ContactList{Contacts: []*types.Recipient{
		{ACI: farid, ContactName: "Farid Haddad", Profile: types.Profile{Name: "Bank Support"}},
		{ACI: mallory, Profile: types.Profile{Name: "Mom"}},
		{ACI: sam, Nickname: "Sam"},
		{ACI: stranger, PNI: strangerPNI, Profile: types.Profile{Name: "Kim"}},
		{ACI: lou, Whitelisted: new(true), Profile: types.Profile{Name: "Lou"}},
		{ACI: pat, Profile: types.Profile{Name: "Pat"}},
		{ACI: blocked, ContactName: "Ex", Blocked: true},
	}})
	for id, want := range map[string]string{
		farid.String():   "Farid Haddad",
		mallory.String(): "~Mom",
		sam.String():     "Sam",
		lou.String():     "~Lou",
	} {
		var got string
		if err := f.st.DB().QueryRow(`SELECT COALESCE(name, '') FROM containers WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got != want {
			t.Errorf("%s named %q, want %q", id, got, want)
		}
	}
	var n int
	if err := f.st.DB().QueryRow(`SELECT count(*) FROM containers WHERE id IN (?, ?)`, pat.String(), blocked.String()).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a stranger or a blocked contact became a chat: %d, %v", n, err)
	}
	var parts string
	if err := f.st.DB().QueryRow(`SELECT participants_json FROM containers WHERE id = ?`, pniChat).Scan(&parts); err != nil {
		t.Fatal(err)
	}
	if want := `[{"id":"` + stranger.String() + `","name":"~Kim"}]`; parts != want {
		t.Errorf("PNI DM participants %s, want %s", parts, want)
	}
}

// The owner's own chat is named as Signal names it, also when stored
// earlier under the owner's profile name, and a contact sync keeps it.
func TestOwnChatIsNoteToSelf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	if err := f.st.UpsertContainer(ctx, store.Container{ID: owner.String(), Kind: "dm", Name: "Owner"}); err != nil {
		t.Fatal(err)
	}
	in := newIngester(f.cli(), f.c.dev, f.st, f.sess, discard)
	in.syncChats(ctx)
	in.handle(ctx, &events.ContactList{Contacts: []*types.Recipient{{ACI: owner, Profile: types.Profile{Name: "Owner"}}}})
	in.handle(ctx, &events.ChatEvent{Info: events.MessageInfo{Sender: owner, ChatID: owner.String()}, Event: &signalpb.DataMessage{
		Body: new("milk"), Timestamp: new(uint64(1798000000000)),
	}})
	var got string
	if err := f.st.DB().QueryRow(`SELECT COALESCE(name, '') FROM containers WHERE id = ?`, owner.String()).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != noteToSelf {
		t.Errorf("own chat named %q, want %q", got, noteToSelf)
	}
}

// A sync stores a chat for every contact and every group the account is
// in, without messages.
func TestSyncChats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	quiet := types.GroupIdentifier(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	left := types.GroupIdentifier(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)))
	f.groups[quiet] = &signalmeow.Group{GroupIdentifier: quiet, Title: "Book club", Members: []*signalmeow.GroupMember{{ACI: owner}, {ACI: sam}}}
	for _, gid := range []types.GroupIdentifier{climbing, quiet, left} {
		if err := f.c.dev.GroupStore.StoreMasterKey(ctx, gid, "key"); err != nil {
			t.Fatal(err)
		}
	}
	in := newIngester(f.cli(), f.c.dev, f.st, f.sess, discard)
	in.syncChats(ctx)

	rows, err := f.st.DB().Query(`SELECT id, kind, COALESCE(name, ''), COALESCE(last_message_at, ''), participants_json FROM containers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, kind, name, last, parts string
		if err := rows.Scan(&id, &kind, &name, &last, &parts); err != nil {
			t.Fatal(err)
		}
		got[id] = strings.Join([]string{kind, name, last, parts}, "|")
	}
	member := func(id uuid.UUID, name string) string { return `{"id":"` + id.String() + `","name":"` + name + `"}` }
	want := map[string]string{
		string(quiet):      "group|Book club||[" + member(owner, "Owner") + "," + member(sam, "Sam") + "]",
		string(climbing):   "group|Climbing crew||[" + member(owner, "Owner") + "," + member(farid, "Farid Haddad") + "," + member(mallory, "~Mom") + "]",
		string(bouldering): "group|Climbing crew||[]",
		owner.String():     "dm|Note to Self||[]",
		farid.String():     "dm|Farid Haddad||[]",
		sam.String():       "dm|Sam||[]",
		mallory.String():   "dm|~Mom||[]",
		stranger.String():  "dm|+15557654321||[]", // no name but a number
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chats\n got %q\nwant %q", got, want)
	}
}
