package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	qrInstructions = "Google Messages → your profile picture → Device pairing → QR code scanner, then scan this code"

	firstCodeWait = 30 * time.Second
	// A code is good for about 30 s; like mautrix-gmessages, a pairing shows
	// six before it expires.
	qrLifetime = 30 * time.Second
	qrCodes    = 6
	// The phone needs a moment to save the pairing before the first connect.
	settleTime   = 2 * time.Second
	activateWait = 7 * time.Second
	// Each ping wakes the phone; mautrix-gmessages pings every 20 minutes
	// rather than libgm's default of one.
	pingInterval = 20 * time.Minute
)

// gmClient is the part of *libgm.Client a paired session uses.
type gmClient interface {
	FetchConfig(ctx context.Context) error
	Connect(ctx context.Context) error
	Disconnect()
	Unpair(ctx context.Context) error
	SetActiveSession(ctx context.Context) error
	ListConversations(ctx context.Context, req *gmproto.ListConversationsRequest) (*gmproto.ListConversationsResponse, error)
	FetchMessages(ctx context.Context, conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error)
	GetConversation(ctx context.Context, conversationID string) (*gmproto.Conversation, error)
	SendMessage(ctx context.Context, req *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)
}

// pairClient is the part of *libgm.Client that shows QR codes.
type pairClient interface {
	FetchConfig(ctx context.Context) error
	StartLogin(ctx context.Context) (string, error)
	RefreshPhoneRelay() (string, error)
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
	// newPairing returns a client that calls paired once the phone has
	// scanned one of its codes and filled in auth.
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
	}
	// Connect fails only when Google refuses the session; network errors are
	// retried by libgm.
	if err := cli.Connect(c.ctx); err != nil {
		c.log.Error("connect", "err", err)
		c.unpair(cli, "Google Messages refused the pairing")
	}
}

func (c *gmConnector) disconnect() {
	c.mu.Lock()
	cli, p := c.cli, c.pairing
	c.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	if cli != nil {
		cli.Disconnect()
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
	case *gmproto.RevokePairData, *events.GaiaLoggedOut:
		c.unpair(cli, "unpaired from the phone")
	case *events.ListenFatalError:
		c.log.Error("google messages refused the session", "err", e.Error)
		c.unpair(cli, "Google Messages refused the pairing")
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

func (c *gmConnector) store(auth *libgm.AuthData) error {
	data, err := json.Marshal(auth)
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

// pairing is one QR session: a code every qrLifetime, until one is scanned
// or qrCodes have expired.
type pairing struct {
	cancel context.CancelFunc
	ready  chan struct{} // closed at the first code or at the end

	// Guarded by gmConnector.mu.
	qr      string
	done    bool
	expired bool
	err     error
}

func (c *gmConnector) Setup(ctx context.Context, args map[string]string) (connector.SetupStep, error) {
	for k := range args {
		return connector.SetupStep{}, connector.Invalid("unknown arg %q", k)
	}
	for range 2 {
		p, step, err := c.pairingFor()
		if p == nil {
			return step, err
		}
		select {
		case <-p.ready:
		case <-ctx.Done():
			return connector.SetupStep{}, ctx.Err()
		case <-time.After(firstCodeWait):
			return connector.SetupStep{}, &connector.Error{Status: http.StatusGatewayTimeout, Message: "google messages sent no QR code; try again"}
		}
		c.mu.Lock()
		qr, expired := p.qr, p.expired
		c.mu.Unlock()
		if expired {
			continue
		}
		if p, step, err := c.pairingFor(); p == nil {
			return step, err
		}
		return connector.SetupStep{QR: qr, Instructions: qrInstructions}, nil
	}
	return connector.SetupStep{}, &connector.Error{Status: http.StatusGatewayTimeout, Message: "the QR codes expired; try again"}
}

// pairingFor returns the pairing in progress, starting one if needed, or
// the step or error that ends linking.
func (c *gmConnector) pairingFor() (*pairing, connector.SetupStep, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pairing
	switch {
	case p != nil && p.done:
		c.pairing = nil
		return nil, connector.SetupStep{Done: true}, nil
	case c.cli != nil:
		return nil, connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "already linked"}
	case p != nil && p.err != nil:
		c.pairing = nil
		return nil, connector.SetupStep{}, &connector.Error{Status: http.StatusBadGateway, Message: p.err.Error()}
	case p != nil && !p.expired:
		return p, connector.SetupStep{}, nil
	}
	ctx, cancel := context.WithCancel(c.ctx)
	p = &pairing{cancel: cancel, ready: make(chan struct{})}
	c.pairing = p
	go c.pair(ctx, p)
	return p, connector.SetupStep{}, nil
}

func (c *gmConnector) pair(ctx context.Context, p *pairing) {
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
		c.mu.Lock()
		first := p.qr == ""
		p.qr = qr
		c.mu.Unlock()
		if first {
			close(p.ready)
		}
		select {
		case <-scanned:
			c.paired(p, pc, auth)
			return
		case <-ctx.Done():
			// Unlinked, or shutting down.
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

// paired stores the session the phone just paired, unless Unlink dropped
// the pairing meanwhile, and connects with it.
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
	c.log.Info("paired")
	select {
	case <-time.After(c.settleTime):
	case <-c.ctx.Done():
		return
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
	first := p.qr == ""
	c.mu.Unlock()
	if first {
		close(p.ready)
	}
}
