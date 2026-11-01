package connector

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
)

const (
	testRPID   = "box.example.ts.net"
	testOrigin = "https://box.example.ts.net:8781"
)

type testConfig struct {
	Greeting string `json:"greeting"`
}

func (c testConfig) Validate() error {
	if c.Greeting == "bad" {
		return errors.New("greeting must not be bad")
	}
	return nil
}

// echo has one write op and records what Main handed it.
type echo struct {
	env    *Env
	cfg    testConfig
	sent   int
	closed bool
}

func (e *echo) Spec() Spec {
	return Spec{Name: "echo", Ops: []Op{{Name: "send", Kind: Write, Params: []Param{{Name: "text"}}}}}
}

func (e *echo) Prepare(_ context.Context, _ string, args map[string]string) (Prepared, error) {
	return Prepared{Args: args, Display: []approval.Field{{Label: "Text", Value: args["text"]}}}, nil
}

func (e *echo) Execute(context.Context, approval.Record) Result {
	e.sent++
	return Result{Status: Done}
}

func (e *echo) Close() error {
	e.closed = true
	return nil
}

type launched struct {
	echo   *echo
	client *Client
	cancel context.CancelFunc
	done   chan error
}

// launch runs Main's body with args plus --socket and --state in a temp dir
// and waits until it serves.
func launch(t *testing.T, state string, args ...string) *launched {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "c.sock")
	l := &launched{echo: &echo{}, done: make(chan error, 1), client: Dial(sock, 5*time.Second)}
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	args = append([]string{"--socket", sock, "--state", state}, args...)
	go func() {
		l.done <- run(ctx, "mandate-echo", args, func(_ context.Context, env *Env, cfg testConfig) (Connector, error) {
			l.echo.env, l.echo.cfg = env, cfg
			close(started)
			return l.echo, nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-l.done
	})
	select {
	case <-started:
	case err := <-l.done:
		l.done <- err
		t.Fatalf("exited before start: %v", err)
	}
	for i := 0; ; i++ {
		_, err := l.client.Spec(context.Background())
		if err == nil {
			return l
		}
		select {
		case err := <-l.done:
			l.done <- err
			t.Fatalf("exited before serving: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if i == 200 {
			t.Fatal(err)
		}
	}
}

func (l *launched) stop(t *testing.T) error {
	t.Helper()
	l.cancel()
	err := <-l.done
	l.done <- err
	return err
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runOnce(args []string) error {
	return run(context.Background(), "mandate-echo", args, func(context.Context, *Env, testConfig) (Connector, error) {
		return nil, errors.New("start must not run")
	})
}

func approve(t *testing.T, key *approvaltest.Authenticator) approval.Approval {
	t.Helper()
	b, err := approval.Encode(approval.Record{
		ID: "p1", Connector: "echo", Op: "send",
		Args: map[string]string{"text": "hi"}, Display: []approval.Field{{Label: "Text", Value: "hi"}},
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return key.Approve(b, time.Now())
}

func TestMainServesWithConfigKeysAndSecrets(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	key := approvaltest.New(t, testRPID, testOrigin)
	args := []string{"--config", writeFile(t, "config.json", `{"greeting":"hello"}`), "--rp-id", testRPID, "--origin", testOrigin}
	l := launch(t, state, args...)

	if l.echo.cfg.Greeting != "hello" {
		t.Errorf("config = %+v", l.echo.cfg)
	}
	if got := l.echo.env.DataPath(); got != filepath.Join(state, "data.db") {
		t.Errorf("DataPath = %s", got)
	}
	fi, err := os.Stat(filepath.Join(state, "private"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("private dir: %v %v", fi, err)
	}
	secret := l.echo.env.Secret("token")
	if b, err := secret.Load(); b != nil || err != nil {
		t.Errorf("unset secret = %q, %v", b, err)
	}
	if err := secret.Store([]byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(state, "private", "token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", fi, err)
	}

	spec, err := l.client.Spec(context.Background())
	if err != nil || spec.Protocol != ProtocolVersion {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	log := approvaltest.Keys(t, key).Log()
	if _, err := l.client.PutKeys(context.Background(), log); err != nil {
		t.Fatalf("put keys: %v", err)
	}
	a := approve(t, key)
	if res, err := l.client.Execute(context.Background(), a); err != nil || res.Status != Done {
		t.Fatalf("execute: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(state, "private", "seen.json")); err != nil {
		t.Fatalf("seen log: %v", err)
	}
	if err := l.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if l.echo.sent != 1 || !l.echo.closed {
		t.Fatalf("sent %d, closed %v", l.echo.sent, l.echo.closed)
	}

	// Keys and secrets survive a restart, and only an extension replaces the keys.
	l = launch(t, state, args...)
	if b, err := l.echo.env.Secret("token").Load(); string(b) != "s3cret" || err != nil {
		t.Errorf("secret after restart = %q, %v", b, err)
	}
	if err := l.echo.env.Secret("token").Delete(); err != nil {
		t.Fatal(err)
	}
	if b, err := l.echo.env.Secret("token").Load(); b != nil || err != nil {
		t.Errorf("deleted secret = %q, %v", b, err)
	}
	if h, err := l.client.Keys(context.Background()); err != nil || h.Length != len(log) {
		t.Fatalf("keys after restart = %+v, %v", h, err)
	}
	divergent := approvaltest.Keys(t, approvaltest.New(t, testRPID, testOrigin)).Log()
	if _, err := l.client.PutKeys(context.Background(), divergent); err == nil {
		t.Fatal("accepted a divergent key log")
	} else if e, ok := errors.AsType[*Error](err); !ok || e.Status != http.StatusConflict {
		t.Fatalf("put divergent keys: %v", err)
	}
}

func TestMainWithoutKeysRefusesWrites(t *testing.T) {
	t.Parallel()
	key := approvaltest.New(t, testRPID, testOrigin)
	for name, args := range map[string][]string{
		"no rp-id or origin": nil,
		"empty key log":      {"--rp-id", testRPID, "--origin", testOrigin},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := launch(t, t.TempDir(), args...)
			_, err := l.client.Execute(context.Background(), approve(t, key))
			if e, ok := errors.AsType[*Error](err); !ok || e.Status != http.StatusForbidden {
				t.Fatalf("execute: %v", err)
			}
			if l.echo.sent != 0 {
				t.Fatal("write ran without trusted keys")
			}
		})
	}
}

func TestMainStartupErrors(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	broken := t.TempDir()
	if err := os.Mkdir(filepath.Join(broken, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "private", "keys.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "c.sock")
	base := []string{"--socket", sock, "--state", state}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no socket":      {[]string{"--state", state}, "--socket and --state are required"},
		"no state":       {[]string{"--socket", sock}, "--socket and --state are required"},
		"rp-id alone":    {append(base, "--rp-id", testRPID), "--rp-id and --origin go together"},
		"origin alone":   {append(base, "--origin", testOrigin), "--rp-id and --origin go together"},
		"broken key log": {[]string{"--socket", sock, "--state", broken, "--rp-id", testRPID, "--origin", testOrigin}, "open key log"},
		"invalid config": {append(base, "--config", writeFile(t, "c.json", `{"greeting":"bad"}`)), "greeting must not be bad"},
		"missing config": {append(base, "--config", filepath.Join(state, "nope.json")), "read config"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := runOnce(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := os.Stat(sock); err == nil {
		t.Fatal("a failed start left a socket")
	}
}

func TestCheckConfigTouchesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		config string
		want   string
	}{
		"valid":         {`{"greeting":"hi"}`, ""},
		"unknown key":   {`{"greting":"hi"}`, "unknown field"},
		"invalid":       {`{"greeting":"bad"}`, "greeting must not be bad"},
		"trailing data": {`{} {}`, "trailing data"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := filepath.Join(t.TempDir(), "state")
			sock := filepath.Join(t.TempDir(), "c.sock")
			err := runOnce([]string{
				"--check-config", writeFile(t, "c.json", tc.config),
				"--socket", sock, "--state", state,
			})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, p := range []string{state, sock} {
				if _, err := os.Stat(p); err == nil {
					t.Fatalf("--check-config created %s", p)
				}
			}
		})
	}
}

func TestFailExitsWithItsError(t *testing.T) {
	t.Parallel()
	l := launch(t, t.TempDir())
	fault := errors.New("cannot persist state")
	l.echo.env.Fail(fault)
	if err := <-l.done; !errors.Is(err, fault) {
		t.Fatalf("run = %v, want %v", err, fault)
	}
	l.done <- nil
	if !l.echo.closed {
		t.Fatal("connector not closed after Fail")
	}
}
