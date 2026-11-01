package main

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/google/uuid"
	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	sgstore "go.mau.fi/mautrix-signal/pkg/signalmeow/store"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

var (
	owner       = uuid.MustParse("00000000-0000-4000-8000-000000000001")
	farid       = uuid.MustParse("00000000-0000-4000-8000-000000000002")
	sam         = uuid.MustParse("00000000-0000-4000-8000-000000000003")
	mallory     = uuid.MustParse("00000000-0000-4000-8000-000000000004")
	stranger    = uuid.MustParse("00000000-0000-4000-8000-000000000005")
	strangerPNI = uuid.MustParse("00000000-0000-4000-8000-000000000105")
	climbing    = types.GroupIdentifier(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	bouldering  = types.GroupIdentifier(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type sent struct {
	to    string
	text  string
	timer uint32
}

type fakeClient struct {
	dev    *sgstore.Device
	groups map[types.GroupIdentifier]*signalmeow.Group

	mu            sync.Mutex
	status        chan signalmeow.SignalConnectionStatus
	starts, stops int
	syncs         int
	unlinks       int
	unlinkErr     error
	sendErr       error
	failMembers   int
	sent          []sent
}

func (f *fakeClient) StartReceiveLoops(context.Context) (chan signalmeow.SignalConnectionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.status = make(chan signalmeow.SignalConnectionStatus, 8)
	return f.status, nil
}

func (f *fakeClient) StopReceiveLoops() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.status != nil {
		close(f.status)
		f.status = nil
	}
	return nil
}

func (f *fakeClient) QueueStorageSync(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
}

func (f *fakeClient) Unlink(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlinks++
	return f.unlinkErr
}

func (f *fakeClient) SendMessage(_ context.Context, to libsignalgo.ServiceID, c *signalpb.Content) signalmeow.SendMessageResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	dm := c.GetDataMessage()
	f.sent = append(f.sent, sent{to: to.String(), text: dm.GetBody(), timer: dm.GetExpireTimer()})
	if f.sendErr != nil {
		return signalmeow.SendMessageResult{FailedSendResult: signalmeow.FailedSendResult{Recipient: to, Error: f.sendErr}}
	}
	return signalmeow.SendMessageResult{WasSuccessful: true, SuccessfulSendResult: signalmeow.SuccessfulSendResult{Recipient: to}}
}

func (f *fakeClient) SendGroupMessage(_ context.Context, gid types.GroupIdentifier, c *signalpb.Content) (*signalmeow.GroupMessageSendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dm := c.GetDataMessage()
	f.sent = append(f.sent, sent{to: string(gid), text: dm.GetBody(), timer: dm.GetExpireTimer()})
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	res := &signalmeow.GroupMessageSendResult{}
	for i, m := range f.groups[gid].Members {
		to := libsignalgo.NewACIServiceID(m.ACI)
		if i < f.failMembers {
			res.FailedToSendTo = append(res.FailedToSendTo, signalmeow.FailedSendResult{Recipient: to, Error: errors.New("unreachable")})
		} else {
			res.SuccessfullySentTo = append(res.SuccessfullySentTo, signalmeow.SuccessfulSendResult{Recipient: to})
		}
	}
	return res, nil
}

func (f *fakeClient) ContactByACI(ctx context.Context, aci uuid.UUID) (*types.Recipient, error) {
	return f.dev.RecipientStore.LoadAndUpdateRecipient(ctx, aci, uuid.Nil, nil)
}

func (f *fakeClient) RetrieveGroupByID(_ context.Context, gid types.GroupIdentifier, _ uint32) (*signalmeow.Group, *signalmeow.SendEndorsementCache, error) {
	if g, ok := f.groups[gid]; ok {
		return g, nil, nil
	}
	return nil, nil, errors.New("no such group")
}

func (f *fakeClient) emit(s signalmeow.SignalConnectionStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status <- s
}

func (f *fakeClient) calls() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

type fixture struct {
	c      *sgConnector
	st     *store.Store
	sess   *session
	groups map[types.GroupIdentifier]*signalmeow.Group

	mu         sync.Mutex
	clients    []*fakeClient
	provisions []chan signalmeow.ProvisioningResponse
}

func newBareFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	sess, err := openSession(context.Background(), filepath.Join(dir, "session.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	st, err := store.Open(context.Background(), filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &fixture{st: st, sess: sess, groups: map[types.GroupIdentifier]*signalmeow.Group{}}
}

func (f *fixture) start(t *testing.T, dev *sgstore.Device) {
	t.Helper()
	f.c = newConnector(context.Background(), f.st, discard, f.sess, dev,
		func(context.Context) <-chan signalmeow.ProvisioningResponse {
			f.mu.Lock()
			defer f.mu.Unlock()
			ch := make(chan signalmeow.ProvisioningResponse, 4)
			ch <- signalmeow.ProvisioningResponse{
				State:           signalmeow.StateProvisioningURLReceived,
				ProvisioningURL: fmt.Sprintf("sgnl://linkdevice?uuid=%d", len(f.provisions)+1),
			}
			f.provisions = append(f.provisions, ch)
			return ch
		},
		func(dev *sgstore.Device, _ func(events.SignalEvent)) sgClient {
			cli := &fakeClient{dev: dev, groups: f.groups}
			f.mu.Lock()
			f.clients = append(f.clients, cli)
			f.mu.Unlock()
			return cli
		})
	if err := f.c.connect(); err != nil {
		t.Fatal(err)
	}
}

// cli is the latest client.
func (f *fixture) cli() *fakeClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients[len(f.clients)-1]
}

func (f *fixture) provisioned() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.provisions)
}

// provide sends resp on the latest provisioning session; final responses
// close it, as signalmeow does.
func (f *fixture) provide(resp signalmeow.ProvisioningResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := f.provisions[len(f.provisions)-1]
	ch <- resp
	if resp.State != signalmeow.StateProvisioningURLReceived {
		close(ch)
	}
}

// putDevice stores the owner's device as provisioning does.
func (f *fixture) putDevice(t *testing.T) *sgstore.DeviceData {
	t.Helper()
	kp, err := libsignalgo.GenerateIdentityKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	data := &sgstore.DeviceData{ACIIdentityKeyPair: kp, ACI: owner, ACIRegistrationID: 1, DeviceID: 2, Number: "+15559990000", Password: "pw"}
	if err := f.sess.devices.PutDevice(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	return data
}

// newFixture builds a linked, connected connector with an address book.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newBareFixture(t)
	f.putDevice(t)
	dev, err := f.sess.devices.DeviceByACI(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*types.Recipient{
		{ACI: owner, E164: "+15559990000", Profile: types.Profile{Name: "Owner"}},
		{ACI: farid, E164: "+15551234567", ContactName: "Farid Haddad", Profile: types.Profile{Name: "Bank Support"}},
		{ACI: sam, E164: "+15550001111", ContactName: "Samuel Lee", Nickname: "Sam"},
		{ACI: mallory, Profile: types.Profile{Name: "Mom"}},
		{ACI: stranger, PNI: strangerPNI, E164: "+15557654321"},
	} {
		if err := dev.RecipientStore.StoreRecipient(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	f.groups[climbing] = &signalmeow.Group{GroupIdentifier: climbing, Title: "Climbing crew", Members: []*signalmeow.GroupMember{
		{ACI: owner}, {ACI: farid}, {ACI: mallory},
	}}
	for _, c := range []store.Container{
		{
			ID: string(climbing), Kind: "group", IsGroup: true, Name: "Climbing crew",
			Participants: []store.Participant{{ID: owner.String()}, {ID: farid.String()}, {ID: mallory.String()}},
		},
		{ID: string(bouldering), Kind: "group", IsGroup: true, Name: "Climbing crew"},
		{ID: farid.String(), Kind: "dm", Name: "Bank Support"},
		{ID: mallory.String(), Kind: "dm"},
		{ID: stranger.String(), Kind: "dm"},
	} {
		if err := f.st.UpsertContainer(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	f.start(t, dev)
	f.cli().emit(signalmeow.SignalConnectionStatus{Event: signalmeow.SignalConnectionEventConnected})
	eventually(t, func() bool { return f.c.Health(ctx).State == connector.Healthy })
	return f
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPrepareKnownChat(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for chat, want := range map[string]approval.Field{
		farid.String():     {Label: "To", Value: "Farid Haddad (+15551234567)"},
		mallory.String():   {Label: "To", Value: "~Mom (" + mallory.String() + ")"},
		stranger.String():  {Label: "To", Value: "+15557654321 (not in your contacts)"},
		string(climbing):   {Label: "Group", Value: "~Climbing crew (" + string(climbing) + ")"},
		string(bouldering): {Label: "Group", Value: "~Climbing crew (" + string(bouldering) + ")"},
	} {
		ctx := context.Background()
		p, err := f.c.Prepare(ctx, "send", map[string]string{"chat": chat, "text": "on my way"})
		if err != nil {
			t.Fatalf("%s: %v", chat, err)
		}
		wantP := connector.Prepared{
			Args:      map[string]string{"chat": chat, "text": "on my way"},
			Display:   []approval.Field{want, {Label: "Text", Value: "on my way", Param: "text"}},
			Container: chat,
		}
		if !reflect.DeepEqual(p, wantP) {
			t.Errorf("%s: prepared\n got %+v\nwant %+v", chat, p, wantP)
		}
		if again, err := f.c.Prepare(ctx, "send", p.Args); err != nil || !reflect.DeepEqual(again, p) {
			t.Errorf("%s: not idempotent: %+v %v", chat, again, err)
		}
	}
}

// Prepare warns of safety numbers that changed in the last week.
func TestPrepareShowsIdentityChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	security := func(chat string) string {
		t.Helper()
		p, err := f.c.Prepare(ctx, "send", map[string]string{"chat": chat, "text": "hi"})
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

	in := newIngester(f.cli(), f.c.dev, f.st, f.sess, discard)
	in.handle(ctx, &events.IdentityChange{ServiceID: libsignalgo.NewPNIServiceID(strangerPNI)})
	if got := security(stranger.String()); !strings.HasPrefix(got, "! safety number changed 20") {
		t.Fatalf("stranger: %q", got)
	}

	at := time.Now().UTC().Add(-6 * 24 * time.Hour)
	for id, when := range map[string]time.Time{farid.String(): at, mallory.String(): at.Add(-2 * 24 * time.Hour)} {
		if err := f.sess.identities.changed(ctx, id, when); err != nil {
			t.Fatal(err)
		}
	}
	want := "! safety number changed " + at.Format("2006-01-02 15:04 UTC")
	if got := security(farid.String()); got != want {
		t.Fatalf("farid: %q, want %q", got, want)
	}
	if got := security(mallory.String()); got != "" {
		t.Fatalf("change from 8 days ago shown: %q", got)
	}
	want = "! safety number changed for +15551234567 " + at.Format("2006-01-02 15:04 UTC")
	if got := security(string(climbing)); got != want {
		t.Fatalf("group: %q, want %q", got, want)
	}
}

func TestPrepareRefusesUnknownChat(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, chat := range []string{sam.String(), "Sam", "+15550001111", "PNI:" + strangerPNI.String(), ""} {
		_, err := f.c.Prepare(context.Background(), "send", map[string]string{"chat": chat, "text": "hi"})
		var e *connector.Error
		if !errors.As(err, &e) || e.Status != http.StatusBadRequest || !strings.Contains(e.Message, "mandate signal chats --search") {
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
	rows, err := st.DB().Query(`SELECT id, container_id, COALESCE(sender_id, ''), COALESCE(sender_name, ''), is_from_me,
		COALESCE(body, ''), COALESCE(media_type, ''), COALESCE(reply_to_id, '') FROM messages ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []store.Message
	for rows.Next() {
		var m store.Message
		if err := rows.Scan(&m.ID, &m.ContainerID, &m.SenderID, &m.SenderName, &m.IsFromMe, &m.Body, &m.MediaType, &m.ReplyToID); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestExecute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		chat        string
		connected   bool
		sendErr     error
		failMembers int
		want        connector.Status
		wantDetail  string
		wantSends   int
	}{
		{name: "dm", chat: farid.String(), connected: true, want: connector.Done, wantSends: 1},
		{name: "group", chat: string(climbing), connected: true, want: connector.Done, wantSends: 1},
		{
			name: "group, some members unreachable", chat: string(climbing), connected: true, failMembers: 1,
			want: connector.Done, wantDetail: "not delivered to 1 of 3 members", wantSends: 1,
		},
		{name: "not connected", chat: farid.String(), want: connector.Failed},
		{name: "dm send error", chat: farid.String(), connected: true, sendErr: errors.New("websocket closed"), want: connector.Unknown, wantSends: 1},
		{name: "group send error", chat: string(climbing), connected: true, sendErr: errors.New("websocket closed"), want: connector.Unknown, wantSends: 1},
		{name: "group reached nobody", chat: string(climbing), connected: true, failMembers: 3, want: connector.Unknown, wantSends: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			cli := f.cli()
			cli.sendErr, cli.failMembers = tt.sendErr, tt.failMembers
			if !tt.connected {
				cli.emit(signalmeow.SignalConnectionStatus{Event: signalmeow.SignalConnectionEventDisconnected})
				eventually(t, func() bool { return f.c.Health(context.Background()).State == connector.Broken })
			}
			rec := approval.Record{
				ID: "p1", Connector: "signal", Op: "send",
				Args: map[string]string{"chat": tt.chat, "text": "running late"},
			}

			res := f.c.Execute(context.Background(), rec)
			if res.Status != tt.want || tt.wantDetail != "" && res.Detail != tt.wantDetail {
				t.Fatalf("result = %+v, want %s %q", res, tt.want, tt.wantDetail)
			}
			if got := cli.calls(); len(got) != tt.wantSends {
				t.Fatalf("send called %d times, want %d", len(got), tt.wantSends)
			}
			msgs := messages(t, f.st)
			if tt.want != connector.Done {
				if len(msgs) != 0 {
					t.Fatalf("stored %+v", msgs)
				}
				return
			}
			want := []store.Message{{
				ID: tt.chat + "/" + owner.String() + "/" + res.Ref, ContainerID: tt.chat,
				SenderID: owner.String(), IsFromMe: true, Body: "running late",
			}}
			if !reflect.DeepEqual(msgs, want) {
				t.Fatalf("stored\n got %+v\nwant %+v", msgs, want)
			}
		})
	}
}

// A message sent without the chat's disappearing timer would turn it off.
func TestExecuteKeepsTimer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.groups[climbing].DisappearingMessagesDuration = 86400
	in := newIngester(f.cli(), f.c.dev, f.st, f.sess, discard)
	for _, m := range []*signalpb.DataMessage{
		{Body: new("hi"), Timestamp: new(uint64(1)), ExpireTimer: new(uint32(3600)), ExpireTimerVersion: new(uint32(2))},
		// A message from a client that has not seen the latest change.
		{Body: new("hi"), Timestamp: new(uint64(2)), ExpireTimer: new(uint32(60)), ExpireTimerVersion: new(uint32(1))},
	} {
		if !in.handle(ctx, &events.ChatEvent{Info: events.MessageInfo{Sender: farid, ChatID: farid.String()}, Event: m}) {
			t.Fatal("handle failed")
		}
	}
	for _, chat := range []string{farid.String(), mallory.String(), string(climbing)} {
		res := f.c.Execute(ctx, approval.Record{Op: "send", Args: map[string]string{"chat": chat, "text": "x"}})
		if res.Status != connector.Done {
			t.Fatalf("%s: %+v", chat, res)
		}
	}
	got := f.cli().calls()
	want := []sent{
		{to: farid.String(), text: "x", timer: 3600},
		{to: mallory.String(), text: "x"},
		{to: string(climbing), text: "x", timer: 86400},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sends\n got %+v\nwant %+v", got, want)
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
		Keys:      approvaltest.Keys(t, key),
		Seen:      seen,
		Log:       discard,
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

	p, err := client.Prepare(context.Background(), "send", map[string]string{"chat": farid.String(), "text": "running late"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := approval.Encode(approval.Record{
		ID: "p1", Connector: "signal", Op: "send", Args: p.Args, Display: p.Display,
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
	if got := f.cli().calls(); !reflect.DeepEqual(got, []sent{{to: farid.String(), text: "running late"}}) {
		t.Fatalf("sends = %+v", got)
	}
}
