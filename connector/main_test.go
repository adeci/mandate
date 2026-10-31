package connector

import (
	"context"
	"encoding/json"
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
func launch(t *testing.T, state, credDir string, args ...string) *launched {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "c.sock")
	l := &launched{echo: &echo{}, done: make(chan error, 1), client: Dial(sock, 5*time.Second)}
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	args = append([]string{"--socket", sock, "--state", state}, args...)
	go func() {
		l.done <- run(ctx, "mandate-echo", args, credDir, func(_ context.Context, env *Env, cfg testConfig) (Connector, error) {
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

func approvers(t *testing.T, key *approvaltest.Authenticator) string {
	t.Helper()
	b, err := json.Marshal([]approval.Approver{key.Approver("phone")})
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, "approvers.json", string(b))
}

func runOnce(args []string) error {
	return run(context.Background(), "mandate-echo", args, "", func(context.Context, *Env, testConfig) (Connector, error) {
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

func TestMainServesWithConfigApproversAndCredentials(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	creds := t.TempDir()
	if err := os.WriteFile(filepath.Join(creds, "token"), []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := approvaltest.New(t, testRPID, testOrigin)
	l := launch(t, state, creds,
		"--config", writeFile(t, "config.json", `{"greeting":"hello"}`),
		"--approvers", approvers(t, key), "--rp-id", testRPID, "--origin", testOrigin)

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
	if tok, err := l.echo.env.Credential("token"); err != nil || string(tok) != "s3cret" {
		t.Errorf("Credential = %q, %v", tok, err)
	}
	for _, name := range []string{"missing", "../token", "."} {
		if _, err := l.echo.env.Credential(name); err == nil {
			t.Errorf("Credential(%q) succeeded", name)
		}
	}

	spec, err := l.client.Spec(context.Background())
	if err != nil || spec.Protocol != ProtocolVersion {
		t.Fatalf("spec = %+v, %v", spec, err)
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
}

func TestMainWithoutApproversRefusesWrites(t *testing.T) {
	t.Parallel()
	key := approvaltest.New(t, testRPID, testOrigin)
	for name, args := range map[string][]string{
		"no flag":      nil,
		"missing file": {"--approvers", filepath.Join(t.TempDir(), "approvers.json")},
		"empty file":   {"--approvers", writeFile(t, "approvers.json", "")},
		"empty list":   {"--approvers", writeFile(t, "approvers.json", "[]")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := launch(t, t.TempDir(), "", args...)
			_, err := l.client.Execute(context.Background(), approve(t, key))
			if e, ok := errors.AsType[*Error](err); !ok || e.Status != http.StatusForbidden {
				t.Fatalf("execute: %v", err)
			}
			if _, err := l.echo.env.Credential("token"); err == nil || !strings.Contains(err.Error(), "CREDENTIALS_DIRECTORY") {
				t.Errorf("Credential without a credentials dir: %v", err)
			}
			if l.echo.sent != 0 {
				t.Fatal("write ran without approvers")
			}
		})
	}
}

func TestMainStartupErrors(t *testing.T) {
	t.Parallel()
	key := approvaltest.New(t, testRPID, testOrigin)
	enrolled := approvers(t, key)
	state := t.TempDir()
	sock := filepath.Join(t.TempDir(), "c.sock")
	base := []string{"--socket", sock, "--state", state}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no socket":           {[]string{"--state", state}, "--socket and --state are required"},
		"no state":            {[]string{"--socket", sock}, "--socket and --state are required"},
		"approvers no rp-id":  {append(base, "--approvers", enrolled, "--origin", testOrigin), "--rp-id and --origin are required"},
		"approvers no origin": {append(base, "--approvers", enrolled, "--rp-id", testRPID), "--rp-id and --origin are required"},
		"broken approvers":    {append(base, "--approvers", writeFile(t, "a.json", "{")), "parse approvers"},
		"unknown config key":  {append(base, "--config", writeFile(t, "c.json", `{"greting":"hi"}`)), "unknown field"},
		"invalid config":      {append(base, "--config", writeFile(t, "c.json", `{"greeting":"bad"}`)), "greeting must not be bad"},
		"missing config":      {append(base, "--config", filepath.Join(state, "nope.json")), "read config"},
		"stray argument":      {append(base, "serve"), "unexpected arguments"},
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
		"empty object":  {`{}`, ""},
		"unknown key":   {`{"greting":"hi"}`, "unknown field"},
		"wrong type":    {`{"greeting":1}`, "cannot unmarshal"},
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
				"--approvers", filepath.Join(state, "broken"),
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
	l := launch(t, t.TempDir(), "")
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
