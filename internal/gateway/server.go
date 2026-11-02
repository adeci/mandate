// Package gateway is the agent API: fixed reads over filtered views of the
// source databases, connector ops, write proposals, and raw reads.
package gateway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const maxRequestBytes = 64 << 10

type Config struct {
	Listen       string
	ExposureFile string
	// StateDir holds keys.json, the key log copied from the console, and
	// access.json, the IDs that stay revoked.
	StateDir string
	// RPID and Origin are the console's WebAuthn relying party.
	RPID, Origin string
	Sources      map[string]string // source name -> data.db path
	Timeout      time.Duration
	// Connector name -> socket. A connector without a source of the same
	// name may not touch containers at all.
	Connectors map[string]string
	// ConsoleSocket serves agents, grants and proposals.
	ConsoleSocket string
	// ConsoleURL is where the owner fixes connectors; it goes into errors
	// that ask the agent to fetch them.
	ConsoleURL string
	// MaxMedia is the largest file agents may fetch, in bytes.
	MaxMedia int64
}

// The name keys the source's exposure.toml section.
type sourceFlag map[string]string

func (s sourceFlag) String() string { return fmt.Sprint(map[string]string(s)) }

func (s sourceFlag) Set(v string) error {
	name, path, err := namedPath(v)
	if err != nil {
		return err
	}
	if name == "auth" {
		return errors.New(`source name "auth" is taken by the [auth] section of exposure.toml`)
	}
	s[name] = path
	return nil
}

type connectorFlag map[string]string

func (c connectorFlag) String() string { return fmt.Sprint(map[string]string(c)) }

func (c connectorFlag) Set(v string) error {
	name, path, err := namedPath(v)
	if err != nil {
		return err
	}
	c[name] = path
	return nil
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// "proposals" and "access" would be shadowed by /v1/proposals/{id} and
// /v1/access/requests.
func namedPath(v string) (name, path string, err error) {
	name, path, ok := strings.Cut(v, "=")
	switch {
	case !ok || path == "":
		return "", "", fmt.Errorf("want name=path, got %q", v)
	case !validName.MatchString(name):
		return "", "", fmt.Errorf("name %q must match %s", name, validName)
	case name == "proposals" || name == "access":
		return "", "", fmt.Errorf("name %q is taken by /v1/%s", name, name)
	}
	return name, path, nil
}

func Command(ctx context.Context, args []string) error {
	cfg := Config{Sources: map[string]string{}, Connectors: map[string]string{}}
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8780", "listen address")
	fs.StringVar(&cfg.StateDir, "state", "", "state directory for the key log and revoked IDs (required)")
	fs.StringVar(&cfg.RPID, "rp-id", "", "WebAuthn relying party ID of the console (required)")
	fs.StringVar(&cfg.Origin, "origin", "", "WebAuthn origin of the console (required)")
	fs.StringVar(&cfg.ExposureFile, "exposure", "/var/lib/mandate-gateway/exposure.toml", "exposure config (missing = nothing hidden)")
	fs.Var(sourceFlag(cfg.Sources), "source", "source database as name=path (repeatable)")
	fs.DurationVar(&cfg.Timeout, "timeout", 5*time.Second, "maximum query execution time")
	fs.Var(connectorFlag(cfg.Connectors), "connector", "connector socket as name=path (repeatable; a source of the same name filters it)")
	fs.StringVar(&cfg.ConsoleSocket, "console-socket", "", "console proposal socket (required)")
	fs.StringVar(&cfg.ConsoleURL, "console-url", "", "console base URL, shown to agents when a connector needs the owner")
	fs.Int64Var(&cfg.MaxMedia, "max-media", 100<<20, "largest media file agents may fetch, in bytes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, f := range []struct{ name, v string }{
		{"state", cfg.StateDir}, {"rp-id", cfg.RPID}, {"origin", cfg.Origin}, {"console-socket", cfg.ConsoleSocket},
	} {
		if f.v == "" {
			return fmt.Errorf("--%s is required", f.name)
		}
	}
	return Serve(ctx, cfg)
}

func Serve(ctx context.Context, cfg Config) error {
	console := proposal.Dial(cfg.ConsoleSocket)
	acc, err := openAccess(cfg.StateDir, cfg.RPID, cfg.Origin, console, cfg.Connectors)
	if err != nil {
		return err
	}
	// Until the console answers, no agent is known and every token is refused.
	if err := acc.sync(ctx); err != nil {
		slog.Error("access sync", "err", err)
	}
	go acc.run(ctx)
	h := newHandler(cfg, acc)
	go h.reportReads(ctx)
	// Loading now logs a broken file or hidden IDs that match nothing.
	if _, err := h.policy.get(); err != nil {
		slog.Error("exposure", "err", err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Fits the longest answer: a proposal long-poll plus its last fetch.
		WriteTimeout: maxWait + callTimeout + 10*time.Second,
		IdleTimeout:  60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	slog.Info("gateway listening", "addr", ln.Addr().String(), "sources", cfg.Sources, "connectors", cfg.Connectors,
		"agents", len(acc.agents()))
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		return err
	}
}

type handler struct {
	cfg        Config
	access     *access
	policy     *policyLoader
	mux        *http.ServeMux
	connectors *connectors
	console    *proposal.Client
	reads      chan proposal.ReadEvent

	dropMu  sync.Mutex
	dropped map[string]int // read events dropped per agent ID

	seenMu sync.Mutex
	seen   map[string]time.Time // when each agent ID was last reported seen
}

func newHandler(cfg Config, acc *access) *handler {
	h := &handler{
		cfg:        cfg,
		access:     acc,
		policy:     &policyLoader{path: cfg.ExposureFile, sources: cfg.Sources},
		mux:        http.NewServeMux(),
		connectors: newConnectors(cfg.Connectors),
		console:    proposal.Dial(cfg.ConsoleSocket),
		reads:      make(chan proposal.ReadEvent, readQueue),
		dropped:    map[string]int{},
		seen:       map[string]time.Time{},
	}
	h.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	h.mux.HandleFunc("GET /v1/connectors", h.authed(h.list))
	h.mux.HandleFunc("GET /v1/access", h.authed(h.grants))
	h.mux.HandleFunc("POST /v1/access/requests", h.authed(h.requestAccess))
	h.mux.HandleFunc("GET /v1/access/requests/{id}", h.authed(h.requestStatus))
	h.mux.HandleFunc("GET /v1/proposals/{id}", h.authed(h.status))
	h.mux.HandleFunc("GET /v1/{connector}/{verb}", h.authed(h.verb))
	h.mux.HandleFunc("POST /v1/{connector}/{op}", h.authed(h.op))
	h.mux.HandleFunc("/v1/forward/{connector}/{path...}", h.authed(h.forward))
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *handler) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var ag *agent
		if ok {
			ag = h.access.find(token)
		}
		if ag == nil {
			slog.Warn("unauthorized", "remote", r.RemoteAddr, "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.markSeen(ag.ID)
		next(w, r.WithContext(withAgent(r.Context(), ag)))
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	connector.WriteError(w, &connector.Error{Status: status, Message: msg})
}
