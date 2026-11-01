package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func newUnlinkedFixture(t *testing.T) *fixture {
	t.Helper()
	f := newBareFixture(t)
	f.start(t, nil)
	return f
}

func wantHealth(t *testing.T, c *sgConnector, state connector.HealthState) {
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
func setupUntil(t *testing.T, c *sgConnector, ok func(connector.SetupStep, error) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		step, err := c.Setup(context.Background(), nil)
		if ok(step, err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup: last step %+v, err %v", step, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// scan ends the latest provisioning session as scanning its code does.
func (f *fixture) scan(t *testing.T) {
	t.Helper()
	f.provide(signalmeow.ProvisioningResponse{State: signalmeow.StateProvisioningDataReceived, ProvisioningData: f.putDevice(t)})
}

func TestLinkFromConsole(t *testing.T) {
	t.Parallel()
	f := newUnlinkedFixture(t)
	ctx := context.Background()
	if h := f.c.Health(ctx); h.State != connector.NeedsSetup || h.Detail != "not linked" {
		t.Fatalf("health %+v", h)
	}
	if _, err := f.c.Prepare(ctx, "send", map[string]string{"chat": farid.String(), "text": "hi"}); err == nil {
		t.Fatal("prepared a send while not linked")
	}

	for range 2 {
		step, err := f.c.Setup(ctx, nil)
		if err != nil || step.QR != "sgnl://linkdevice?uuid=1" || step.Instructions == "" || step.Done {
			t.Fatalf("step %+v, %v", step, err)
		}
	}
	if n := f.provisioned(); n != 1 {
		t.Fatalf("polling restarted linking: %d sessions", n)
	}

	f.scan(t)
	setupUntil(t, f.c, func(s connector.SetupStep, _ error) bool { return s.Done })
	_, err := f.c.Setup(ctx, nil)
	wantStatus(t, err, http.StatusConflict)
	if h := f.c.Health(ctx); h.State != connector.Broken || h.Detail != "connecting…" {
		t.Fatalf("health %+v", h)
	}
	cli := f.cli()
	cli.mu.Lock()
	starts := cli.starts
	cli.mu.Unlock()
	if starts != 1 {
		t.Fatalf("receive loops started %d times", starts)
	}
	cli.emit(signalmeow.SignalConnectionStatus{Event: signalmeow.SignalConnectionEventConnected})
	eventually(t, func() bool { return f.c.Health(ctx).State == connector.Healthy })
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
	// Signal closes an unscanned session; the next poll shows a new code.
	f.provide(signalmeow.ProvisioningResponse{State: signalmeow.StateProvisioningError, Err: websocket.CloseError{Code: websocket.StatusGoingAway}})
	setupUntil(t, f.c, func(s connector.SetupStep, _ error) bool { return s.QR == "sgnl://linkdevice?uuid=2" })

	// A failure is reported once, then the next call starts again.
	f.provide(signalmeow.ProvisioningResponse{
		State: signalmeow.StateProvisioningError,
		Err:   signalmeow.DeviceLinkError{StatusCode: http.StatusLengthRequired},
	})
	setupUntil(t, f.c, func(_ connector.SetupStep, err error) bool {
		var e *connector.Error
		return errors.As(err, &e) && e.Status == http.StatusBadGateway
	})
	step, err := f.c.Setup(context.Background(), nil)
	if err != nil || step.QR != "sgnl://linkdevice?uuid=3" {
		t.Fatalf("after failure: %+v, %v", step, err)
	}
}

func TestSetupRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.c.Setup(context.Background(), nil)
	wantStatus(t, err, http.StatusConflict)

	f = newUnlinkedFixture(t)
	_, err = f.c.Setup(context.Background(), map[string]string{"phone": "+15551234567"})
	wantStatus(t, err, http.StatusBadRequest)
	if f.provisioned() != 0 {
		t.Fatal("refused setup started linking")
	}
}

func TestLoggedOutFromPhone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, logout := range []func(*fixture){
		func(f *fixture) {
			f.cli().emit(signalmeow.SignalConnectionStatus{Event: signalmeow.SignalConnectionEventLoggedOut})
		},
		func(f *fixture) { f.c.event(f.cli(), &events.LoggedOut{Error: errors.New("prekeys rejected")}) },
	} {
		f := newFixture(t)
		old := f.cli()
		logout(f)
		// The session is deleted just after the health changes.
		eventually(t, func() bool {
			dev, err := f.sess.devices.DeviceByACI(ctx, owner)
			return f.c.Health(ctx).Detail == "logged out from the phone" && dev == nil && err == nil
		})
		wantHealth(t, f.c, connector.NeedsSetup)
		_, err := f.c.Prepare(ctx, "send", map[string]string{"chat": farid.String(), "text": "hi"})
		wantStatus(t, err, http.StatusServiceUnavailable)
		res := f.c.Execute(ctx, approval.Record{Op: "send", Args: map[string]string{"chat": farid.String(), "text": "hi"}})
		if res.Status != connector.Failed || len(old.calls()) != 0 {
			t.Fatalf("execute after logout: %+v", res)
		}

		step, err := f.c.Setup(ctx, nil)
		if err != nil || step.QR != "sgnl://linkdevice?uuid=1" {
			t.Fatalf("re-link: %+v, %v", step, err)
		}
		f.scan(t)
		setupUntil(t, f.c, func(s connector.SetupStep, _ error) bool { return s.Done })
		if f.cli() == old {
			t.Fatal("re-linking reused the old client")
		}
		// A late status from the replaced client must not unlink the new session.
		f.c.status(old, signalmeow.SignalConnectionStatus{Event: signalmeow.SignalConnectionEventLoggedOut})
		wantHealth(t, f.c, connector.Broken)
	}
}

func TestUnlink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, unlinkErr := range []error{nil, errors.New("offline")} {
		f := newFixture(t)
		cli := f.cli()
		cli.unlinkErr = unlinkErr
		if err := f.c.Unlink(ctx); err != nil {
			t.Fatalf("unlink error %v: %v", unlinkErr, err)
		}
		cli.mu.Lock()
		unlinks, stops := cli.unlinks, cli.stops
		cli.mu.Unlock()
		if unlinks != 1 || stops != 1 {
			t.Fatalf("unlinks %d, stops %d", unlinks, stops)
		}
		if devs, err := f.sess.devices.GetAllDevices(ctx); err != nil || len(devs) != 0 {
			t.Fatalf("session kept: %d devices, %v", len(devs), err)
		}
		if h := f.c.Health(ctx); h.State != connector.NeedsSetup || h.Detail != "not linked" {
			t.Fatalf("health %+v", h)
		}
		step, err := f.c.Setup(ctx, nil)
		if err != nil || step.QR != "sgnl://linkdevice?uuid=1" {
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
	if err := f.c.Unlink(ctx); err != nil {
		t.Fatal(err)
	}
	// A scan of the dropped code links nothing, and its session is forgotten.
	f.scan(t)
	eventually(t, func() bool {
		devs, err := f.sess.devices.GetAllDevices(ctx)
		return err == nil && len(devs) == 0
	})
	wantHealth(t, f.c, connector.NeedsSetup)
	if step, err := f.c.Setup(ctx, nil); err != nil || step.QR != "sgnl://linkdevice?uuid=2" {
		t.Fatalf("relink: %+v, %v", step, err)
	}
}

func TestHealthReconnecting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.cli().emit(signalmeow.SignalConnectionStatus{Event: signalmeow.SignalConnectionEventDisconnected, Err: errors.New("eof")})
	eventually(t, func() bool { return f.c.Health(ctx).Detail == "reconnecting…" })
	wantHealth(t, f.c, connector.Broken)
	f.cli().emit(signalmeow.SignalConnectionStatus{Event: signalmeow.SignalConnectionEventConnected})
	eventually(t, func() bool { return f.c.Health(ctx).State == connector.Healthy })
	cli := f.cli()
	cli.mu.Lock()
	defer cli.mu.Unlock()
	if cli.syncs != 2 {
		t.Fatalf("storage synced %d times, want once per connect", cli.syncs)
	}
}
