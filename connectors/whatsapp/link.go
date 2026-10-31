package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	qrInstructions    = "WhatsApp → Settings → Linked devices → Link a device, then scan this code"
	phoneInstructions = "WhatsApp → Settings → Linked devices → Link a device → Link with phone number instead, then enter this code"
	outdated          = "WhatsApp rejected this client version; update mandate"

	firstCodeWait = 30 * time.Second
)

// waClient is the part of *whatsmeow.Client the connector uses.
type waClient interface {
	IsConnected() bool
	IsLoggedIn() bool
	SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType whatsmeow.PairClientType, clientDisplayName string) (string, error)
	ConnectContext(ctx context.Context) error
	Disconnect()
}

// waConnector links the device from the console and sends approved text
// messages.
type waConnector struct {
	st  *store.Store
	log *slog.Logger
	// ctx outlives requests: the connection and pairing run until shutdown.
	ctx       context.Context
	newDevice func() *wastore.Device
	// newClient wires onEvent, then ingest, into a client for dev.
	newClient func(dev *wastore.Device, onEvent func(evt any)) waClient

	mu sync.Mutex
	// Replaced after a logout: whatsmeow deletes a logged-out device, and a
	// deleted device cannot pair again.
	cli waClient
	dev *wastore.Device
	// linked is tracked here rather than read from dev.ID, which whatsmeow
	// writes from its own goroutines.
	linked    bool
	loggedOut bool
	failure   string // why whatsmeow stopped reconnecting
	pairing   *pairing
}

func newConnector(ctx context.Context, st *store.Store, log *slog.Logger, dev *wastore.Device,
	newDevice func() *wastore.Device, newClient func(*wastore.Device, func(any)) waClient,
) *waConnector {
	c := &waConnector{st: st, log: log, ctx: ctx, newDevice: newDevice, newClient: newClient}
	c.use(dev)
	c.linked = dev.ID != nil
	return c
}

// use needs c.mu, except during construction.
func (c *waConnector) use(dev *wastore.Device) {
	var cli waClient
	cli = c.newClient(dev, func(evt any) { c.event(cli, evt) })
	c.cli, c.dev = cli, dev
}

func (c *waConnector) current() (waClient, *wastore.Device, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cli, c.dev, c.linked
}

// connect connects a linked device; an unlinked one waits for Setup.
func (c *waConnector) connect() error {
	cli, _, linked := c.current()
	if !linked {
		c.log.Warn("not linked; link it from the console")
		return nil
	}
	c.log.Info("connecting")
	if err := cli.ConnectContext(c.ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

func (c *waConnector) disconnect() {
	cli, _, _ := c.current()
	cli.Disconnect()
}

func (c *waConnector) Health(context.Context) connector.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.linked && c.loggedOut:
		return connector.Health{State: connector.NeedsSetup, Detail: "logged out from the phone"}
	case !c.linked:
		return connector.Health{State: connector.NeedsSetup, Detail: "not linked"}
	case c.failure != "":
		return connector.Health{State: connector.Broken, Detail: c.failure}
	case c.cli.IsConnected() && c.cli.IsLoggedIn():
		return connector.Health{State: connector.Healthy}
	default:
		return connector.Health{State: connector.Broken, Detail: "connecting…"}
	}
}

// event handles events from cli after which whatsmeow does not reconnect.
func (c *waConnector) event(cli waClient, evt any) {
	if _, ok := evt.(events.PermanentDisconnect); !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cli != c.cli {
		return
	}
	switch e := evt.(type) {
	case *events.LoggedOut:
		c.log.Warn("logged out from the phone; link again from the console", "reason", e.Reason.String())
		c.linked, c.loggedOut, c.failure, c.pairing = false, true, "", nil
		c.use(c.newDevice())
		// Disconnect takes the socket lock, which whatsmeow may hold while
		// dispatching.
		go cli.Disconnect()
	case *events.ClientOutdated:
		c.log.Error("whatsapp rejected this client version")
		c.failure = outdated
	case *events.StreamReplaced:
		c.log.Error("session taken over by another client using the same session file")
		c.failure = "session taken over by another client using the same session file; stop it, then restart mandate-whatsapp"
	case events.PermanentDisconnect:
		d := e.PermanentDisconnectDescription()
		c.log.Error("permanent disconnect", "reason", d)
		c.failure = d + "; restart mandate-whatsapp"
	}
}

// pairing is one QR session: WhatsApp rotates its codes until one is
// scanned or they run out.
type pairing struct {
	cli   waClient
	ready chan struct{} // closed at the first code or at the end

	// Guarded by waConnector.mu.
	qr      string
	done    bool
	expired bool
	err     error

	phoneMu   sync.Mutex // one pairing-code request at a time
	phone     string
	phoneCode string
}

func (c *waConnector) Setup(ctx context.Context, args map[string]string) (connector.SetupStep, error) {
	var phone string
	for k, v := range args {
		if k != "phone" {
			return connector.SetupStep{}, connector.Invalid("unknown arg %q", k)
		}
		d, ok := phoneDigits(v)
		if !ok {
			return connector.SetupStep{}, connector.Invalid("phone must be a number with country code, e.g. +15551234567")
		}
		phone = "+" + d
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
			return connector.SetupStep{}, &connector.Error{Status: http.StatusGatewayTimeout, Message: "whatsapp sent no QR code; try again"}
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
		if phone == "" {
			return connector.SetupStep{QR: qr, Instructions: qrInstructions}, nil
		}
		code, err := c.phoneCode(ctx, p, phone)
		if err != nil {
			return connector.SetupStep{}, err
		}
		return connector.SetupStep{Code: code, Instructions: phoneInstructions}, nil
	}
	return connector.SetupStep{}, &connector.Error{Status: http.StatusGatewayTimeout, Message: "the QR codes expired; try again"}
}

// pairingFor returns the pairing in progress, starting one if needed, or
// the step or error that ends linking.
func (c *waConnector) pairingFor() (*pairing, connector.SetupStep, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pairing
	switch {
	case p != nil && p.done:
		c.pairing = nil
		return nil, connector.SetupStep{Done: true}, nil
	case c.linked:
		return nil, connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "already linked"}
	case p != nil && p.err != nil:
		c.pairing = nil
		return nil, connector.SetupStep{}, &connector.Error{Status: http.StatusBadGateway, Message: p.err.Error()}
	case p != nil && !p.expired:
		return p, connector.SetupStep{}, nil
	}
	p = &pairing{cli: c.cli, ready: make(chan struct{})}
	c.pairing = p
	go c.pair(p)
	return p, connector.SetupStep{}, nil
}

func (c *waConnector) pair(p *pairing) {
	ch, err := p.cli.GetQRChannel(c.ctx)
	if err == nil {
		err = p.cli.ConnectContext(c.ctx)
	}
	if err != nil {
		c.log.Error("start pairing", "err", err)
		p.cli.Disconnect()
		c.endPairing(p, fmt.Errorf("connect to whatsapp: %w", err))
		return
	}
	for item := range ch {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			c.mu.Lock()
			first := p.qr == ""
			p.qr = item.Code
			c.mu.Unlock()
			if first {
				close(p.ready)
			}
			continue
		case whatsmeow.QRChannelSuccess.Event:
			c.mu.Lock()
			c.linked, c.loggedOut, c.failure = true, false, ""
			p.done = true
			c.mu.Unlock()
			c.log.Info("linked")
			c.endPairing(p, nil)
			return
		case whatsmeow.QRChannelTimeout.Event:
			// The codes ran out: expired, and the next Setup starts again.
		case whatsmeow.QRChannelClientOutdated.Event:
			err = errors.New(outdated)
		case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
			err = errors.New("the phone asked for passkey pairing, which mandate does not support; link with the QR code or a phone number")
		case whatsmeow.QRChannelEventError:
			err = fmt.Errorf("pairing failed: %w", item.Error)
		default:
			err = fmt.Errorf("pairing failed: %s", item.Event)
		}
		break
	}
	// whatsmeow disconnects on some of these, but only after closing the
	// channel; the next pairing needs the socket closed.
	p.cli.Disconnect()
	c.endPairing(p, err)
}

// endPairing ends p with err, or as expired unless it succeeded.
func (c *waConnector) endPairing(p *pairing, err error) {
	c.mu.Lock()
	switch {
	case err != nil:
		p.err = err
	case !p.done:
		p.expired = true
	}
	first := p.qr == ""
	c.mu.Unlock()
	if first {
		close(p.ready)
	}
}

// phoneCode requests a pairing code for phone once per pairing; each request
// notifies the phone.
func (c *waConnector) phoneCode(ctx context.Context, p *pairing, phone string) (string, error) {
	p.phoneMu.Lock()
	defer p.phoneMu.Unlock()
	if p.phone == phone {
		return p.phoneCode, nil
	}
	code, err := p.cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return "", &connector.Error{Status: http.StatusBadGateway, Message: "request pairing code: " + err.Error()}
	}
	p.phone, p.phoneCode = phone, code
	return code, nil
}
