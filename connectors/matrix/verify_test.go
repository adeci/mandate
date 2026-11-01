package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/connector"
)

// elementVerifier is the owner tapping through verification in Element: it
// accepts mandate's request unless ignore is set, and shows the emoji; the
// test confirms them.
type elementVerifier struct {
	vh     *verificationhelper.VerificationHelper
	ignore bool // leave mandate's requests alone

	mu        sync.Mutex
	requested id.VerificationTransactionID // mandate's latest request
	sasTxn    id.VerificationTransactionID
	emoji     string
	done      bool
	cancelled map[id.VerificationTransactionID]bool
}

func (e *elementVerifier) VerificationRequested(ctx context.Context, txn id.VerificationTransactionID, _ id.UserID, _ id.DeviceID) {
	e.mu.Lock()
	e.requested = txn
	e.mu.Unlock()
	if !e.ignore {
		// The helper holds its lock during callbacks.
		go e.vh.AcceptVerification(context.WithoutCancel(ctx), txn)
	}
}

func (e *elementVerifier) VerificationReady(context.Context, id.VerificationTransactionID, id.DeviceID, bool, bool, *verificationhelper.QRCode) {
}

func (e *elementVerifier) ShowSAS(_ context.Context, txn id.VerificationTransactionID, emoji []rune, _ []string, _ []int) {
	parts := make([]string, len(emoji))
	for i, r := range emoji {
		parts[i] = string(r)
	}
	e.mu.Lock()
	e.sasTxn, e.emoji = txn, strings.Join(parts, " ")
	e.mu.Unlock()
}

func (e *elementVerifier) VerificationCancelled(_ context.Context, txn id.VerificationTransactionID, _ event.VerificationCancelCode, _ string) {
	e.mu.Lock()
	e.cancelled[txn] = true
	e.mu.Unlock()
}

func (e *elementVerifier) VerificationDone(context.Context, id.VerificationTransactionID, event.VerificationMethod) {
	e.mu.Lock()
	e.done = true
	e.mu.Unlock()
}

func (e *elementVerifier) state() (emoji string, done bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.emoji, e.done
}

func (e *elementVerifier) wasCancelled(txn id.VerificationTransactionID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelled[txn]
}

func (e *elementVerifier) confirm(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	txn := e.sasTxn
	e.mu.Unlock()
	if err := e.vh.ConfirmSAS(context.Background(), txn); err != nil {
		t.Fatal(err)
	}
}

// verifier runs Element's side of verification and secret sharing,
// receiving its to-device messages until the test ends.
func (f *fixture) verifier(ignore bool) *elementVerifier {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e := &elementVerifier{ignore: ignore, cancelled: map[id.VerificationTransactionID]bool{}}
	e.vh = verificationhelper.NewVerificationHelper(f.element.cli, f.element.mach, nil, e, false, false, true)
	if err := e.vh.Init(ctx); err != nil {
		f.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if resp := f.elementMail(); len(resp.ToDevice.Events) > 0 {
				f.element.toDevice(ctx, resp, "")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	f.t.Cleanup(func() { cancel(); <-done })
	return e
}

// elementMail takes Element's to-device inbox as a sync response.
func (f *fixture) elementMail() *mautrix.RespSync {
	f.t.Helper()
	f.hs.mu.Lock()
	mail, err := json.Marshal(map[string]any{"next_batch": "x", "to_device": map[string]any{"events": f.hs.inbox["ELEMENT"]}})
	f.hs.inbox["ELEMENT"] = nil
	f.hs.mu.Unlock()
	if err != nil {
		f.t.Fatal(err)
	}
	var resp mautrix.RespSync
	if err := json.Unmarshal(mail, &resp); err != nil {
		f.t.Fatal(err)
	}
	return &resp
}

// shareBackupKey lets Element answer requests for the key backup key.
func (f *fixture) shareBackupKey() {
	f.t.Helper()
	key := base64.RawStdEncoding.EncodeToString(f.backupKey.Bytes())
	if err := f.element.crypto.PutSecret(context.Background(), id.SecretMegolmBackupV1, key); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) verifyArgs() map[string]string {
	args := f.setupArgs()
	delete(args, "recovery_key")
	return args
}

// untilConfirm polls setup as the console does until it asks about the emoji.
func (f *fixture) untilConfirm() connector.SetupStep {
	f.t.Helper()
	var step connector.SetupStep
	eventually(f.t, "the emoji are shown", func() bool {
		var err error
		if step, err = f.c.Setup(context.Background(), f.verifyArgs()); err != nil {
			f.t.Fatal(err)
		}
		return step.Confirm != ""
	})
	return step
}

// compareAndLink checks mandate and Element show the same emoji, confirms on
// both, and waits until mandate has linked.
func (f *fixture) compareAndLink(element *elementVerifier) {
	f.t.Helper()
	ctx := context.Background()
	step := f.untilConfirm()
	emoji, _ := element.state()
	if step.Code != emoji || len(strings.Fields(step.Code)) != 7 || len(strings.Split(step.Instructions, ", ")) != 7 {
		f.t.Fatalf("mandate shows %q (%s), Element %q", step.Code, step.Instructions, emoji)
	}
	element.confirm(f.t)
	step, err := f.c.Setup(ctx, map[string]string{connector.SetupConfirm: "yes"})
	if err != nil || step.Confirm != "" {
		f.t.Fatalf("confirm: %+v %v", step, err)
	}
	eventually(f.t, "linking finishes", func() bool {
		step, err := f.c.Setup(ctx, f.verifyArgs())
		if err != nil {
			f.t.Fatal(err)
		}
		return step.Done
	})
	if _, done := element.state(); !done {
		f.t.Fatal("Element did not finish verifying")
	}
}

func TestVerifyLink(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	old, enc := f.encrypt(vaultRoom, "$old", "the old plan", 1000)
	f.backUp(vaultRoom, enc)
	f.shareBackupKey()
	element := f.verifier(false)
	ctx := context.Background()

	step, err := f.c.Setup(ctx, f.verifyArgs())
	if err != nil || !strings.Contains(step.Instructions, "accept mandate's verification request") {
		t.Fatalf("setup: %+v %v", step, err)
	}
	f.compareAndLink(element)
	if h := f.c.Health(ctx); h.State != connector.Healthy || h.Detail != "" {
		t.Fatalf("health = %+v", h)
	}

	a := f.c.use()
	dev := a.sess.DeviceID
	if a.mach.CrossSigningKeys != nil {
		t.Error("mandate holds cross-signing keys")
	}
	for _, s := range []id.Secret{id.SecretXSMaster, id.SecretXSSelfSigning, id.SecretXSUserSigning, id.SecretMegolmBackupV1} {
		if v, err := a.crypto.GetSecret(ctx, s); err != nil || v != "" {
			t.Errorf("crypto store keeps %s: %q %v", s, v, err)
		}
	}
	a.release()
	// Element signed the new device with its self-signing key.
	f.hs.mu.Lock()
	var selfKey id.Ed25519
	for _, k := range f.hs.selfSigning.Keys {
		selfKey = k
	}
	signed := false
	for _, req := range f.hs.signatures {
		if sig := req[owner][dev.String()].Signatures[owner]; sig[id.NewKeyID(id.KeyAlgorithmEd25519, selfKey.String())] != "" {
			signed = true
		}
	}
	f.hs.mu.Unlock()
	if !signed {
		t.Fatalf("device %s was not cross-signed", dev)
	}

	// History from the backup decrypts.
	n := f.hs.push(joined(map[id.RoomID]map[string]any{vaultRoom: room(
		[]map[string]any{stateEvent("m.room.encryption", "", owner, map[string]any{"algorithm": "m.megolm.v1.aes-sha2"}), member(owner, "Owner")},
		[]map[string]any{old},
	)}))
	f.hs.waitSynced(n)
	if got := f.message("$old"); got.Body != "the old plan" {
		t.Fatalf("$old = %+v", got)
	}
}

func TestVerifyMismatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	element := f.verifier(false)
	ctx := context.Background()
	if _, err := f.c.Setup(ctx, f.verifyArgs()); err != nil {
		t.Fatal(err)
	}
	f.untilConfirm()
	_, err := f.c.Setup(ctx, map[string]string{connector.SetupConfirm: "no"})
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || !strings.Contains(ce.Message, "did not match") {
		t.Fatalf("no: %v", err)
	}
	if got := locked(f.hs, func() []id.DeviceID { return f.hs.loggedOut }); len(got) != 1 || got[0] != "MANDATE1" {
		t.Fatalf("logged out %v", got)
	}
	if entries, _ := os.ReadDir(f.env.PrivateDir()); len(entries) != 0 {
		t.Fatalf("private dir keeps %v", entries)
	}
	if h := f.c.Health(ctx); h.State != connector.NeedsSetup || h.Detail != "not linked" {
		t.Fatalf("health = %+v", h)
	}
	eventually(t, "Element learns of the cancellation", func() bool {
		element.mu.Lock()
		defer element.mu.Unlock()
		return element.cancelled[element.sasTxn]
	})
	if _, err := f.c.Setup(ctx, map[string]string{connector.SetupConfirm: "yes"}); !errors.As(err, &ce) || ce.Status != http.StatusConflict {
		t.Fatalf("confirm after cancelling: %v", err)
	}
}

func TestVerifyWithoutBackupKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.c.backupWait = 300 * time.Millisecond
	element := f.verifier(false)
	ctx := context.Background()
	if _, err := f.c.Setup(ctx, f.verifyArgs()); err != nil {
		t.Fatal(err)
	}
	f.compareAndLink(element)
	if h := f.c.Health(ctx); h.State != connector.Healthy || h.Detail != noBackupDetail {
		t.Fatalf("health = %+v", h)
	}
}

// The owner ignores mandate's request and verifies from Element's session
// list instead.
func TestVerifyFromElement(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.shareBackupKey()
	element := f.verifier(true)
	ctx := context.Background()
	if _, err := f.c.Setup(ctx, f.verifyArgs()); err != nil {
		t.Fatal(err)
	}
	var stale id.VerificationTransactionID
	eventually(t, "mandate's request reaches Element", func() bool {
		element.mu.Lock()
		defer element.mu.Unlock()
		stale = element.requested
		return stale != ""
	})
	// Element learns of the new device from sync.
	if _, err := f.element.mach.FetchKeys(ctx, []id.UserID{owner}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := element.vh.StartVerification(ctx, owner); err != nil {
		t.Fatal(err)
	}
	f.compareAndLink(element)
	if !element.wasCancelled(stale) {
		t.Fatal("mandate's own request was not cancelled")
	}
	if h := f.c.Health(ctx); h.State != connector.Healthy || h.Detail != "" {
		t.Fatalf("health = %+v", h)
	}
}

// mandate asks again until the owner acts.
func TestVerifyRequestsAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.c.resendEvery = 50 * time.Millisecond
	element := f.verifier(true)
	if _, err := f.c.Setup(context.Background(), f.verifyArgs()); err != nil {
		t.Fatal(err)
	}
	var first id.VerificationTransactionID
	eventually(t, "a first request", func() bool {
		element.mu.Lock()
		defer element.mu.Unlock()
		first = element.requested
		return first != ""
	})
	eventually(t, "the first request is replaced", func() bool {
		element.mu.Lock()
		defer element.mu.Unlock()
		return element.requested != first && element.cancelled[first]
	})
}

func TestVerifyTimesOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.c.verifyTimeout = 300 * time.Millisecond
	ctx := context.Background()
	// Nobody accepts.
	if _, err := f.c.Setup(ctx, f.verifyArgs()); err != nil {
		t.Fatal(err)
	}
	var err error
	eventually(t, "the verification times out", func() bool {
		_, err = f.c.Setup(ctx, f.verifyArgs())
		return err != nil
	})
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("setup: %v", err)
	}
	if got := locked(f.hs, func() []id.DeviceID { return f.hs.loggedOut }); len(got) != 1 {
		t.Fatalf("logged out %v", got)
	}
}
