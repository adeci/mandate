package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	qrInstructions    = "Google Messages → your profile picture → Device pairing → QR code scanner, then scan this code"
	emojiInstructions = "On your phone, Google Messages asks which emoji you see; tap this one"

	// Starting a Google account pairing waits up to 20 s for the phone.
	firstStepWait = 45 * time.Second
	// A code is good for about 30 s; like mautrix-gmessages, a pairing shows
	// six before it expires.
	qrLifetime = 30 * time.Second
	qrCodes    = 6
	// libgm sends Google account pairing requests with a 5-minute lifetime.
	emojiWait = 5 * time.Minute
	// The phone needs a moment to save a QR pairing before the first connect.
	settleTime   = 2 * time.Second
	activateWait = 7 * time.Second
	// Each ping wakes the phone; mautrix-gmessages pings every 20 minutes
	// rather than libgm's default of one.
	pingInterval = 20 * time.Minute
)

// gmClient is the part of *libgm.Client a paired session uses.
type gmClient interface {
	FetchConfig(ctx context.Context) error
	// Account is the Google account the fetched config is signed in as.
	Account() string
	Connect(ctx context.Context) error
	Disconnect()
	Unpair(ctx context.Context) error
	SetActiveSession(ctx context.Context) error
	ListConversations(ctx context.Context, req *gmproto.ListConversationsRequest) (*gmproto.ListConversationsResponse, error)
	FetchMessages(ctx context.Context, conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error)
	GetConversation(ctx context.Context, conversationID string) (*gmproto.Conversation, error)
	SendMessage(ctx context.Context, req *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)
}

// pairClient is the part of *libgm.Client that pairs, by QR code or through
// the Google account.
type pairClient interface {
	FetchConfig(ctx context.Context) error
	Account() string
	StartLogin(ctx context.Context) (string, error)
	RefreshPhoneRelay() (string, error)
	StartGaiaPairing(ctx, bgCtx context.Context) (string, *libgm.PairingSession, error)
	FinishGaiaPairing(ctx context.Context, ps *libgm.PairingSession) (string, error)
	Disconnect()
	Unpair(ctx context.Context) error
}

type gmConnector struct {
	st      *store.Store
	log     *slog.Logger
	in      *ingester
	session connector.Secret
	// ctx outlives requests: the connection and pairing run until shutdown.
	ctx context.Context
	// newClient wires onEvent into a client for auth.
	newClient func(auth *libgm.AuthData, onEvent func(any)) gmClient
	// newPairing returns a client for a new session. For a QR pairing it
	// calls paired once the phone has scanned a code and filled in auth.
	newPairing func(auth *libgm.AuthData, paired func()) pairClient
	qrLifetime time.Duration
	qrCodes    int
	settleTime time.Duration

	mu sync.Mutex
	// cli and auth are nil until paired.
	cli  gmClient
	auth *libgm.AuthData
	// The phone accepted this browser session (BROWSER_ACTIVE).
	active bool
	// Google Messages for Web was opened elsewhere, which pauses this session.
	elsewhere bool
	phoneDown bool
	listenErr error
	// unpaired says why the last pairing ended, until the next one.
	unpaired string
	pairing  *pairing
}

func newConnector(ctx context.Context, st *store.Store, log *slog.Logger, session connector.Secret,
	newClient func(*libgm.AuthData, func(any)) gmClient, newPairing func(*libgm.AuthData, func()) pairClient,
) *gmConnector {
	return &gmConnector{
		st: st, log: log, in: newIngester(st, log), session: session, ctx: ctx,
		newClient: newClient, newPairing: newPairing, qrLifetime: qrLifetime, qrCodes: qrCodes, settleTime: settleTime,
	}
}

// resume connects with the stored session, if any.
func (c *gmConnector) resume() error {
	data, err := c.session.Load()
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if data == nil {
		c.log.Warn("not linked; link it from the console")
		return nil
	}
	auth := &libgm.AuthData{}
	if err := json.Unmarshal(data, auth); err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	c.mu.Lock()
	c.use(auth)
	cli := c.cli
	c.mu.Unlock()
	c.connect(cli)
	return nil
}

// use needs c.mu.
func (c *gmConnector) use(auth *libgm.AuthData) {
	var cli gmClient
	cli = c.newClient(auth, func(evt any) { c.event(cli, evt) })
	c.cli, c.auth = cli, auth
	c.active, c.elsewhere, c.phoneDown, c.listenErr, c.unpaired = false, false, false, nil, ""
}

func (c *gmConnector) connect(cli gmClient) {
	c.log.Info("connecting")
	if err := cli.FetchConfig(c.ctx); err != nil {
		c.log.Warn("fetch config", "err", err)
	} else if c.google(cli) && cli.Account() == "" {
		c.log.Error("google sent no account for the stored cookies")
		c.refused(cli)
		return
	}
	// Connect fails only when Google refuses the session; network errors are
	// retried by libgm.
	if err := cli.Connect(c.ctx); err != nil {
		c.log.Error("connect", "err", err)
		c.refused(cli)
	}
}

// google reports whether cli's session was paired through a Google account.
func (c *gmConnector) google(cli gmClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cli == c.cli && c.auth.IsGoogleAccount()
}

// refused forgets cli's session after Google turned it down.
func (c *gmConnector) refused(cli gmClient) {
	if c.google(cli) {
		c.unpair(cli, "Google signed out the browser session the cookies came from; link again with new cookies")
		return
	}
	c.unpair(cli, "Google Messages refused the pairing")
}

// disconnect stops the session and saves it, since libgm keeps the cookies
// Google renews in memory.
func (c *gmConnector) disconnect() {
	c.mu.Lock()
	cli, p := c.cli, c.pairing
	c.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	if cli != nil {
		cli.Disconnect()
		c.save(cli)
	}
}

// state is the current client and the health of its session.
func (c *gmConnector) state() (gmClient, connector.Health) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.cli == nil && c.unpaired != "":
		return nil, connector.Health{State: connector.NeedsSetup, Detail: c.unpaired}
	case c.cli == nil:
		return nil, connector.Health{State: connector.NeedsSetup, Detail: "not linked"}
	case c.elsewhere:
		return c.cli, connector.Health{
			State:  connector.Broken,
			Detail: "Google Messages for Web was opened in another browser, which pauses mandate; close it, then restart mandate-gmessages",
		}
	case c.phoneDown:
		return c.cli, connector.Health{
			State:  connector.Broken,
			Detail: "the phone is not responding; it needs to be on and online",
		}
	case c.listenErr != nil:
		return c.cli, connector.Health{State: connector.Broken, Detail: "reconnecting to Google Messages: " + c.listenErr.Error()}
	case !c.active:
		return c.cli, connector.Health{State: connector.Broken, Detail: "connecting…"}
	}
	return c.cli, connector.Health{State: connector.Healthy}
}

func (c *gmConnector) Health(context.Context) connector.Health {
	_, h := c.state()
	return h
}

// event handles an event from cli. libgm calls it synchronously from its
// polling goroutine.
func (c *gmConnector) event(cli gmClient, evt any) {
	if !c.isCurrent(cli) {
		return
	}
	switch e := evt.(type) {
	case *events.AuthTokenRefreshed:
		c.save(cli)
	case *gmproto.RevokePairData:
		c.unpair(cli, "unpaired from the phone")
	case *events.GaiaLoggedOut:
		c.unpair(cli, "the Google account was signed out of Google Messages for Web")
	case *events.ListenFatalError:
		c.log.Error("google messages refused the session", "err", e.Error)
		c.refused(cli)
	case *events.PingFailed:
		if errors.Is(e.Error, events.ErrRequestedEntityNotFound) {
			c.unpair(cli, "unpaired from the phone")
		}
	case *events.ListenTemporaryError:
		c.log.Warn("connection to google messages failed; retrying", "err", e.Error)
		c.update(cli, func() { c.listenErr = e.Error })
	case *events.ListenRecovered:
		c.log.Info("reconnected")
		c.update(cli, func() { c.listenErr = nil })
		go c.in.sync(c.ctx, cli)
	case *events.PhoneNotResponding:
		c.log.Warn("the phone is not responding")
		c.update(cli, func() { c.phoneDown = true })
	case *events.PhoneRespondingAgain:
		c.log.Info("the phone is responding again")
		c.update(cli, func() { c.phoneDown = false })
		go c.activate(cli)
		go c.in.sync(c.ctx, cli)
	case *events.HackySetActiveMayFail:
		// The phone may miss libgm's first activation; ask again if it stays
		// unanswered, as mautrix-gmessages does.
		go func() {
			select {
			case <-time.After(activateWait):
				c.activate(cli)
			case <-c.ctx.Done():
			}
		}()
	case *gmproto.UserAlertEvent:
		c.alert(cli, e.GetAlertType())
	case *gmproto.Settings:
		c.in.settings(e)
	case *gmproto.Conversation:
		c.in.conversationEvent(c.ctx, e)
	case *libgm.WrappedMessage:
		c.in.message(c.ctx, cli, e.Message)
	}
}

func (c *gmConnector) alert(cli gmClient, t gmproto.AlertType) {
	switch t {
	case gmproto.AlertType_BROWSER_ACTIVE:
		c.log.Info("connected")
		c.update(cli, func() { c.active, c.elsewhere = true, false })
		go c.in.sync(c.ctx, cli)
	case gmproto.AlertType_BROWSER_INACTIVE:
		c.log.Warn("google messages for web was opened in another browser; restart to take the session back")
		c.update(cli, func() { c.active, c.elsewhere = false, true })
	case gmproto.AlertType_BROWSER_INACTIVE_FROM_TIMEOUT, gmproto.AlertType_BROWSER_INACTIVE_FROM_INACTIVITY:
		c.log.Info("the phone paused the session; resuming it")
		c.update(cli, func() { c.active = false })
		go c.activate(cli)
	case gmproto.AlertType_MOBILE_DATABASE_SYNC_STARTED:
		c.in.setPhoneSyncing(true)
	case gmproto.AlertType_MOBILE_DATABASE_SYNC_COMPLETE:
		c.in.setPhoneSyncing(false)
		go c.in.sync(c.ctx, cli)
	}
}

// activate asks the phone to make cli's session the active one, unless it
// is already, or another browser took it over.
func (c *gmConnector) activate(cli gmClient) {
	c.mu.Lock()
	skip := cli != c.cli || c.active || c.elsewhere
	c.mu.Unlock()
	if skip {
		return
	}
	if err := cli.SetActiveSession(c.ctx); err != nil {
		c.log.Warn("resume session", "err", err)
	}
}

// update runs f under c.mu if cli is current.
func (c *gmConnector) update(cli gmClient, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cli == c.cli {
		f()
	}
}

// save stores the session after libgm refreshed its token.
func (c *gmConnector) save(cli gmClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cli != c.cli {
		return
	}
	if err := c.store(c.auth); err != nil {
		c.log.Error("save session", "err", err)
	}
}

// store saves auth, including the cookies libgm renews from its own
// goroutines under CookiesLock.
func (c *gmConnector) store(auth *libgm.AuthData) error {
	auth.CookiesLock.RLock()
	data, err := json.Marshal(auth)
	auth.CookiesLock.RUnlock()
	if err != nil {
		return err
	}
	return c.session.Store(data)
}

// unpair forgets cli's session after the phone or Google ended it.
func (c *gmConnector) unpair(cli gmClient, why string) {
	c.mu.Lock()
	if cli != c.cli {
		c.mu.Unlock()
		return
	}
	c.cli, c.auth, c.unpaired = nil, nil, why
	c.mu.Unlock()
	c.log.Warn(why + "; pair again from the console")
	// Disconnecting from libgm's own goroutine would close the stream it is
	// reading.
	go cli.Disconnect()
	if err := c.session.Delete(); err != nil {
		c.log.Error("delete session", "err", err)
	}
	c.in.reset()
}

// Unlink unpairs from the phone and forgets the session.
func (c *gmConnector) Unlink(ctx context.Context) error {
	c.mu.Lock()
	cli, p := c.cli, c.pairing
	c.pairing = nil
	c.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	if cli != nil {
		// Offline, the phone lists the pairing until it expires; the session
		// is forgotten either way.
		if err := cli.Unpair(ctx); err != nil {
			c.log.Warn("unpairing failed; forgetting the session anyway", "err", err)
		}
		cli.Disconnect()
	}
	if err := c.session.Delete(); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	c.mu.Lock()
	if c.cli == cli {
		c.cli, c.auth, c.unpaired = nil, nil, ""
	}
	c.mu.Unlock()
	c.in.reset()
	c.log.Info("unlinked")
	return nil
}

// pairing is one attempt to link. By QR code it shows a code every
// qrLifetime until one is scanned or qrCodes have expired; through the Google
// account it shows one emoji until the phone answers.
type pairing struct {
	// key tells pairings apart: the cookies of a Google account pairing,
	// empty for a QR one.
	key    string
	cancel context.CancelFunc
	ready  chan struct{} // closed at the first code or at the end

	// Guarded by gmConnector.mu.
	qr      string
	emoji   string
	done    bool
	expired bool
	err     error
}

func (p *pairing) shown() bool { return p.qr != "" || p.emoji != "" }

// Setup pairs through the Google account when given cookies, else by QR
// code. The console repeats the same args while it polls.
func (c *gmConnector) Setup(ctx context.Context, args map[string]string) (connector.SetupStep, error) {
	var cookies map[string]string
	for k, v := range args {
		if k != "cookies" {
			return connector.SetupStep{}, connector.Invalid("unknown arg %q", k)
		}
		if strings.TrimSpace(v) == "" {
			continue
		}
		var err error
		if cookies, err = parseCookies(v); err != nil {
			return connector.SetupStep{}, connector.Invalid("%s", err)
		}
	}
	key := cookieKey(cookies)
	for range 2 {
		p, step, err := c.pairingFor(key, cookies)
		if p == nil {
			return step, err
		}
		select {
		case <-p.ready:
		case <-ctx.Done():
			return connector.SetupStep{}, ctx.Err()
		case <-time.After(firstStepWait):
			return connector.SetupStep{}, &connector.Error{Status: http.StatusGatewayTimeout, Message: "google messages did not answer; try again"}
		}
		c.mu.Lock()
		qr, emoji, expired := p.qr, p.emoji, p.expired
		c.mu.Unlock()
		if expired {
			continue
		}
		if p, step, err := c.pairingFor(key, cookies); p == nil {
			return step, err
		}
		if emoji != "" {
			return connector.SetupStep{Code: emoji, Instructions: emojiInstructions}, nil
		}
		return connector.SetupStep{QR: qr, Instructions: qrInstructions}, nil
	}
	return connector.SetupStep{}, &connector.Error{Status: http.StatusGatewayTimeout, Message: "the QR codes expired; try again"}
}

func cookieKey(cookies map[string]string) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(cookies)) {
		b.WriteString(name + "=" + cookies[name] + ";")
	}
	return b.String()
}

// pairingFor returns the pairing in progress for key, starting one if
// needed, or the step or error that ends linking. A pairing the other way
// is dropped.
func (c *gmConnector) pairingFor(key string, cookies map[string]string) (*pairing, connector.SetupStep, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pairing
	switch {
	case p != nil && p.done:
		c.pairing = nil
		return nil, connector.SetupStep{Done: true}, nil
	case c.cli != nil:
		return nil, connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "already linked"}
	case p != nil && p.key == key && p.err != nil:
		c.pairing = nil
		return nil, connector.SetupStep{}, &connector.Error{Status: http.StatusBadGateway, Message: p.err.Error()}
	case p != nil && p.key == key && !p.expired:
		return p, connector.SetupStep{}, nil
	case p != nil:
		p.cancel()
	}
	ctx, cancel := context.WithCancel(c.ctx)
	p = &pairing{key: key, cancel: cancel, ready: make(chan struct{})}
	c.pairing = p
	if cookies != nil {
		go c.pairGoogle(ctx, p, cookies)
	} else {
		go c.pairQR(ctx, p)
	}
	return p, connector.SetupStep{}, nil
}

func (c *gmConnector) pairQR(ctx context.Context, p *pairing) {
	defer p.cancel()
	auth := libgm.NewAuthData()
	scanned := make(chan struct{}, 1)
	pc := c.newPairing(auth, func() {
		select {
		case scanned <- struct{}{}:
		default:
		}
	})
	err := pc.FetchConfig(ctx)
	var qr string
	if err == nil {
		qr, err = pc.StartLogin(ctx)
	}
	if err != nil {
		pc.Disconnect()
		c.log.Error("start pairing", "err", err)
		c.endPairing(p, fmt.Errorf("connect to google messages: %w", err))
		return
	}
	for n := 1; ; n++ {
		c.show(p, qr, "")
		select {
		case <-scanned:
			c.paired(p, pc, auth)
			return
		case <-ctx.Done():
			// Unlinked, replaced, or shutting down.
			pc.Disconnect()
			c.endPairing(p, nil)
			return
		case <-time.After(c.qrLifetime):
		}
		if n == c.qrCodes {
			pc.Disconnect()
			c.endPairing(p, nil)
			return
		}
		if qr, err = pc.RefreshPhoneRelay(); err != nil {
			pc.Disconnect()
			c.log.Error("refresh QR code", "err", err)
			c.endPairing(p, fmt.Errorf("refresh the QR code: %w", err))
			return
		}
	}
}

var errNotSignedIn = errors.New("these cookies are not signed in to Google Messages for Web; copy them again from a browser signed in to messages.google.com")

// pairGoogle pairs as the Google account of cookies: the phone shows
// emoji, and the owner taps the one shown here.
func (c *gmConnector) pairGoogle(ctx context.Context, p *pairing, cookies map[string]string) {
	defer p.cancel()
	auth := libgm.NewAuthData()
	auth.Cookies = cookies
	pc := c.newPairing(auth, func() {})
	fail := func(err error) {
		pc.Disconnect()
		if ctx.Err() == nil {
			c.log.Error("google account pairing", "err", err)
		}
		c.endPairing(p, googleError(err))
	}
	// The config names the account and the device ID pairing uses.
	if err := pc.FetchConfig(ctx); err != nil {
		fail(err)
		return
	}
	if pc.Account() == "" {
		fail(errNotSignedIn)
		return
	}
	emoji, ps, err := pc.StartGaiaPairing(ctx, ctx)
	if err != nil {
		fail(err)
		return
	}
	c.show(p, "", emoji)
	fctx, cancel := context.WithTimeout(ctx, emojiWait)
	_, err = pc.FinishGaiaPairing(fctx, ps)
	cancel()
	if err != nil {
		fail(err)
		return
	}
	c.paired(p, pc, auth)
}

// googleError says plainly why a Google account pairing failed.
func googleError(err error) error {
	var httpErr events.HTTPError
	switch {
	case errors.Is(err, errNotSignedIn):
		return err
	case errors.Is(err, libgm.ErrNoDevicesFound):
		return errors.New("no phone was found for this Google account; in Google Messages on the phone, check that Device pairing uses this Google account")
	case errors.Is(err, libgm.ErrPairingInitTimeout):
		return errors.New("the phone did not answer; open Google Messages on it, then try again")
	case errors.Is(err, events.ErrCallerNoPermission):
		return errors.New("this Google account cannot use Google Messages for Web")
	case errors.Is(err, libgm.ErrIncorrectEmoji):
		return errors.New("a different emoji was tapped on the phone; try again")
	case errors.Is(err, libgm.ErrPairingCancelled):
		return errors.New("pairing was cancelled on the phone")
	case errors.Is(err, libgm.ErrPairingTimeout), errors.Is(err, context.DeadlineExceeded):
		return errors.New("the phone did not confirm in time; try again")
	case errors.Is(err, events.ErrInvalidCredentials),
		errors.As(err, &httpErr) && (httpErr.Resp.StatusCode == http.StatusUnauthorized || httpErr.Resp.StatusCode == http.StatusForbidden):
		return errors.New("these cookies were not accepted by Google; copy them again from a browser signed in to messages.google.com")
	}
	return fmt.Errorf("pairing failed: %w", err)
}

func (c *gmConnector) show(p *pairing, qr, emoji string) {
	c.mu.Lock()
	first := !p.shown()
	p.qr, p.emoji = qr, emoji
	c.mu.Unlock()
	if first {
		close(p.ready)
	}
}

// paired stores the session the phone just paired, unless the pairing was
// dropped meanwhile, and connects with it.
func (c *gmConnector) paired(p *pairing, pc pairClient, auth *libgm.AuthData) {
	pc.Disconnect()
	c.mu.Lock()
	current := c.pairing == p && c.cli == nil
	var err error
	if current {
		if err = c.store(auth); err == nil {
			c.use(auth)
			p.done = true
		}
	}
	cli := c.cli
	c.mu.Unlock()
	if !current || err != nil {
		if err != nil {
			c.log.Error("save session", "err", err)
			c.endPairing(p, fmt.Errorf("save session: %w", err))
		}
		if err := pc.Unpair(c.ctx); err != nil {
			c.log.Warn("unpair a dropped pairing", "err", err)
		}
		return
	}
	c.log.Info("paired", "google_account", auth.IsGoogleAccount())
	if !auth.IsGoogleAccount() {
		select {
		case <-time.After(c.settleTime):
		case <-c.ctx.Done():
			return
		}
	}
	if c.isCurrent(cli) {
		c.connect(cli)
	}
	// Unlinked meanwhile: Unlink disconnected cli, perhaps before Connect.
	if !c.isCurrent(cli) {
		cli.Disconnect()
	}
}

func (c *gmConnector) isCurrent(cli gmClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cli == c.cli
}

func (c *gmConnector) endPairing(p *pairing, err error) {
	c.mu.Lock()
	switch {
	case p.done:
	case err == nil:
		p.expired = true
	default:
		p.err = err
	}
	first := !p.shown()
	c.mu.Unlock()
	if first {
		close(p.ready)
	}
}
