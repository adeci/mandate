package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/adeci/mandate/approval"
)

// Env is what Main hands a connector at start.
type Env struct {
	// StateDir is the connector's state directory. A source keeps its
	// messages in DataPath, readable by the gateway; anything secret goes
	// in PrivateDir.
	StateDir string
	Log      *slog.Logger

	credDir string
	fail    context.CancelCauseFunc
}

func (e *Env) DataPath() string { return filepath.Join(e.StateDir, "data.db") }

// PrivateDir exists with mode 0700.
func (e *Env) PrivateDir() string { return filepath.Join(e.StateDir, "private") }

// Credential reads $CREDENTIALS_DIRECTORY/name with surrounding whitespace
// trimmed. Errors never include the contents.
func (e *Env) Credential(name string) ([]byte, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, fmt.Errorf("credential name %q must be a plain file name", name)
	}
	if e.credDir == "" {
		return nil, fmt.Errorf("credential %s: $CREDENTIALS_DIRECTORY is not set", name)
	}
	b, err := os.ReadFile(filepath.Join(e.credDir, name))
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", name, err)
	}
	return bytes.TrimSpace(b), nil
}

// Fail stops the connector after a fault only a restart can clear; Main
// exits non-zero with err. Conditions the owner must fix belong in Health.
func (e *Env) Fail(err error) { e.fail(err) }

// Main runs a connector program:
//
//	NAME --socket PATH --state DIR [--config FILE]
//	     [--approvers FILE --rp-id ID --origin URL]
//	NAME --check-config FILE
//
// It decodes --config strictly into C (no file decodes "{}"), runs its
// Validate method if it has one, then calls start and serves the protocol
// until SIGINT or SIGTERM. --check-config only decodes and validates.
// Without approvers the Server has no Verifier and refuses every write. If
// the Connector is an io.Closer, it is closed after serving stops.
func Main[C any](start func(ctx context.Context, env *Env, cfg C) (Connector, error)) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, filepath.Base(os.Args[0]), os.Args[1:], os.Getenv("CREDENTIALS_DIRECTORY"), start)
	stop()
	switch {
	case errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	case err != nil:
		fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
		os.Exit(1)
	}
}

func run[C any](ctx context.Context, name string, args []string, credDir string, start func(context.Context, *Env, C) (Connector, error)) (err error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	socket := flags.String("socket", "", "serve the connector protocol on this Unix socket (required)")
	stateDir := flags.String("state", "", "state directory (required)")
	configPath := flags.String("config", "", "JSON settings")
	checkPath := flags.String("check-config", "", "only validate this settings file, then exit")
	approversPath := flags.String("approvers", "", "enrolled passkeys (JSON); missing or empty refuses all writes")
	rpID := flags.String("rp-id", "", "WebAuthn relying party ID of the console")
	origin := flags.String("origin", "", "origin of the console, e.g. https://mandate.example.com")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}
	if *checkPath != "" {
		_, err := loadConfig[C](*checkPath)
		return err
	}
	if *socket == "" || *stateDir == "" {
		return errors.New("--socket and --state are required")
	}
	cfg, err := loadConfig[C](*configPath)
	if err != nil {
		return err
	}
	verifier, err := loadVerifier(*approversPath, *rpID, *origin)
	if err != nil {
		return err
	}
	env := &Env{StateDir: *stateDir, Log: slog.Default(), credDir: credDir}
	if err := os.MkdirAll(env.PrivateDir(), 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(env.PrivateDir(), 0o700); err != nil {
		return err
	}
	srv := &Server{Verifier: verifier, Log: env.Log}
	if verifier == nil {
		env.Log.Warn("no approvers; writes are refused")
	} else if srv.Seen, err = approval.OpenSeen(filepath.Join(env.PrivateDir(), "seen.json")); err != nil {
		return fmt.Errorf("open seen log: %w", err)
	}

	ctx, env.fail = context.WithCancelCause(ctx)
	defer env.fail(nil)
	if srv.Connector, err = start(ctx, env, cfg); err != nil {
		return err
	}
	if c, ok := srv.Connector.(io.Closer); ok {
		defer func() { err = errors.Join(err, c.Close()) }()
	}
	env.Log.Info("serving", "socket", *socket)
	if err := srv.Serve(ctx, *socket); err != nil {
		return err
	}
	if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

func loadConfig[C any](path string) (C, error) {
	var cfg C
	b := []byte("{}")
	if path != "" {
		var err error
		if b, err = os.ReadFile(path); err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return cfg, errors.New("parse config: trailing data")
	}
	if v, ok := any(&cfg).(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return cfg, fmt.Errorf("config: %w", err)
		}
	}
	return cfg, nil
}

// loadVerifier returns nil unless approvers are enrolled. The console writes
// the file on first enrollment, so a missing or empty one is not an error.
func loadVerifier(path, rpID, origin string) (*approval.Verifier, error) {
	if path == "" {
		return nil, nil
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && fi.Size() == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read approvers: %w", err)
	}
	approvers, err := approval.LoadApprovers(path)
	if err != nil {
		return nil, err
	}
	if len(approvers) == 0 {
		return nil, nil
	}
	if rpID == "" || origin == "" {
		return nil, errors.New("--rp-id and --origin are required when approvers are enrolled")
	}
	return &approval.Verifier{RPID: rpID, Origin: origin, Approvers: approvers}, nil
}
