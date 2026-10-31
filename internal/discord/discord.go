// Package discord is the on-demand, read-only Discord service behind the gateway.
//
// Account integrity comes first: every request looks like Chrome on Linux and
// is paced like a person. Only GETs are sent, and nothing at startup.
package discord

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	_ "time/tzdata" // --timezone must resolve on hosts without zoneinfo

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"github.com/adeci/mandate/internal/discordapi"
	"github.com/adeci/mandate/internal/exitcode"
	"github.com/adeci/mandate/internal/store"
)

const usage = "usage: mandate discord serve --data PATH --state PATH --token-file PATH --listen PATH [--timezone TZ] [--active-hours HH-HH]"

const maxCallBody = 64 << 10

func Command(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; %s", args[0], usage)
	}
}

func defaultTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" && !strings.HasPrefix(tz, ":") {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	return "America/New_York"
}

func serve(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("discord serve", flag.ContinueOnError)
	dataPath := flags.String("data", "", "path to the discord store database (required)")
	statePath := flags.String("state", "", "path to the private state file (required)")
	tokenFile := flags.String("token-file", "", "path to a file holding the Discord user token (required)")
	listen := flags.String("listen", "", "Unix socket to serve the gateway on (required)")
	timezone := flags.String("timezone", defaultTimezone(), "IANA time zone for X-Discord-Timezone and active hours")
	hoursFlag := flags.String("active-hours", "08-24", "local hours HH-HH in which requests may be sent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dataPath == "" || *statePath == "" || *tokenFile == "" || *listen == "" {
		return errors.New("--data, --state, --token-file, and --listen are required; " + usage)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q; %s", flags.Args(), usage)
	}
	loc, err := time.LoadLocation(*timezone)
	if err != nil {
		return fmt.Errorf("--timezone: %w", err)
	}
	hours, err := parseActiveHours(*hoursFlag, loc)
	if err != nil {
		return err
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, *dataPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	client, jar, err := newBrowserClient()
	if err != nil {
		return err
	}
	return run(ctx, config{
		store:     st,
		statePath: *statePath,
		listen:    *listen,
		token:     token,
		timezone:  *timezone,
		hours:     hours,
		transport: client,
		jar:       jar,
		apiBase:   defaultAPIBase,
		webBase:   defaultWebBase,
		clock:     realClock{},
		rng:       globalRand{},
		log:       slog.New(slog.NewTextHandler(os.Stderr, nil)).With("source", "discord"),
	})
}

// Chrome 152's TLS/HTTP2 fingerprint. Redirects are not followed: Discord
// never redirects a logged-in browser.
func newBrowserClient() (tls_client.HttpClient, tls_client.CookieJar, error) {
	jar := tls_client.NewCookieJar()
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithClientProfile(profiles.Chrome_152),
		tls_client.WithRandomTLSExtensionOrder(),
		tls_client.WithCookieJar(jar),
		tls_client.WithTimeoutSeconds(int(requestTimeout/time.Second)),
		tls_client.WithNotFollowRedirects(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create http client: %w", err)
	}
	return client, jar, nil
}

// Errors never include the file's contents.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	switch {
	case token == "":
		return "", fmt.Errorf("token file %s is empty", path)
	case strings.HasPrefix(token, "Bot ") || strings.HasPrefix(token, "Bearer "):
		return "", fmt.Errorf("token file %s must hold a bare user token (no Bot/Bearer prefix)", path)
	case strings.ContainsAny(token, " \t\r\n"):
		return "", fmt.Errorf("token file %s holds more than one token", path)
	}
	return token, nil
}

type config struct {
	store            *store.Store
	statePath        string
	listen           string
	token            string
	timezone         string
	hours            activeHours
	transport        transport
	jar              fhttp.CookieJar // nil in tests
	apiBase, webBase string
	clock            clock
	rng              rng
	log              *slog.Logger
}

func run(ctx context.Context, cfg config) error {
	st, err := loadState(cfg.statePath)
	if err != nil {
		return err
	}
	st.prune(cfg.clock.Now())
	st.ensureIdentity()
	if err := st.save(cfg.statePath); err != nil {
		return err
	}
	up := &upstream{
		tr:        cfg.transport,
		jar:       cfg.jar,
		token:     cfg.token,
		timezone:  cfg.timezone,
		apiBase:   cfg.apiBase,
		webBase:   cfg.webBase,
		clock:     cfg.clock,
		pace:      newPacer(st, cfg.rng, cfg.hours),
		st:        st,
		statePath: cfg.statePath,
		log:       cfg.log,
	}
	if err := up.restoreCookies(); err != nil {
		return err
	}
	if st.CooldownUntil.After(cfg.clock.Now()) {
		cfg.log.Warn("in rate-limit cooldown", "until", st.CooldownUntil.Format(time.RFC3339))
	}

	ln, err := listenUnix(cfg.listen)
	if err != nil {
		return err
	}
	workCtx, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	w := &worker{
		svc:   newService(cfg.store, up, cfg.clock),
		clock: cfg.clock,
		ctx:   workCtx,
		jobs:  make(chan *job),
		fatal: make(chan error, 1),
		log:   cfg.log,
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		w.loop()
	}()
	srv := &http.Server{Handler: w.mux(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	cfg.log.Info("serving", "socket", cfg.listen, "timezone", cfg.timezone,
		"active_hours", fmt.Sprintf("%02d-%02d", cfg.hours.start, cfg.hours.end))

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-w.fatal:
	case err := <-served:
		runErr = fmt.Errorf("serve socket: %w", err)
	}
	// Stop the worker first: a call still waiting for its slot is answered
	// right away instead of holding up the exit. Calls already answered (such
	// as the one that hit a stop condition) finish writing during Shutdown.
	stopWork()
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		cfg.log.Warn("socket shutdown", "err", err)
	}
	<-workerDone
	return runErr
}

// listenUnix listens on path with mode 0660, replacing a stale socket but
// never another kind of file.
func listenUnix(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("--listen %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
}

// job is one gateway call, run on the worker goroutine.
type job struct {
	ctx      context.Context
	deadline time.Time // latest acceptable upstream slot
	run      func(ctx context.Context, dl time.Time) (any, error)
	done     chan jobResult
}

type jobResult struct {
	v   any
	err error
}

// worker runs calls one at a time, so every upstream request is strictly
// serialized on its goroutine.
type worker struct {
	svc     *service
	clock   clock
	ctx     context.Context // canceled when the service stops
	jobs    chan *job
	fatal   chan error
	stopped error // set after a stop condition; later calls fail fast
	log     *slog.Logger
}

func (w *worker) loop() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case j := <-w.jobs:
			if w.stopped != nil {
				j.done <- jobResult{err: w.stopped}
				continue
			}
			ctx, cancel := context.WithCancel(j.ctx)
			stop := context.AfterFunc(w.ctx, cancel)
			v, err := j.run(ctx, j.deadline)
			stop()
			cancel()
			j.done <- jobResult{v: v, err: err}
			if f := w.svc.up.fatal; f != nil {
				w.stopped = f
				w.fatal <- f
			}
		}
	}
}

func (w *worker) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+discordapi.PathSync, func(rw http.ResponseWriter, r *http.Request) {
		var req discordapi.SyncRequest
		w.handle(rw, r, &req, func() error { return validateSync(&req) },
			func(ctx context.Context, dl time.Time) (any, error) { return w.svc.sync(ctx, dl, req) })
	})
	mux.HandleFunc("POST "+discordapi.PathSearch, func(rw http.ResponseWriter, r *http.Request) {
		var req discordapi.SearchRequest
		w.handle(rw, r, &req, func() error { return validateSearch(&req) },
			func(ctx context.Context, dl time.Time) (any, error) { return w.svc.search(ctx, dl, req) })
	})
	mux.HandleFunc("POST "+discordapi.PathFetch, func(rw http.ResponseWriter, r *http.Request) {
		var req discordapi.FetchRequest
		w.handle(rw, r, &req, func() error { return validateFetch(&req) },
			func(ctx context.Context, dl time.Time) (any, error) { return w.svc.fetch(ctx, dl, req) })
	})
	return mux
}

// handle decodes and validates a call, then hands it to the worker, waiting
// at most maxSlotWait to be picked up.
func (w *worker) handle(rw http.ResponseWriter, r *http.Request, req any, validate func() error,
	run func(ctx context.Context, dl time.Time) (any, error),
) {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxCallBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		writeError(rw, http.StatusBadRequest, "invalid JSON body", 0)
		return
	}
	if err := validate(); err != nil {
		writeError(rw, http.StatusBadRequest, err.Error(), 0)
		return
	}
	j := &job{ctx: r.Context(), deadline: w.clock.Now().Add(maxSlotWait), run: run, done: make(chan jobResult, 1)}
	wait := time.NewTimer(maxSlotWait)
	defer wait.Stop()
	select {
	case w.jobs <- j:
	case <-wait.C:
		writeError(rw, http.StatusTooManyRequests, "discord service busy with another call", retrySeconds(maxSlotWait))
		return
	case <-w.ctx.Done():
		writeError(rw, http.StatusServiceUnavailable, "discord service stopping", 0)
		return
	case <-r.Context().Done():
		return
	}
	res := <-j.done
	if res.err != nil {
		status, retry, msg := errorResponse(res.err)
		if status >= 500 {
			w.log.Warn("call failed", "path", r.URL.Path, "status", status, "err", res.err)
		} else {
			w.log.Info("call refused", "path", r.URL.Path, "status", status, "err", res.err)
		}
		writeError(rw, status, msg, retry)
		return
	}
	writeJSON(rw, http.StatusOK, res.v)
}

// errorResponse maps a call error to its status, retry hint, and message.
func errorResponse(err error) (status, retryAfter int, msg string) {
	var (
		paced    *pacedError
		denied   *deniedError
		notReady *notReadyError
		bad      *badRequestError
		up       *upstreamError
	)
	switch {
	case errors.As(err, &paced):
		return http.StatusTooManyRequests, retrySeconds(paced.retryAfter), paced.Error()
	case errors.As(err, &denied):
		return denied.status, 0, denied.Error()
	case errors.As(err, &notReady):
		return http.StatusServiceUnavailable, retrySeconds(notReady.retryAfter), notReady.Error()
	case errors.Is(err, exitcode.ErrNeedsOwner):
		return http.StatusServiceUnavailable, 0, "discord account needs manual action; the service is stopping"
	case errors.As(err, &bad):
		return http.StatusBadRequest, 0, bad.Error()
	case errors.As(err, &up):
		return http.StatusBadGateway, 0, up.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, 0, "call canceled"
	default:
		return http.StatusInternalServerError, 0, "internal error"
	}
}

func writeError(rw http.ResponseWriter, status int, msg string, retryAfter int) {
	writeJSON(rw, status, discordapi.ErrorResponse{Error: msg, RetryAfterSeconds: retryAfter})
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}
