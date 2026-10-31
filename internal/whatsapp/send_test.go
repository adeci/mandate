package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

var (
	owner    = types.NewADJID("15559990000", 0, 3)
	farid    = types.NewJID("15551234567", types.DefaultUserServer)
	samLee   = types.NewJID("15550001111", types.DefaultUserServer)
	samPark  = types.NewJID("447700900123", types.DefaultUserServer)
	samParkL = types.NewJID("111222333", types.HiddenUserServer)
	stranger = types.NewJID("15557654321", types.DefaultUserServer)
	climbing = types.NewJID("120363000000000777", types.GroupServer)
	samGroup = types.NewJID("120363000000000888", types.GroupServer)
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

// newBareFixture opens a real session database and data.db.
func newBareFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	container, _, err := openSession(context.Background(), filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Close() })
	st, err := store.Open(context.Background(), filepath.Join(dir, "data.db"))
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
	for _, g := range []store.Container{
		{ID: climbing.String(), Kind: "group", IsGroup: true, Name: "Climbing crew"},
		{ID: samGroup.String(), Kind: "group", IsGroup: true, Name: "Sam"},
		// DM containers can carry push names; they must not resolve names.
		{ID: stranger.String(), Kind: "dm", Name: "Mom"},
	} {
		if err := f.st.UpsertContainer(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	f.start(dev)
	f.cli.connected, f.cli.loggedIn = true, true
	return f
}

func invalid(err error) bool {
	var e *connector.Error
	return errors.As(err, &e) && e.Status == http.StatusBadRequest
}

func TestPrepareResolvesRecipient(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tests := []struct {
		name        string
		to          string
		wantTo      types.JID
		wantAliases []string
		want        approval.Field
		wantErr     string
	}{
		{
			name: "user JID", to: "15551234567@s.whatsapp.net", wantTo: farid,
			want: approval.Field{Label: "To", Value: "Farid Haddad (+1 555 123 4567)"},
		},
		{
			name: "device JID", to: "15551234567:4@s.whatsapp.net", wantTo: farid,
			want: approval.Field{Label: "To", Value: "Farid Haddad (+1 555 123 4567)"},
		},
		{
			name: "E.164", to: "+15551234567", wantTo: farid,
			want: approval.Field{Label: "To", Value: "Farid Haddad (+1 555 123 4567)"},
		},
		{
			name: "digits", to: "15551234567", wantTo: farid,
			want: approval.Field{Label: "To", Value: "Farid Haddad (+1 555 123 4567)"},
		},
		{
			name: "number not in contacts", to: "+1 555-765-4321", wantTo: stranger,
			want: approval.Field{Label: "To", Value: "+1 555 765 4321 (not in your contacts)"},
		},
		{
			name: "full name in any case", to: "farid HADDAD", wantTo: farid,
			want: approval.Field{Label: "To", Value: "Farid Haddad (+1 555 123 4567)"},
		},
		{
			name: "first name", to: "Farid", wantTo: farid,
			want: approval.Field{Label: "To", Value: "Farid Haddad (+1 555 123 4567)"},
		},
		{
			name: "contact listed under its LID", to: "Sam Park", wantTo: samPark, wantAliases: []string{samParkL.String()},
			want: approval.Field{Label: "To", Value: "Sam Park (+44 770 090 0123)"},
		},
		{
			name: "number with a LID", to: "+44 7700 900123", wantTo: samPark, wantAliases: []string{samParkL.String()},
			want: approval.Field{Label: "To", Value: "Sam Park (+44 770 090 0123)"},
		},
		{
			name: "LID JID", to: samParkL.String(), wantTo: samPark, wantAliases: []string{samParkL.String()},
			want: approval.Field{Label: "To", Value: "Sam Park (+44 770 090 0123)"},
		},
		{
			name: "group name", to: "climbing crew", wantTo: climbing,
			want: approval.Field{Label: "Group", Value: "Climbing crew"},
		},
		{
			name: "group JID", to: climbing.String(), wantTo: climbing,
			want: approval.Field{Label: "Group", Value: "Climbing crew"},
		},

		{name: "unknown name", to: "Nobody", wantErr: `no address-book contact or group is named "Nobody"`},
		{name: "push name", to: "Mom", wantErr: `no address-book contact or group is named "Mom"`},
		{name: "unknown group", to: "120363000000000999@g.us", wantErr: "unknown group"},
		{name: "LID without a known number", to: "999888777@lid", wantErr: "no phone number is known"},
		{name: "malformed user JID", to: "+15551234567@s.whatsapp.net", wantErr: "invalid user JID"},
		{name: "empty", to: " ", wantErr: "to is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			p, err := f.c.Prepare(ctx, "send", map[string]string{"to": tt.to, "text": "on my way"})
			if tt.wantErr != "" {
				if !invalid(err) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want Invalid containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := connector.Prepared{
				Args:      map[string]string{"to": tt.wantTo.String(), "text": "on my way"},
				Display:   []approval.Field{tt.want, {Label: "Text", Value: "on my way", Param: "text"}},
				Container: tt.wantTo.String(),
				Aliases:   tt.wantAliases,
			}
			if !reflect.DeepEqual(p, want) {
				t.Fatalf("prepared\n got %+v\nwant %+v", p, want)
			}
			again, err := f.c.Prepare(ctx, "send", p.Args)
			if err != nil || !reflect.DeepEqual(again, p) {
				t.Fatalf("not idempotent: %+v %v", again, err)
			}
		})
	}
}

// The gateway drops hidden candidates before the agent sees them, so the
// message itself must not name anyone.
func TestAmbiguousNameCandidates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.c.Prepare(context.Background(), "send", map[string]string{"to": "sam", "text": "hi"})
	var e *connector.Error
	if !errors.As(err, &e) || e.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 connector.Error", err)
	}
	want := []connector.Candidate{
		{ID: samGroup.String(), Label: "Sam (group)"},
		{ID: samLee.String(), Label: "Sam Lee (+1 ••• ••• 1111)"},
		{ID: samPark.String(), Aliases: []string{samParkL.String()}, Label: "Sam Park (+44 ••• ••• 0123)"},
	}
	if !reflect.DeepEqual(e.Candidates, want) {
		t.Fatalf("candidates\n got %+v\nwant %+v", e.Candidates, want)
	}
	for _, leak := range []string{"Lee", "Park", "1111", "0123", "@"} {
		if strings.Contains(e.Message, leak) {
			t.Errorf("message %q names a candidate (%q)", e.Message, leak)
		}
	}
}

func TestPrepareRejectsBadArgs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for name, args := range map[string]map[string]string{
		"missing text":    {"to": "Farid"},
		"blank text":      {"to": "Farid", "text": " \n"},
		"unknown arg":     {"to": "Farid", "text": "hi", "reply_to": "x"},
		"missing to":      {"text": "hi"},
		"newsletter JID":  {"to": "123@newsletter", "text": "hi"},
		"broadcast JID":   {"to": "status@broadcast", "text": "hi"},
		"short digit run": {"to": "12345", "text": "hi"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.c.Prepare(context.Background(), "send", args); !invalid(err) {
				t.Fatalf("err = %v, want Invalid", err)
			}
		})
	}
}

func TestDisplayNeverShowsPushName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, to := range []string{farid.String(), stranger.String(), "Farid"} {
		p, err := f.c.Prepare(context.Background(), "send", map[string]string{"to": to, "text": "hi"})
		if err != nil {
			t.Fatal(err)
		}
		for _, push := range []string{"Bank Support", "Mom"} {
			if strings.Contains(p.Display[0].Value, push) {
				t.Fatalf("%s: display %q shows push name %q", to, p.Display[0].Value, push)
			}
		}
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
		{name: "not connected", loggedIn: true, want: connector.Failed},
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
				Args: map[string]string{"to": farid.String(), "text": "running late"},
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
			var name string
			if err := f.st.DB().QueryRow(`SELECT name FROM containers WHERE id = ?`, farid.String()).Scan(&name); err != nil || name != "Farid Haddad" {
				t.Fatalf("container name = %q, %v", name, err)
			}
		})
	}
}

func TestApprovedSendRunsOnce(t *testing.T) {
	t.Parallel()
	const rpID, origin = "box.example.ts.net", "https://box.example.ts.net:8781"
	f := newFixture(t)
	key := approvaltest.New(t, rpID, origin)
	dir := t.TempDir()
	seen, err := approval.OpenSeen(filepath.Join(dir, "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &connector.Server{
		Connector: f.c,
		Verifier:  &approval.Verifier{RPID: rpID, Origin: origin, Approvers: []approval.Approver{key.Approver("phone")}},
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

	p, err := client.Prepare(context.Background(), "send", map[string]string{"to": "Farid", "text": "running late"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := approval.Encode(approval.Record{
		ID: "p1", Connector: "whatsapp", Op: "send", Args: p.Args, Display: p.Display,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	a := key.Approve(b, time.Now())

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

func TestLoadVerifier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	approver := approvaltest.New(t, "rp", "https://rp").Approver("phone")
	enrolled, err := json.Marshal([]approval.Approver{approver})
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name        string
		path        string
		rpID        string
		wantRefuses bool
		wantErr     bool
	}{
		{name: "no flag", path: "", wantRefuses: true},
		{name: "missing file", path: filepath.Join(dir, "absent.json"), wantRefuses: true},
		{name: "empty file", path: write("empty.json", ""), wantRefuses: true},
		{name: "no approvers", path: write("none.json", "[]"), wantRefuses: true},
		{name: "garbage", path: write("bad.json", "{"), wantErr: true},
		{name: "approvers without rp-id", path: write("one.json", string(enrolled)), wantErr: true},
		{name: "approvers", path: write("ok.json", string(enrolled)), rpID: "rp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := loadVerifier(tt.path, tt.rpID, "https://rp")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if !tt.wantErr && (v == nil) != tt.wantRefuses {
				t.Fatalf("verifier = %+v, want nil: %v", v, tt.wantRefuses)
			}
		})
	}
}
