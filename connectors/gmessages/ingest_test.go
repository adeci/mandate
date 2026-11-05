package main

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/store"
)

type row struct {
	ID, Chat, Sender, Name, Body, Media, ReplyTo string
	FromMe, Deleted                              bool
}

func messages(t *testing.T, f *fixture) []row {
	t.Helper()
	rows, err := f.st.DB().Query(`SELECT id, container_id, COALESCE(sender_id, ''), COALESCE(sender_name, ''), COALESCE(body, ''),
COALESCE(media_type, ''), COALESCE(reply_to_id, ''), is_from_me, deleted FROM messages ORDER BY sent_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Chat, &r.Sender, &r.Name, &r.Body, &r.Media, &r.ReplyTo, &r.FromMe, &r.Deleted); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func chats(t *testing.T, f *fixture) map[string]string {
	t.Helper()
	rows, err := f.st.DB().Query(`SELECT id, kind || ' ' || name || ' ' || COALESCE(last_message_at, '-') FROM containers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		out[id] = v
	}
	return out
}

func TestSync(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	f.phone.setConvs(faridDM(), strangerDM(), family())
	f.phone.setMessages("1",
		message("m3", "1", "1", outgoing, ts(2), "sure"),
		message("m1", "1", "11", incoming, ts(1), "dinner?"))
	f.phone.setMessages("2", message("m2", "2", "12", incoming, ts(1), "you won"))
	f.phone.fetchErr["3"] = errors.New("phone busy")

	f.c.in.sync(ctx, f.cli())
	if got := f.phone.fetched(); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Fatalf("fetched %v", got)
	}
	want := map[string]string{
		"1": "dm Farid Haddad 2026-10-01T12:02:00.000Z",
		"2": "dm (555) 999-0002 2026-10-01T12:01:00.000Z",
		// Its activity is recorded once its messages are in.
		"3": "group Family -",
	}
	if got := chats(t, f); !maps.Equal(got, want) {
		t.Fatalf("chats %v", got)
	}
	if got := messages(t, f); len(got) != 3 || got[0].ID != "m1" || got[2].ID != "m3" || !got[2].FromMe {
		t.Fatalf("messages %+v", got)
	}

	// Only conversations with newer activity are fetched again.
	delete(f.phone.fetchErr, "3")
	f.phone.setMessages("3", message("m4", "3", "22", incoming, ts(3), "hi all"))
	f.c.in.sync(ctx, f.cli())
	if got := f.phone.fetched(); !slices.Equal(got, []string{"3"}) {
		t.Fatalf("second sync fetched %v", got)
	}
	newer := faridDM()
	newer.LastMessageTimestamp = ts(5)
	f.phone.setConvs(newer, strangerDM(), family())
	f.phone.setMessages("1", message("m5", "1", "11", incoming, ts(5), "7pm"))
	f.c.in.sync(ctx, f.cli())
	if got := f.phone.fetched(); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("third sync fetched %v", got)
	}
	if got := chats(t, f)["1"]; got != "dm Farid Haddad 2026-10-01T12:05:00.000Z" {
		t.Fatalf("chat 1: %s", got)
	}

	// A phone that stops answering ends the sync.
	newer = faridDM()
	newer.LastMessageTimestamp = ts(9)
	f.phone.setConvs(newer, strangerDM(), family())
	f.phone.fetchErr["1"] = libgm.ErrPhoneNotResponding
	if err := f.c.in.syncOnce(ctx, f.cli()); !errors.Is(err, libgm.ErrPhoneNotResponding) {
		t.Fatalf("sync with the phone gone: %v", err)
	}
}

// A sync stores every conversation, past the first page and archived; only
// the recent ones get their messages and activity.
func TestSyncListsAllConversations(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	conv := func(id string, at int64) *gmproto.Conversation {
		return &gmproto.Conversation{
			ConversationID: id, Name: "Chat " + id, LastMessageTimestamp: at, DefaultOutgoingID: "1",
			Participants: []*gmproto.Participant{self("1"), contact(participant("p"+id, "+1555"+id, id), "Chat "+id)},
		}
	}
	var inbox []*gmproto.Conversation
	for i := range syncConversations + 5 {
		inbox = append(inbox, conv(strconv.Itoa(1000+i), ts(2)))
	}
	f.phone.set(func(p *phone) { p.convs, p.archived = inbox, []*gmproto.Conversation{conv("9001", ts(1))} })

	f.c.in.sync(context.Background(), f.cli())
	got := chats(t, f)
	if len(got) != syncConversations+6 {
		t.Fatalf("%d chats stored", len(got))
	}
	for id, want := range map[string]string{
		"1000": "dm Chat 1000 2026-10-01T12:02:00.000Z",
		"1104": "dm Chat 1104 -",
		"9001": "dm Chat 9001 -",
	} {
		if got[id] != want {
			t.Errorf("chat %s: %q, want %q", id, got[id], want)
		}
	}
	if n := len(f.phone.fetched()); n != syncConversations {
		t.Errorf("fetched the messages of %d conversations", n)
	}
}

func TestLiveEvents(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	cli := f.cli()
	f.phone.setConvs(family())

	cli.emit(faridDM())
	cli.emit(&libgm.WrappedMessage{Message: message("m1", "1", "11", incoming, ts(1), "dinner?")})
	// An update of the same message replaces it.
	edited := message("m1", "1", "11", incoming, ts(1), "dinner at 7?")
	cli.emit(&libgm.WrappedMessage{Message: edited})
	// A message of a conversation not seen yet waits for its lookup.
	cli.emit(&libgm.WrappedMessage{Message: message("m4", "3", "23", incoming, ts(3), "hi all")})
	eventually(t, "the group message", func() bool { return len(messages(t, f)) == 2 })
	deleted := &libgm.WrappedMessage{Message: message("m4", "3", "", gmproto.MessageStatusType_MESSAGE_DELETED, ts(4), "")}
	// Deletions reported while the phone rebuilds its database are ignored.
	cli.emit(&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_MOBILE_DATABASE_SYNC_STARTED})
	cli.emit(deleted)
	if got := messages(t, f); got[1].Deleted {
		t.Fatal("deleted during the phone's database sync")
	}
	cli.emit(&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_MOBILE_DATABASE_SYNC_COMPLETE})
	cli.emit(deleted)

	got := messages(t, f)
	want := []row{
		{ID: "m1", Chat: "1", Sender: "+15551230001", Name: "Farid Haddad", Body: "dinner at 7?"},
		{ID: "m4", Chat: "3", Sender: "+15551230003", Name: "Mina", Deleted: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("messages\n got %+v\nwant %+v", got, want)
	}
	if got := chats(t, f); got["1"] != "dm Farid Haddad 2026-10-01T12:02:00.000Z" || got["3"] != "group Family 2026-10-01T12:03:00.000Z" {
		t.Fatalf("chats %v", got)
	}
}

// A DM with only the owner's messages names no sender, so hide_senders
// matches it by its stored participant, as the gateway does; DMs stored
// without one get it from the phone.
func TestDMHiddenByNumber(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	if err := f.st.UpsertContainer(ctx, store.Container{ID: "1", Kind: "dm", Name: "Farid Haddad"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertMessage(ctx, store.Message{ID: "m3", ContainerID: "1", SentAt: t0, IsFromMe: true, Body: "sure"}); err != nil {
		t.Fatal(err)
	}
	hidden := func(chat, number string) bool {
		var n int
		if err := f.st.DB().QueryRow(`SELECT count(*) FROM containers AS c, json_each(c.participants_json) AS p
WHERE c.id = ? AND c.kind = 'dm' AND p.value ->> 'id' = ?`, chat, number).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	f.phone.setConvs(faridDM())
	if err := f.c.in.fillDMs(ctx, f.cli()); err != nil {
		t.Fatal(err)
	}
	if !hidden("1", "+15551230001") {
		t.Error("stored DM not matched by its number")
	}
	f.cli().emit(strangerDM())
	if !hidden("2", "+15559990002") {
		t.Error("new DM not matched by its number")
	}
}
