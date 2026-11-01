package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/adeci/mandate/store"
)

// A full contact sync fills the contact store without per-contact events, so
// DMs stored before it are named afterwards; only the address book gives an
// unmarked name.
func TestNameDMs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil))).nameDMs(ctx)
	for id, want := range map[string]string{
		farid.String():    "Farid Haddad", // replaces the push name stored earlier
		samParkL.String(): "Sam Park",     // listed in the address book under its LID
		stranger.String(): "~Mom",         // not in the address book
	} {
		var got string
		if err := f.st.DB().QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM containers WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s named %q, want %q", id, got, want)
		}
	}
}

// Any group member can name any message ID in an edit or delete; only the
// author's apply, and edits only within the edit window.
func TestEditsAndDeletesFromAuthor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sent := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for id, from := range map[string]types.JID{"F1": farid, "F2": farid, "S1": samPark} {
		if err := f.st.UpsertMessage(ctx, store.Message{
			ID: messageID(climbing, id), ContainerID: climbing.String(), SenderID: from.String(), SentAt: sent, Body: "original",
		}); err != nil {
			t.Fatal(err)
		}
	}
	change := func(from types.JID, target string, p *waE2E.ProtocolMessage, after time.Duration) {
		p.Key = &waCommon.MessageKey{ID: proto.String(target)}
		in.handle(ctx, &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: climbing, Sender: from, IsGroup: true},
				ID:            "X" + target + after.String(), Timestamp: sent.Add(after),
			},
			Message: &waE2E.Message{ProtocolMessage: p},
		})
	}
	edit := func(body string) *waE2E.ProtocolMessage {
		return &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), EditedMessage: &waE2E.Message{Conversation: proto.String(body)},
		}
	}
	revoke := func() *waE2E.ProtocolMessage {
		return &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}
	}

	change(samParkL, "F1", edit("by sam"), time.Minute)
	change(farid, "F1", edit("by farid"), 2*time.Minute)
	change(farid, "F1", edit("too late"), 21*time.Minute)
	change(samParkL, "F2", revoke(), time.Minute)
	change(samParkL, "S1", edit("sam, by lid"), time.Minute) // stored under the phone number
	change(samParkL, "S1", revoke(), 2*time.Minute)

	for id, want := range map[string]struct {
		body    sql.NullString
		deleted bool
	}{
		"F1": {body: sql.NullString{String: "by farid", Valid: true}},
		"F2": {body: sql.NullString{String: "original", Valid: true}},
		"S1": {deleted: true},
	} {
		var body sql.NullString
		var deleted bool
		if err := f.st.DB().QueryRowContext(ctx, `SELECT body, deleted FROM messages WHERE id = ?`, messageID(climbing, id)).
			Scan(&body, &deleted); err != nil {
			t.Fatal(err)
		}
		if body != want.body || deleted != want.deleted {
			t.Errorf("%s: body %v, deleted %v; want %v, %v", id, body, deleted, want.body, want.deleted)
		}
	}
}
