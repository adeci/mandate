// Command mandate-matrix reads a Matrix account as its own device, named
// mandate, and serves the matrix connector.
//
// The only messages it sends are writes the owner approved with a passkey,
// verified by connector.Server. It syncs with presence offline and sends no
// receipts or typing notifications.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func main() {
	connector.Main(start)
}

func start(ctx context.Context, env *connector.Env, _ struct{}) (connector.Connector, error) {
	st, err := store.Open(ctx, env.DataPath())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	zl := zerolog.New(os.Stderr).Level(zerolog.WarnLevel).With().Timestamp().Str("module", "mautrix").Logger()
	// Long enough for a slow initial sync; long polls end after syncTimeout.
	c := newConnector(ctx, st, env, zl, &http.Client{Timeout: 5 * time.Minute})
	if err := c.resume(); err != nil {
		st.Close()
		return nil, err
	}
	return c, nil
}

type mxConnector struct {
	// ctx outlives requests: the sync loop runs until shutdown.
	ctx     context.Context
	st      *store.Store
	dir     string           // the private state directory
	session connector.Secret // the session JSON, with the access token
	log     *slog.Logger
	zl      zerolog.Logger
	http    *http.Client
	// How long the owner has to verify, and the other device to share the
	// key backup key.
	verifyTimeout, backupWait time.Duration

	setup sync.Mutex // held while linking, unlinking, or forgetting a logged-out device

	mu        sync.Mutex
	acct      *account   // nil until linked
	verifying *verifying // a link waiting for verification from another device
	loggedOut bool       // the homeserver ended the session
}

func newConnector(ctx context.Context, st *store.Store, env *connector.Env, zl zerolog.Logger, hc *http.Client) *mxConnector {
	return &mxConnector{
		ctx: ctx, st: st, dir: env.PrivateDir(), session: env.Secret("session.json"),
		log: env.Log, zl: zl, http: hc, verifyTimeout: verifyTimeout, backupWait: backupWait,
	}
}

func (c *mxConnector) cryptoPath() string { return filepath.Join(c.dir, "crypto.db") }

// resume opens the stored session, if any, and starts syncing.
func (c *mxConnector) resume() error {
	sess, err := loadSession(c.session)
	if err != nil || sess == nil {
		return err
	}
	a, err := openAccount(c.ctx, *sess, c.cryptoPath(), c.http, c.zl)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.acct = a
	c.mu.Unlock()
	c.run(a)
	c.log.Info("syncing", "user", sess.UserID, "device", sess.DeviceID)
	return nil
}

// run syncs a until it is stopped or the homeserver logs it out.
func (c *mxConnector) run(a *account) {
	ctx, cancel := context.WithCancel(c.ctx)
	a.cancel = cancel
	go func() {
		err := c.sync(ctx, a)
		close(a.done)
		if errors.Is(err, errLoggedOut) {
			c.log.Warn("the homeserver logged this device out; link again from the console")
			c.drop(a)
		}
	}()
}

// drop forgets a after the homeserver logged it out.
func (c *mxConnector) drop(a *account) {
	c.setup.Lock()
	defer c.setup.Unlock()
	c.mu.Lock()
	current := c.acct == a
	if current {
		c.acct, c.loggedOut = nil, true
	}
	c.mu.Unlock()
	if !current {
		return
	}
	a.close()
	if err := c.forget(); err != nil {
		c.log.Error("forget the logged-out session", "err", err)
	}
}

// use returns the linked account, or nil. Until release, unlinking waits.
func (c *mxConnector) use() *account {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.acct != nil {
		c.acct.ops.RLock()
	}
	return c.acct
}

func (a *account) release() { a.ops.RUnlock() }

func (c *mxConnector) Health(context.Context) connector.Health {
	c.mu.Lock()
	a, v, out := c.acct, c.verifying, c.loggedOut
	verifying := v != nil && !v.ended
	c.mu.Unlock()
	switch {
	case a == nil && verifying:
		return connector.Health{State: connector.NeedsSetup, Detail: "linking: waiting for verification from another device"}
	case a == nil && out:
		return connector.Health{State: connector.NeedsSetup, Detail: "logged out by the homeserver; link again"}
	case a == nil:
		return connector.Health{State: connector.NeedsSetup, Detail: "not linked"}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.syncErr != "":
		return connector.Health{State: connector.Broken, Detail: a.syncErr}
	case a.sess.NoBackup:
		return connector.Health{State: connector.Healthy, Detail: noBackupDetail}
	}
	return connector.Health{State: connector.Healthy}
}

const noBackupDetail = "linked without the key backup, so encrypted messages from before linking stay unreadable; " +
	"relink with the recovery key to read them"

// Close stops syncing once serving has stopped, then closes the databases.
func (c *mxConnector) Close() error {
	c.endVerifying(verifyFailure{msg: "mandate-matrix stopped", cancel: event.VerificationCancelCodeUser})
	c.mu.Lock()
	a := c.acct
	c.acct = nil
	c.mu.Unlock()
	if a != nil {
		a.stop()
		a.close()
	}
	return c.st.Close()
}
