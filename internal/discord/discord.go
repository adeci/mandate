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
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // --timezone must resolve on hosts without zoneinfo

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/connector"
	"github.com/adeci/mandate/internal/store"
)

const usage = "usage: mandate discord serve --data PATH --state PATH --token-file PATH --listen PATH [--timezone TZ] [--active-hours HH-HH]"

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
	listen := flags.String("listen", "", "Unix socket to serve the connector protocol on (required)")
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

	ln, err := connector.ListenUnix(cfg.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.listen, err)
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
	w.snapshot()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		w.loop()
	}()
	// No verifier: every /execute is refused before reaching the worker.
	cs := &connector.Server{Connector: w, Log: cfg.log}
	srv := &http.Server{Handler: cs.Handler(), ReadHeaderTimeout: 10 * time.Second}
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

// job is one gateway call, run on the worker goroutine.
type job struct {
	ctx      context.Context
	deadline time.Time // latest acceptable upstream slot
	run      func(ctx context.Context, dl time.Time) (connector.ReadResult, error)
	done     chan jobResult
}

type jobResult struct {
	v   connector.ReadResult
	err error
}

// worker runs calls one at a time, so every upstream request is strictly
// serialized on its goroutine.
type worker struct {
	svc     *service
	clock   clock
	ctx     context.Context // canceled when the service stops
	jobs    chan *job
	fatal   chan error // stop conditions a restart may clear
	stopped error      // set after a stop condition; later calls fail fast
	log     *slog.Logger

	// What Health reads; the pacer and state belong to the worker goroutine.
	mu    sync.Mutex
	pace  state
	owner string // what the owner must do, after an owner stop
}

// snapshot runs on the worker goroutine, or before it starts.
func (w *worker) snapshot() {
	st := w.svc.up.st
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pace = state{CooldownUntil: st.CooldownUntil, BreakUntil: st.BreakUntil, Requests: slices.Clone(st.Requests)}
	var stop *ownerStop
	if errors.As(w.stopped, &stop) {
		w.owner = stop.Error()
	}
}

func (w *worker) Health(context.Context) connector.Health {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.owner != "" {
		return connector.Health{State: connector.NeedsSetup, Detail: w.owner}
	}
	p := &pacer{st: &w.pace, hours: w.svc.up.pace.hours}
	if until, why := p.rest(w.clock.Now()); !until.IsZero() {
		return connector.Health{State: connector.Resting, Detail: why, Until: until}
	}
	return connector.Health{State: connector.Healthy}
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
			if f := w.svc.up.fatal; f != nil {
				w.stopped = f
				var stop *ownerStop
				if !errors.As(f, &stop) {
					w.fatal <- f
				}
			}
			w.snapshot()
			j.done <- jobResult{v: v, err: err}
		}
	}
}

var spec = connector.Spec{Name: "discord", Ops: []connector.Op{
	{
		Name: "sync", Kind: connector.Read,
		Summary: "Refresh the list of Discord servers (guilds) and DMs; with guild_id, also that server's channels and threads. " +
			"Run this first to learn IDs; lists are cached for a day, so repeats are free.",
		Params: []connector.Param{
			{Name: "guild_id", Doc: "Server ID whose channels and threads to list. Omit to list only servers and DMs.", Container: true},
		},
	},
	{
		Name: "search", Kind: connector.Read,
		Summary: "Search one server's message history the way Discord's search box does, 25 results per page, newest first. " +
			"Needs at least one of content, author_id, channel_id, or has. Results are cached for an hour.",
		Params: []connector.Param{
			{Name: "guild_id", Doc: "Server ID to search (from sync).", Required: true, Container: true},
			{Name: "content", Doc: "Words to match in message text, at most 512 characters."},
			{Name: "author_id", Doc: "Only messages by this user ID."},
			{Name: "channel_id", Doc: "Only messages in this channel or thread ID.", Container: true},
			{Name: "has", Doc: "Only messages with this attachment kind: link, embed, file, video, image, or sound."},
			{Name: "offset", Doc: "Skip this many results to page further, in steps of 25 (0 to 9975)."},
		},
	},
	{
		Name: "fetch", Kind: connector.Read,
		Summary: "Read recent messages from one channel, thread, or DM, newest first. " +
			"Anchor with at most one of before, after, or around to page through history.",
		Params: []connector.Param{
			{Name: "channel_id", Doc: "Channel, thread, or DM ID (from sync).", Required: true, Container: true},
			{Name: "before", Doc: "Only messages older than this message ID."},
			{Name: "after", Doc: "Only messages newer than this message ID."},
			{Name: "around", Doc: "Messages surrounding this message ID."},
			{Name: "limit", Doc: "How many messages, 1 to 100 (default 50)."},
		},
	},
}}

func (w *worker) Spec() connector.Spec { return spec }

func (w *worker) Prepare(context.Context, string, map[string]string) (connector.Prepared, error) {
	return connector.Prepared{}, connector.Invalid("discord has no write ops")
}

func (w *worker) Execute(context.Context, approval.Record) connector.Result {
	return connector.Result{Status: connector.Failed, Detail: "discord has no write ops"}
}

func (w *worker) Read(ctx context.Context, op string, args map[string]string) (json.RawMessage, error) {
	var run func(ctx context.Context, dl time.Time) (connector.ReadResult, error)
	switch op {
	case "sync":
		req, err := parseSync(args)
		if err != nil {
			return nil, toConnectorError(err)
		}
		run = func(ctx context.Context, dl time.Time) (connector.ReadResult, error) { return w.svc.sync(ctx, dl, req) }
	case "search":
		req, err := parseSearch(args)
		if err != nil {
			return nil, toConnectorError(err)
		}
		run = func(ctx context.Context, dl time.Time) (connector.ReadResult, error) {
			return w.svc.search(ctx, dl, req)
		}
	case "fetch":
		req, err := parseFetch(args)
		if err != nil {
			return nil, toConnectorError(err)
		}
		run = func(ctx context.Context, dl time.Time) (connector.ReadResult, error) {
			return w.svc.fetch(ctx, dl, req)
		}
	default:
		return nil, connector.ErrUnknownOp
	}
	res, err := w.submit(ctx, op, run)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

// submit hands a call to the worker, waiting at most maxSlotWait for it to be
// picked up.
func (w *worker) submit(ctx context.Context, op string, run func(ctx context.Context, dl time.Time) (connector.ReadResult, error)) (connector.ReadResult, error) {
	j := &job{ctx: ctx, deadline: w.clock.Now().Add(maxSlotWait), run: run, done: make(chan jobResult, 1)}
	wait := time.NewTimer(maxSlotWait)
	defer wait.Stop()
	select {
	case w.jobs <- j:
	case <-wait.C:
		return connector.ReadResult{}, &connector.Error{
			Status: http.StatusTooManyRequests, Message: "discord service busy with another call", RetryAfter: retrySeconds(maxSlotWait),
		}
	case <-w.ctx.Done():
		return connector.ReadResult{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "discord service stopping"}
	case <-ctx.Done():
		return connector.ReadResult{}, ctx.Err()
	}
	res := <-j.done
	if res.err != nil {
		e := toConnectorError(res.err)
		if e.Status >= 500 {
			w.log.Warn("call failed", "op", op, "status", e.Status, "err", res.err)
		} else {
			w.log.Info("call refused", "op", op, "status", e.Status, "err", res.err)
		}
		return connector.ReadResult{}, e
	}
	return res.v, nil
}

// toConnectorError maps a call error to the refusal relayed to the agent.
// Denials are always 404, so a 403 never confirms that a channel exists.
func toConnectorError(err error) *connector.Error {
	var (
		paced    *pacedError
		denied   *deniedError
		notReady *notReadyError
		bad      *badRequestError
		up       *upstreamError
		stop     *ownerStop
	)
	switch {
	case errors.As(err, &paced):
		return &connector.Error{Status: http.StatusTooManyRequests, Message: paced.Error(), RetryAfter: retrySeconds(paced.retryAfter)}
	case errors.As(err, &denied):
		return &connector.Error{Status: http.StatusNotFound, Message: denied.Error()}
	case errors.As(err, &notReady):
		return &connector.Error{Status: http.StatusServiceUnavailable, Message: notReady.Error(), RetryAfter: retrySeconds(notReady.retryAfter)}
	case errors.As(err, &stop):
		return &connector.Error{Status: http.StatusServiceUnavailable, Message: stop.Error()}
	case errors.As(err, &bad):
		return &connector.Error{Status: http.StatusBadRequest, Message: bad.Error()}
	case errors.As(err, &up):
		return &connector.Error{Status: http.StatusBadGateway, Message: up.Error()}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &connector.Error{Status: http.StatusServiceUnavailable, Message: "call canceled"}
	default:
		return &connector.Error{Status: http.StatusInternalServerError, Message: "internal error"}
	}
}
