// Package whatsapp ingests WhatsApp messages as a whatsmeow linked device and
// serves the whatsapp connector.
//
// The only messages it sends are writes the owner approved with a passkey,
// verified by connector.Server. Otherwise it sends nothing: no receipts beyond
// whatsmeow's automatic protocol acks, no presence, no typing.
package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mdp/qrterminal/v3"
	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/connector"
	"github.com/adeci/mandate/internal/exitcode"
	"github.com/adeci/mandate/internal/store"
)

const usage = `usage: mandate whatsapp run --data PATH --session PATH [--listen SOCKET]
                           [--approvers FILE --rp-id ID --origin URL] [--seen PATH]
       mandate whatsapp pair --session PATH [--phone +E164]`

var errNotPaired = fmt.Errorf(`not paired: run "mandate whatsapp pair": %w`, exitcode.ErrNeedsOwner)

func Command(ctx context.Context, args []string) error {
	if len(args) < 1 {
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

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("whatsapp run", flag.ContinueOnError)
	dataPath := flags.String("data", "", "message store database (required)")
	sessionPath := flags.String("session", "", "whatsmeow session database (required)")
	listen := flags.String("listen", "/run/mandate-whatsapp/connector.sock", "connector socket")
	approversPath := flags.String("approvers", "", "enrolled passkeys (JSON); missing or empty refuses all sends")
	rpID := flags.String("rp-id", "", "WebAuthn relying party ID of the approval page")
	origin := flags.String("origin", "", "WebAuthn origin of the approval page")
	seenPath := flags.String("seen", "", "used-approval log (default: seen.json next to --session)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dataPath == "" || *sessionPath == "" || *listen == "" {
		return errors.New("--data, --session, and --listen are required")
	}
	if *seenPath == "" {
		*seenPath = filepath.Join(filepath.Dir(*sessionPath), "seen.json")
	}
	ctx = withLibraryLogger(ctx)
	log := slog.Default().With("source", "whatsapp")

	verifier, err := loadVerifier(*approversPath, *rpID, *origin)
	if err != nil {
		return err
	}
	var seen *approval.Seen
	if verifier == nil {
		log.Warn("no approvers enrolled; sends are refused")
	} else if seen, err = approval.OpenSeen(*seenPath); err != nil {
		return fmt.Errorf("open seen log: %w", err)
	}

	st, err := store.Open(ctx, *dataPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	container, device, err := openSession(ctx, *sessionPath)
	if err != nil {
		return err
	}
	defer container.Close()
	if device.ID == nil {
		return errNotPaired
	}

	cli := whatsmeow.NewClient(device, newWALogger("whatsmeow"))
	cli.BackgroundEventCtx = ctx
	// Ack only after the handler stored the message; the decrypted-event
	// buffer lets a redelivered (unacked) message be decrypted again.
	cli.SynchronousAck = true
	cli.EnableDecryptedEventBuffer = true
	cli.InitialAutoReconnect = true

	in := newIngester(cli, st, log)
	fatal := make(chan error, 1)
	cli.AddEventHandlerWithSuccessStatus(func(evt any) bool {
		if err := permanentError(evt); err != nil {
			select {
			case fatal <- err:
			default:
			}
			return true
		}
		return in.handle(ctx, evt)
	})

	srv := &connector.Server{
		Connector: newConnector(cli, device, st, log),
		Verifier:  verifier,
		Seen:      seen,
		Log:       log,
	}
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(serveCtx, *listen) }()
	// Stop serving before disconnecting so an in-flight send can finish.
	shutdown := func() error {
		stopServe()
		err := <-served
		cli.Disconnect()
		if err != nil {
			return fmt.Errorf("serve connector: %w", err)
		}
		return nil
	}

	log.Info("connecting", "jid", device.ID.String(), "listen", *listen)
	if err := cli.ConnectContext(ctx); err != nil {
		return errors.Join(fmt.Errorf("connect: %w", err), shutdown())
	}
	select {
	case <-ctx.Done():
		if err := shutdown(); err != nil {
			return err
		}
		log.Info("disconnected")
		return nil
	case err := <-fatal:
		return errors.Join(err, shutdown())
	case err := <-served:
		cli.Disconnect()
		return fmt.Errorf("serve connector: %w", err)
	}
}

// loadVerifier returns nil, refusing all sends, unless approvers are enrolled.
func loadVerifier(path, rpID, origin string) (*approval.Verifier, error) {
	if path == "" {
		return nil, nil
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && fi.Size() == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read approvers: %w", err)
	}
	approvers, err := approval.LoadApprovers(path)
	if err != nil {
		return nil, err
	}
	if len(approvers) == 0 {
		return nil, nil
	}
	if rpID == "" || origin == "" {
		return nil, errors.New("--rp-id and --origin are required when approvers are enrolled")
	}
	return &approval.Verifier{RPID: rpID, Origin: origin, Approvers: approvers}, nil
}

// permanentError maps events after which whatsmeow will not reconnect.
func permanentError(evt any) error {
	switch e := evt.(type) {
	case *events.LoggedOut:
		return fmt.Errorf("logged out (%s): %w", e.Reason, errNotPaired)
	case *events.StreamReplaced:
		return errors.New("session taken over by another client using the same session file")
	case events.PermanentDisconnect:
		return fmt.Errorf("permanent disconnect: %s", e.PermanentDisconnectDescription())
	}
	return nil
}

func pair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("whatsapp pair", flag.ContinueOnError)
	sessionPath := fs.String("session", "", "whatsmeow session database (required)")
	phone := fs.String("phone", "", "pair by phone number (+E164) and print a code instead of a QR")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionPath == "" {
		return errors.New("--session is required")
	}
	ctx = withLibraryLogger(ctx)

	container, device, err := openSession(ctx, *sessionPath)
	if err != nil {
		return err
	}
	defer container.Close()
	if device.ID != nil {
		return fmt.Errorf("already paired as %s; remove %s to pair again", device.ID, *sessionPath)
	}

	// Shown under Linked devices on the phone.
	wastore.SetOSInfo("mandate", [3]uint32{0, 1, 0})
	cli := whatsmeow.NewClient(device, newWALogger("whatsmeow"))
	cli.BackgroundEventCtx = ctx
	qr, err := cli.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := cli.ConnectContext(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cli.Disconnect()

	codeShown := false
	for {
		var item whatsmeow.QRChannelItem
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, ok = <-qr:
		}
		if !ok {
			return errors.New("pairing aborted")
		}
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			if *phone == "" {
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
				fmt.Printf("Scan in WhatsApp: Settings > Linked devices > Link a device (expires in %s)\n\n", item.Timeout)
				continue
			}
			if codeShown {
				continue
			}
			// The websocket is ready once the first QR item arrives.
			code, err := cli.PairPhone(ctx, *phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
			if err != nil {
				return fmt.Errorf("request pairing code: %w", err)
			}
			codeShown = true
			fmt.Printf("Pairing code: %s\nIn WhatsApp: Settings > Linked devices > Link a device > Link with phone number instead\n", code)
		case whatsmeow.QRChannelEventPasskeyRequest:
			return errors.New("phone requested passkey pairing, which mandate does not support; retry with QR or --phone")
		case whatsmeow.QRChannelEventPasskeyResponse:
			fmt.Printf("Confirm code on phone: %s\n", item.PasskeyConfirmation.Code)
		case whatsmeow.QRChannelSuccess.Event:
			// Disconnect before the post-pair login: anything delivered now
			// (history sync) would be acked without being stored. The run
			// service receives it instead.
			cli.Disconnect()
			fmt.Printf("Paired as %s. Start the run service now to receive history sync.\n", cli.Store.ID)
			return nil
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("pairing failed: %w", item.Error)
		default:
			return fmt.Errorf("pairing failed: %s", item.Event)
		}
	}
}

// The session database is created with mode 0600.
func openSession(ctx context.Context, path string) (*sqlstore.Container, *wastore.Device, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		f.Close()
	} else if !errors.Is(err, os.ErrExist) {
		return nil, nil, fmt.Errorf("create session file: %w", err)
	}
	dsn := "file:" + path +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open session: %w", err)
	}
	container := sqlstore.NewWithDB(db, "sqlite3", newWALogger("whatsmeow/store"))
	if err := container.Upgrade(ctx); err != nil {
		container.Close()
		return nil, nil, fmt.Errorf("upgrade session: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		container.Close()
		return nil, nil, fmt.Errorf("load device: %w", err)
	}
	return container, device, nil
}

// withLibraryLogger attaches an info-level stderr zerolog logger, which some
// whatsmeow code paths read from the context instead of the waLog logger.
func withLibraryLogger(ctx context.Context) context.Context {
	zl := zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().
		Timestamp().Str("module", "whatsmeow").Logger()
	return zl.WithContext(ctx)
}

type waLogger struct {
	l *slog.Logger
	m string
}

func newWALogger(module string) waLog.Logger {
	return waLogger{l: slog.Default(), m: module}
}

func (w waLogger) Errorf(msg string, args ...any) {
	w.l.Error(fmt.Sprintf(msg, args...), "module", w.m)
}

func (w waLogger) Warnf(msg string, args ...any) { w.l.Warn(fmt.Sprintf(msg, args...), "module", w.m) }

func (w waLogger) Infof(msg string, args ...any)  { w.l.Info(fmt.Sprintf(msg, args...), "module", w.m) }
func (w waLogger) Debugf(string, ...any)          {}
func (w waLogger) Sub(module string) waLog.Logger { return waLogger{l: w.l, m: w.m + "/" + module} }
