package whatsapp

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
)

func newUnlinkedFixture(t *testing.T) *fixture {
	t.Helper()
	f := newBareFixture(t)
	f.start(f.container.NewDevice())
	return f
}

func wantHealth(t *testing.T, c *waConnector, state connector.HealthState, detail string) {
	t.Helper()
	if h := c.Health(context.Background()); h.State != state || h.Detail != detail {
		t.Fatalf("health = %+v, want %s %q", h, state, detail)
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
	wantHealth(t, f.c, connector.NeedsSetup, "not linked")
	if _, err := f.c.Prepare(ctx, "send", map[string]string{"to": "+15551234567", "text": "hi"}); err == nil {
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
	wantHealth(t, f.c, connector.Broken, "connecting…")
	f.cli.mu.Lock()
	f.cli.loggedIn = true
	f.cli.mu.Unlock()
	wantHealth(t, f.c, connector.Healthy, "")
	if _, err := f.c.Prepare(ctx, "send", map[string]string{"to": "+15551234567", "text": "hi"}); err != nil {
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
	for _, args := range []map[string]string{{"phone": "555"}, {"phone": ""}, {"qr": "1"}} {
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
	wantHealth(t, f.c, connector.Healthy, "")

	f.c.event(f.cli, &events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	wantHealth(t, f.c, connector.NeedsSetup, "logged out from the phone")
	deadline := time.Now().Add(5 * time.Second)
	for f.cli.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("still syncing after logout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err := f.c.Prepare(ctx, "send", map[string]string{"to": "Farid", "text": "hi"})
	wantStatus(t, err, http.StatusServiceUnavailable)
	res := f.c.Execute(ctx, approval.Record{Op: "send", Args: map[string]string{"to": farid.String(), "text": "hi"}})
	if res.Status != connector.Failed || len(f.cli.calls()) != 0 {
		t.Fatalf("execute after logout: %+v", res)
	}
	// Events from the logged-out client no longer count.
	f.c.event(f.cli, &events.ClientOutdated{})

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
	wantHealth(t, f.c, connector.Broken, "connecting…")
}

func TestHealthDown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cli.mu.Lock()
	f.cli.connected = false
	f.cli.mu.Unlock()
	wantHealth(t, f.c, connector.Broken, "connecting…")

	for _, evt := range []any{&events.ClientOutdated{}, &events.StreamReplaced{}, &events.TemporaryBan{Code: events.TempBanBlockedByUsers}} {
		f := newFixture(t)
		f.c.event(f.cli, &events.Disconnected{})
		wantHealth(t, f.c, connector.Healthy, "")
		f.c.event(f.cli, evt)
		if h := f.c.Health(context.Background()); h.State != connector.Broken || h.Detail == "" {
			t.Errorf("%T: health %+v", evt, h)
		}
	}
	f = newFixture(t)
	f.c.event(f.cli, &events.ClientOutdated{})
	wantHealth(t, f.c, connector.Broken, "WhatsApp rejected this client version; update mandate")
}
