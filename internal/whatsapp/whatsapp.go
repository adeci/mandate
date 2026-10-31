// Package whatsapp ingests WhatsApp messages as a whatsmeow linked device.
//
// The ingester is read-only toward WhatsApp: it never sends messages,
// receipts beyond whatsmeow's automatic protocol acks, presence, or typing.
package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/mdp/qrterminal/v3"
	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/adeci/mandate/internal/exitcode"
	"github.com/adeci/mandate/internal/store"
)

const usage = `usage: mandate whatsapp run --data PATH --session PATH
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
	fs := flag.NewFlagSet("whatsapp run", flag.ContinueOnError)
	dataPath := fs.String("data", "", "message store database (required)")
	sessionPath := fs.String("session", "", "whatsmeow session database (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataPath == "" || *sessionPath == "" {
		return errors.New("--data and --session are required")
	}
	ctx = withLibraryLogger(ctx)

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

	log := slog.Default().With("source", "whatsapp")
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

	log.Info("connecting", "jid", device.ID.String())
	if err := cli.ConnectContext(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	select {
	case <-ctx.Done():
		cli.Disconnect()
		log.Info("disconnected")
		return nil
	case err := <-fatal:
		cli.Disconnect()
		return err
	}
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
