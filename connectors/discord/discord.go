// Command mandate-discord is the on-demand, read-only Discord connector.
//
// Account integrity comes first: every request looks like Chrome on Linux and
// is paced like a person. Only GETs are sent, and nothing at startup.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // TZ must resolve on hosts without zoneinfo

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

type Config struct {
	// ActiveHours are the local hours HH-HH in which requests may be sent.
	ActiveHours string `json:"active_hours"`
}

const defaultActiveHours = "08-24"

func (c Config) hours(loc *time.Location) (activeHours, error) {
	if c.ActiveHours == "" {
		return parseActiveHours(defaultActiveHours, loc)
	}
	return parseActiveHours(c.ActiveHours, loc)
}

func (c Config) Validate() error {
	_, err := c.hours(time.UTC)
	return err
}

func main() {
	connector.Main(func(ctx context.Context, env *connector.Env, c Config) (connector.Connector, error) {
		// Sent as X-Discord-Timezone, so it must be a name a browser reports;
		// unset means UTC, as everywhere else on Unix.
		tz := cmp.Or(os.Getenv("TZ"), "UTC")
		loc, err := time.LoadLocation(tz)
		if strings.HasPrefix(tz, ":") || err != nil {
			return nil, fmt.Errorf("TZ must name an IANA time zone such as Europe/Berlin, got %q", tz)
		}
		hours, err := c.hours(loc)
		if err != nil {
			return nil, err
		}
		st, err := store.Open(ctx, env.DataPath())
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
		client, jar, err := newBrowserClient()
		if err != nil {
			st.Close()
			return nil, err
		}
		w, err := start(ctx, config{
			store:     st,
			statePath: filepath.Join(env.PrivateDir(), "state.json"),
			token:     env.Secret("token"),
			timezone:  tz,
			hours:     hours,
			transport: client,
			jar:       jar,
			apiBase:   defaultAPIBase,
			webBase:   defaultWebBase,
			clock:     realClock{},
			rng:       globalRand{},
			log:       env.Log,
			fail:      env.Fail,
		})
		if err != nil {
			st.Close()
			return nil, err
		}
		return withStore{w, st}, nil
	})
}

// withStore closes the store once the worker has stopped.
type withStore struct {
	*worker
	store *store.Store
}

func (w withStore) Close() error { return errors.Join(w.worker.Close(), w.store.Close()) }

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

// Errors never include the token.
func checkToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	switch {
	case token == "":
		return "", errors.New("the token is empty")
	case strings.HasPrefix(token, "Bot ") || strings.HasPrefix(token, "Bearer "):
		return "", errors.New("paste a bare user token, without Bot or Bearer")
	case strings.ContainsAny(token, " \t\r\n"):
		return "", errors.New("that is more than one token")
	}
	return token, nil
}

type config struct {
	store            *store.Store
	statePath        string
	token            connector.Secret
	timezone         string
	hours            activeHours
	transport        transport
	jar              fhttp.CookieJar // nil in tests
	apiBase, webBase string
	clock            clock
	rng              rng
	log              *slog.Logger
	// fail stops the connector on a stop condition a restart may clear.
	fail func(error)
}

// start runs the worker until ctx is done; Close waits for it to stop.
func start(ctx context.Context, cfg config) (*worker, error) {
	st, err := loadState(cfg.statePath)
	if err != nil {
		return nil, err
	}
	st.prune(cfg.clock.Now())
	st.ensureIdentity()
	if err := st.save(cfg.statePath); err != nil {
		return nil, err
	}
	raw, err := cfg.token.Load()
	if err != nil {
		return nil, fmt.Errorf("load token: %w", err)
	}
	var token string
	if raw != nil {
		if token, err = checkToken(string(raw)); err != nil {
			return nil, fmt.Errorf("stored token: %w", err)
		}
	}
	up := &upstream{
		tr:        cfg.transport,
		jar:       cfg.jar,
		token:     token,
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
		return nil, err
	}
	if st.CooldownUntil.After(cfg.clock.Now()) {
		cfg.log.Warn("in rate-limit cooldown", "until", st.CooldownUntil.Format(time.RFC3339))
	}
	// When ctx is done the worker stops before the socket shuts down, so a
	// call still waiting for its slot is answered right away instead of
	// holding up the exit.
	w := &worker{
		svc:   newService(cfg.store, up, cfg.clock),
		token: cfg.token,
		clock: cfg.clock,
		ctx:   ctx,
		jobs:  make(chan *job),
		fail:  cfg.fail,
		done:  make(chan struct{}),
		log:   cfg.log,
	}
	w.snapshot()
	go func() {
		defer close(w.done)
		w.loop()
	}()
	cfg.log.Info("started", "linked", token != "", "timezone", cfg.timezone,
		"active_hours", fmt.Sprintf("%02d-%02d", cfg.hours.start, cfg.hours.end))
	return w, nil
}

func (w *worker) Close() error {
	<-w.done
	return nil
}

// job is one gateway call, run on the worker goroutine.
type job struct {
	ctx      context.Context
	deadline time.Time // latest acceptable upstream slot
	// link jobs set up or drop the token; they run even when reads may not.
	link bool
	run  func(ctx context.Context, dl time.Time) (connector.ReadResult, error)
	done chan jobResult
}

type jobResult struct {
	v   connector.ReadResult
	err error
}

// worker runs calls one at a time, so every upstream request is strictly
// serialized on its goroutine.
type worker struct {
	svc      *service
	token    connector.Secret
	clock    clock
	ctx      context.Context // canceled when the service stops
	jobs     chan *job
	fail     func(error) // for stop conditions a restart may clear
	stopped  error       // set after a stop condition; later calls fail fast
	rejected *ownerStop  // why Discord refused the last token
	done     chan struct{}
	log      *slog.Logger

	// What Health reads; the pacer and state belong to the worker goroutine.
	mu     sync.Mutex
	pace   state
	linked bool
	owner  string // what the owner must do, after an owner stop
}

// snapshot runs on the worker goroutine, or before it starts.
func (w *worker) snapshot() {
	st := w.svc.up.st
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pace = state{CooldownUntil: st.CooldownUntil, BreakUntil: st.BreakUntil, Requests: slices.Clone(st.Requests)}
	w.linked = w.svc.up.token != ""
	w.owner = ""
	var stop *ownerStop
	switch {
	case errors.As(w.stopped, &stop):
		w.owner = stop.Error()
	case !w.linked && w.rejected != nil:
		w.owner = w.rejected.Error()
	}
}

func (w *worker) Health(context.Context) connector.Health {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.owner != "":
		return connector.Health{State: connector.NeedsSetup, Detail: w.owner}
	case !w.linked:
		return connector.Health{State: connector.NeedsSetup, Detail: "needs a token"}
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
			switch {
			case j.link:
			case w.stopped != nil:
				j.done <- jobResult{err: w.stopped}
				continue
			case w.svc.up.token == "":
				j.done <- jobResult{err: errNotLinked}
				continue
			}
			ctx, cancel := context.WithCancel(j.ctx)
			stop := context.AfterFunc(w.ctx, cancel)
			v, err := j.run(ctx, j.deadline)
			stop()
			cancel()
			w.check()
			w.snapshot()
			j.done <- jobResult{v: v, err: err}
		}
	}
}

// check acts on what upstream ran into. A rejected token is discarded so the
// owner can paste a new one; other owner stops hold every read until a
// restart or unlink.
func (w *worker) check() {
	f := w.svc.up.fatal
	var stop *ownerStop
	switch {
	case f == nil:
	case errors.As(f, &stop) && stop.action == fixToken:
		w.svc.up.fatal = nil
		w.rejected = stop
		w.log.Warn("discord rejected the token; discarded it", "reason", stop.reason)
		if err := w.forget(); err != nil {
			w.stopped = err
			w.fail(err)
		}
	case errors.As(f, &stop):
		w.stopped = f
	default:
		w.stopped = f
		w.fail(f)
	}
}

// forget drops the token and everything learned with it. It runs on the
// worker goroutine.
func (w *worker) forget() error {
	up := w.svc.up
	up.token = ""
	up.st.SelfID, up.st.SelfCheckedAt = "", time.Time{}
	up.st.Denied = nil
	w.svc.forget()
	if err := up.saveState(); err != nil {
		return err
	}
	if err := w.token.Delete(); err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return nil
}

// Setup checks the token the way reads learn the account's own ID, then
// keeps it.
func (w *worker) Setup(ctx context.Context, args map[string]string) (connector.SetupStep, error) {
	for k := range args {
		if k != "token" {
			return connector.SetupStep{}, connector.Invalid("unknown arg %q", k)
		}
	}
	token, err := checkToken(args["token"])
	if err != nil {
		return connector.SetupStep{}, connector.Invalid("%v", err)
	}
	_, err = w.submit(ctx, "setup", true, func(ctx context.Context, dl time.Time) (connector.ReadResult, error) {
		return connector.ReadResult{}, w.link(ctx, dl, token)
	})
	if err != nil {
		return connector.SetupStep{}, err
	}
	return connector.SetupStep{Done: true}, nil
}

// link runs on the worker goroutine.
func (w *worker) link(ctx context.Context, dl time.Time, token string) error {
	up := w.svc.up
	if up.token != "" {
		return &connector.Error{Status: http.StatusConflict, Message: "already linked"}
	}
	// Whatever was learned before belongs to the last token, maybe another account.
	if err := w.forget(); err != nil {
		return err
	}
	up.token = token
	up.pace.atKeyboard = true
	_, err := w.svc.selfID(ctx, dl)
	up.pace.atKeyboard = false
	if err != nil {
		up.token = ""
		var stop *ownerStop
		if errors.As(up.fatal, &stop) {
			up.fatal = nil
		}
		return err
	}
	if err := w.token.Store([]byte(token)); err != nil {
		up.token = ""
		return fmt.Errorf("store token: %w", err)
	}
	w.rejected = nil
	w.log.Info("linked")
	return nil
}

func (w *worker) Unlink(ctx context.Context) error {
	_, err := w.submit(ctx, "unlink", true, func(context.Context, time.Time) (connector.ReadResult, error) {
		var stop *ownerStop
		if errors.As(w.stopped, &stop) {
			w.stopped, w.svc.up.fatal = nil, nil
		}
		w.rejected = nil
		return connector.ReadResult{}, w.forget()
	})
	return err
}

var spec = connector.Spec{
	Name: "discord",
	Doc: "sync, fetch and guild-search ask Discord and store what they get; chats, messages and search read only what is stored.\n" +
		"Discord calls are paced like a person and may take a while. Start with sync, then sync guild_id=ID for a server's channels.",
	Setup: []connector.Param{{
		Name: "token", Required: true, Secret: true,
		Doc: "the Authorization header of any discord.com/api request in your browser's devtools",
	}},
	Ops: []connector.Op{
		{
			Name: "sync", Kind: connector.Read,
			Summary: "Store the server and DM lists; with guild_id, also that server's channels and threads. Cached for a day.",
			Params: []connector.Param{
				{Name: "guild_id", Doc: "server ID", Container: true},
			},
		},
		{
			Name: "guild-search", Kind: connector.Read,
			Summary: "Search a server on Discord, newest first, 25 per page. Needs content, author_id, channel_id or has. Cached for an hour.",
			Params: []connector.Param{
				{Name: "guild_id", Doc: "server ID", Required: true, Container: true},
				{Name: "content", Doc: "text to match, up to 512 characters"},
				{Name: "author_id", Doc: "the author's sender_id"},
				{Name: "channel_id", Doc: "channel or thread ID", Container: true},
				{Name: "has", Doc: "link, embed, file, video, image or sound"},
				{Name: "offset", Doc: "results to skip, a multiple of 25 up to 9975"},
			},
		},
		{
			Name: "fetch", Kind: connector.Read,
			Summary: "Fetch messages of a channel, thread or DM from Discord, newest first. Page with one of before, after or around.",
			Params: []connector.Param{
				{Name: "channel_id", Doc: "channel, thread or DM ID", Required: true, Container: true},
				{Name: "before", Doc: "seq; older messages"},
				{Name: "after", Doc: "seq; newer messages"},
				{Name: "around", Doc: "seq; messages around it"},
				{Name: "limit", Doc: "1 to 100, default 50"},
			},
		},
	},
}

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
	case "guild-search":
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
	res, err := w.submit(ctx, op, false, run)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

// submit hands a call to the worker, waiting at most maxSlotWait for it to be
// picked up and for its upstream slots. Link calls wait at most linkSlotWait.
func (w *worker) submit(ctx context.Context, op string, link bool, run func(ctx context.Context, dl time.Time) (connector.ReadResult, error)) (connector.ReadResult, error) {
	limit := maxSlotWait
	if link {
		limit = linkSlotWait
	}
	j := &job{ctx: ctx, deadline: w.clock.Now().Add(limit), link: link, run: run, done: make(chan jobResult, 1)}
	wait := time.NewTimer(limit)
	defer wait.Stop()
	select {
	case w.jobs <- j:
	case <-wait.C:
		return connector.ReadResult{}, &connector.Error{
			Status: http.StatusTooManyRequests, Message: "discord service busy with another call", RetryAfter: retrySeconds(limit),
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

// Denials are always 404, so a 403 never confirms that a channel exists.
func toConnectorError(err error) *connector.Error {
	var (
		paced    *pacedError
		denied   *deniedError
		notReady *notReadyError
		bad      *badRequestError
		up       *upstreamError
		stop     *ownerStop
		ce       *connector.Error
	)
	switch {
	case errors.As(err, &ce):
		return ce
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
