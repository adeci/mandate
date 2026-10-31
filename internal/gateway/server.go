// Package gateway is the agent API: read-only SQL over filtered views of the
// source databases, connector reads, write proposals, and raw reads.
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const (
	maxSQLBytes     = 16 << 10
	maxRequestBytes = 64 << 10
)

type Config struct {
	Listen       string
	TokenFile    string
	ExposureFile string
	Sources      map[string]string // source name -> data.db path
	MaxRows      int
	Timeout      time.Duration
	// Connector name -> socket. A connector without a source of the same
	// name may not touch containers at all.
	Connectors map[string]string
	// Empty disables /v1/actions.
	ConsoleSocket string
	// ConsoleURL is where the owner fixes connectors; it goes into errors
	// that ask the agent to fetch them.
	ConsoleURL string
}

// The name prefixes the source's views and keys its exposure.toml section.
type sourceFlag map[string]string

func (s sourceFlag) String() string { return fmt.Sprint(map[string]string(s)) }

func (s sourceFlag) Set(v string) error {
	name, path, ok := strings.Cut(v, "=")
	if !ok || path == "" {
		return fmt.Errorf("want name=path, got %q", v)
	}
	if !validName.MatchString(name) {
		return fmt.Errorf("source name %q must match %s", name, validName)
	}
	if name == "auth" {
		return errors.New(`source name "auth" is taken by the [auth] section of exposure.toml`)
	}
	s[name] = path
	return nil
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

type connectorFlag map[string]string

func (c connectorFlag) String() string { return fmt.Sprint(map[string]string(c)) }

func (c connectorFlag) Set(v string) error {
	name, path, ok := strings.Cut(v, "=")
	if !ok || path == "" {
		return fmt.Errorf("want name=path, got %q", v)
	}
	if !validName.MatchString(name) {
		return fmt.Errorf("connector name %q must match %s", name, validName)
	}
	c[name] = path
	return nil
}

func Command(ctx context.Context, args []string) error {
	cfg := Config{Sources: map[string]string{}, Connectors: map[string]string{}}
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8780", "listen address")
	fs.StringVar(&cfg.TokenFile, "token-file", "", "file containing the bearer token (required)")
	fs.StringVar(&cfg.ExposureFile, "exposure", "/var/lib/mandate-gateway/exposure.toml", "exposure config (missing = nothing hidden)")
	fs.Var(sourceFlag(cfg.Sources), "source", "source database as name=path (repeatable)")
	fs.IntVar(&cfg.MaxRows, "max-rows", 5000, "maximum rows per query")
	fs.DurationVar(&cfg.Timeout, "timeout", 5*time.Second, "maximum query execution time")
	fs.Var(connectorFlag(cfg.Connectors), "connector", "connector socket as name=path (repeatable; a source of the same name filters it)")
	fs.StringVar(&cfg.ConsoleSocket, "console-socket", "", "console proposal socket (enables /v1/actions)")
	fs.StringVar(&cfg.ConsoleURL, "console-url", "", "console base URL, shown to agents when a connector needs the owner")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.TokenFile == "" {
		return errors.New("--token-file is required")
	}
	return Serve(ctx, cfg)
}

func Serve(ctx context.Context, cfg Config) error {
	raw, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 {
		return errors.New("token must be at least 32 characters")
	}
	h := newHandler(cfg, token)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Fits the longest answer: an action long-poll plus its last fetch.
		WriteTimeout: maxWait + callTimeout + 10*time.Second,
		IdleTimeout:  60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	slog.Info("gateway listening", "addr", ln.Addr().String(), "sources", cfg.Sources, "connectors", cfg.Connectors,
		"console", cfg.ConsoleSocket != "")
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
	tokenHash  [32]byte
	policy     *policyLoader
	mux        *http.ServeMux
	connectors *connectors
	console    *proposal.Client // nil when disabled
}

func newHandler(cfg Config, token string) *handler {
	h := &handler{
		cfg:        cfg,
		tokenHash:  sha256.Sum256([]byte(token)),
		policy:     &policyLoader{path: cfg.ExposureFile, sources: slices.Sorted(maps.Keys(cfg.Sources))},
		mux:        http.NewServeMux(),
		connectors: newConnectors(cfg.Connectors),
	}
	if cfg.ConsoleSocket != "" {
		h.console = proposal.Dial(cfg.ConsoleSocket)
	}
	h.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	h.mux.HandleFunc("GET /v1/schema", h.authed(h.schema))
	h.mux.HandleFunc("POST /v1/query", h.authed(h.query))
	h.mux.HandleFunc("POST /v1/read/{connector}/{op}", h.authed(h.read))
	h.mux.HandleFunc("POST /v1/actions", h.authed(h.propose))
	h.mux.HandleFunc("GET /v1/actions/{id}", h.authed(h.status))
	h.mux.HandleFunc("/v1/forward/{connector}/{path...}", h.authed(h.forward))
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *handler) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], h.tokenHash[:]) != 1 {
			slog.Warn("unauthorized", "remote", r.RemoteAddr, "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func (h *handler) open() (*queryConn, error) {
	pol, err := h.policy.get()
	if err != nil {
		return nil, err
	}
	return openQueryConn(h.cfg.Sources, pol)
}

func (h *handler) schema(w http.ResponseWriter, r *http.Request) {
	q, err := h.open()
	if err != nil {
		slog.Error("open query connection", "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable")
		return
	}
	defer q.close()
	views, err := q.describe()
	if err != nil {
		slog.Error("describe", "err", err)
		writeError(w, http.StatusInternalServerError, "describe failed")
		return
	}
	connector.WriteJSON(w, http.StatusOK, map[string]any{
		"views":      views,
		"connectors": h.connectors.list(r.Context()),
		"limits": map[string]any{
			"max_rows":   h.cfg.MaxRows,
			"timeout_ms": h.cfg.Timeout.Milliseconds(),
		},
	})
}

type queryRequest struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
}

func (h *handler) query(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req queryRequest
	if !connector.DecodeJSON(w, r, &req, maxRequestBytes) {
		return
	}
	if strings.TrimSpace(req.SQL) == "" || len(req.SQL) > maxSQLBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("sql must be 1..%d bytes", maxSQLBytes))
		return
	}

	q, err := h.open()
	if err != nil {
		slog.Error("open query connection", "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable")
		return
	}
	defer q.close()

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	res, err := q.run(ctx, req.SQL, req.Params, h.cfg.MaxRows)

	attrs := []any{"remote", r.RemoteAddr, "sql", req.SQL, "params", len(req.Params), "duration_ms", time.Since(start).Milliseconds()}
	if err != nil {
		slog.Info("query", append(attrs, "err", err.Error())...)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Info("query", append(attrs, "rows", len(res.Rows), "truncated", res.Truncated)...)
	connector.WriteJSON(w, http.StatusOK, res)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	connector.WriteError(w, &connector.Error{Status: status, Message: msg})
}
