package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/adeci/mandate/approval"
)

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
//	NAME --socket PATH --state DIR [--config FILE] [--rp-id ID --origin URL]
//	NAME --check-config FILE
//
// It decodes --config strictly into C (no file decodes "{}"), runs its
// Validate method if it has one, then calls start and serves the protocol
// until SIGINT or SIGTERM. --check-config only decodes and validates.
// Trusted keys live in DIR/private/keys.json and change through PUT /keys.
// Without --rp-id and --origin there are none and writes are refused. If the
// Connector is an io.Closer, it is closed after serving stops.
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
	if (*rpID == "") != (*origin == "") {
		return errors.New("--rp-id and --origin go together")
	}
	cfg, err := loadConfig[C](*configPath)
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
	srv := &Server{Log: env.Log}
	if *rpID == "" {
		env.Log.Warn("no --rp-id and --origin; writes are refused")
	} else {
		if srv.Keys, err = approval.OpenKeys(filepath.Join(env.PrivateDir(), "keys.json"), *rpID, *origin); err != nil {
			return fmt.Errorf("open key log: %w", err)
		}
		if srv.Seen, err = approval.OpenSeen(filepath.Join(env.PrivateDir(), "seen.json")); err != nil {
			return fmt.Errorf("open seen log: %w", err)
		}
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
