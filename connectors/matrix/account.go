package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/sqlstatestore"

	"github.com/adeci/mandate/connector"
)

// session is what linking leaves in the private directory, next to the
// crypto database it unlocks.
type session struct {
	Homeserver  string      `json:"homeserver"`
	UserID      id.UserID   `json:"user_id"`
	DeviceID    id.DeviceID `json:"device_id"`
	AccessToken string      `json:"access_token"`
	PickleKey   []byte      `json:"pickle_key"`
	// NoBackup is set when the device was linked without the key backup, so
	// messages from before it cannot be decrypted.
	NoBackup bool `json:"no_backup,omitempty"`
}

func loadSession(s connector.Secret) (*session, error) {
	b, err := s.Load()
	if err != nil || b == nil {
		return nil, err
	}
	var sess session
	if err := json.Unmarshal(b, &sess); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	return &sess, nil
}

// account is a linked device: the client, its crypto, and the room state
// encryption needs.
type account struct {
	sess   session
	cli    *mautrix.Client
	mach   *crypto.OlmMachine
	crypto *crypto.SQLCryptoStore
	state  *sqlstatestore.SQLStateStore
	db     *dbutil.Database

	ops    sync.RWMutex // read-held by requests using the account; close takes it
	cancel context.CancelFunc
	done   chan struct{} // closed when the sync loop has stopped

	// Only the sync loop uses these.
	direct map[id.RoomID]id.UserID // DM rooms and who they are with, from m.direct
	since  *string                 // nil until loaded

	mu      sync.Mutex
	syncErr string
	// pending holds events that could not be decrypted yet, by session, for
	// when the key arrives; arrived lists sessions received since.
	pending map[id.SessionID]map[id.EventID]*event.Event
	npend   int
	arrived []id.SessionID
}

const maxPending = 1000

func newClient(hs string, userID id.UserID, token string, hc *http.Client, zl zerolog.Logger) (*mautrix.Client, error) {
	cli, err := mautrix.NewClient(hs, userID, token)
	if err != nil {
		return nil, err
	}
	cli.Client = hc
	cli.Log = zl
	// A failed request is never retried here, so a send leaves at most once.
	cli.DefaultHTTPRetries = 0
	return cli, nil
}

func openAccount(ctx context.Context, sess session, dbPath string, hc *http.Client, zl zerolog.Logger) (*account, error) {
	cli, err := newClient(sess.Homeserver, sess.UserID, sess.AccessToken, hc, zl)
	if err != nil {
		return nil, err
	}
	cli.DeviceID = sess.DeviceID
	db, err := openCryptoDB(dbPath, zl)
	if err != nil {
		return nil, err
	}
	a := &account{sess: sess, cli: cli, db: db, done: make(chan struct{}), pending: map[id.SessionID]map[id.EventID]*event.Event{}}
	if err := a.load(ctx, zl); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *account) load(ctx context.Context, zl zerolog.Logger) error {
	a.state = sqlstatestore.NewSQLStateStore(a.db, dbutil.ZeroLogger(zl), false)
	if err := a.state.Upgrade(ctx); err != nil {
		return fmt.Errorf("upgrade state store: %w", err)
	}
	a.crypto = crypto.NewSQLCryptoStore(a.db, dbutil.ZeroLogger(zl), a.sess.UserID.String(), a.sess.DeviceID, a.sess.PickleKey)
	if err := a.crypto.DB.Upgrade(ctx); err != nil {
		return fmt.Errorf("upgrade crypto store: %w", err)
	}
	a.cli.StateStore = a.state
	a.mach = crypto.NewOlmMachine(a.cli, &zl, a.crypto, a.state)
	// mautrix sends room keys to any device by default, so one the
	// homeserver adds to a member's account would read the room.
	a.mach.SendKeysMinTrust = id.TrustStateCrossSignedTOFU
	a.mach.ShareKeysMinTrust = id.TrustStateCrossSignedTOFU
	a.cli.Crypto = olmCrypto{a}
	a.mach.SessionReceived = func(_ context.Context, _ id.RoomID, sid id.SessionID, _ uint32) {
		a.mu.Lock()
		if a.pending[sid] != nil {
			a.arrived = append(a.arrived, sid)
		}
		a.mu.Unlock()
	}
	if err := a.mach.Load(ctx); err != nil {
		return fmt.Errorf("load olm account: %w", err)
	}
	return nil
}

// olmCrypto is the mautrix.CryptoHelper that verificationhelper requires.
// Sends encrypt explicitly, so SendMessageEvent never encrypts through it.
type olmCrypto struct{ a *account }

func (o olmCrypto) Encrypt(ctx context.Context, room id.RoomID, t event.Type, content any) (*event.EncryptedEventContent, error) {
	return o.a.encrypt(ctx, room, t, content)
}

func (o olmCrypto) Decrypt(ctx context.Context, evt *event.Event) (*event.Event, error) {
	return o.a.mach.DecryptMegolmEvent(ctx, evt)
}

func (o olmCrypto) WaitForSession(ctx context.Context, room id.RoomID, key id.SenderKey, sid id.SessionID, d time.Duration) bool {
	return o.a.mach.WaitForSession(ctx, room, key, sid, d)
}

func (olmCrypto) RequestSession(context.Context, id.RoomID, id.SenderKey, id.SessionID, id.UserID, id.DeviceID) {
}

func (olmCrypto) Init(context.Context) error { return nil }

// toDevice hands a sync's to-device messages to the machine (keys, secrets)
// and the rest to the client's syncer (verification).
func (a *account) toDevice(ctx context.Context, resp *mautrix.RespSync, since string) {
	a.mach.ProcessSyncResponse(ctx, resp, since)
	syncer := a.cli.Syncer.(mautrix.DispatchableSyncer)
	for _, evt := range resp.ToDevice.Events {
		if evt.Type != event.ToDeviceEncrypted {
			syncer.Dispatch(ctx, evt)
		}
	}
}

// openCryptoDB opens the database holding the device's keys, created with
// mode 0600.
func openCryptoDB(path string, zl zerolog.Logger) (*dbutil.Database, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		f.Close()
	} else if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create crypto database: %w", err)
	}
	raw, err := sql.Open("sqlite3", "file:"+path+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open crypto database: %w", err)
	}
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		raw.Close()
		return nil, err
	}
	db.Log = dbutil.ZeroLogger(zl)
	return db, nil
}

func removeCryptoDB(path string) error {
	var errs []error
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stop ends the sync loop and waits for it.
func (a *account) stop() {
	if a.cancel != nil {
		a.cancel()
		<-a.done
	}
}

// close waits for requests using a, then closes its database. The sync loop
// must have stopped.
func (a *account) close() {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.mach.Destroy()
	a.db.Close()
}

// forget deletes the session and the device's keys.
func (c *mxConnector) forget() error {
	return errors.Join(c.session.Delete(), removeCryptoDB(c.cryptoPath()))
}

// keep remembers evt to decrypt again when its session arrives.
func (a *account) keep(evt *event.Event) {
	sid := evt.Content.AsEncrypted().SessionID
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.npend >= maxPending {
		return
	}
	if a.pending[sid] == nil {
		a.pending[sid] = map[id.EventID]*event.Event{}
	}
	if _, ok := a.pending[sid][evt.ID]; !ok {
		a.pending[sid][evt.ID] = evt
		a.npend++
	}
}

// retry takes the kept events whose sessions have arrived.
func (a *account) retry() []*event.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var evts []*event.Event
	for _, sid := range a.arrived {
		for _, evt := range a.pending[sid] {
			evts = append(evts, evt)
		}
		a.npend -= len(a.pending[sid])
		delete(a.pending, sid)
	}
	a.arrived = nil
	return evts
}
