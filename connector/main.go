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

	"github.com/adeci/mandate/internal/peer"
)

// Env is what Main gives a connector's start function.
type Env struct {
	// Name is the name mandate configured the connector under.
	Name     string
	StateDir string
	Log      *slog.Logger

	fail context.CancelCauseFunc
}

// DataPath is the store, readable by the gateway.
func (e *Env) DataPath() string { return filepath.Join(e.StateDir, "data.db") }

// PrivateDir exists with mode 0700 and holds the session and credentials.
func (e *Env) PrivateDir() string { return filepath.Join(e.StateDir, "private") }

// Secret is the file PrivateDir/name, which name must be a plain file name.
func (e *Env) Secret(name string) Secret { return Secret{path: filepath.Join(e.PrivateDir(), name)} }

// Fail stops the connector after a fault only a restart can clear; Main
// exits non-zero with err. Conditions the owner must fix belong in Health.
func (e *Env) Fail(err error) { e.fail(err) }

// Secret holds a value the owner gives Setup, such as an API token, in a
// file only the connector can read.
type Secret struct{ path string }

// Load returns the stored value, or nil if there is none.
func (s Secret) Load() ([]byte, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// Store replaces the value with b, mode 0600.
func (s Secret) Store(b []byte) error { return writeFile(s.path, b) }

// Delete forgets the value; there being none is not an error.
func (s Secret) Delete() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Main runs a connector program:
//
//	PROGRAM --name NAME --socket PATH --state DIR [--config FILE] --gateway-group GROUP --owner-group GROUP
//	PROGRAM --check-config FILE
//
// It decodes the JSON config strictly into C (no --config decodes "{}") and
// runs its Validate method if it has one, then calls start and serves until
// SIGTERM. If the Connector is an io.Closer, it is closed after serving.
func Main[C any](start func(ctx context.Context, env *Env, cfg C) (Connector, error)) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, filepath.Base(os.Args[0]), os.Args[1:], start)
	stop()
	switch {
	case errors.Is(err, flag.ErrHelp):
	case err != nil:
		fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
		os.Exit(1)
	}
}

func run[C any](ctx context.Context, program string, args []string, start func(context.Context, *Env, C) (Connector, error)) (err error) {
	flags := flag.NewFlagSet(program, flag.ContinueOnError)
	name := flags.String("name", "", "the name mandate configured this connector under")
	socket := flags.String("socket", "", "serve on this Unix socket")
	stateDir := flags.String("state", "", "state directory")
	configPath := flags.String("config", "", "JSON config")
	checkPath := flags.String("check-config", "", "only check this config file")
	gatewayGroup := flags.String("gateway-group", "", "primary group of mandate's gateway")
	ownerGroup := flags.String("owner-group", "", "primary group of mandate's owner side")
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
	if *name == "" || *socket == "" || *stateDir == "" || *gatewayGroup == "" || *ownerGroup == "" {
		return errors.New("--name, --socket, --state, --gateway-group and --owner-group are required")
	}
	cfg, err := loadConfig[C](*configPath)
	if err != nil {
		return err
	}
	gatewayGID, err := peer.LookupGID(*gatewayGroup)
	if err != nil {
		return fmt.Errorf("--gateway-group: %w", err)
	}
	ownerGID, err := peer.LookupGID(*ownerGroup)
	if err != nil {
		return fmt.Errorf("--owner-group: %w", err)
	}
	env := &Env{Name: *name, StateDir: *stateDir, Log: slog.Default()}
	if err := os.MkdirAll(env.PrivateDir(), 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(env.PrivateDir(), 0o700); err != nil {
		return err
	}

	// The connector's context outlives the signal, so writes in flight
	// finish; Fail stops serving too.
	connCtx, fail := context.WithCancelCause(context.Background())
	env.fail = fail
	defer fail(nil)
	c, err := start(connCtx, env, cfg)
	if err != nil {
		return err
	}
	if cl, ok := c.(io.Closer); ok {
		defer func() { err = errors.Join(err, cl.Close()) }()
	}
	srv, err := NewServer(c, *name, *stateDir, gatewayGID, ownerGID, env.Log)
	if err != nil {
		return err
	}
	ln, err := peer.Listen(*socket)
	if err != nil {
		return err
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(connCtx, cancel)()
	env.Log.Info("serving", "socket", *socket)
	if err := srv.Serve(serveCtx, ln); err != nil {
		return err
	}
	if cause := context.Cause(connCtx); cause != nil {
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
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("parse config %s: trailing data", path)
	}
	if v, ok := any(&cfg).(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return cfg, fmt.Errorf("config %s: %w", path, err)
		}
	}
	return cfg, nil
}
