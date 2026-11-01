package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

var (
	owner     = types.NewADJID("15559990000", 0, 3)
	farid     = types.NewJID("15551234567", types.DefaultUserServer)
	samLee    = types.NewJID("15550001111", types.DefaultUserServer)
	samPark   = types.NewJID("447700900123", types.DefaultUserServer)
	samParkL  = types.NewJID("111222333", types.HiddenUserServer)
	stranger  = types.NewJID("15557654321", types.DefaultUserServer)
	climbing  = types.NewJID("120363000000000777", types.GroupServer)
	climbing2 = types.NewJID("120363000000000888", types.GroupServer)
)

type sent struct {
	to   types.JID
	text string
}

type fakeClient struct {
	mu          sync.Mutex
	connected   bool
	loggedIn    bool
	err         error
	sent        []sent
	qrs         []chan whatsmeow.QRChannelItem
	phones      []string
	disconnects int
	logouts     int
	logoutErr   error
}

func (f *fakeClient) IsConnected() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connected }
func (f *fakeClient) IsLoggedIn() bool  { f.mu.Lock(); defer f.mu.Unlock(); return f.loggedIn }

func (f *fakeClient) SendMessage(_ context.Context, to types.JID, msg *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sent{to: to, text: msg.GetConversation()})
	if f.err != nil {
		return whatsmeow.SendResponse{}, f.err
	}
	return whatsmeow.SendResponse{ID: "3EB0AA", Timestamp: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}, nil
}

// GetQRChannel opens a QR session whose first code is "qr-<session>-1".
func (f *fakeClient) GetQRChannel(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan whatsmeow.QRChannelItem, 8)
	ch <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: fmt.Sprintf("qr-%d-1", len(f.qrs)+1)}
	f.qrs = append(f.qrs, ch)
	return ch, nil
}

func (f *fakeClient) PairPhone(_ context.Context, phone string, _ bool, _ whatsmeow.PairClientType, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phones = append(f.phones, phone)
	return "ABCD-EFGH", nil
}

func (f *fakeClient) ConnectContext(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = true
	return nil
}

func (f *fakeClient) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = false
	f.disconnects++
}

func (f *fakeClient) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts++
	return f.logoutErr
}

func (f *fakeClient) calls() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

// qr emits item on the latest QR session; final items close it, as whatsmeow does.
func (f *fakeClient) qr(item whatsmeow.QRChannelItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := f.qrs[len(f.qrs)-1]
	ch <- item
	if item.Event != whatsmeow.QRChannelEventCode {
		close(ch)
	}
}

func (f *fakeClient) sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.qrs)
}

type fixture struct {
	c         *waConnector
	cli       *fakeClient // the first client
	st        *store.Store
	container *sqlstore.Container

	mu      sync.Mutex
	clients []*fakeClient
}

func newBareFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	container, _, err := openSession(context.Background(), filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Close() })
	st, err := openStore(context.Background(), filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &fixture{st: st, container: container}
}

func (f *fixture) start(dev *wastore.Device) {
	f.c = newConnector(context.Background(), f.st, slog.New(slog.NewTextHandler(io.Discard, nil)), dev, f.container.NewDevice,
		func(*wastore.Device, func(any)) waClient {
			cli := &fakeClient{}
			f.mu.Lock()
			f.clients = append(f.clients, cli)
			f.mu.Unlock()
			return cli
		})
	f.cli = f.client(0)
}

func (f *fixture) client(i int) *fakeClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients[i]
}

// pair stores dev as whatsmeow does on a successful pairing.
func (f *fixture) pair(t *testing.T, dev *wastore.Device) {
	t.Helper()
	dev.ID = &owner
	dev.PushName = "Owner"
	// Placeholder pairing data, sized to satisfy the session schema.
	dev.Account = &waAdv.ADVSignedDeviceIdentity{
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err := f.container.PutDevice(context.Background(), dev); err != nil {
		t.Fatal(err)
	}
}

// newFixture builds a linked, connected connector with an address book.
// Every contact also has a push name, which must never be shown as identity.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newBareFixture(t)
	dev := f.container.NewDevice()
	f.pair(t, dev)
	for _, c := range []struct {
		jid         types.JID
		first, full string
		push        string
	}{
		{farid, "Farid", "Farid Haddad", "Bank Support"},
		{samLee, "Sam", "Sam Lee", "S"},
		{samParkL, "Sam", "Sam Park", "sp"},
		{stranger, "", "", "Mom"},
	} {
		if c.full != "" {
			if err := dev.Contacts.PutContactName(ctx, c.jid, c.first, c.full); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := dev.Contacts.PutPushName(ctx, c.jid, c.push); err != nil {
			t.Fatal(err)
		}
	}
	if err := dev.LIDs.PutLIDMapping(ctx, samParkL, samPark); err != nil {
		t.Fatal(err)
	}
	for _, c := range []store.Container{
		{
			ID: climbing.String(), Kind: "group", IsGroup: true, Name: "Climbing crew",
			Participants: []store.Participant{{ID: farid.String()}, {ID: samPark.String()}},
		},
		{ID: climbing2.String(), Kind: "group", IsGroup: true, Name: "Climbing crew"},
		{ID: farid.String(), Kind: "dm", Name: "Bank Support"},
		{ID: samParkL.String(), Kind: "dm"},
		{ID: stranger.String(), Kind: "dm", Name: "Mom"},
	} {
		if err := f.st.UpsertContainer(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	f.start(dev)
	f.cli.connected, f.cli.loggedIn = true, true
	return f
}

func TestPrepareKnownChat(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for chat, want := range map[types.JID]approval.Field{
		farid:     {Label: "To", Value: "Farid Haddad (+15551234567)"},
		samParkL:  {Label: "To", Value: "Sam Park (+447700900123)"},
		stranger:  {Label: "To", Value: "+15557654321 (not in your contacts)"},
		climbing:  {Label: "Group", Value: "~Climbing crew (120363000000000777@g.us)"},
		climbing2: {Label: "Group", Value: "~Climbing crew (120363000000000888@g.us)"},
	} {
		ctx := context.Background()
		p, err := f.c.Prepare(ctx, "send", map[string]string{"chat": chat.String(), "text": "on my way"})
		if err != nil {
			t.Fatalf("%s: %v", chat, err)
		}
		wantP := connector.Prepared{
			Args:      map[string]string{"chat": chat.String(), "text": "on my way"},
			Display:   []approval.Field{want, {Label: "Text", Value: "on my way", Param: "text"}},
			Container: chat.String(),
		}
		if !reflect.DeepEqual(p, wantP) {
			t.Errorf("%s: prepared\n got %+v\nwant %+v", chat, p, wantP)
		}
		if again, err := f.c.Prepare(ctx, "send", p.Args); err != nil || !reflect.DeepEqual(again, p) {
			t.Errorf("%s: not idempotent: %+v %v", chat, again, err)
		}
	}
}

func TestPrepareRefusesUnknownChat(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, chat := range []string{samLee.String(), "Farid", "+15551234567", "15551234567:4@s.whatsapp.net", ""} {
		_, err := f.c.Prepare(context.Background(), "send", map[string]string{"chat": chat, "text": "hi"})
		var e *connector.Error
		if !errors.As(err, &e) || e.Status != http.StatusBadRequest || !strings.Contains(e.Message, "mandate whatsapp chats --search") {
			t.Errorf("%q: err = %v, want 400 with the chats hint", chat, err)
		}
	}
	for _, args := range []map[string]string{
		{"chat": farid.String(), "text": " \n"},
		{"chat": farid.String(), "text": "hi", "reply_to": "x"},
	} {
		_, err := f.c.Prepare(context.Background(), "send", args)
		wantStatus(t, err, http.StatusBadRequest)
	}
}

func messages(t *testing.T, st *store.Store) []store.Message {
	t.Helper()
	rows, err := st.DB().Query(`SELECT id, container_id, sender_id, is_from_me, body FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []store.Message
	for rows.Next() {
		var m store.Message
		if err := rows.Scan(&m.ID, &m.ContainerID, &m.SenderID, &m.IsFromMe, &m.Body); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestExecute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		connected bool
		loggedIn  bool
		sendErr   error
		want      connector.Status
		wantSends int
		wantStore bool
	}{
		{name: "sent", connected: true, loggedIn: true, want: connector.Done, wantSends: 1, wantStore: true},
		{name: "not logged in", connected: true, want: connector.Failed},
		{
			name: "send error", connected: true, loggedIn: true, sendErr: errors.New("websocket closed"),
			want: connector.Unknown, wantSends: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.cli.connected, f.cli.loggedIn, f.cli.err = tt.connected, tt.loggedIn, tt.sendErr
			rec := approval.Record{
				ID: "p1", Connector: "whatsapp", Op: "send",
				Args: map[string]string{"chat": farid.String(), "text": "running late"},
			}

			res := f.c.Execute(context.Background(), rec)
			if res.Status != tt.want {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Detail, tt.want)
			}
			if got := f.cli.calls(); len(got) != tt.wantSends {
				t.Fatalf("send called %d times, want %d", len(got), tt.wantSends)
			}
			msgs := messages(t, f.st)
			if !tt.wantStore {
				if len(msgs) != 0 {
					t.Fatalf("stored %+v", msgs)
				}
				return
			}
			if res.Ref != "3EB0AA" {
				t.Fatalf("ref = %q", res.Ref)
			}
			want := []store.Message{{
				ID: farid.String() + "/3EB0AA", ContainerID: farid.String(),
				SenderID: "15559990000@s.whatsapp.net", IsFromMe: true, Body: "running late",
			}}
			if !reflect.DeepEqual(msgs, want) {
				t.Fatalf("stored\n got %+v\nwant %+v", msgs, want)
			}
		})
	}
}

// serve runs f's connector behind connector.Server, which checks approvals.
func serve(t *testing.T, f *fixture) (*connector.Client, *approvaltest.Authenticator) {
	t.Helper()
	const rpID, origin = "box.example.ts.net", "https://box.example.ts.net:8781"
	key := approvaltest.New(t, rpID, origin)
	dir := t.TempDir()
	seen, err := approval.OpenSeen(filepath.Join(dir, "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &connector.Server{
		Connector: f.c,
		Keys:      approvaltest.Keys(t, key),
		Seen:      seen,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	sock := filepath.Join(dir, "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, sock) }()
	t.Cleanup(func() { cancel(); <-done })
	client := connector.Dial(sock, 5*time.Second)
	for i := 0; ; i++ {
		if _, err := client.Spec(context.Background()); err == nil {
			break
		} else if i == 100 {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return client, key
}

func approve(t *testing.T, key *approvaltest.Authenticator, p connector.Prepared) approval.Approval {
	t.Helper()
	b, err := approval.Encode(approval.Record{
		ID: "p1", Connector: "whatsapp", Op: "send", Args: p.Args, Display: p.Display,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return key.Approve(b, time.Now())
}

func TestApprovedSendRunsOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	client, key := serve(t, f)
	p, err := client.Prepare(context.Background(), "send", map[string]string{"chat": farid.String(), "text": "running late"})
	if err != nil {
		t.Fatal(err)
	}
	a := approve(t, key, p)

	res, err := client.Execute(context.Background(), a)
	if err != nil || res.Status != connector.Done {
		t.Fatalf("execute: %+v %v", res, err)
	}
	var cerr *connector.Error
	if _, err := client.Execute(context.Background(), a); !errors.As(err, &cerr) || cerr.Status != http.StatusConflict {
		t.Fatalf("replay: %v", err)
	}
	if got := f.cli.calls(); !reflect.DeepEqual(got, []sent{{to: farid, text: "running late"}}) {
		t.Fatalf("sends = %+v", got)
	}
}

// A security code that changed in the last week is shown on the approval,
// and one that changes after approving stops the send.
func TestIdentityChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client, key := serve(t, f)
	security := func(chat types.JID) string {
		t.Helper()
		p, err := f.c.Prepare(ctx, "send", map[string]string{"chat": chat.String(), "text": "hi"})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range p.Display {
			if d.Label == "Security" {
				return d.Value
			}
		}
		return ""
	}

	now := time.Now().UTC()
	in.handle(ctx, &events.IdentityChange{JID: farid, Timestamp: now.Add(-8 * 24 * time.Hour)})
	in.handle(ctx, &events.IdentityChange{JID: types.NewADJID(stranger.User, 0, 5), Timestamp: now, Implicit: true})
	if got := security(farid) + security(stranger); got != "" {
		t.Fatalf("old change or relinked device shown: %q", got)
	}

	in.handle(ctx, &events.IdentityChange{JID: samParkL, Timestamp: now.Add(-time.Hour)})
	at := now.Add(-time.Hour).Format("2006-01-02 15:04 UTC")
	if got, want := security(samParkL), "! security code changed "+at; got != want {
		t.Fatalf("dm: %q, want %q", got, want)
	}
	if got, want := security(climbing), "! security code changed for +447700900123 "+at; got != want {
		t.Fatalf("group: %q, want %q", got, want)
	}

	p, err := client.Prepare(ctx, "send", map[string]string{"chat": samParkL.String(), "text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	a := approve(t, key, p)
	in.handle(ctx, &events.IdentityChange{JID: samParkL, Timestamp: now})
	var cerr *connector.Error
	if _, err := client.Execute(ctx, a); !errors.As(err, &cerr) || cerr.Status != http.StatusConflict {
		t.Fatalf("execute after a change: %v", err)
	}
	if got := f.cli.calls(); len(got) != 0 {
		t.Fatalf("sent %+v", got)
	}
}
