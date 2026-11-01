package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	sgstore "go.mau.fi/mautrix-signal/pkg/signalmeow/store"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	qrInstructions = "Signal → Settings → Linked devices → Link new device, then scan this code"

	firstCodeWait = 30 * time.Second
	unlinkTimeout = 10 * time.Second
)

type sgClient interface {
	lookup
	StartReceiveLoops(ctx context.Context) (chan signalmeow.SignalConnectionStatus, error)
	StopReceiveLoops() error
	QueueStorageSync(ctx context.Context)
	Unlink(ctx context.Context) error
	SendMessage(ctx context.Context, to libsignalgo.ServiceID, content *signalpb.Content) signalmeow.SendMessageResult
	SendGroupMessage(ctx context.Context, gid types.GroupIdentifier, content *signalpb.Content) (*signalmeow.GroupMessageSendResult, error)
}

// devices is the session's device store.
type devices interface {
	DeviceByACI(ctx context.Context, aci uuid.UUID) (*sgstore.Device, error)
	DeleteDevice(ctx context.Context, dev *sgstore.DeviceData) error
}

type sgConnector struct {
	st         *store.Store
	log        *slog.Logger
	devices    devices
	timers     *timers
	identities *identities
	// ctx outlives requests: the connection and linking run until shutdown.
	ctx       context.Context
	provision func(ctx context.Context) <-chan signalmeow.ProvisioningResponse
	// newClient wires onEvent, then ingest, into a client for dev.
	newClient func(dev *sgstore.Device, onEvent func(events.SignalEvent)) sgClient

	mu sync.Mutex
	// cli and dev are nil until linked.
	cli           sgClient
	dev           *sgstore.Device
	connected     bool
	everConnected bool
	loggedOut     bool
	pairing       *pairing
}

func newConnector(ctx context.Context, st *store.Store, log *slog.Logger, sess *session, dev *sgstore.Device,
	provision func(context.Context) <-chan signalmeow.ProvisioningResponse,
	newClient func(*sgstore.Device, func(events.SignalEvent)) sgClient,
) *sgConnector {
	c := &sgConnector{
		st: st, log: log, devices: sess.devices, timers: sess.timers, identities: sess.identities,
		ctx: ctx, provision: provision, newClient: newClient,
	}
	if dev != nil {
		c.use(dev)
	}
	return c
}

// use needs c.mu, except during construction.
func (c *sgConnector) use(dev *sgstore.Device) {
	var cli sgClient
	cli = c.newClient(dev, func(evt events.SignalEvent) { c.event(cli, evt) })
	c.cli, c.dev = cli, dev
}

func (c *sgConnector) current() (sgClient, *sgstore.Device, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cli, c.dev, c.connected
}

func (c *sgConnector) connect() error {
	cli, _, _ := c.current()
	if cli == nil {
		c.log.Warn("not linked; link it from the console")
		return nil
	}
	c.log.Info("connecting")
	return c.start(cli)
}

func (c *sgConnector) start(cli sgClient) error {
	ch, err := cli.StartReceiveLoops(c.ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	go func() {
		for s := range ch {
			c.status(cli, s)
		}
	}()
	return nil
}

func (c *sgConnector) disconnect() {
	cli, _, _ := c.current()
	if cli != nil {
		if err := cli.StopReceiveLoops(); err != nil {
			c.log.Warn("disconnect", "err", err)
		}
	}
}

func (c *sgConnector) Health(context.Context) connector.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.dev == nil && c.loggedOut:
		return connector.Health{State: connector.NeedsSetup, Detail: "logged out from the phone"}
	case c.dev == nil:
		return connector.Health{State: connector.NeedsSetup, Detail: "not linked"}
	case c.connected:
		return connector.Health{State: connector.Healthy}
	case c.everConnected:
		return connector.Health{State: connector.Broken, Detail: "reconnecting…"}
	default:
		return connector.Health{State: connector.Broken, Detail: "connecting…"}
	}
}

func (c *sgConnector) status(cli sgClient, s signalmeow.SignalConnectionStatus) {
	if s.Event == signalmeow.SignalConnectionEventLoggedOut {
		c.loggedOutFrom(cli, true)
		return
	}
	c.mu.Lock()
	if cli != c.cli {
		c.mu.Unlock()
		return
	}
	c.connected = s.Event == signalmeow.SignalConnectionEventConnected
	c.everConnected = c.everConnected || c.connected
	c.mu.Unlock()
	switch s.Event {
	case signalmeow.SignalConnectionEventConnected:
		c.log.Info("connected")
		// Contact names come from the storage service.
		cli.QueueStorageSync(c.ctx)
	case signalmeow.SignalConnectionEventDisconnected, signalmeow.SignalConnectionEventError,
		signalmeow.SignalConnectionEventFatalError:
		c.log.Warn("disconnected; signalmeow will reconnect", "err", s.Err)
	}
}

// event handles events from cli that end the session.
func (c *sgConnector) event(cli sgClient, evt events.SignalEvent) {
	if _, ok := evt.(*events.LoggedOut); ok {
		// signalmeow has already stopped cli.
		c.loggedOutFrom(cli, false)
	}
}

func (c *sgConnector) loggedOutFrom(cli sgClient, stop bool) {
	c.mu.Lock()
	if cli != c.cli {
		c.mu.Unlock()
		return
	}
	dev := c.dev
	c.cli, c.dev, c.connected, c.everConnected, c.loggedOut = nil, nil, false, false, true
	c.mu.Unlock()
	c.log.Warn("logged out from the phone; link again from the console")
	if stop {
		if err := cli.StopReceiveLoops(); err != nil {
			c.log.Warn("disconnect", "err", err)
		}
	}
	if err := c.devices.DeleteDevice(c.ctx, &dev.DeviceData); err != nil {
		c.log.Error("delete session", "err", err)
	}
}

// Unlink removes the device from the account and forgets the session.
func (c *sgConnector) Unlink(ctx context.Context) error {
	c.mu.Lock()
	cli, dev, p := c.cli, c.dev, c.pairing
	c.pairing = nil
	c.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	if dev != nil {
		if err := c.forget(ctx, cli, dev); err != nil {
			return err
		}
	}
	c.mu.Lock()
	if c.cli == cli {
		c.cli, c.dev, c.connected, c.everConnected, c.loggedOut = nil, nil, false, false, false
	}
	c.mu.Unlock()
	c.log.Info("unlinked")
	return nil
}

// forget removes dev from the account, best effort, and deletes its session.
func (c *sgConnector) forget(ctx context.Context, cli sgClient, dev *sgstore.Device) error {
	// Offline the phone keeps listing the device until it expires; the
	// session is forgotten either way.
	uctx, cancel := context.WithTimeout(ctx, unlinkTimeout)
	err := cli.Unlink(uctx)
	cancel()
	if err != nil {
		c.log.Warn("removing the linked device failed; forgetting the session anyway", "err", err)
	}
	if err := cli.StopReceiveLoops(); err != nil {
		c.log.Warn("disconnect", "err", err)
	}
	if err := c.devices.DeleteDevice(ctx, &dev.DeviceData); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// pairing is one provisioning session: one QR code, valid until scanned or
// until Signal closes the session.
type pairing struct {
	cancel context.CancelFunc
	ready  chan struct{} // closed at the code or at the end

	// Guarded by sgConnector.mu.
	qr      string
	done    bool
	expired bool
	err     error
}

func (c *sgConnector) Setup(ctx context.Context, args map[string]string) (connector.SetupStep, error) {
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
			return connector.SetupStep{}, &connector.Error{Status: http.StatusGatewayTimeout, Message: "signal sent no QR code; try again"}
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
	return connector.SetupStep{}, &connector.Error{Status: http.StatusGatewayTimeout, Message: "the QR code expired; try again"}
}

// pairingFor returns the pairing in progress, starting one if needed, or
// the step or error that ends linking.
func (c *sgConnector) pairingFor() (*pairing, connector.SetupStep, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pairing
	switch {
	case p != nil && p.done:
		c.pairing = nil
		return nil, connector.SetupStep{Done: true}, nil
	case c.dev != nil:
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

func (c *sgConnector) pair(ctx context.Context, p *pairing) {
	defer p.cancel()
	var data *sgstore.DeviceData
	var err error
	for resp := range c.provision(ctx) {
		switch {
		case resp.Err != nil:
			err = resp.Err
		case resp.State == signalmeow.StateProvisioningURLReceived:
			c.mu.Lock()
			p.qr = resp.ProvisioningURL
			c.mu.Unlock()
			close(p.ready)
			continue
		case resp.State == signalmeow.StateProvisioningDataReceived:
			data = resp.ProvisioningData
		default:
			err = fmt.Errorf("unexpected provisioning state %s", resp.State)
		}
		break
	}
	if data == nil {
		c.endPairing(p, linkError(err))
		return
	}
	dev, err := c.devices.DeviceByACI(c.ctx, data.ACI)
	if err == nil && dev == nil {
		err = errors.New("device not stored")
	}
	if err != nil {
		c.log.Error("load linked device", "err", err)
		c.endPairing(p, fmt.Errorf("load linked device: %w", err))
		return
	}
	c.mu.Lock()
	// Unlink drops the pairing in progress.
	current := c.pairing == p && c.dev == nil
	if current {
		c.use(dev)
		c.connected, c.everConnected, c.loggedOut = false, false, false
		p.done = true
	}
	cli := c.cli
	c.mu.Unlock()
	if !current {
		if err := c.devices.DeleteDevice(c.ctx, &dev.DeviceData); err != nil {
			c.log.Error("forget the device of a dropped pairing", "err", err)
		}
		return
	}
	c.log.Info("linked")
	c.endPairing(p, nil)
	if err := c.start(cli); err != nil {
		c.log.Error("connect after linking", "err", err)
	}
}

func (c *sgConnector) endPairing(p *pairing, err error) {
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

// linkError is nil when the code expired unscanned: the next Setup shows a
// new one.
func linkError(err error) error {
	var le signalmeow.DeviceLinkError
	switch {
	case err == nil, errors.Is(err, context.DeadlineExceeded), websocket.CloseStatus(err) == websocket.StatusGoingAway:
		return nil
	case errors.Is(err, context.Canceled):
		return err
	case errors.As(err, &le) && le.StatusCode == http.StatusLengthRequired:
		return errors.New("this account has the most linked devices Signal allows; remove one on the phone and try again")
	case errors.As(err, &le) && le.StatusCode == http.StatusTooManyRequests:
		return errors.New("signal is rate-limiting linking; wait a few minutes and try again")
	}
	return fmt.Errorf("linking failed: %w", err)
}
