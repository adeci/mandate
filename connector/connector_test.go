package connector_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/connector"
)

const (
	rpID   = "box.example.ts.net"
	origin = "https://box.example.ts.net:8781"
)

// fake sends "messages" to a fixed contact book.
type fake struct {
	contacts map[string]string // name -> id
	sent     atomic.Int32
}

func (f *fake) Spec() connector.Spec {
	return connector.Spec{Name: "fake", Ops: []connector.Op{
		{Name: "send", Kind: connector.Write, Params: []connector.Param{
			{Name: "to", Required: true}, {Name: "text", Required: true, Editable: true},
		}},
		{Name: "fetch", Kind: connector.Read},
	}}
}

func (f *fake) Prepare(_ context.Context, _ string, args map[string]string) (connector.Prepared, error) {
	to := args["to"]
	if id, ok := f.contacts[to]; ok {
		to = id
	}
	name := ""
	for n, id := range f.contacts {
		if id == to {
			name = n
		}
	}
	if name == "" {
		return connector.Prepared{}, connector.Invalid("no contact %q", args["to"])
	}
	return connector.Prepared{
		Args:      map[string]string{"to": to, "text": args["text"]},
		Display:   []approval.Field{{Label: "To", Value: name}, {Label: "Text", Value: args["text"], Param: "text"}},
		Container: to,
	}, nil
}

func (f *fake) Execute(context.Context, approval.Record) connector.Result {
	f.sent.Add(1)
	return connector.Result{Status: connector.Done, Ref: "m1"}
}

func (f *fake) Read(context.Context, string, map[string]string) (json.RawMessage, error) {
	return json.RawMessage(`{"ids":["a"]}`), nil
}

type harness struct {
	fake   *fake
	client *connector.Client
	key    *approvaltest.Authenticator
}

func start(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	key := approvaltest.New(t, rpID, origin)
	seen, err := approval.OpenSeen(filepath.Join(dir, "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{contacts: map[string]string{"Farid": "id-farid", "Mallory": "id-mallory"}}
	srv := &connector.Server{
		Connector: f,
		Verifier:  &approval.Verifier{RPID: rpID, Origin: origin, Approvers: []approval.Approver{key.Approver("phone")}},
		Seen:      seen,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	sock := filepath.Join(dir, "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, sock) }()
	t.Cleanup(func() { cancel(); <-done })
	c := connector.Dial(sock, 5*time.Second)
	for i := 0; ; i++ {
		if _, err := c.Spec(context.Background()); err == nil {
			break
		} else if i == 100 {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &harness{fake: f, client: c, key: key}
}

// approved prepares args and signs the resulting record, as the console does.
func (h *harness) approved(t *testing.T, edit func(*approval.Record)) approval.Approval {
	t.Helper()
	p, err := h.client.Prepare(context.Background(), "send", map[string]string{"to": "Farid", "text": "running late"})
	if err != nil {
		t.Fatal(err)
	}
	rec := approval.Record{
		ID: "p1", Connector: "fake", Op: "send", Args: p.Args, Display: p.Display,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	if edit != nil {
		edit(&rec)
	}
	b, err := approval.Encode(rec)
	if err != nil {
		t.Fatal(err)
	}
	return h.key.Approve(b, time.Now())
}

func status(err error) int {
	var e *connector.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestExecuteRunsApprovedWriteExactlyOnce(t *testing.T) {
	t.Parallel()
	h := start(t)
	a := h.approved(t, nil)
	res, err := h.client.Execute(context.Background(), a)
	if err != nil || res.Status != connector.Done {
		t.Fatalf("execute: %+v %v", res, err)
	}
	if _, err := h.client.Execute(context.Background(), a); status(err) != http.StatusConflict {
		t.Fatalf("replay: %v", err)
	}
	if n := h.fake.sent.Load(); n != 1 {
		t.Fatalf("sent %d times", n)
	}
}

func TestExecuteRefusesWithoutRunning(t *testing.T) {
	t.Parallel()
	h := start(t)
	other := approvaltest.New(t, rpID, origin)

	for name, tc := range map[string]struct {
		a    approval.Approval
		want int
	}{
		"display names a different recipient than args": {h.approved(t, func(r *approval.Record) {
			r.Args["to"] = "id-mallory"
		}), http.StatusConflict},
		"approval for another connector": {h.approved(t, func(r *approval.Record) { r.Connector = "other" }), http.StatusForbidden},
		"unenrolled passkey": {func() approval.Approval {
			a := h.approved(t, nil)
			return other.Approve(a.Record, time.Now())
		}(), http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.client.Execute(context.Background(), tc.a); status(err) != tc.want {
				t.Fatalf("err = %v, want status %d", err, tc.want)
			}
		})
	}
	if n := h.fake.sent.Load(); n != 0 {
		t.Fatalf("refused writes still sent %d messages", n)
	}
}

func TestPrepareAndReadRelayErrors(t *testing.T) {
	t.Parallel()
	h := start(t)
	_, err := h.client.Prepare(context.Background(), "send", map[string]string{"to": "Nobody"})
	if status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), `no contact "Nobody"`) {
		t.Fatalf("prepare unknown contact: %v", err)
	}
	if _, err := h.client.Prepare(context.Background(), "fetch", nil); status(err) != http.StatusNotFound {
		t.Fatalf("prepare on a read op: %v", err)
	}
	if _, err := h.client.Read(context.Background(), "send", nil); status(err) != http.StatusNotFound {
		t.Fatalf("read on a write op: %v", err)
	}
	out, err := h.client.Read(context.Background(), "fetch", nil)
	if err != nil || string(out) != `{"ids":["a"]}` {
		t.Fatalf("read: %s %v", out, err)
	}
}

func TestClientRefusesOtherProtocol(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- connector.ServeUnix(ctx, sock, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"protocol":2,"name":"next","ops":[]}`))
		}))
	}()
	t.Cleanup(func() { cancel(); <-done })
	c := connector.Dial(sock, 5*time.Second)
	var err error
	for range 100 {
		if _, err = c.Spec(context.Background()); errors.As(err, new(*connector.ProtocolError)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err == nil || err.Error() != "speaks protocol 2, this mandate speaks 1" {
		t.Fatalf("Spec = %v", err)
	}
}
