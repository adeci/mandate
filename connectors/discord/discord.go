// Command mandate-discord is the Discord connector: it reads chats, sends the
// messages the owner approves, and keeps the gateway connection an open
// Discord tab keeps, storing the messages it pushes.
//
// Account integrity comes first: every request looks like Chrome on Linux and
// is paced like a person. Reads are GETs, a send is a typing indicator and one
// POST, a file is a refreshed link and its download, and at startup it loads
// the app page and connects, as a browser restoring the tab does.
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
	"github.com/bogdanfinn/websocket"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

type Config struct{}

func main() {
	connector.Main(func(ctx context.Context, env *connector.Env, c Config) (connector.Connector, error) {
		// Sent as X-Discord-Timezone, so it must be a name a browser reports;
		// unset means UTC, as everywhere else on Unix.
		tz := cmp.Or(os.Getenv("TZ"), "UTC")
		if _, err := time.LoadLocation(tz); strings.HasPrefix(tz, ":") || err != nil {
			return nil, fmt.Errorf("TZ must name an IANA time zone such as Europe/Berlin, got %q", tz)
		}
		st, err := store.Open(ctx, env.DataPath())
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
		client, cdn, jar, err := newBrowserClients()
		if err != nil {
			st.Close()
			return nil, err
		}
		dial, err := newGatewayDialer()
		if err != nil {
			st.Close()
			return nil, err
		}
		w, err := start(ctx, config{
			store:       st,
			statePath:   filepath.Join(env.PrivateDir(), "state.json"),
			token:       env.Secret("token"),
			timezone:    tz,
			transport:   client,
			cdn:         cdn,
			jar:         jar,
			apiBase:     defaultAPIBase,
			webBase:     defaultWebBase,
			cdnBase:     defaultCDNBase,
			gatewayURL:  defaultGatewayURL,
			dialGateway: dial,
			clock:       realClock{},
			rng:         globalRand{},
			log:         env.Log,
			fail:        env.Fail,
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

// tls-client's newest profile is Chrome 152 and the UA claims Chrome 156, the
// owner's own browser on this account. Claiming 152 would make this session
// an older Chrome than the owner's; a handshake a few releases behind its UA
// is the smaller tell. Redirects are not followed: Discord never redirects a
// logged-in browser. Attachment downloads get a client of their own, since a
// large file takes longer than any API call may.
func newBrowserClients() (api, cdn tls_client.HttpClient, jar tls_client.CookieJar, err error) {
	jar = tls_client.NewCookieJar()
	client := func(timeout time.Duration) (tls_client.HttpClient, error) {
		return tls_client.NewHttpClient(tls_client.NewNoopLogger(),
			tls_client.WithClientProfile(profiles.Chrome_152),
			tls_client.WithRandomTLSExtensionOrder(),
			tls_client.WithCookieJar(jar),
			tls_client.WithTimeoutSeconds(int(timeout/time.Second)),
			tls_client.WithNotFollowRedirects(),
		)
	}
	if api, err = client(requestTimeout); err != nil {
		return nil, nil, nil, fmt.Errorf("create http client: %w", err)
	}
	if cdn, err = client(0); err != nil {
		return nil, nil, nil, fmt.Errorf("create http client: %w", err)
	}
	return api, cdn, jar, nil
}

// newGatewayDialer opens WebSockets with the same TLS profile over HTTP/1.1,
// as Chrome does.
func newGatewayDialer() (dialFunc, error) {
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithClientProfile(profiles.Chrome_152),
		tls_client.WithRandomTLSExtensionOrder(),
		tls_client.WithForceHttp1(),
	)
	if err != nil {
		return nil, fmt.Errorf("create websocket client: %w", err)
	}
	return func(ctx context.Context, url string, h fhttp.Header) (*websocket.Conn, error) {
		ws, err := tls_client.NewWebsocket(tls_client.NewNoopLogger(),
			tls_client.WithTlsClient(client),
			tls_client.WithUrl(url),
			tls_client.WithHeaders(h),
			tls_client.WithHandshakeTimeoutMilliseconds(int(requestTimeout/time.Millisecond)),
		)
		if err != nil {
			return nil, err
		}
		return ws.Connect(ctx)
	}, nil
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
	transport        transport
	cdn              transport
	jar              fhttp.CookieJar // nil in tests
	apiBase, webBase string
	cdnBase          string
	gatewayURL       string
	dialGateway      dialFunc
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
		cdn:       cfg.cdn,
		jar:       cfg.jar,
		token:     token,
		timezone:  cfg.timezone,
		apiBase:   cfg.apiBase,
		webBase:   cfg.webBase,
		cdnBase:   cfg.cdnBase,
		clock:     cfg.clock,
		pace:      newPacer(st, cfg.rng),
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
	moved, err := reparentThreads(ctx, cfg.store.DB())
	if err != nil {
		return nil, fmt.Errorf("move threads under their channels: %w", err)
	}
	if moved > 0 {
		cfg.log.Info("moved threads under their channels", "count", moved)
	}
	// When ctx is done the worker stops before the socket shuts down, so a
	// call still waiting for its slot is answered right away instead of
	// holding up the exit.
	gw := newGateway(cfg.gatewayURL, cfg.dialGateway, cfg.clock, cfg.rng, cfg.store, cfg.log)
	w := &worker{
		svc:   newService(cfg.store, up, gw, cfg.clock),
		token: cfg.token,
		clock: cfg.clock,
		ctx:   ctx,
		jobs:  make(chan *job),
		fail:  cfg.fail,
		done:  make(chan struct{}),
		log:   cfg.log,
	}
	w.snapshot()
	if token != "" {
		gw.setStatus("live updates connecting") // until the app page has loaded
	}
	go func() {
		defer close(w.done)
		w.loop()
	}()
	cfg.log.Info("started", "linked", token != "", "timezone", cfg.timezone)
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
	p := &pacer{st: &w.pace}
	if until, why := p.rest(w.clock.Now()); !until.IsZero() {
		return connector.Health{State: connector.Resting, Detail: why, Until: until}
	}
	if detail := w.svc.gw.health(); detail != "" {
		return connector.Health{State: connector.Healthy, Detail: detail}
	}
	return connector.Health{State: connector.Healthy}
}

func (w *worker) loop() {
	// A tab being closed.
	defer w.svc.gw.stop(websocket.CloseGoingAway)
	w.syncGateway()
	for {
		select {
		case <-w.ctx.Done():
			return
		case token := <-w.svc.gw.rejected:
			if token == "" || token != w.svc.up.token {
				continue // a token already replaced
			}
			w.svc.up.fatal = &ownerStop{"token rejected by the gateway (4004)", fixToken}
			w.check()
			w.snapshot()
			w.syncGateway()
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
			w.syncGateway()
		}
	}
}

// syncGateway keeps the gateway connection open while the account is linked
// and usable, for the tab the REST calls come from. A tab that opens loads
// the app page first, as a browser restoring it does. It runs on the worker
// goroutine.
func (w *worker) syncGateway() {
	up := w.svc.up
	if up.token == "" || w.stopped != nil {
		w.svc.gw.stop(closeNone)
		return
	}
	if !up.relaunched && !w.svc.gw.running() {
		err := up.openApp(w.ctx, w.clock.Now().Add(maxSlotWait))
		var pe *pacedError
		if errors.As(err, &pe) || w.ctx.Err() != nil || up.fatal != nil {
			// Nothing loaded; the next call tries again.
			w.log.Info("not opening the app yet", "err", err)
			w.svc.gw.setStatus("")
			w.check()
			w.snapshot()
			return
		}
	}
	st := up.st
	// The web client reports an analytics session on connecting, starting
	// a new one when the last has lapsed.
	if now := w.clock.Now(); st.HeartbeatSessionID == "" || now.Sub(st.HeartbeatUsedAt) >= heartbeatTimeout {
		up.touchHeartbeat(now)
	}
	t := tab{
		token: up.token, buildNumber: st.buildNumber(), launchID: st.LaunchID, installationID: st.InstallationID,
		heartbeatID: st.HeartbeatSessionID, heartbeatStart: st.HeartbeatStartedAt,
	}
	if up.relaunched {
		up.relaunched = false
		w.svc.gw.reload(t)
		return
	}
	w.svc.gw.start(t)
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
	err := up.ensureInstallation(ctx, dl)
	if err == nil {
		up.token = token
		_, err = w.svc.selfID(ctx, dl)
	}
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
	Doc: "sync, fetch and guild-search ask Discord and store what they get; chats, messages and search answer from what is stored.\n" +
		"send proposes a message; it goes out from the owner's account once they approve it.\n" +
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
		{
			Name: "send", Kind: connector.Write,
			Summary: "Send a message to a DM, group, channel or thread.",
			Params: []connector.Param{
				{Name: "chat", Doc: "chat ID from mandate discord chats", Required: true, Container: true},
				{Name: "text", Doc: "message text, up to 2000 characters", Required: true, Editable: true},
			},
		},
	},
}

func (w *worker) Spec() connector.Spec { return spec }

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
