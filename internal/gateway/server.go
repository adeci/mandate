// Package gateway serves read-only SQL over filtered views of the source
// databases.
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
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
	// Empty disables the /v1/discord/* endpoints.
	DiscordSocket string
}

type sourceFlag map[string]string

func (s sourceFlag) String() string { return fmt.Sprint(map[string]string(s)) }

func (s sourceFlag) Set(v string) error {
	name, path, ok := strings.Cut(v, "=")
	if !ok || path == "" {
		return fmt.Errorf("want name=path, got %q", v)
	}
	known := false
	for _, k := range knownSources {
		known = known || k == name
	}
	if !known {
		return fmt.Errorf("unknown source %q (known: %s)", name, strings.Join(knownSources, ", "))
	}
	s[name] = path
	return nil
}

func Command(ctx context.Context, args []string) error {
	cfg := Config{Sources: map[string]string{}}
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8780", "listen address")
	fs.StringVar(&cfg.TokenFile, "token-file", "", "file containing the bearer token (required)")
	fs.StringVar(&cfg.ExposureFile, "exposure", "/var/lib/mandate-gateway/exposure.toml", "exposure config (missing = nothing hidden)")
	fs.Var(sourceFlag(cfg.Sources), "source", "source database as name=path (repeatable; names: sms, whatsapp, discord)")
	fs.IntVar(&cfg.MaxRows, "max-rows", 5000, "maximum rows per query")
	fs.DurationVar(&cfg.Timeout, "timeout", 5*time.Second, "maximum query execution time")
	fs.StringVar(&cfg.DiscordSocket, "discord-socket", "", "Discord service Unix socket (enables /v1/discord/*)")
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
		WriteTimeout:      discordCallTimeout + 10*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	slog.Info("gateway listening", "addr", ln.Addr().String(), "sources", cfg.Sources)
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
	cfg       Config
	tokenHash [32]byte
	policy    *policyLoader
	mux       *http.ServeMux
}

func newHandler(cfg Config, token string) *handler {
	h := &handler{
		cfg:       cfg,
		tokenHash: sha256.Sum256([]byte(token)),
		policy:    &policyLoader{path: cfg.ExposureFile},
		mux:       http.NewServeMux(),
	}
	h.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	h.mux.HandleFunc("GET /v1/schema", h.authed(h.schema))
	h.mux.HandleFunc("POST /v1/query", h.authed(h.query))
	if cfg.DiscordSocket != "" {
		newDiscordProxy(h, cfg.DiscordSocket).register(h.mux)
	} else {
		h.mux.HandleFunc("POST /v1/discord/", h.authed(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "discord is not enabled on this gateway")
		}))
	}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"views": views,
		"usage": "POST /v1/query with JSON {\"sql\": \"SELECT ...\", \"params\": [...]}. One read-only SELECT over the listed views; positional params bind to ?1, ?2, ...",
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
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
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
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
