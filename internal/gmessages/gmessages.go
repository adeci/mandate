// Package gmessages ingests SMS/RCS from Google Messages for Web via libgm. It
// never sends, reacts, marks read, or changes settings.
package gmessages

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
	"go.mau.fi/util/exhttp"

	"github.com/adeci/sieve/internal/exitcode"
	"github.com/adeci/sieve/internal/store"
)

const usage = `usage:
  sieve messages pair --session PATH
  sieve messages run  --data PATH --session PATH [--backfill N] [--reclaim=true|false]`

const (
	// deviceName is shown in the phone's "Device pairing" list.
	deviceName = "sieve"
	// qrRefreshInterval and qrMaxAttempts match the bridge's QR login.
	qrRefreshInterval = 30 * time.Second
	qrMaxAttempts     = 6
	// pairActivateTimeout bounds the post-pairing connect; pairAckFlushWait
	// outlasts libgm's 5-second ack interval.
	pairActivateTimeout = 30 * time.Second
	pairAckFlushWait    = 6 * time.Second
	// pingInterval matches the bridge's default ping_interval; libgm's own
	// default (1 minute) wakes the phone far more often.
	pingInterval = 20 * time.Minute
)

func Command(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "run":
		return run(ctx, args[1:])
	case "pair":
		return pair(ctx, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", args[0], usage)
	}
}

func pair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("messages pair", flag.ContinueOnError)
	sessionPath := fs.String("session", "", "session state file (written with mode 0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionPath == "" || fs.NArg() > 0 {
		return errors.New(usage)
	}
	log := newLogger()

	util.BrowserDetailsMessage.OS = deviceName
	sess := &sessionFile{path: *sessionPath, auth: libgm.NewAuthData()}
	cli := libgm.NewClient(sess.auth, nil, libgmLogger(log), exhttp.SensibleClientSettings)
	cli.SetEventHandler(func(evt any) {
		log.Debug("event before pairing completed", "type", fmt.Sprintf("%T", evt))
	})
	// As in the bridge, a pair callback replaces the PairSuccessful event so
	// libgm does not start a logged-in reconnect while we are exiting.
	paired := make(chan *gmproto.PairedData, 1)
	onPaired := func(data *gmproto.PairedData) {
		select {
		case paired <- data:
		default:
		}
	}
	cli.PairCallback.Store(&onPaired)

	if err := cli.FetchConfig(ctx); err != nil {
		return fmt.Errorf("fetch config: %w", err)
	}
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	defer cli.Disconnect()
	qr, err := cli.StartLogin(pollCtx)
	if err != nil {
		return fmt.Errorf("start pairing: %w", err)
	}
	for attempt := 1; ; attempt++ {
		fmt.Fprintln(os.Stdout, "\nOn your Android phone, open Google Messages > profile icon > Device pairing > QR code scanner, and scan:")
		qrterminal.GenerateHalfBlock(qr, qrterminal.L, os.Stdout)
		timer := time.NewTimer(qrRefreshInterval)
		select {
		case data := <-paired:
			timer.Stop()
			cli.Disconnect()
			cancelPoll()
			if err := sess.save(); err != nil {
				return err
			}
			log.Info("paired with phone", "phone_id", data.GetMobile().GetSourceID(), "session", *sessionPath)
			confirmPairing(ctx, cli, sess, log)
			fmt.Fprintln(os.Stdout, `Paired. Start the ingester with "sieve messages run".`)
			return nil
		case <-timer.C:
			if attempt >= qrMaxAttempts {
				return errors.New("pairing timed out: QR code was not scanned")
			}
			if qr, err = cli.RefreshPhoneRelay(); err != nil {
				return fmt.Errorf("refresh QR code: %w", err)
			}
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// Follows the bridge's finishLogin: wait for the phone to store the pairing,
// then connect once so the phone activates the session and libgm flushes its
// pending acks. Failures are logged only; the session is already saved.
func confirmPairing(ctx context.Context, cli *libgm.Client, sess *sessionFile, log *slog.Logger) {
	if !sleepCtx(ctx, 2*time.Second) {
		return
	}
	active := make(chan struct{}, 1)
	cli.SetEventHandler(func(raw any) {
		switch evt := raw.(type) {
		case *gmproto.UserAlertEvent:
			if evt.GetAlertType() == gmproto.AlertType_BROWSER_ACTIVE {
				select {
				case active <- struct{}{}:
				default:
				}
			}
		case *events.AuthTokenRefreshed:
			if err := sess.save(); err != nil {
				log.Error("save session after token refresh", "err", err)
			}
		}
	})
	if err := cli.Connect(ctx); err != nil {
		log.Warn("connecting after pairing failed; \"run\" will retry", "err", err)
		return
	}
	defer cli.Disconnect()
	select {
	case <-active:
		log.Info("phone activated the session")
		sleepCtx(ctx, pairAckFlushWait)
	case <-time.After(pairActivateTimeout):
		log.Warn("phone did not activate the session yet; is it online?")
	case <-ctx.Done():
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("messages run", flag.ContinueOnError)
	dataPath := fs.String("data", "", "store database path")
	sessionPath := fs.String("session", "", "session state file from \"sieve messages pair\"")
	backfill := fs.Int("backfill", 50, "messages to fetch per conversation on (re)connect; 0 disables")
	reclaim := fs.Bool("reclaim", true, "re-take the active session when another Messages for Web client takes it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataPath == "" || *sessionPath == "" || fs.NArg() > 0 {
		return errors.New(usage)
	}
	if *backfill < 0 {
		return errors.New("--backfill must be >= 0")
	}
	log := newLogger()

	sess, err := loadSession(*sessionPath)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, *dataPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	cli := libgm.NewClient(sess.auth, nil, libgmLogger(log), exhttp.SensibleClientSettings)
	cli.SetPingInterval(pingInterval)
	in := newIngester(ctx, cli, st, sess, log, int64(*backfill), *reclaim)
	cli.SetEventHandler(in.handleEvent)
	go in.writeLoop()
	go in.backfillLoop()

	if err := cli.FetchConfig(ctx); err != nil {
		log.Warn("fetch config failed, connecting anyway", "err", err)
	}
	if err := cli.Connect(ctx); err != nil {
		in.stop()
		switch {
		case errors.Is(err, events.ErrRequestedEntityNotFound):
			// The phone no longer knows this browser; the bridge drops the pairing.
			sess.auth.ClearPairing()
			if serr := sess.save(); serr != nil {
				log.Error("save session", "err", serr)
			}
			return fmt.Errorf("pairing no longer valid (%w); %w", err, errNotPaired)
		case errors.Is(err, events.ErrInvalidCredentials):
			return fmt.Errorf("session rejected by Google (%w); re-pair with \"sieve messages pair\": %w", err, exitcode.ErrNeedsOwner)
		default:
			return fmt.Errorf("connect: %w", err)
		}
	}
	if err := sess.save(); err != nil {
		log.Error("save session", "err", err)
	}
	log.Info("connected to Google Messages; waiting for the phone to activate this session")

	var runErr error
	var clearPairing bool
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case f := <-in.fatal:
		runErr, clearPairing = f.err, f.clearPairing
	}
	cli.Disconnect()
	in.stop()
	if clearPairing {
		sess.auth.ClearPairing()
	}
	if err := sess.save(); err != nil {
		log.Error("save session", "err", err)
	}
	return runErr
}
