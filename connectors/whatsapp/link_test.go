package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func newUnlinkedFixture(t *testing.T) *fixture {
	t.Helper()
	f := newBareFixture(t)
	f.start(f.container.NewDevice())
	return f
}

func wantHealth(t *testing.T, c *waConnector, state connector.HealthState) {
	t.Helper()
	if h := c.Health(context.Background()); h.State != state {
		t.Fatalf("health = %+v, want %s", h, state)
	}
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var e *connector.Error
	if !errors.As(err, &e) || e.Status != status {
		t.Fatalf("err = %v, want status %d", err, status)
	}
}

// setupUntil polls Setup like the console does until ok accepts the step.
func setupUntil(t *testing.T, c *waConnector, args map[string]string, ok func(connector.SetupStep) bool) connector.SetupStep {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		step, err := c.Setup(context.Background(), args)
		if err == nil && ok(step) {
			return step
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup: last step %+v, err %v", step, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLinkFromConsole(t *testing.T) {
	t.Parallel()
	f := newUnlinkedFixture(t)
	ctx := context.Background()
	wantHealth(t, f.c, connector.NeedsSetup)
	if _, err := f.c.Prepare(ctx, "send", map[string]string{"chat": farid.String(), "text": "hi"}); err == nil {
		t.Fatal("prepared a send while not linked")
	}

	step, err := f.c.Setup(ctx, nil)
	if err != nil || step.QR != "qr-1-1" || step.Instructions == "" || step.Done {
		t.Fatalf("first step %+v, %v", step, err)
	}
	f.cli.qr(whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "qr-1-2"})
	setupUntil(t, f.c, nil, func(s connector.SetupStep) bool { return s.QR == "qr-1-2" })

	for range 2 {
		step, err = f.c.Setup(ctx, map[string]string{"phone": "+1 555 123 4567"})
		if err != nil || step.Code != "ABCD-EFGH" || step.QR != "" {
			t.Fatalf("phone step %+v, %v", step, err)
		}
	}
	if f.cli.phones[0] != "+15551234567" || len(f.cli.phones) != 1 {
		t.Fatalf("pairing codes requested for %q, want one", f.cli.phones)
	}
	if n := f.cli.sessions(); n != 1 {
		t.Fatalf("polling restarted linking: %d QR sessions", n)
	}

	f.pair(t, f.c.dev)
	f.cli.qr(whatsmeow.QRChannelSuccess)
	setupUntil(t, f.c, nil, func(s connector.SetupStep) bool { return s.Done })
	_, err = f.c.Setup(ctx, nil)
	wantStatus(t, err, http.StatusConflict)
	// The pairing connection carries on as the session; no restart.
	wantHealth(t, f.c, connector.Broken)
	f.cli.mu.Lock()
	f.cli.loggedIn = true
	f.cli.mu.Unlock()
	wantHealth(t, f.c, connector.Healthy)
	if err := f.st.UpsertContainer(ctx, store.Container{ID: farid.String(), Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Prepare(ctx, "send", map[string]string{"chat": farid.String(), "text": "hi"}); err != nil {
		t.Fatalf("prepare after linking: %v", err)
	}
}

func TestPairingEndsAndStartsAgain(t *testing.T) {
	t.Parallel()
	f := newUnlinkedFixture(t)
	if _, err := f.c.Setup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	// Expired codes start a new session on the next poll.
	f.cli.qr(whatsmeow.QRChannelTimeout)
	setupUntil(t, f.c, nil, func(s connector.SetupStep) bool { return s.QR == "qr-2-1" })

	// A failure is reported once, then the next call starts again.
	f.cli.qr(whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: errors.New("bad pairing")})
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := f.c.Setup(context.Background(), nil)
		var e *connector.Error
		if errors.As(err, &e) && e.Status == http.StatusBadGateway {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pairing error not reported: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	step, err := f.c.Setup(context.Background(), nil)
	if err != nil || step.QR != "qr-3-1" {
		t.Fatalf("after failure: %+v, %v", step, err)
	}
	f.cli.mu.Lock()
	defer f.cli.mu.Unlock()
	if f.cli.disconnects < 2 {
		t.Errorf("ended sessions left connected: %d disconnects", f.cli.disconnects)
	}
}

func TestSetupRefusedWhileLinked(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, args := range []map[string]string{nil, {"phone": "+15551234567"}} {
		_, err := f.c.Setup(context.Background(), args)
		wantStatus(t, err, http.StatusConflict)
	}
	if f.cli.sessions() != 0 || len(f.cli.phones) != 0 {
		t.Fatal("setup started pairing while linked")
	}
}

func TestSetupRejectsBadArgs(t *testing.T) {
	t.Parallel()
	f := newUnlinkedFixture(t)
	for _, args := range []map[string]string{{"phone": "555"}, {"qr": "1"}} {
		_, err := f.c.Setup(context.Background(), args)
		wantStatus(t, err, http.StatusBadRequest)
	}
	if f.cli.sessions() != 0 {
		t.Fatal("bad args started pairing")
	}
}

func TestLoggedOutFromPhone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	wantHealth(t, f.c, connector.Healthy)

	f.c.event(f.cli, &events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	wantHealth(t, f.c, connector.NeedsSetup)
	deadline := time.Now().Add(5 * time.Second)
	for f.cli.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("still syncing after logout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err := f.c.Prepare(ctx, "send", map[string]string{"chat": farid.String(), "text": "hi"})
	wantStatus(t, err, http.StatusServiceUnavailable)
	res := f.c.Execute(ctx, approval.Record{Op: "send", Args: map[string]string{"chat": farid.String(), "text": "hi"}})
	if res.Status != connector.Failed || len(f.cli.calls()) != 0 {
		t.Fatalf("execute after logout: %+v", res)
	}

	step, err := f.c.Setup(ctx, nil)
	if err != nil || step.QR != "qr-1-1" {
		t.Fatalf("re-link: %+v, %v", step, err)
	}
	if fresh := f.client(1); fresh.sessions() != 1 || f.cli.sessions() != 0 {
		t.Fatal("re-linking did not use a fresh client")
	}
	f.pair(t, f.c.dev)
	f.client(1).qr(whatsmeow.QRChannelSuccess)
	setupUntil(t, f.c, nil, func(s connector.SetupStep) bool { return s.Done })
	wantHealth(t, f.c, connector.Broken)
	// A late logout from the replaced client must not unlink the new session.
	f.c.event(f.cli, &events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	wantHealth(t, f.c, connector.Broken)
}

func TestUnlink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, logoutErr := range []error{nil, errors.New("offline")} {
		f := newFixture(t)
		f.cli.logoutErr = logoutErr
		if err := f.c.Unlink(ctx); err != nil {
			t.Fatalf("logout error %v: %v", logoutErr, err)
		}
		if f.cli.logouts != 1 || f.cli.IsConnected() {
			t.Fatalf("logouts %d, connected %v", f.cli.logouts, f.cli.IsConnected())
		}
		if devs, err := f.container.GetAllDevices(ctx); err != nil || len(devs) != 0 {
			t.Fatalf("session kept: %d devices, %v", len(devs), err)
		}
		if h := f.c.Health(ctx); h.State != connector.NeedsSetup || h.Detail != "not linked" {
			t.Fatalf("health %+v", h)
		}
		step, err := f.c.Setup(ctx, nil)
		if err != nil || step.QR != "qr-1-1" || f.client(1).sessions() != 1 {
			t.Fatalf("relink: %+v, %v", step, err)
		}
	}
}

func TestUnlinkWhilePairing(t *testing.T) {
	t.Parallel()
	f := newUnlinkedFixture(t)
	ctx := context.Background()
	if _, err := f.c.Setup(ctx, nil); err != nil {
		t.Fatal(err)
	}
	_, dev, _ := f.c.current()
	if err := f.c.Unlink(ctx); err != nil {
		t.Fatal(err)
	}
	// A scan of the dropped code links nothing, and its device is forgotten.
	f.pair(t, dev)
	f.cli.qr(whatsmeow.QRChannelSuccess)
	deadline := time.Now().Add(5 * time.Second)
	for {
		devs, err := f.container.GetAllDevices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(devs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dropped pairing's session kept")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.cli.mu.Lock()
	logouts := f.cli.logouts
	f.cli.mu.Unlock()
	wantHealth(t, f.c, connector.NeedsSetup)
	if step, err := f.c.Setup(ctx, nil); err != nil || step.QR != "qr-1-1" || logouts != 1 {
		t.Fatalf("relink: %+v, %v, %d logouts", step, err, logouts)
	}
}

func TestHealthDown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cli.mu.Lock()
	f.cli.connected = false
	f.cli.mu.Unlock()
	wantHealth(t, f.c, connector.Broken)

	for _, evt := range []any{&events.ClientOutdated{}, &events.StreamReplaced{}, &events.TemporaryBan{Code: events.TempBanBlockedByUsers}} {
		f := newFixture(t)
		f.c.event(f.cli, &events.Disconnected{})
		wantHealth(t, f.c, connector.Healthy)
		f.c.event(f.cli, evt)
		if h := f.c.Health(context.Background()); h.State != connector.Broken || h.Detail == "" {
			t.Errorf("%T: health %+v", evt, h)
		}
	}
}
