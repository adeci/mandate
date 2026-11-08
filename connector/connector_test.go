package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adeci/mandate/record"
)

type fake struct{ runs atomic.Int32 }

func (*fake) Spec() Spec {
	return Spec{Ops: []Op{
		{Name: "fetch", Kind: Read},
		{Name: "send", Kind: Write, Params: []Param{
			{Name: "chat", Required: true, Container: true},
			{Name: "text", Required: true, Editable: true},
		}},
	}}
}

func (*fake) Health(context.Context) Health { return Health{State: Healthy} }

func (*fake) Setup(context.Context, string, map[string]string) (Step, error) {
	return Step{Done: true}, nil
}

func (*fake) Prepare(_ context.Context, _ string, args map[string]string) (Prepared, error) {
	return Prepared{Args: args, Display: display(args["chat"], args["text"]), Container: args["chat"]}, nil
}

func display(chat, text string) []record.Field {
	return []record.Field{{Label: "To", Value: chat}, {Label: "Text", Value: text, Param: "text"}}
}

func (f *fake) Execute(context.Context, record.Write) Result {
	f.runs.Add(1)
	return Result{Status: Done, Ref: "sent"}
}

func (*fake) Read(context.Context, string, map[string]string) (ReadResult, error) {
	return ReadResult{}, nil
}

// serve runs a Server for c with its state in dir and returns a client and
// a function that stops the server.
func serve(t *testing.T, c Connector, dir string, gateway, owner uint32) (*Client, func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(c, "test", dir, gateway, owner, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	// Socket paths are short; t.TempDir's can be too long.
	sockDir, err := os.MkdirTemp("", "conn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(stop)
	return Dial("test", sock), stop
}

func status(err error) int {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Status
	}
	if err == nil {
		return http.StatusOK
	}
	return 0
}

func TestCallerClasses(t *testing.T) {
	self := uint32(os.Getgid())
	other := self + 1
	const gw, ow = 1, 2
	routes := []struct {
		method, path string
		who          int
	}{
		{"GET", "/spec", gw | ow},
		{"GET", "/health", gw | ow},
		{"GET", "/keys", gw | ow},
		{"POST", "/prepare", gw | ow},
		{"POST", "/read", gw},
		{"POST", "/media", gw},
		{"POST", "/setup", ow},
		{"POST", "/unlink", ow},
		{"POST", "/execute", ow},
		{"PUT", "/keys", ow},
		{"GET", "/containers", ow},
		{"GET", "/icon", ow},
	}
	for _, caller := range []struct {
		name           string
		is             int
		gateway, owner uint32
	}{
		{"gateway", gw, self, other},
		{"owner", ow, other, self},
		{"stranger", 0, other, other},
	} {
		t.Run(caller.name, func(t *testing.T) {
			c, _ := serve(t, &fake{}, t.TempDir(), caller.gateway, caller.owner)
			for _, r := range routes {
				// No body: an allowed caller gets a 400 or a 200, never a 403.
				err := c.do(t.Context(), r.method, r.path, nil, nil)
				if forbidden := status(err) == http.StatusForbidden; forbidden == (r.who&caller.is != 0) {
					t.Errorf("%s %s: %v", r.method, r.path, err)
				}
			}
		})
	}
}

type device struct {
	priv *ecdsa.PrivateKey
	key  record.Key
}

func newDevice(t *testing.T, name string) device {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := record.P256Key(&priv.PublicKey, name)
	if err != nil {
		t.Fatal(err)
	}
	return device{priv, k}
}

func sign(t *testing.T, r record.Record, at time.Time, by device) record.Envelope {
	t.Helper()
	e, err := record.New(r, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := record.Sign(&e, by.priv); err != nil {
		t.Fatal(err)
	}
	return e
}

// appendKey appends an entry adding or removing k, signed by by.
func appendKey(t *testing.T, log []record.Envelope, op string, k record.Key, by device) []record.Envelope {
	t.Helper()
	prev := ""
	if len(log) > 0 {
		h := sha256.Sum256(log[len(log)-1].Record)
		prev = hex.EncodeToString(h[:])
	}
	now := time.Now()
	e := sign(t, record.KeyEntry{Seq: len(log), Op: op, Key: k, Prev: prev, At: record.Time{Time: now}}, now, by)
	return append(log, e)
}

func write(text string) record.Write {
	now := time.Now()
	args := map[string]string{"chat": "c1", "text": text}
	return record.Write{
		ID: record.NewID(), Connector: "test", Op: "send", Args: args, Display: display("c1", text),
		CreatedAt: record.Time{Time: now}, ExpiresAt: record.Time{Time: now.Add(time.Hour)},
	}
}

func TestExecute(t *testing.T) {
	ctx := t.Context()
	self := uint32(os.Getgid())
	dir := t.TempDir()
	f := &fake{}
	c, stop := serve(t, f, dir, self, self)
	phone, laptop, stranger := newDevice(t, "phone"), newDevice(t, "laptop"), newDevice(t, "stranger")
	log := appendKey(t, nil, "add", phone.key, phone)
	log = appendKey(t, log, "add", laptop.key, phone)
	log = appendKey(t, log, "remove", phone.key, laptop)
	if _, err := c.PutKeys(ctx, log); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	signed := func(edit func(w *record.Write), at time.Time, by device) record.Envelope {
		w := write("hi")
		if edit != nil {
			edit(&w)
		}
		return sign(t, w, at, by)
	}
	bad := signed(nil, now, laptop)
	bad.Sig[0] ^= 1
	for _, tc := range []struct {
		name   string
		e      record.Envelope
		status int
	}{
		{"bad signature", bad, http.StatusForbidden},
		{"untrusted key", signed(nil, now, stranger), http.StatusForbidden},
		{"removed key", signed(nil, now, phone), http.StatusForbidden},
		{"stale", signed(nil, now.Add(-10*time.Minute), laptop), http.StatusForbidden},
		{"expired", signed(func(w *record.Write) { w.ExpiresAt.Time = now.Add(-time.Second) }, now, laptop), http.StatusForbidden},
		{"other connector", signed(func(w *record.Write) { w.Connector = "other" }, now, laptop), http.StatusForbidden},
		{"read op", signed(func(w *record.Write) { w.Op = "fetch" }, now, laptop), http.StatusForbidden},
		{"changed display", signed(func(w *record.Write) { w.Display[1].Value = "bye" }, now, laptop), http.StatusConflict},
		{"reordered display", signed(func(w *record.Write) { w.Display[0], w.Display[1] = w.Display[1], w.Display[0] }, now, laptop), http.StatusConflict},
	} {
		if _, err := c.Execute(ctx, tc.e); status(err) != tc.status {
			t.Errorf("%s: got %v, want status %d", tc.name, err, tc.status)
		}
	}
	if n := f.runs.Load(); n != 0 {
		t.Fatalf("refused approvals ran %d writes", n)
	}

	w := write("on my way")
	good := sign(t, w, now, laptop)
	if res, err := c.Execute(ctx, good); err != nil || res.Status != Done {
		t.Fatalf("good approval: %+v, %v", res, err)
	}
	sameID := sign(t, w, now, laptop)
	sameNonce := sign(t, write("again"), now, laptop)
	sameNonce.Nonce = good.Nonce
	if err := record.Sign(&sameNonce, laptop.priv); err != nil {
		t.Fatal(err)
	}
	replays := map[string]record.Envelope{"replay": good, "same id": sameID, "same nonce": sameNonce}
	check := func() {
		t.Helper()
		for name, e := range replays {
			if _, err := c.Execute(ctx, e); status(err) != http.StatusConflict {
				t.Errorf("%s: got %v, want 409", name, err)
			}
		}
		if n := f.runs.Load(); n != 1 {
			t.Fatalf("ran %d writes, want 1", n)
		}
	}
	check()

	stop()
	c, _ = serve(t, f, dir, self, self)
	check()
	// The key log survived the restart too.
	if res, err := c.Execute(ctx, sign(t, write("later"), now, laptop)); err != nil || res.Status != Done {
		t.Fatalf("approval after restart: %+v, %v", res, err)
	}
}

func TestPutKeysRefusesRewrite(t *testing.T) {
	ctx := t.Context()
	self := uint32(os.Getgid())
	dir := t.TempDir()
	c, stop := serve(t, &fake{}, dir, self, self)
	phone, laptop, tablet := newDevice(t, "phone"), newDevice(t, "laptop"), newDevice(t, "tablet")
	base := appendKey(t, nil, "add", phone.key, phone)
	held := appendKey(t, base, "add", laptop.key, phone)
	want, err := c.PutKeys(ctx, held)
	if err != nil {
		t.Fatal(err)
	}

	for name, log := range map[string][]record.Envelope{
		"rewritten":  appendKey(t, base, "add", tablet.key, phone),
		"truncated":  base,
		"other root": appendKey(t, nil, "add", tablet.key, tablet),
	} {
		if _, err := c.PutKeys(ctx, log); status(err) != http.StatusConflict {
			t.Errorf("%s log: got %v, want 409", name, err)
		}
	}

	stop()
	c, _ = serve(t, &fake{}, dir, self, self)
	if got, err := c.Keys(ctx); err != nil || got != want {
		t.Fatalf("key log after restart = %+v, %v; want %+v", got, err, want)
	}
	if _, err := c.PutKeys(ctx, appendKey(t, held, "add", tablet.key, laptop)); err != nil {
		t.Fatalf("extension refused: %v", err)
	}
}
