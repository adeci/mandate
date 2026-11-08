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
	"go.mau.fi/mautrix-signal/pkg/signalmeow/web"
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
	web.SignalHTTPClient.Transport = capMedia{web.SignalHTTPClient.Transport}

	st, err := openStore(ctx, env.DataPath())
	if err != nil {
		return nil, err
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
	newClient := func(dev *sgstore.Device, onEvent func(events.SignalEvent)) (sgClient, *ingester) {
		var in *ingester
		cli := signalmeow.NewClient(dev, zl, func(evt events.SignalEvent) bool {
			onEvent(evt)
			return in.handle(ctx, evt)
		})
		in = newIngester(cli, dev, st, sess, env.Log)
		return cli, in
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

// openStore opens the store and dates each chat by its newest stored
// message, unless it was active later. Running it again changes nothing.
func openStore(ctx context.Context, path string) (*store.Store, error) {
	st, err := store.Open(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
UPDATE containers SET last_message_at = (SELECT max(sent_at) FROM messages WHERE container_id = containers.id)
WHERE (SELECT max(sent_at) FROM messages WHERE container_id = containers.id) > COALESCE(last_message_at, '')`); err != nil {
		st.Close()
		return nil, fmt.Errorf("date chats: %w", err)
	}
	return st, nil
}

type session struct {
	db         *sql.DB
	devices    *sgstore.Container
	timers     *timers
	identities *identities
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
	for _, schema := range []string{timersSchema, identitiesSchema} {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			db.Close()
			return nil, fmt.Errorf("upgrade session: %w", err)
		}
	}
	return &session{db: db, devices: devices, timers: &timers{db: db}, identities: &identities{db: db}}, nil
}
