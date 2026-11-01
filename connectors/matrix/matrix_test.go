package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/utils"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	dmRoom    = id.RoomID("!dm:example.org")
	teamRoom  = id.RoomID("!team:example.org")
	vaultRoom = id.RoomID("!vault:example.org")
)

type fixture struct {
	t   *testing.T
	hs  *homeserver
	env *connector.Env
	st  *store.Store
	c   *mxConnector

	// element is the owner's existing device, which set up cross-signing,
	// secret storage and key backup.
	element     *account
	recoveryKey string
	backupKey   *backup.MegolmBackupKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{t: t, hs: newHomeserver(t)}

	token := f.hs.addDevice(owner, "ELEMENT")
	el, err := openAccount(ctx, session{
		Homeserver: f.hs.srv.URL, UserID: owner, DeviceID: "ELEMENT", AccessToken: token, PickleKey: []byte("element"),
	}, filepath.Join(t.TempDir(), "element.db"), f.hs.srv.Client(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(el.close)
	f.element = el
	if err := el.mach.ShareKeys(ctx, -1); err != nil {
		t.Fatal(err)
	}
	if f.recoveryKey, _, err = el.mach.GenerateAndUploadCrossSigningKeys(ctx, nil, ""); err != nil {
		t.Fatal(err)
	}
	keyID, meta, err := el.mach.SSSS.GetDefaultKeyData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key, err := meta.VerifyRecoveryKey(keyID, f.recoveryKey)
	if err != nil {
		t.Fatal(err)
	}
	if f.backupKey, err = backup.NewMegolmBackupKey(); err != nil {
		t.Fatal(err)
	}
	if err := el.mach.SSSS.SetEncryptedAccountData(ctx, event.AccountDataMegolmBackupKey, f.backupKey.Bytes(), key); err != nil {
		t.Fatal(err)
	}
	f.hs.backupVersion = &mautrix.RespRoomKeysVersion[backup.MegolmAuthData]{
		Algorithm: id.KeyBackupAlgorithmMegolmBackupV1,
		AuthData:  backup.MegolmAuthData{PublicKey: id.Ed25519(base64.RawStdEncoding.EncodeToString(f.backupKey.PublicKey().Bytes()))},
		Version:   "1",
	}

	f.env = &connector.Env{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := os.MkdirAll(f.env.PrivateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if f.st, err = store.Open(ctx, f.env.DataPath()); err != nil {
		t.Fatal(err)
	}
	f.c = newConnector(ctx, f.st, f.env, zerolog.Nop(), f.hs.srv.Client())
	t.Cleanup(func() { f.c.Close() })
	return f
}

func (f *fixture) setupArgs() map[string]string {
	return map[string]string{"homeserver": f.hs.srv.URL, "user_id": owner.String(), "password": password, "recovery_key": f.recoveryKey}
}

// link links the connector and returns its device ID.
func (f *fixture) link() id.DeviceID {
	f.t.Helper()
	step, err := f.c.Setup(context.Background(), f.setupArgs())
	if err != nil || !step.Done {
		f.t.Fatalf("setup: %+v %v", step, err)
	}
	a := f.c.use()
	defer a.release()
	return a.sess.DeviceID
}

// encrypt encrypts text as the owner's Element would in room, in a new
// room key that it shares with the owner's devices.
func (f *fixture) encrypt(room id.RoomID, eventID, text string, ts int64) (map[string]any, *event.EncryptedEventContent) {
	f.t.Helper()
	ctx := context.Background()
	el := f.element
	if err := el.crypto.RemoveOutboundGroupSession(ctx, room); err != nil {
		f.t.Fatal(err)
	}
	// Element would learn of new devices from sync.
	if _, err := el.mach.FetchKeys(ctx, []id.UserID{owner}, true); err != nil {
		f.t.Fatal(err)
	}
	if err := el.mach.ShareGroupSession(ctx, room, []id.UserID{owner}); err != nil {
		f.t.Fatal(err)
	}
	enc, err := el.mach.EncryptMegolmEvent(ctx, room, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: text})
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := json.Marshal(enc)
	if err != nil {
		f.t.Fatal(err)
	}
	return map[string]any{
		"type": "m.room.encrypted", "event_id": eventID, "sender": owner, "origin_server_ts": ts, "content": json.RawMessage(raw),
	}, enc
}

// backUp puts the room key of enc in the server-side key backup.
func (f *fixture) backUp(room id.RoomID, enc *event.EncryptedEventContent) {
	f.t.Helper()
	igs, err := f.element.crypto.GetGroupSession(context.Background(), room, enc.SessionID)
	if err != nil {
		f.t.Fatal(err)
	}
	exported, err := igs.Internal.Export(igs.Internal.FirstKnownIndex())
	if err != nil {
		f.t.Fatal(err)
	}
	data, err := backup.EncryptSessionData(f.backupKey, backup.MegolmSessionData{
		Algorithm:         id.AlgorithmMegolmV1,
		SenderKey:         igs.SenderKey,
		SenderClaimedKeys: backup.SenderClaimedKeys{Ed25519: igs.SigningKey},
		SessionKey:        string(exported),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.hs.mu.Lock()
	defer f.hs.mu.Unlock()
	if f.hs.backupKeys.Rooms == nil {
		f.hs.backupKeys.Rooms = map[id.RoomID]mautrix.RespRoomKeyBackup[sessionData]{}
	}
	f.hs.backupKeys.Rooms[room] = mautrix.RespRoomKeyBackup[sessionData]{
		Sessions: map[id.SessionID]mautrix.RespKeyBackupData[sessionData]{enc.SessionID: {SessionData: data}},
	}
}

type storedMessage struct {
	Sender, Body, Media, ReplyTo string
	FromMe, Edited, Deleted      bool
}

func (f *fixture) message(eventID string) storedMessage {
	f.t.Helper()
	var m storedMessage
	err := f.st.DB().QueryRow(`SELECT COALESCE(sender_name, ''), COALESCE(body, ''), COALESCE(media_type, ''),
COALESCE(reply_to_id, ''), is_from_me, edited_at IS NOT NULL, deleted FROM messages WHERE id = ?1`, eventID).
		Scan(&m.Sender, &m.Body, &m.Media, &m.ReplyTo, &m.FromMe, &m.Edited, &m.Deleted)
	if err != nil {
		f.t.Fatalf("message %s: %v", eventID, err)
	}
	return m
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 500 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}

func TestSetupRefusesBadInput(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, tc := range []struct {
		name string
		edit func(map[string]string)
		want string
	}{
		{"unknown arg", func(a map[string]string) { a["token"] = "x" }, `unknown arg "token"`},
		{"user without server", func(a map[string]string) { a["user_id"] = "owner" }, "user_id must be a full Matrix ID"},
		{"no password", func(a map[string]string) { delete(a, "password") }, "password is required"},
		{"not a recovery key", func(a map[string]string) { a["recovery_key"] = "EsTc 1234" }, "that is not a recovery key"},
		{"plain http", func(a map[string]string) { a["homeserver"] = "http://matrix.example.org" }, "homeserver must use https"},
	} {
		args := f.setupArgs()
		tc.edit(args)
		_, err := f.c.Setup(context.Background(), args)
		var ce *connector.Error
		if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || !strings.Contains(ce.Message, tc.want) {
			t.Errorf("%s: %v, want 400 %q", tc.name, err, tc.want)
		}
	}
	if n := locked(f.hs, func() int { return len(f.hs.logins) }); n != 0 {
		t.Errorf("logged in %d times", n)
	}
}

func TestSetupWrongPassword(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	args := f.setupArgs()
	args["password"] = "wrong"
	_, err := f.c.Setup(context.Background(), args)
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || ce.Message != "wrong user ID or password" {
		t.Fatalf("setup: %v", err)
	}
	if h := f.c.Health(context.Background()); h.State != connector.NeedsSetup {
		t.Fatalf("health = %+v", h)
	}
}

func TestSetupWrongRecoveryKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	args := f.setupArgs()
	args["recovery_key"] = utils.EncodeBase58RecoveryKey(make([]byte, 32))
	_, err := f.c.Setup(context.Background(), args)
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || ce.Message != "wrong recovery key" {
		t.Fatalf("setup: %v", err)
	}
	if got := locked(f.hs, func() []id.DeviceID { return f.hs.loggedOut }); !reflect.DeepEqual(got, []id.DeviceID{"MANDATE1"}) {
		t.Fatalf("logged out %v, want the new device", got)
	}
	if entries, _ := os.ReadDir(f.env.PrivateDir()); len(entries) != 0 {
		t.Fatalf("private dir keeps %v", entries)
	}
	if h := f.c.Health(context.Background()); h.State != connector.NeedsSetup {
		t.Fatalf("health = %+v", h)
	}
	// The password still works for a second try.
	f.link()
}

func TestLink(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dev := f.link()

	if got := locked(f.hs, func() mautrix.ReqLogin { return f.hs.logins[0] }); got.InitialDeviceDisplayName != "mandate" || got.Identifier.User != owner.String() {
		t.Fatalf("login %+v", got)
	}
	if h := f.c.Health(context.Background()); h.State != connector.Healthy {
		t.Fatalf("health = %+v", h)
	}
	b, err := os.ReadFile(filepath.Join(f.env.PrivateDir(), "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), password) {
		t.Fatal("the session file holds the password")
	}
	// The device is signed by the self-signing key from secret storage.
	f.hs.mu.Lock()
	var selfKey id.Ed25519
	for _, k := range f.hs.selfSigning.Keys {
		selfKey = k
	}
	signed := false
	for _, req := range f.hs.signatures {
		if sig := req[owner][dev.String()].Signatures[owner]; sig[id.NewKeyID(id.KeyAlgorithmEd25519, selfKey.String())] != "" {
			signed = true
		}
	}
	f.hs.mu.Unlock()
	if !signed {
		t.Fatalf("device %s was not cross-signed", dev)
	}

	if _, err := f.c.Setup(context.Background(), f.setupArgs()); err == nil || !strings.Contains(err.Error(), "already linked") {
		t.Fatalf("second setup: %v", err)
	}
}

func TestSyncStoresRoomsAndMessages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.link()
	n := f.hs.push(map[string]any{
		"account_data": map[string]any{"events": []map[string]any{
			{"type": "m.direct", "content": map[string]any{bob.String(): []id.RoomID{dmRoom}}},
		}},
		"rooms": map[string]any{"join": map[id.RoomID]map[string]any{
			dmRoom: room(
				[]map[string]any{member(owner, "Owner"), member(bob, "Bob")},
				[]map[string]any{
					textEvent("$hi", bob, 1000, "hi"),
					msgEvent("$photo", owner, 2000, map[string]any{"msgtype": "m.image", "body": "beach!", "filename": "beach.jpg"}),
					msgEvent("$re", bob, 3000, map[string]any{
						"msgtype": "m.text", "body": "nice",
						"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$photo"}},
					}),
					msgEvent("$edit", bob, 4000, map[string]any{
						"msgtype": "m.text", "body": "* hi there",
						"m.new_content": map[string]any{"msgtype": "m.text", "body": "hi there"},
						"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": "$hi"},
					}),
					// Neither may change the other's messages.
					msgEvent("$spoof", owner, 5000, map[string]any{
						"msgtype": "m.text", "body": "* hacked",
						"m.new_content": map[string]any{"msgtype": "m.text", "body": "hacked"},
						"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": "$hi"},
					}),
					{"type": "m.room.redaction", "event_id": "$rm1", "sender": bob, "origin_server_ts": 6000, "redacts": "$photo", "content": map[string]any{}},
				},
			),
			teamRoom: room(
				[]map[string]any{member(owner, "Owner"), member(bob, "Bob"), stateEvent("m.room.name", "", bob, map[string]any{"name": "Team"})},
				[]map[string]any{textEvent("$standup", bob, 7000, "standup?")},
			),
		}},
	})
	f.hs.waitSynced(n)

	type container struct {
		Kind, Name, Participants string
		Group                    bool
		Last                     string
	}
	got := map[string]container{}
	rows, err := f.st.DB().Query(`SELECT id, kind, COALESCE(name, ''), is_group, participants_json, COALESCE(last_message_at, '') FROM containers`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var idv string
		var c container
		if err := rows.Scan(&idv, &c.Kind, &c.Name, &c.Group, &c.Participants, &c.Last); err != nil {
			t.Fatal(err)
		}
		got[idv] = c
	}
	rows.Close()
	want := map[string]container{
		dmRoom.String():   {Kind: "dm", Name: "Bob", Participants: `[{"id":"@bob:example.org","name":"Bob"}]`, Last: "1970-01-01T00:00:05.000Z"},
		teamRoom.String(): {Kind: "group", Name: "Team", Participants: "[]", Group: true, Last: "1970-01-01T00:00:07.000Z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("containers\n got %+v\nwant %+v", got, want)
	}

	for idv, want := range map[string]storedMessage{
		"$hi":      {Sender: "Bob", Body: "hi there", Edited: true},
		"$photo":   {Sender: "Owner", Body: "beach!", Media: "image", FromMe: true},
		"$re":      {Sender: "Bob", Body: "nice", ReplyTo: "$photo"},
		"$standup": {Sender: "Bob", Body: "standup?"},
	} {
		if got := f.message(idv); got != want {
			t.Errorf("%s\n got %+v\nwant %+v", idv, got, want)
		}
	}
	var n2 int
	f.st.DB().QueryRow(`SELECT count(*) FROM messages WHERE id IN ('$edit', '$spoof', '$rm1')`).Scan(&n2)
	if n2 != 0 {
		t.Errorf("stored %d edits or redactions as messages", n2)
	}

	n = f.hs.push(joined(map[id.RoomID]map[string]any{dmRoom: room(nil, []map[string]any{
		{"type": "m.room.redaction", "event_id": "$rm2", "sender": bob, "origin_server_ts": 8000, "content": map[string]any{"redacts": "$re"}},
	})}))
	f.hs.waitSynced(n)
	if m := f.message("$re"); !m.Deleted {
		t.Fatalf("$re after its sender redacted it: %+v", m)
	}
}

func TestSyncDecrypts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// History from before linking, readable through the key backup.
	old, enc := f.encrypt(vaultRoom, "$old", "the old plan", 1000)
	f.backUp(vaultRoom, enc)
	dev := f.link()

	encryption := stateEvent("m.room.encryption", "", owner, map[string]any{"algorithm": "m.megolm.v1.aes-sha2"})
	// A key only Element has, then a key Element shares with this device
	// but whose message arrives first.
	f.hs.mu.Lock()
	f.hs.holdMail = true
	f.hs.mu.Unlock()
	secret, _ := f.encrypt(vaultRoom, "$secret", "nobody else", 2000)
	f.hs.mu.Lock()
	f.hs.inbox[dev] = nil
	f.hs.mu.Unlock()
	late, _ := f.encrypt(vaultRoom, "$late", "the new plan", 3000)
	n := f.hs.push(joined(map[id.RoomID]map[string]any{vaultRoom: room(
		[]map[string]any{encryption, member(owner, "Owner"), member(bob, "Bob")},
		[]map[string]any{old, secret, late},
	)}))
	f.hs.waitSynced(n)

	for idv, want := range map[string]storedMessage{
		"$old":    {Sender: "Owner", Body: "the old plan", FromMe: true},
		"$secret": {Sender: "Owner", Media: "encrypted", FromMe: true},
		"$late":   {Sender: "Owner", Media: "encrypted", FromMe: true},
	} {
		if got := f.message(idv); got != want {
			t.Errorf("%s\n got %+v\nwant %+v", idv, got, want)
		}
	}

	f.hs.mu.Lock()
	f.hs.holdMail = false
	f.hs.notify()
	f.hs.mu.Unlock()
	eventually(t, "the late key decrypts $late", func() bool { return f.message("$late").Body != "" })
	if got, want := f.message("$late"), (storedMessage{Sender: "Owner", Body: "the new plan", FromMe: true}); got != want {
		t.Fatalf("$late\n got %+v\nwant %+v", got, want)
	}
}

func TestFetch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.link()
	f.hs.mu.Lock()
	f.hs.history[teamRoom] = []map[string]any{
		textEvent("$h4", bob, 4000, "four"),
		member(bob, "Bobby"),
		textEvent("$h3", bob, 3000, "three"),
		textEvent("$h2", owner, 2000, "two"),
		textEvent("$h1", bob, 1000, "one"),
	}
	f.hs.members[teamRoom] = []map[string]any{member(bob, "Bob"), member(owner, "Owner")}
	f.hs.mu.Unlock()
	n := f.hs.push(joined(map[id.RoomID]map[string]any{teamRoom: room(
		[]map[string]any{stateEvent("m.room.name", "", bob, map[string]any{"name": "Team"})}, nil,
	)}))
	f.hs.waitSynced(n)

	read := func(args map[string]string) []string {
		t.Helper()
		raw, err := f.c.Read(context.Background(), "fetch", args)
		if err != nil {
			t.Fatal(err)
		}
		var res connector.ReadResult
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		return res.MessageIDs
	}
	if got := read(map[string]string{"chat": teamRoom.String(), "limit": "2"}); !reflect.DeepEqual(got, []string{"$h4", "$h3"}) {
		t.Fatalf("first page %v", got)
	}
	var seq string
	f.st.DB().QueryRow(`SELECT seq FROM messages WHERE id = '$h3'`).Scan(&seq)
	if got := read(map[string]string{"chat": teamRoom.String(), "before": seq, "limit": "2"}); !reflect.DeepEqual(got, []string{"$h2", "$h1"}) {
		t.Fatalf("second page %v", got)
	}
	if got := f.message("$h1"); got != (storedMessage{Sender: "Bob", Body: "one"}) {
		t.Fatalf("$h1 = %+v", got)
	}

	_, err := f.c.Read(context.Background(), "fetch", map[string]string{"chat": "!other:example.org"})
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
		t.Fatalf("unknown room: %v", err)
	}
}

func TestPrepare(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.c.Prepare(ctx, "send", map[string]string{"chat": dmRoom.String(), "text": "hi"}); !errors.Is(err, errNotLinked) {
		t.Fatalf("unlinked: %v", err)
	}
	f.link()
	if err := f.st.UpsertContainers(ctx, []store.Container{
		{ID: dmRoom.String(), Kind: kindDM, Name: "Bob"},
		{ID: teamRoom.String(), Kind: kindGroup, Name: "Team", IsGroup: true},
	}); err != nil {
		t.Fatal(err)
	}
	for room, want := range map[id.RoomID]approval.Field{dmRoom: {Label: "To", Value: "Bob"}, teamRoom: {Label: "Room", Value: "Team"}} {
		p, err := f.c.Prepare(ctx, "send", map[string]string{"chat": room.String(), "text": "on my way"})
		if err != nil {
			t.Fatal(err)
		}
		if p.Container != room.String() || !reflect.DeepEqual(p.Display, []approval.Field{want, {Label: "Text", Value: "on my way", Param: "text"}}) {
			t.Fatalf("prepared %+v", p)
		}
	}
	_, err := f.c.Prepare(ctx, "send", map[string]string{"chat": "!other:example.org", "text": "hi"})
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || !strings.Contains(ce.Message, "unknown chat") {
		t.Fatalf("unknown room: %v", err)
	}
	if len(f.hs.sent()) != 0 {
		t.Fatal("prepare sent something")
	}
}

func TestApprovedSendRunsOnce(t *testing.T) {
	t.Parallel()
	const rpID, origin = "box.example.ts.net", "https://box.example.ts.net:8781"
	f := newFixture(t)
	f.link()
	if err := f.st.UpsertContainer(context.Background(), store.Container{ID: dmRoom.String(), Kind: kindDM, Name: "Bob"}); err != nil {
		t.Fatal(err)
	}
	key := approvaltest.New(t, rpID, origin)
	dir := t.TempDir()
	seen, err := approval.OpenSeen(filepath.Join(dir, "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &connector.Server{Connector: f.c, Keys: approvaltest.Keys(t, key), Seen: seen, Log: f.env.Log}
	sock := filepath.Join(dir, "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, sock) }()
	t.Cleanup(func() { cancel(); <-done })
	client := connector.Dial(sock, 5*time.Second)
	eventually(t, "the socket serves", func() bool { _, err := client.Spec(context.Background()); return err == nil })

	p, err := client.Prepare(context.Background(), "send", map[string]string{"chat": dmRoom.String(), "text": "running late"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := approval.Encode(approval.Record{
		ID: "p1", Connector: "matrix", Op: "send", Args: p.Args, Display: p.Display,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	a := key.Approve(b, time.Now())
	res, err := client.Execute(context.Background(), a)
	if err != nil || res.Status != connector.Done || res.Ref != "$sent1" {
		t.Fatalf("execute: %+v %v", res, err)
	}
	var ce *connector.Error
	if _, err := client.Execute(context.Background(), a); !errors.As(err, &ce) || ce.Status != http.StatusConflict {
		t.Fatalf("replay: %v", err)
	}
	sent := f.hs.sent()
	if len(sent) != 1 || sent[0].room != dmRoom || sent[0].typ != "m.room.message" || sent[0].txn != "mandate-p1" ||
		!strings.Contains(string(sent[0].content), `"body":"running late"`) {
		t.Fatalf("sent %+v", sent)
	}
	if got := f.message("$sent1"); got != (storedMessage{Body: "running late", FromMe: true}) {
		t.Fatalf("stored %+v", got)
	}
}

func TestExecuteNeverRetries(t *testing.T) {
	t.Parallel()
	for mode, want := range map[string]connector.Status{"forbidden": connector.Failed, "hangup": connector.Unknown} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.link()
			f.hs.mu.Lock()
			f.hs.sendMode = mode
			f.hs.mu.Unlock()
			res := f.c.Execute(context.Background(), approval.Record{ID: "p1", Args: map[string]string{"chat": dmRoom.String(), "text": "hi"}})
			if res.Status != want {
				t.Fatalf("result %+v, want %s", res, want)
			}
			if n := len(f.hs.sent()); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
			var stored int
			f.st.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&stored)
			if stored != 0 {
				t.Fatal("stored a message that was not sent")
			}
		})
	}
}

func TestSendEncrypted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.link()
	n := f.hs.push(joined(map[id.RoomID]map[string]any{vaultRoom: room([]map[string]any{
		stateEvent("m.room.encryption", "", owner, map[string]any{"algorithm": "m.megolm.v1.aes-sha2"}),
		member(owner, "Owner"), member(bob, "Bob"),
	}, nil)}))
	f.hs.waitSynced(n)

	res := f.c.Execute(context.Background(), approval.Record{ID: "p1", Args: map[string]string{"chat": vaultRoom.String(), "text": "for your eyes"}})
	if res.Status != connector.Done {
		t.Fatalf("execute: %+v", res)
	}
	sent := f.hs.sent()
	if len(sent) != 1 || sent[0].typ != "m.room.encrypted" || strings.Contains(string(sent[0].content), "for your eyes") {
		t.Fatalf("sent %+v", sent)
	}

	// Element gets the room key and reads the message.
	ctx := context.Background()
	f.hs.mu.Lock()
	mail, err := json.Marshal(map[string]any{"next_batch": "x", "to_device": map[string]any{"events": f.hs.inbox["ELEMENT"]}})
	f.hs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var resp mautrix.RespSync
	if err := json.Unmarshal(mail, &resp); err != nil {
		t.Fatal(err)
	}
	f.element.mach.ProcessSyncResponse(ctx, &resp, "x")
	evt := &event.Event{Type: event.EventEncrypted, ID: "$sent1", Sender: owner, RoomID: vaultRoom, Timestamp: 1}
	if err := json.Unmarshal(sent[0].content, &evt.Content); err != nil {
		t.Fatal(err)
	}
	prepare(evt, vaultRoom)
	dec, err := f.element.mach.DecryptMegolmEvent(ctx, evt)
	if err != nil {
		t.Fatal(err)
	}
	if body := dec.Content.AsMessage().Body; body != "for your eyes" {
		t.Fatalf("element read %q", body)
	}
}

func TestUnlink(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dev := f.link()
	n := f.hs.push(joined(map[id.RoomID]map[string]any{teamRoom: room(nil, []map[string]any{textEvent("$kept", bob, 1000, "kept")})}))
	f.hs.waitSynced(n)

	if err := f.c.Unlink(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := locked(f.hs, func() []id.DeviceID { return f.hs.loggedOut }); !reflect.DeepEqual(got, []id.DeviceID{dev}) {
		t.Fatalf("logged out %v", got)
	}
	if entries, _ := os.ReadDir(f.env.PrivateDir()); len(entries) != 0 {
		t.Fatalf("private dir keeps %v", entries)
	}
	if h := f.c.Health(context.Background()); h.State != connector.NeedsSetup {
		t.Fatalf("health = %+v", h)
	}
	if res := f.c.Execute(context.Background(), approval.Record{ID: "p1", Args: map[string]string{"chat": teamRoom.String(), "text": "hi"}}); res.Status != connector.Failed {
		t.Fatalf("execute after unlink: %+v", res)
	}
	f.message("$kept")
	f.link() // and it links again
}

func TestLoggedOutByHomeserver(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dev := f.link()
	f.hs.mu.Lock()
	delete(f.hs.tokens, "token-"+string(dev))
	f.hs.notify()
	f.hs.mu.Unlock()
	f.hs.push(map[string]any{})
	eventually(t, "the connector notices", func() bool {
		return f.c.Health(context.Background()).State == connector.NeedsSetup
	})
	if h := f.c.Health(context.Background()); !strings.Contains(h.Detail, "logged out") {
		t.Fatalf("health = %+v", h)
	}
	eventually(t, "the session is forgotten", func() bool {
		_, err := os.Stat(filepath.Join(f.env.PrivateDir(), "session.json"))
		return errors.Is(err, os.ErrNotExist)
	})
}
