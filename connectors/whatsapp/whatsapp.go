// Command mandate-whatsapp ingests WhatsApp messages as a whatsmeow linked
// device and serves the whatsapp connector.
//
// The only messages it sends are writes the owner approved with a passkey,
// verified by connector.Server. Otherwise it sends nothing: no receipts beyond
// whatsmeow's automatic protocol acks, no presence, no typing.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	signalLog "go.mau.fi/libsignal/logger"
	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/adeci/mandate/connector"
)

func main() {
	connector.Main(start)
}

func start(ctx context.Context, env *connector.Env, _ struct{}) (connector.Connector, error) {
	ctx = withLibraryLogger(ctx)
	st, err := openStore(ctx, env.DataPath())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	container, device, err := openSession(ctx, filepath.Join(env.PrivateDir(), "session.db"))
	if err != nil {
		st.Close()
		return nil, err
	}

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
		in := newIngester(cli, st, env.Log)
		cli.AddEventHandlerWithSuccessStatus(func(evt any) bool {
			onEvent(evt)
			return in.handle(ctx, evt)
		})
		return cli
	}
	c := newConnector(ctx, st, env.Log, device, container.NewDevice, newClient)
	if err := c.connect(); err != nil {
		container.Close()
		st.Close()
		return nil, err
	}
	return &program{waConnector: c, closers: []io.Closer{container, st}}, nil
}

// program disconnects once serving has stopped, so an in-flight send can
// finish, then closes the databases.
type program struct {
	*waConnector
	closers []io.Closer
}

func (p *program) Close() error {
	p.disconnect()
	var errs []error
	for _, c := range p.closers {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
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
