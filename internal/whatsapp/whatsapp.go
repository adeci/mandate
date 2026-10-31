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

	"github.com/rs/zerolog"
	signalLog "go.mau.fi/libsignal/logger"
	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const usage = `usage: mandate whatsapp run --data PATH --session PATH [--listen SOCKET]
                           [--approvers FILE --rp-id ID --origin URL] [--seen PATH]`

func Command(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New(usage)
	}
	switch args[0] {
	case "run":
		return run(ctx, args[1:])
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

	// Shown under Linked devices on the phone.
	wastore.SetOSInfo("mandate", [3]uint32{0, 1, 0})
	newClient := func(dev *wastore.Device, onEvent func(any)) waClient {
		cli := whatsmeow.NewClient(dev, newWALogger("whatsmeow"))
		cli.BackgroundEventCtx = ctx
		// Ack only after the handler stored the message; the decrypted-event
		// buffer lets a redelivered (unacked) message be decrypted again.
		cli.SynchronousAck = true
		cli.EnableDecryptedEventBuffer = true
		cli.InitialAutoReconnect = true
		in := newIngester(cli, st, log)
		cli.AddEventHandlerWithSuccessStatus(func(evt any) bool {
			onEvent(evt)
			return in.handle(ctx, evt)
		})
		return cli
	}
	c := newConnector(ctx, st, log, device, container.NewDevice, newClient)

	srv := &connector.Server{Connector: c, Verifier: verifier, Seen: seen, Log: log}
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(serveCtx, *listen) }()
	// Stop serving before disconnecting so an in-flight send can finish.
	shutdown := func() error {
		stopServe()
		err := <-served
		c.disconnect()
		if err != nil {
			return fmt.Errorf("serve connector: %w", err)
		}
		return nil
	}

	log.Info("serving", "listen", *listen)
	if err := c.connect(); err != nil {
		return errors.Join(err, shutdown())
	}
	select {
	case <-ctx.Done():
		if err := shutdown(); err != nil {
			return err
		}
		log.Info("disconnected")
		return nil
	case err := <-served:
		c.disconnect()
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

// libsignal installs its global logger lazily and unsynchronized on first use,
// from whichever goroutine gets there first, and the default prints to stdout.
func init() {
	var l signalLog.Loggable = signalLogger{}
	signalLog.Setup(&l)
}

type signalLogger struct{}

func (signalLogger) Debug(string, string) {}
func (signalLogger) Info(string, string)  {}
func (signalLogger) Configure(string)     {}
func (signalLogger) Warning(caller, msg string) {
	slog.Warn(msg, "module", "libsignal", "caller", caller)
}

func (signalLogger) Error(caller, msg string) {
	slog.Error(msg, "module", "libsignal", "caller", caller)
}
