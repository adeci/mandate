package main

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
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
		{ID: "m4", Chat: "3", Sender: "+15551230003", Name: "Mina", Body: "hi all", Deleted: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("messages\n got %+v\nwant %+v", got, want)
	}
	if got := chats(t, f); got["1"] != "dm Farid Haddad 2026-10-01T12:02:00.000Z" || got["3"] != "group Family 2026-10-01T12:03:00.000Z" {
		t.Fatalf("chats %v", got)
	}
}
