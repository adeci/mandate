package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/adeci/mandate/store"
)

// A full contact sync fills the contact store without per-contact events, so
// DMs stored before it are named afterwards; only the address book gives an
// unmarked name. The owner's own chat gets WhatsApp's name for it.
func TestNameDMs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	self := owner.ToNonAD()
	if err := f.st.UpsertContainer(ctx, store.Container{ID: self.String(), Kind: "dm", Name: "Owner"}); err != nil {
		t.Fatal(err)
	}
	newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil))).nameDMs(ctx)
	for id, want := range map[string]string{
		farid.String():    "Farid Haddad", // replaces the push name stored earlier
		samParkL.String(): "Sam Park",     // listed in the address book under its LID
		stranger.String(): "~Mom",         // not in the address book
		self.String():     messageYourself,
	} {
		var got string
		if err := f.st.DB().QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM containers WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s named %q, want %q", id, got, want)
		}
	}
	// The DM stored under Sam's LID lists him by the number his messages carry.
	var parts string
	if err := f.st.DB().QueryRowContext(ctx, `SELECT participants_json FROM containers WHERE id = ?`, samParkL.String()).Scan(&parts); err != nil {
		t.Fatal(err)
	}
	if want := `[{"id":"447700900123@s.whatsapp.net","name":"Sam Park"}]`; parts != want {
		t.Errorf("participants of the LID DM: %s, want %s", parts, want)
	}
}

// Every address-book contact and joined group gets a chat without
// messages; someone known only by a push name does not.
func TestStoreChatsWithoutMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pushOnly := types.NewJID("15550002222", types.DefaultUserServer)
	if _, _, err := dev.Contacts.PutPushName(ctx, pushOnly, "Pat"); err != nil {
		t.Fatal(err)
	}
	in := newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	in.storeContacts(ctx)
	quiet := types.NewJID("120363000000000999", types.GroupServer)
	in.storeGroups(ctx, []*types.GroupInfo{{
		JID: quiet, GroupName: types.GroupName{Name: "Book club"},
		Participants: []types.GroupParticipant{{JID: samLee}, {JID: pushOnly}},
	}})

	rows, err := f.st.DB().QueryContext(ctx, `SELECT id, kind, COALESCE(name, ''), COALESCE(last_message_at, ''), participants_json FROM containers`)
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
	want := map[string]string{
		quiet.String():           `group|Book club||[{"id":"15550001111@s.whatsapp.net","name":"Sam Lee"},{"id":"15550002222@s.whatsapp.net","name":"~Pat"}]`,
		climbing.String():        `group|Climbing crew||[{"id":"15551234567@s.whatsapp.net"},{"id":"447700900123@s.whatsapp.net"}]`,
		climbing2.String():       "group|Climbing crew||[]",
		owner.ToNonAD().String(): "dm|" + messageYourself + "||[]",
		farid.String():           "dm|Farid Haddad||[]",
		samLee.String():          "dm|Sam Lee||[]",
		samPark.String():         "dm|Sam Park||[]",
		samParkL.String():        "dm|||[]",
		stranger.String():        "dm|Mom||[]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chats\n got %q\nwant %q", got, want)
	}
}

// storedChat reads the name and last activity stored for id.
func storedChat(t *testing.T, st *store.Store, id types.JID) (name, last string) {
	t.Helper()
	if err := st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(name, ''), COALESCE(last_message_at, '') FROM containers WHERE id = ?`,
		id.String()).Scan(&name, &last); err != nil {
		t.Fatal(err)
	}
	return name, last
}

// A chat stored without a name gets one: a DM the push name kept under the
// person's other ID, else their number, and a group whose lookup failed its
// subject on the next connect. WhatsApp describes groups to members only,
// so one the owner left keeps none.
func TestNamelessChats(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pat, patL := types.NewJID("15550005555", types.DefaultUserServer), types.NewJID("222333444", types.HiddenUserServer)
	if err := dev.LIDs.PutLIDMapping(ctx, patL, pat); err != nil {
		t.Fatal(err)
	}
	// As the history sync's push names are, under the LID only.
	if _, _, err := dev.Contacts.PutPushName(ctx, patL, "Pat"); err != nil {
		t.Fatal(err)
	}
	nobody := types.NewJID("15550003333", types.DefaultUserServer)
	left := types.NewJID("120363000000000666", types.GroupServer)
	for _, c := range []store.Container{
		{ID: pat.String(), Kind: "dm"},
		{ID: nobody.String(), Kind: "dm"},
		{ID: left.String(), Kind: "group", IsGroup: true},
	} {
		if err := f.st.UpsertContainer(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	names := func(want map[types.JID]string) {
		t.Helper()
		for id, w := range want {
			if got, _ := storedChat(t, f.st, id); got != w {
				t.Errorf("%s named %q, want %q", id, got, w)
			}
		}
	}

	in := newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	in.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) { return nil, whatsmeow.ErrIQRateOverLimit }
	plumber := types.NewJID("15550004444", types.DefaultUserServer)
	running := types.NewJID("120363000000000555", types.GroupServer)
	for _, src := range []types.MessageSource{
		{Chat: plumber, Sender: me, IsFromMe: true}, // written from the phone
		{Chat: running, Sender: farid, IsGroup: true},
	} {
		in.handle(ctx, &events.Message{
			Info:    types.MessageInfo{MessageSource: src, ID: "M" + src.Chat.User, Timestamp: time.Now()},
			Message: &waE2E.Message{Conversation: proto.String("hi")},
		})
	}
	names(map[types.JID]string{plumber: "+15550004444", running: ""})

	in.groupInfo = func(_ context.Context, j types.JID) (*types.GroupInfo, error) {
		if j != running {
			return nil, whatsmeow.ErrNotInGroup
		}
		return &types.GroupInfo{JID: running, GroupName: types.GroupName{Name: "Running club"}}, nil
	}
	in.nameDMs(ctx)
	in.nameGroups(ctx)
	names(map[types.JID]string{pat: "~Pat", nobody: "+15550003333", running: "Running club", left: ""})
}

// A chat is dated by its history conversation, which WhatsApp may date after
// its last message, and by every newer message, so chats sort by recency
// before any is fetched.
func TestChatActivity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	day := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	unix := func(at time.Time) *uint64 { return proto.Uint64(uint64(at.Unix())) }
	sent := func(chat types.JID, id string, at time.Time) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), FromMe: proto.Bool(true), ID: proto.String(id)},
			MessageTimestamp: unix(at),
			Message:          &waE2E.Message{Conversation: proto.String("hi")},
		}}
	}
	dated := func(want map[types.JID]time.Time) {
		t.Helper()
		for id, w := range want {
			if _, got := storedChat(t, f.st, id); got != w.Format("2006-01-02T15:04:05.000Z") {
				t.Errorf("%s last active %q, want %s", id, got, w)
			}
		}
	}

	in.historySync(ctx, &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		Conversations: []*waHistorySync.Conversation{
			{
				ID: proto.String(climbing.String()), Name: proto.String("Climbing crew"), ConversationTimestamp: unix(day),
				Messages: []*waHistorySync.HistorySyncMsg{sent(climbing, "H1", day.Add(-time.Hour))},
			},
			// Undated by WhatsApp: dated by its newest message.
			{ID: proto.String(farid.String()), Messages: []*waHistorySync.HistorySyncMsg{
				sent(farid, "H3", day.Add(-time.Hour)), sent(farid, "H2", day.Add(-2*time.Hour)),
			}},
		},
	})
	dated(map[types.JID]time.Time{climbing: day, farid: day.Add(-time.Hour)})

	in.handle(ctx, &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: farid, Sender: farid},
			ID:            "L1", Timestamp: day.Add(time.Hour),
		},
		Message: &waE2E.Message{Conversation: proto.String("still on for tonight?")},
	})
	dated(map[types.JID]time.Time{climbing: day, farid: day.Add(time.Hour)})
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

// Stanza IDs are chosen by the sending device, so a new message may reuse
// a stored ID; only its author's redelivery replaces the stored message.
func TestReusedIDFromOtherAuthor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sent := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for id, from := range map[string]types.JID{"O1": me, "F1": farid, "S1": samPark} {
		if err := f.st.UpsertMessage(ctx, store.Message{
			ID: messageID(climbing, id), ContainerID: climbing.String(), SenderID: from.String(),
			IsFromMe: from == me, SentAt: sent, Body: "original",
		}); err != nil {
			t.Fatal(err)
		}
	}
	send := func(from types.JID, id, body string) {
		if !in.handle(ctx, &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: climbing, Sender: from, IsGroup: true},
				ID:            id, Timestamp: sent.Add(time.Minute),
			},
			Message: &waE2E.Message{Conversation: proto.String(body)},
		}) {
			t.Fatalf("%s from %s not acked", id, from)
		}
	}
	send(farid, "O1", "farid over the owner")
	send(samParkL, "F1", "sam over farid")
	send(samParkL, "S1", "sam again")

	for id, want := range map[string]string{"O1": "original", "F1": "original", "S1": "sam again"} {
		var body string
		if err := f.st.DB().QueryRowContext(ctx, `SELECT body FROM messages WHERE id = ?`, messageID(climbing, id)).
			Scan(&body); err != nil {
			t.Fatal(err)
		}
		if body != want {
			t.Errorf("%s: body %q, want %q", id, body, want)
		}
	}
}
