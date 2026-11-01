package main

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
	"sync"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// phone is what the fake clients of one fixture talk to.
type phone struct {
	mu        sync.Mutex
	convs     []*gmproto.Conversation
	msgs      map[string][]*gmproto.Message
	fetchErr  map[string]error
	fetches   []string
	sent      []*gmproto.SendMessageRequest
	sendResp  *gmproto.SendMessageResponse
	sendErr   error
	connected map[*fakeClient]bool
}

func (p *phone) setConvs(convs ...*gmproto.Conversation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.convs = convs
}

func (p *phone) setMessages(conv string, msgs ...*gmproto.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs[conv] = msgs
}

func (p *phone) fetched() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.fetches
	p.fetches = nil
	return out
}

type fakeClient struct {
	p    *phone
	auth *libgm.AuthData
	emit func(any)

	mu          sync.Mutex
	connectErr  error
	connects    int
	unpairs     int
	activations int
}

func (f *fakeClient) FetchConfig(context.Context) error { return nil }

func (f *fakeClient) Connect(context.Context) error {
	f.mu.Lock()
	f.connects++
	err := f.connectErr
	f.mu.Unlock()
	if err == nil {
		f.p.mu.Lock()
		f.p.connected[f] = true
		f.p.mu.Unlock()
	}
	return err
}

func (f *fakeClient) Disconnect() {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	delete(f.p.connected, f)
}

func (f *fakeClient) isConnected() bool {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	return f.p.connected[f]
}

func (f *fakeClient) Unpair(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpairs++
	return nil
}

func (f *fakeClient) SetActiveSession(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activations++
	return nil
}

func (f *fakeClient) ListConversations(context.Context, *gmproto.ListConversationsRequest) (*gmproto.ListConversationsResponse, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	return &gmproto.ListConversationsResponse{Conversations: f.p.convs}, nil
}

func (f *fakeClient) FetchMessages(_ context.Context, id string, count int64, _ *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	f.p.fetches = append(f.p.fetches, id)
	if err := f.p.fetchErr[id]; err != nil {
		return nil, err
	}
	if count != syncMessages {
		return nil, fmt.Errorf("fetched %d messages", count)
	}
	return &gmproto.ListMessagesResponse{Messages: f.p.msgs[id]}, nil
}

func (f *fakeClient) GetConversation(_ context.Context, id string) (*gmproto.Conversation, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	for _, c := range f.p.convs {
		if c.GetConversationID() == id {
			return c, nil
		}
	}
	return nil, errors.New("no such conversation")
}

func (f *fakeClient) SendMessage(_ context.Context, req *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	f.p.sent = append(f.p.sent, req)
	return f.p.sendResp, f.p.sendErr
}

// fakePairing numbers its codes qr-PAIRING-CODE.
type fakePairing struct {
	n      int
	auth   *libgm.AuthData
	paired func()

	mu           sync.Mutex
	codes        int
	disconnected bool
	unpaired     bool
}

func (f *fakePairing) FetchConfig(context.Context) error { return nil }

func (f *fakePairing) StartLogin(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes = 1
	return fmt.Sprintf("qr-%d-1", f.n), nil
}

func (f *fakePairing) RefreshPhoneRelay() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes++
	return fmt.Sprintf("qr-%d-%d", f.n, f.codes), nil
}

func (f *fakePairing) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected = true
}

func (f *fakePairing) Unpair(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpaired = true
	return nil
}

type pairState struct {
	codes                  int
	disconnected, unpaired bool
}

func (f *fakePairing) state() pairState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return pairState{f.codes, f.disconnected, f.unpaired}
}

// scan is the phone scanning a code: libgm fills in auth, then calls back.
func (f *fakePairing) scan() {
	f.auth.Browser = &gmproto.Device{UserID: 1, SourceID: "browser"}
	f.auth.Mobile = &gmproto.Device{UserID: 1, SourceID: "phone"}
	f.auth.TachyonAuthToken = []byte("token-1")
	f.paired()
}

type fixture struct {
	t       *testing.T
	c       *gmConnector
	st      *store.Store
	session connector.Secret
	phone   *phone

	mu       sync.Mutex
	clients  []*fakeClient
	pairings []*fakePairing
}

func newFixture(t *testing.T, linked bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{
		t:       t,
		st:      st,
		session: (&connector.Env{StateDir: dir}).Secret("session.json"),
		phone: &phone{
			msgs:      make(map[string][]*gmproto.Message),
			fetchErr:  make(map[string]error),
			connected: make(map[*fakeClient]bool),
			sendResp:  &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS},
		},
	}
	if linked {
		auth := libgm.NewAuthData()
		auth.Browser = &gmproto.Device{SourceID: "browser"}
		data, err := json.Marshal(auth)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.session.Store(data); err != nil {
			t.Fatal(err)
		}
	}
	f.start()
	return f
}

// start runs the connector as the program does after a restart.
func (f *fixture) start() {
	f.t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	f.c = newConnector(ctx, f.st, log, f.session, f.newClient, f.newPairing)
	f.c.qrLifetime, f.c.settleTime = 20*time.Millisecond, time.Millisecond
	if err := f.c.resume(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) newClient(auth *libgm.AuthData, onEvent func(any)) gmClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	cli := &fakeClient{p: f.phone, auth: auth, emit: onEvent}
	f.clients = append(f.clients, cli)
	return cli
}

func (f *fixture) newPairing(auth *libgm.AuthData, paired func()) pairClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &fakePairing{n: len(f.pairings) + 1, auth: auth, paired: paired}
	f.pairings = append(f.pairings, p)
	return p
}

func (f *fixture) client(i int) *fakeClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.clients) {
		f.t.Fatalf("no client %d", i)
	}
	return f.clients[i]
}

func (f *fixture) pairing(i int) *fakePairing {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.pairings) {
		f.t.Fatalf("no pairing %d", i)
	}
	return f.pairings[i]
}

// cli is the current client.
func (f *fixture) cli() *fakeClient {
	cli, _ := f.c.state()
	if cli == nil {
		f.t.Fatal("not linked")
	}
	return cli.(*fakeClient)
}

// activate is the phone accepting the session, which starts a sync.
func (f *fixture) activate() {
	f.t.Helper()
	f.c.event(f.cli(), &gmproto.UserAlertEvent{AlertType: gmproto.AlertType_BROWSER_ACTIVE})
	wantHealth(f.t, f.c, connector.Healthy)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func wantHealth(t *testing.T, c *gmConnector, state connector.HealthState) connector.Health {
	t.Helper()
	h := c.Health(context.Background())
	if h.State != state {
		t.Fatalf("health = %+v, want %s", h, state)
	}
	return h
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var e *connector.Error
	if !errors.As(err, &e) || e.Status != status {
		t.Fatalf("err = %v, want status %d", err, status)
	}
}

// setupUntil polls Setup like the console does until ok accepts the step.
func setupUntil(t *testing.T, c *gmConnector, ok func(connector.SetupStep) bool) connector.SetupStep {
	t.Helper()
	var step connector.SetupStep
	var err error
	eventually(t, "setup step", func() bool {
		step, err = c.Setup(context.Background(), nil)
		return err == nil && ok(step)
	})
	return step
}

func storedSession(t *testing.T, s connector.Secret) *libgm.AuthData {
	t.Helper()
	data, err := s.Load()
	if err != nil || data == nil {
		t.Fatalf("session: %q, %v", data, err)
	}
	auth := &libgm.AuthData{}
	if err := json.Unmarshal(data, auth); err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestPairFromConsole(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	ctx := context.Background()
	if h := wantHealth(t, f.c, connector.NeedsSetup); h.Detail != "not linked" {
		t.Fatalf("health %+v", h)
	}
	if _, err := f.c.Setup(ctx, map[string]string{"phone": "+15551230001"}); err == nil {
		t.Fatal("setup took an unknown arg")
	}

	step, err := f.c.Setup(ctx, nil)
	if err != nil || step.QR != "qr-1-1" || step.Instructions == "" || step.Done {
		t.Fatalf("first step %+v, %v", step, err)
	}
	// Codes rotate within the one pairing while the console polls.
	setupUntil(t, f.c, func(s connector.SetupStep) bool { return s.QR == "qr-1-2" })

	f.pairing(0).scan()
	setupUntil(t, f.c, func(s connector.SetupStep) bool { return s.Done })
	if auth := storedSession(t, f.session); auth.Browser.GetSourceID() != "browser" || string(auth.TachyonAuthToken) != "token-1" {
		t.Fatalf("stored session %+v", auth)
	}
	_, err = f.c.Setup(ctx, nil)
	wantStatus(t, err, http.StatusConflict)
	if p := f.pairing(0).state(); !p.disconnected || p.unpaired {
		t.Fatalf("pairing client: %+v", p)
	}
	eventually(t, "connect", f.client(0).isConnected)
	if h := wantHealth(t, f.c, connector.Broken); h.Detail != "connecting…" {
		t.Fatalf("health %+v", h)
	}
	f.activate()
}

func TestPairingExpires(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	f.c.qrCodes = 2
	if _, err := f.c.Setup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	// Once its codes ran out, the next poll starts a new pairing.
	setupUntil(t, f.c, func(s connector.SetupStep) bool { return s.QR == "qr-2-1" })
	if p := f.pairing(0).state(); !p.disconnected || p.codes != 2 {
		t.Fatalf("expired pairing: %+v", p)
	}
}

func TestResume(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	cli := f.cli()
	if !cli.isConnected() || cli.auth.Browser.GetSourceID() != "browser" {
		t.Fatal("did not connect with the stored session")
	}
	_, err := f.c.Setup(context.Background(), nil)
	wantStatus(t, err, http.StatusConflict)

	// A refreshed token is saved, so the next start uses it.
	cli.auth.TachyonAuthToken = []byte("token-2")
	cli.emit(&events.AuthTokenRefreshed{})
	f.c.disconnect()
	f.start()
	if got := string(f.cli().auth.TachyonAuthToken); got != "token-2" || !f.cli().isConnected() {
		t.Fatalf("restarted with token %q", got)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	f.activate()
	cli := f.cli()
	for _, tc := range []struct {
		evt     any
		state   connector.HealthState
		resumed bool
	}{
		{&events.PhoneNotResponding{}, connector.Broken, false},
		{&events.PhoneRespondingAgain{}, connector.Healthy, false},
		{&events.ListenTemporaryError{Error: errors.New("timeout")}, connector.Broken, false},
		{&events.ListenRecovered{}, connector.Healthy, false},
		{&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_BROWSER_INACTIVE}, connector.Broken, false},
		{&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_BROWSER_ACTIVE}, connector.Healthy, false},
		{&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_BROWSER_INACTIVE_FROM_TIMEOUT}, connector.Broken, true},
	} {
		cli.emit(tc.evt)
		if h := f.c.Health(context.Background()); h.State != tc.state || (h.State != connector.Healthy && h.Detail == "") {
			t.Fatalf("after %T: health %+v, want %s", tc.evt, h, tc.state)
		}
		if tc.resumed {
			eventually(t, "resuming the session", func() bool {
				cli.mu.Lock()
				defer cli.mu.Unlock()
				return cli.activations > 0
			})
		}
	}
}

func TestUnpairedFromPhone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, evt := range []any{
		&gmproto.RevokePairData{},
		&events.ListenFatalError{Error: errors.New("http 401 while polling")},
		&events.PingFailed{Error: events.ErrRequestedEntityNotFound},
	} {
		f := newFixture(t, true)
		f.activate()
		old := f.cli()
		old.emit(evt)
		if h := wantHealth(t, f.c, connector.NeedsSetup); h.Detail == "not linked" {
			t.Fatalf("%T: health %+v", evt, h)
		}
		eventually(t, "disconnect", func() bool { return !old.isConnected() })
		if data, err := f.session.Load(); data != nil || err != nil {
			t.Fatalf("%T: session kept", evt)
		}
		_, err := f.c.Prepare(ctx, "send", map[string]string{"chat": "1", "text": "hi"})
		wantStatus(t, err, http.StatusServiceUnavailable)
		if res := f.c.Execute(ctx, approval.Record{Op: "send", Args: map[string]string{"chat": "1", "text": "hi"}}); res.Status != connector.Failed {
			t.Fatalf("%T: execute %+v", evt, res)
		}

		step, err := f.c.Setup(ctx, nil)
		if err != nil || step.QR != "qr-1-1" {
			t.Fatalf("%T: re-pair %+v, %v", evt, step, err)
		}
		f.pairing(0).scan()
		setupUntil(t, f.c, func(s connector.SetupStep) bool { return s.Done })
		// A late event from the old client leaves the new session alone.
		old.emit(&gmproto.RevokePairData{})
		wantHealth(t, f.c, connector.Broken)
	}
}

func TestUnlink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, true)
	cli := f.cli()
	if err := f.c.Unlink(ctx); err != nil {
		t.Fatal(err)
	}
	if cli.unpairs != 1 || cli.isConnected() {
		t.Fatalf("unpairs %d, connected %v", cli.unpairs, cli.isConnected())
	}
	if data, _ := f.session.Load(); data != nil {
		t.Fatal("session kept")
	}
	if h := wantHealth(t, f.c, connector.NeedsSetup); h.Detail != "not linked" {
		t.Fatalf("health %+v", h)
	}

	// Unlinking while pairing ends the pairing; the next Setup starts anew.
	if _, err := f.c.Setup(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Unlink(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the dropped pairing to disconnect", func() bool { return f.pairing(0).state().disconnected })
	step, err := f.c.Setup(ctx, nil)
	if err != nil || step.QR != "qr-2-1" {
		t.Fatalf("after unlink: %+v, %v", step, err)
	}
}
