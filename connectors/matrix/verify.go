package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/connector"
)

const (
	verifyTimeout = 10 * time.Minute
	backupWait    = time.Minute
	// Only to-device messages matter while verifying; rooms wait until linked.
	verifyFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},"room":{"rooms":[]}}`
)

type verifyState int

const (
	stateRequested verifyState = iota
	stateAccepted
	stateCompare
	stateConfirmed
	stateBackup
)

// verifying is a link waiting for the owner to verify the new device from
// another of theirs with emoji (SAS). mandate never holds the cross-signing
// keys: the other device signs this one, then shares the key backup key.
type verifying struct {
	c   *mxConnector
	a   *account
	vh  *verificationhelper.VerificationHelper
	txn id.VerificationTransactionID

	verified chan struct{}      // closed when both sides confirmed
	failed   chan verifyFailure // why it stops, first reason wins
	finished chan struct{}      // closed once linked or cleaned up
	once     sync.Once

	// Guarded by mxConnector.mu.
	state  verifyState
	emoji  []rune
	names  []string
	ended  bool
	result error // why it ended without linking
}

// verifyFailure ends a verification. cancel, if set, is sent to the other
// device.
type verifyFailure struct {
	msg    string
	cancel event.VerificationCancelCode
}

func (v *verifying) fail(f verifyFailure) {
	select {
	case v.failed <- f:
	default:
	}
}

// startVerifying asks the owner's other devices to verify a, then follows
// the verification in the background.
func (c *mxConnector) startVerifying(ctx context.Context, a *account) (connector.SetupStep, error) {
	user := a.sess.UserID
	devices, err := a.mach.FetchKeys(ctx, []id.UserID{user}, true)
	if err != nil {
		return connector.SetupStep{}, upstream("list your devices", err)
	}
	others := 0
	for dev := range devices[user] {
		if dev != a.sess.DeviceID {
			others++
		}
	}
	if others == 0 {
		return connector.SetupStep{}, connector.Invalid("no other device of yours is signed in to verify from; link with the recovery key")
	}
	v := &verifying{
		c: c, a: a,
		verified: make(chan struct{}), failed: make(chan verifyFailure, 1), finished: make(chan struct{}),
	}
	v.vh = verificationhelper.NewVerificationHelper(a.cli, a.mach, nil, v, false, false, true)
	if err := v.vh.Init(ctx); err != nil {
		return connector.SetupStep{}, fmt.Errorf("start verification: %w", err)
	}
	if v.txn, err = v.vh.StartVerification(ctx, user); err != nil {
		return connector.SetupStep{}, upstream("send the verification request", err)
	}
	syncCtx, stopSync := context.WithCancel(c.ctx)
	synced := make(chan struct{})
	go func() {
		defer close(synced)
		c.verifySync(syncCtx, v)
	}()
	c.mu.Lock()
	c.verifying = v
	step := v.step()
	c.mu.Unlock()
	go c.follow(v, func() { stopSync(); <-synced })
	c.log.Info("verification requested", "device", a.sess.DeviceID)
	return step, nil
}

// follow waits for the verification to end, then links a, or logs it out
// and forgets it.
func (c *mxConnector) follow(v *verifying, stopSync func()) {
	defer close(v.finished)
	err := c.awaitVerified(v)
	if err == nil {
		c.setVerifyState(v, stateBackup)
		restored, berr := v.a.restoreBackup(c.ctx, c.backupWait)
		if berr != nil {
			c.log.Warn("restore the key backup", "err", berr)
		}
		v.a.sess.NoBackup = !restored
	}
	stopSync()
	if err == nil {
		err = c.commit(v.a)
	}
	if err != nil {
		c.log.Warn("verification ended without linking", "err", err)
		c.abandon(v.a, v.a.sess)
	}
	c.mu.Lock()
	v.ended, v.result = true, err
	c.mu.Unlock()
}

func (c *mxConnector) awaitVerified(v *verifying) error {
	timer := time.NewTimer(c.verifyTimeout)
	defer timer.Stop()
	var f verifyFailure
	select {
	case <-v.verified:
		return nil
	case f = <-v.failed:
	case <-timer.C:
		f = verifyFailure{msg: "verification timed out; link again", cancel: event.VerificationCancelCodeTimeout}
	case <-c.ctx.Done():
		f = verifyFailure{msg: "mandate-matrix stopped", cancel: event.VerificationCancelCodeUser}
	}
	if f.cancel != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := v.vh.CancelVerification(ctx, v.txn, f.cancel, f.msg); err != nil {
			c.log.Warn("cancel verification", "err", err)
		}
		cancel()
	}
	return errors.New(f.msg)
}

// verifySync receives the to-device messages verification and secret
// sharing need, until ctx is done.
func (c *mxConnector) verifySync(ctx context.Context, v *verifying) {
	var since string
	var wait time.Duration
	for {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		resp, err := v.a.cli.FullSyncRequest(ctx, mautrix.ReqSync{
			Since:       since,
			Timeout:     int(syncTimeout / time.Millisecond),
			FilterID:    verifyFilter,
			SetPresence: event.PresenceOffline,
		})
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, mautrix.MUnknownToken):
			v.fail(verifyFailure{msg: "the homeserver logged the new device out"})
			return
		case err != nil:
			c.log.Warn("sync while verifying", "err", err)
			wait = min(max(2*wait, time.Second), maxBackoff)
			continue
		}
		wait = 0
		v.a.toDevice(ctx, resp, since)
		since = resp.NextBatch
	}
}

func (c *mxConnector) setVerifyState(v *verifying, s verifyState) {
	c.mu.Lock()
	v.state = s
	c.mu.Unlock()
}

// step is what the console shows; c.mu must be held.
func (v *verifying) step() connector.SetupStep {
	switch v.state {
	case stateAccepted:
		return connector.SetupStep{Instructions: "Accepted. Waiting for the emoji…"}
	case stateCompare:
		emoji := make([]string, len(v.emoji))
		for i, r := range v.emoji {
			emoji[i] = string(r)
		}
		return connector.SetupStep{
			Code:         strings.Join(emoji, " "),
			Instructions: strings.Join(v.names, ", "),
			Confirm:      "Does Element show the same emoji in the same order? Confirm in Element too.",
		}
	case stateConfirmed:
		return connector.SetupStep{Instructions: "Waiting for Element to confirm…"}
	case stateBackup:
		return connector.SetupStep{Instructions: "Verified. Waiting for Element to share the key backup…"}
	}
	return connector.SetupStep{Instructions: "Accept the request from mandate in Element on another device"}
}

// verifyingStep reports the verification in progress, or how it ended.
func (c *mxConnector) verifyingStep() (connector.SetupStep, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.verifying
	switch {
	case v == nil:
		return connector.SetupStep{}, nil, false
	case !v.ended:
		return v.step(), nil, true
	}
	c.verifying = nil
	if v.result != nil {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusBadRequest, Message: v.result.Error()}, true
	}
	return connector.SetupStep{Done: true}, nil, true
}

// answer takes the owner's answer to whether the emoji match.
func (c *mxConnector) answer(ctx context.Context, answer string) (connector.SetupStep, error) {
	c.mu.Lock()
	v := c.verifying
	comparing := v != nil && !v.ended && v.state == stateCompare
	c.mu.Unlock()
	if !comparing {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "nothing to confirm"}
	}
	switch answer {
	case "yes":
		if err := v.vh.ConfirmSAS(ctx, v.txn); err != nil {
			return connector.SetupStep{}, fmt.Errorf("confirm verification: %w", err)
		}
		c.setVerifyState(v, stateConfirmed)
		step, err, _ := c.verifyingStep()
		return step, err
	case "no":
		c.endVerifying(verifyFailure{
			msg:    "The emoji did not match, so linking was cancelled and the new device logged out.",
			cancel: event.VerificationCancelCodeSASMismatch,
		})
		return connector.SetupStep{}, connector.Invalid("The emoji did not match, so linking was cancelled and the new device logged out.")
	}
	return connector.SetupStep{}, connector.Invalid("confirm is yes or no")
}

// endVerifying stops a verification in progress and waits until it is
// cleaned up.
func (c *mxConnector) endVerifying(f verifyFailure) {
	c.mu.Lock()
	v := c.verifying
	c.verifying = nil
	c.mu.Unlock()
	if v != nil {
		v.fail(f)
		<-v.finished
	}
}

// restoreBackup asks the owner's verified devices for the key backup key and
// imports the backup with it. It reports whether it did; nobody sharing the
// key within wait is not an error.
func (a *account) restoreBackup(ctx context.Context, wait time.Duration) (bool, error) {
	version, err := a.cli.GetKeyBackupLatestVersion(ctx)
	if errors.Is(err, mautrix.MNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if version.Algorithm != id.KeyBackupAlgorithmMegolmBackupV1 {
		return false, fmt.Errorf("unsupported key backup algorithm %s", version.Algorithm)
	}
	errRestored := errors.New("restored")
	err = a.mach.GetOrRequestSecret(ctx, id.SecretMegolmBackupV1, func(secret string) (bool, error) {
		key := backupKey(secret, version.AuthData.PublicKey)
		if key == nil {
			return false, nil // wait for another device's answer
		}
		if err := a.mach.GetAndStoreKeyBackup(ctx, version.Version, key); err != nil {
			return false, err
		}
		// An error, so GetOrRequestSecret does not store the key: mandate
		// needs it only this once.
		return false, errRestored
	}, wait)
	switch {
	case errors.Is(err, errRestored):
		return true, nil
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return false, nil
	}
	return false, err
}

// backupKey decodes a shared key backup key, or returns nil if it is not the
// key to the backup with public key pub.
func backupKey(secret string, pub id.Ed25519) *backup.MegolmBackupKey {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(secret, "="))
	if err != nil {
		return nil
	}
	key, err := backup.MegolmBackupKeyFromBytes(raw)
	if err != nil || base64.RawStdEncoding.EncodeToString(key.PublicKey().Bytes()) != pub.String() {
		return nil
	}
	return key
}

// The verificationhelper callbacks run on the verification sync goroutine.

func (v *verifying) VerificationRequested(context.Context, id.VerificationTransactionID, id.UserID, id.DeviceID) {
}

func (v *verifying) VerificationReady(ctx context.Context, txn id.VerificationTransactionID, _ id.DeviceID, sas, _ bool, _ *verificationhelper.QRCode) {
	if txn != v.txn {
		return
	}
	if !sas {
		v.fail(verifyFailure{msg: "the other device offered no emoji verification", cancel: event.VerificationCancelCodeUnknownMethod})
		return
	}
	v.c.setVerifyState(v, stateAccepted)
	// The helper holds its lock during callbacks and saves the transaction
	// as ready after this returns.
	go func() {
		if err := v.vh.StartSAS(context.WithoutCancel(ctx), txn); err != nil {
			v.c.log.Info("start emoji verification", "err", err)
		}
	}()
}

func (v *verifying) ShowSAS(_ context.Context, txn id.VerificationTransactionID, emoji []rune, names []string, _ []int) {
	if txn != v.txn {
		return
	}
	v.c.mu.Lock()
	v.state, v.emoji, v.names = stateCompare, emoji, names
	v.c.mu.Unlock()
}

func (v *verifying) VerificationCancelled(_ context.Context, txn id.VerificationTransactionID, _ event.VerificationCancelCode, reason string) {
	if txn == v.txn {
		v.fail(verifyFailure{msg: "verification was cancelled: " + reason})
	}
}

func (v *verifying) VerificationDone(_ context.Context, txn id.VerificationTransactionID, _ event.VerificationMethod) {
	if txn == v.txn {
		v.once.Do(func() { close(v.verified) })
	}
}
