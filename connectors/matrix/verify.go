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
	// Other clients learn of a new device from their next sync, and
	// matrix-rust-sdk (Element) drops verification requests from devices it
	// does not know yet. So the first request waits, and is sent again while
	// nobody has acted on it.
	requestDelay = 10 * time.Second
	resendEvery  = 2 * time.Minute
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
//
// mandate sends requests to the owner's devices and accepts the owner's
// requests to it; the first the owner acts on is followed and the others
// are cancelled.
type verifying struct {
	c  *mxConnector
	a  *account
	vh *verificationhelper.VerificationHelper

	verified chan struct{}      // closed when both sides confirmed
	failed   chan verifyFailure // why it stops, first reason wins
	finished chan struct{}      // closed once linked or cleaned up
	once     sync.Once

	// Guarded by mxConnector.mu.
	txn     id.VerificationTransactionID // mandate's latest request, or the one followed
	adopted bool                         // the owner acted on txn
	state   verifyState
	emoji   []rune
	names   []string
	ended   bool
	result  error // why it ended without linking
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

// startVerifying prepares to verify a from another of the owner's devices,
// then follows the verification in the background.
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
	return step, nil
}

// request sends a verification request to the owner's other devices,
// replacing mandate's earlier one, until the owner acts on one.
func (c *mxConnector) request(v *verifying) {
	c.mu.Lock()
	old, adopted := v.txn, v.adopted
	if !adopted {
		v.txn = ""
	}
	c.mu.Unlock()
	if adopted {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, time.Minute)
	defer cancel()
	if old != "" {
		v.cancel(ctx, old, event.VerificationCancelCodeTimeout, "replaced by a new request")
	}
	txn, err := v.vh.StartVerification(ctx, v.a.sess.UserID)
	if err != nil {
		c.log.Warn("send verification request", "err", err)
		return
	}
	c.mu.Lock()
	adopted = v.adopted
	if !adopted {
		v.txn = txn
	}
	c.mu.Unlock()
	if adopted {
		v.cancel(ctx, txn, event.VerificationCancelCodeUser, "verifying with another request")
		return
	}
	c.log.Info("verification requested", "device", v.a.sess.DeviceID)
}

func (v *verifying) cancel(ctx context.Context, txn id.VerificationTransactionID, code event.VerificationCancelCode, reason string) {
	if err := v.vh.CancelVerification(ctx, txn, code, reason); err != nil {
		v.c.log.Info("cancel verification", "err", err)
	}
}

// adopt makes txn the verification followed once the owner acted on it, and
// returns mandate's request it replaces. It reports false if another one is
// followed already.
func (v *verifying) adopt(txn id.VerificationTransactionID) (id.VerificationTransactionID, bool) {
	v.c.mu.Lock()
	defer v.c.mu.Unlock()
	if v.adopted {
		return "", v.txn == txn
	}
	old := v.txn
	v.txn, v.adopted, v.state = txn, true, stateAccepted
	if old == txn {
		old = ""
	}
	return old, true
}

// current reports whether txn is the verification followed.
func (v *verifying) current(txn id.VerificationTransactionID) bool {
	v.c.mu.Lock()
	defer v.c.mu.Unlock()
	return txn != "" && txn == v.txn
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
	timeout := time.NewTimer(c.verifyTimeout)
	defer timeout.Stop()
	request := time.NewTimer(c.requestDelay)
	defer request.Stop()
	var f verifyFailure
wait:
	for {
		select {
		case <-v.verified:
			return nil
		case f = <-v.failed:
			break wait
		case <-request.C:
			c.request(v)
			request.Reset(c.resendEvery)
		case <-timeout.C:
			f = verifyFailure{msg: "verification timed out; link again", cancel: event.VerificationCancelCodeTimeout}
			break wait
		case <-c.ctx.Done():
			f = verifyFailure{msg: "mandate-matrix stopped", cancel: event.VerificationCancelCodeUser}
			break wait
		}
	}
	c.mu.Lock()
	txn := v.txn
	c.mu.Unlock()
	if f.cancel != "" && txn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		v.cancel(ctx, txn, f.cancel, f.msg)
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
	return connector.SetupStep{Instructions: "In Element, accept mandate's verification request, or open Settings → Sessions → mandate → Verify"}
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
	var txn id.VerificationTransactionID
	if comparing {
		txn = v.txn
	}
	c.mu.Unlock()
	if !comparing {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "nothing to confirm"}
	}
	switch answer {
	case "yes":
		if err := v.vh.ConfirmSAS(ctx, txn); err != nil {
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

// VerificationRequested accepts a request from another of the owner's
// devices, unless the owner already acted on another one.
func (v *verifying) VerificationRequested(ctx context.Context, txn id.VerificationTransactionID, from id.UserID, dev id.DeviceID) {
	if from != v.a.sess.UserID {
		v.cancel(ctx, txn, event.VerificationCancelCodeUser, "mandate verifies only its owner's devices")
		return
	}
	v.c.mu.Lock()
	adopted := v.adopted
	v.c.mu.Unlock()
	if adopted {
		v.cancel(ctx, txn, event.VerificationCancelCodeUser, "verifying with another request")
		return
	}
	v.c.log.Info("verification request received", "from_device", dev)
	// Accepting calls VerificationReady, which follows it.
	if err := v.vh.AcceptVerification(ctx, txn); err != nil {
		v.c.log.Warn("accept verification request", "from_device", dev, "err", err)
	}
}

// VerificationReady is called when the owner accepted mandate's request, or
// mandate accepted theirs.
func (v *verifying) VerificationReady(ctx context.Context, txn id.VerificationTransactionID, _ id.DeviceID, sas, _ bool, _ *verificationhelper.QRCode) {
	// The helper holds its lock during this callback, and saves the
	// transaction as ready after it returns; the requests below wait for it.
	ctx = context.WithoutCancel(ctx)
	old, ok := v.adopt(txn)
	if !ok {
		go v.cancel(ctx, txn, event.VerificationCancelCodeUser, "verifying with another request")
		return
	}
	if old != "" {
		go v.cancel(ctx, old, event.VerificationCancelCodeUser, "verifying with another request")
	}
	if !sas {
		v.fail(verifyFailure{msg: "the other device offered no emoji verification", cancel: event.VerificationCancelCodeUnknownMethod})
		return
	}
	go func() {
		if err := v.vh.StartSAS(ctx, txn); err != nil {
			v.c.log.Info("start emoji verification", "err", err)
		}
	}()
}

func (v *verifying) ShowSAS(_ context.Context, txn id.VerificationTransactionID, emoji []rune, names []string, _ []int) {
	v.c.mu.Lock()
	defer v.c.mu.Unlock()
	if txn == v.txn && v.adopted {
		v.state, v.emoji, v.names = stateCompare, emoji, names
	}
}

// VerificationCancelled ends linking only for the verification followed. A
// request of mandate's the owner declined is sent again later, so they can
// still verify from Settings.
func (v *verifying) VerificationCancelled(_ context.Context, txn id.VerificationTransactionID, _ event.VerificationCancelCode, reason string) {
	v.c.mu.Lock()
	followed := txn == v.txn && v.adopted
	if txn == v.txn && !v.adopted {
		v.txn = ""
	}
	v.c.mu.Unlock()
	if followed {
		v.fail(verifyFailure{msg: "verification was cancelled: " + reason})
	}
}

func (v *verifying) VerificationDone(_ context.Context, txn id.VerificationTransactionID, _ event.VerificationMethod) {
	if v.current(txn) {
		v.once.Do(func() { close(v.verified) })
	}
}
