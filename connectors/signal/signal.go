// Command mandate-signal reads Signal messages as a linked device and serves
// the signal connector.
//
// The only messages it sends are writes the owner approved with a passkey,
// verified by connector.Server. Otherwise it sends what signalmeow sends on
// its own: delivery receipts, and key requests to the owner's own devices.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	sgstore "go.mau.fi/mautrix-signal/pkg/signalmeow/store"
	"go.mau.fi/util/dbutil"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// Shown under Linked devices on the phone.
const deviceName = "mandate"

func main() {
	connector.Main(start)
}

func start(ctx context.Context, env *connector.Env, _ struct{}) (connector.Connector, error) {
	zl := zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().
		Timestamp().Str("module", "signalmeow").Logger()
	ctx = zl.WithContext(ctx)
	signalmeow.SetLogger(zl)

	st, err := store.Open(ctx, env.DataPath())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	sess, err := openSession(ctx, filepath.Join(env.PrivateDir(), "session.db"), dbutil.ZeroLogger(zl))
	if err != nil {
		st.Close()
		return nil, err
	}
	devs, err := sess.devices.GetAllDevices(ctx)
	if err != nil {
		sess.Close()
		st.Close()
		return nil, fmt.Errorf("load device: %w", err)
	}
	var dev *sgstore.Device
	if len(devs) > 0 {
		dev = devs[0]
	}

	provision := func(ctx context.Context) <-chan signalmeow.ProvisioningResponse {
		return signalmeow.PerformProvisioning(ctx, sess.devices, deviceName, false)
	}
	newClient := func(dev *sgstore.Device, onEvent func(events.SignalEvent)) sgClient {
		var in *ingester
		cli := signalmeow.NewClient(dev, zl, func(evt events.SignalEvent) bool {
			onEvent(evt)
			return in.handle(ctx, evt)
		})
		in = newIngester(cli, dev, st, sess.timers, env.Log)
		return cli
	}
	c := newConnector(ctx, st, env.Log, sess, dev, provision, newClient)
	if err := c.connect(); err != nil {
		sess.Close()
		st.Close()
		return nil, err
	}
	return &program{sgConnector: c, closers: []io.Closer{sess, st}}, nil
}

// program disconnects once serving has stopped, so an in-flight send can
// finish, then closes the databases.
type program struct {
	*sgConnector
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

type session struct {
	db      *sql.DB
	devices *sgstore.Container
	timers  *timers
}

func (s *session) Close() error { return s.db.Close() }

// The session database is created with mode 0600.
func openSession(ctx context.Context, path string, log dbutil.DatabaseLogger) (*session, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		f.Close()
	} else if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create session file: %w", err)
	}
	dsn := "file:" + path +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	wrapped, err := dbutil.NewWithDB(db, "sqlite3")
	if err != nil {
		db.Close()
		return nil, err
	}
	devices := sgstore.NewStore(wrapped, log)
	if err := devices.Upgrade(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade session: %w", err)
	}
	if _, err := db.ExecContext(ctx, timersSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade session: %w", err)
	}
	return &session{db: db, devices: devices, timers: &timers{db: db}}, nil
}
