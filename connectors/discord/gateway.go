package main

import (
	"bufio"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"

	"github.com/adeci/mandate/store"
)

// The gateway connection is what a Discord tab left open in the background
// keeps: identified, idle, heartbeating, subscribed to nothing. It stores the
// messages Discord pushes anyway. It does not count against the REST budget.
const (
	defaultGatewayURL   = "wss://gateway.discord.gg"
	gatewayQuery        = "encoding=json&v=9&compress=zlib-stream"
	gatewayCapabilities = 1767421

	helloTimeout   = 30 * time.Second // the web client's
	resumeWithin   = 3 * time.Minute  // a session not acked for longer is identified anew
	backoffMin     = time.Second
	backoffMax     = time.Minute
	backoffGiveUp  = 4  // failed reconnects after which the session is identified anew
	identifyBudget = 50 // identifies per budgetWindow; a tab needs a few a day
	writeTimeout   = 10 * time.Second
	maxFrame       = 32 << 20
	qosVersion     = 31
	callTimeout    = 2 * time.Second // longest a fetch waits to hand over op 13
)

const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opPresence       = 3
	opVoiceState     = 4
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatAck   = 11
	opCallConnect    = 13
	opQoSHeartbeat   = 40
	opTimeSpent      = 41
)

// Close codes: 4004 is a rejected token; 4000 is how the web client drops a
// connection it wants to resume.
const (
	closeAuthFailed = 4004
	closeResume     = 4000
	// closeNone sends a close frame without a code, as WebSocket.close() does
	// when the web client logs out.
	closeNone = websocket.CloseNoStatusReceived
)

// tab is the browser tab the connection belongs to, as the worker last saw it.
type tab struct {
	token          string
	buildNumber    int
	launchID       string
	installationID string
	heartbeatID    string
	heartbeatStart time.Time
}

type dialFunc func(ctx context.Context, url string, h fhttp.Header) (*websocket.Conn, error)

// gateway methods other than run are called from the worker goroutine.
type gateway struct {
	url   string // without the query
	dial  dialFunc
	clock clock
	rng   rng
	store *store.Store
	log   *slog.Logger
	// rejected carries a token Discord refused with 4004 to the worker.
	rejected chan string

	mu         sync.Mutex
	tab        tab
	status     string // what Health shows; "" while connected or stopped
	identifies []time.Time
	cancel     context.CancelFunc // nil when stopped
	done       chan struct{}      // closed when run returns
	closeCode  int
	poke       chan struct{} // the tab changed
	calls      chan callReq
}

type callReq struct {
	channelID string
	done      chan struct{}
}

func newGateway(gwURL string, dial dialFunc, c clock, r rng, st *store.Store, log *slog.Logger) *gateway {
	return &gateway{
		url: gwURL, dial: dial, clock: c, rng: r, store: st, log: log,
		rejected: make(chan string, 1),
	}
}

// start connects with t, or hands t to the running connection.
func (g *gateway) start(t tab) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil && g.tab.token == t.token {
		g.tab = t
		g.wake()
		return
	}
	g.stopLocked(closeNone)
	g.tab = t
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel, g.done = cancel, make(chan struct{})
	g.poke, g.calls = make(chan struct{}, 1), make(chan callReq)
	g.status = "live updates connecting"
	go g.run(ctx, g.done)
}

// reload is the tab loading the app page again: a new connection with a new
// identify.
func (g *gateway) reload(t tab) {
	g.mu.Lock()
	g.stopLocked(websocket.CloseGoingAway)
	g.mu.Unlock()
	g.start(t)
}

// stop closes the connection with code and waits for it to end.
func (g *gateway) stop(code int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopLocked(code)
}

func (g *gateway) stopLocked(code int) {
	if g.cancel == nil {
		return
	}
	g.closeCode = code
	g.cancel()
	done := g.done
	g.mu.Unlock()
	<-done
	g.mu.Lock()
	g.cancel, g.status = nil, ""
}

func (g *gateway) wake() {
	select {
	case g.poke <- struct{}{}:
	default:
	}
}

func (g *gateway) health() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.status
}

func (g *gateway) setStatus(s string) {
	g.mu.Lock()
	g.status = s
	g.mu.Unlock()
}

func (g *gateway) currentTab() tab {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tab
}

// callConnect sends op 13 for a DM about to be opened, as the tab does, and
// returns once it is out or cannot go out soon.
func (g *gateway) callConnect(channelID string) {
	g.mu.Lock()
	running, calls, done := g.cancel != nil, g.calls, g.done
	g.mu.Unlock()
	if !running {
		return
	}
	req := callReq{channelID: channelID, done: make(chan struct{})}
	timeout := time.NewTimer(callTimeout)
	defer timeout.Stop()
	select {
	case calls <- req:
	case <-done:
		return
	case <-timeout.C:
		return
	}
	select {
	case <-req.done:
	case <-done:
	case <-timeout.C:
	}
}

// session is what survives reconnects within one run.
type session struct {
	id        string
	seq       int64
	resumeURL string
	lastAck   time.Time
	selfID    string
	status    string // the presence status last sent, "" if none
	announced string // the analytics session last sent in op 41
	// fastLaunch is the launch ID of the page's fast-connect script; it
	// identifies the first connection after a page load.
	fastLaunch string
}

func (s *session) resumable(now time.Time) bool {
	return s.id != "" && now.Sub(s.lastAck) <= resumeWithin
}

func (s *session) reset() { s.id, s.seq = "", 0 }

// backoff is the web client's gateway backoff: each failure adds up to twice
// the current delay, capped at backoffMax.
type backoff struct {
	current time.Duration
	fails   int
}

func (b *backoff) fail(r rng) time.Duration {
	b.fails++
	b.current = min(b.current+time.Duration(2*float64(b.current)*r.Float64()), backoffMax)
	return b.current
}

func (b *backoff) succeed() { b.current, b.fails = backoffMin, 0 }

// errReconnect is op 7: reconnect at once and resume.
var errReconnect = errors.New("reconnect requested")

func (g *gateway) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	s := &session{fastLaunch: newUUID()}
	bo := backoff{current: backoffMin}
	for {
		err := g.connect(ctx, s, &bo)
		if ctx.Err() != nil {
			return
		}
		var ce *websocket.CloseError
		switch {
		case errors.As(err, &ce) && ce.Code == closeAuthFailed:
			t := g.currentTab()
			g.log.Warn("discord gateway rejected the token")
			select {
			case g.rejected <- t.token:
			default:
			}
			g.setStatus("")
			g.idle(ctx, nil) // until the worker, which discards the token, stops us
			return
		case errors.Is(err, errReconnect):
			continue
		}
		wait := bo.fail(g.rng)
		if bo.fails > backoffGiveUp {
			s.reset()
		}
		g.log.Info("discord gateway disconnected", "err", err, "retry_in", wait.Round(time.Millisecond))
		g.setStatus("live updates reconnecting")
		if !g.idle(ctx, g.clock.After(wait)) {
			return
		}
	}
}

// idle waits for wake while answering op 13 requests, which cannot go out.
func (g *gateway) idle(ctx context.Context, wake <-chan time.Time) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-wake:
			return true
		case req := <-g.calls:
			close(req.done)
		}
	}
}

// identifyAllowed waits until the identify budget has room.
func (g *gateway) identifyAllowed(ctx context.Context) bool {
	now := g.clock.Now()
	g.mu.Lock()
	g.identifies = since(g.identifies, now.Add(-budgetWindow))
	full := len(g.identifies) >= identifyBudget
	var until time.Time
	if full {
		until = g.identifies[0].Add(budgetWindow)
		g.status = "live updates paused after too many reconnects"
	}
	g.mu.Unlock()
	if !full {
		return true
	}
	g.log.Warn("discord gateway identify budget spent; pausing", "until", until.Format(time.RFC3339))
	return g.idle(ctx, g.clock.After(until.Sub(now))) && g.identifyAllowed(ctx)
}

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

type inbound struct {
	p   payload
	err error
}

// frames reads one zlib stream across the binary messages of a connection.
type frames struct {
	c *websocket.Conn
	r io.Reader
}

func (f *frames) Read(p []byte) (int, error) {
	for {
		if f.r != nil {
			n, err := f.r.Read(p)
			if errors.Is(err, io.EOF) {
				f.r = nil
				err = nil
			}
			if n > 0 || err != nil {
				return n, err
			}
			continue
		}
		_, r, err := f.c.NextReader()
		if err != nil {
			return 0, err
		}
		f.r = r
	}
}

// read decodes payloads with one inflater for the whole connection.
func read(c *websocket.Conn, out chan<- inbound, quit <-chan struct{}) {
	zr, err := zlib.NewReader(bufio.NewReader(&frames{c: c}))
	if err == nil {
		dec := json.NewDecoder(zr)
		for {
			var p payload
			if err = dec.Decode(&p); err != nil {
				break
			}
			select {
			case out <- inbound{p: p}:
			case <-quit:
				return
			}
		}
	}
	select {
	case out <- inbound{err: err}:
	case <-quit:
	}
}

// conn is one websocket connection; only the run goroutine uses it.
type conn struct {
	g        *gateway
	ws       *websocket.Conn
	s        *session
	hello    <-chan time.Time // nil once HELLO came
	interval time.Duration
	beat     <-chan time.Time
	acked    bool
	lastBeat time.Time
	ready    bool
}

func (g *gateway) connect(ctx context.Context, s *session, bo *backoff) error {
	resume := s.resumable(g.clock.Now())
	if !resume {
		s.reset()
		if !g.identifyAllowed(ctx) {
			return ctx.Err()
		}
	}
	base := g.url
	if s.resumeURL != "" {
		base = s.resumeURL
	}
	ws, err := g.dial(ctx, base+"/?"+gatewayQuery, gatewayHeaders())
	if err != nil {
		s.resumeURL = "" // the web client forgets it on any socket error
		return err
	}
	ws.SetReadLimit(maxFrame)
	in := make(chan inbound)
	quit := make(chan struct{})
	reader := make(chan struct{})
	go func() {
		defer close(reader)
		read(ws, in, quit)
	}()
	c := &conn{g: g, ws: ws, s: s}
	err = c.serve(ctx, in, resume, bo)
	code := closeResume
	if ctx.Err() != nil {
		g.mu.Lock()
		code = g.closeCode
		g.mu.Unlock()
	}
	var ce *websocket.CloseError
	if !errors.As(err, &ce) {
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
	}
	close(quit)
	ws.Close()
	<-reader
	return err
}

func (c *conn) serve(ctx context.Context, in <-chan inbound, resume bool, bo *backoff) error {
	g, s := c.g, c.s
	// The web client sends RESUME or IDENTIFY as soon as the socket opens.
	if err := c.identifyOrResume(resume); err != nil {
		return err
	}
	c.hello = g.clock.After(helloTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.hello:
			s.resumeURL = ""
			return errors.New("no hello in time")
		case <-c.beat:
			if !c.acked {
				return errors.New("heartbeat not acknowledged")
			}
			c.acked = false
			if err := c.heartbeat(); err != nil {
				return err
			}
			c.beat = g.clock.After(c.interval)
		case <-g.poke:
			if err := c.tabChanged(); err != nil {
				return err
			}
		case req := <-g.calls:
			err := error(nil)
			if c.ready {
				err = c.send(opCallConnect, struct {
					ChannelID string `json:"channel_id"`
				}{req.channelID})
			}
			close(req.done)
			if err != nil {
				return err
			}
		case m := <-in:
			if m.err != nil {
				return m.err
			}
			if err := c.handle(ctx, m.p, bo); err != nil {
				return err
			}
		}
	}
}

func (c *conn) identifyOrResume(resume bool) error {
	t := c.g.currentTab()
	if resume {
		return c.send(opResume, struct {
			Token     string `json:"token"`
			SessionID string `json:"session_id"`
			Seq       int64  `json:"seq"`
		}{t.token, c.s.id, c.s.seq})
	}
	launch, fast := t.launchID, c.s.fastLaunch != ""
	if fast {
		launch = c.s.fastLaunch
	}
	if err := c.send(opIdentify, struct {
		Token        string             `json:"token"`
		Capabilities int                `json:"capabilities"`
		Properties   identifyProperties `json:"properties"`
		ClientState  struct {
			GuildVersions struct{} `json:"guild_versions"`
		} `json:"client_state"`
	}{
		Token:        t.token,
		Capabilities: gatewayCapabilities,
		Properties:   newIdentifyProperties(t.buildNumber, launch, t.installationID, fast),
	}); err != nil {
		return err
	}
	c.s.fastLaunch = ""
	c.g.mu.Lock()
	c.g.identifies = append(c.g.identifies, c.g.clock.Now())
	c.g.mu.Unlock()
	return nil
}

func (c *conn) send(op int, d any) error {
	b, err := json.Marshal(struct {
		Op int `json:"op"`
		D  any `json:"d"`
	}{op, d})
	if err != nil {
		return err
	}
	if err := c.ws.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

type qos struct {
	Active  bool     `json:"active"`
	Ver     int      `json:"ver"`
	Reasons []string `json:"reasons"`
}

// heartbeat is an idle tab's: in the background, not active.
func (c *conn) heartbeat() error {
	c.lastBeat = c.g.clock.Now()
	return c.send(opQoSHeartbeat, struct {
		Seq int64 `json:"seq"`
		QoS qos   `json:"qos"`
	}{c.s.seq, qos{Ver: qosVersion, Reasons: []string{}}})
}

// handle acts on one payload.
func (c *conn) handle(ctx context.Context, p payload, bo *backoff) error {
	g, s := c.g, c.s
	if p.S != nil {
		s.seq = *p.S
	}
	switch p.Op {
	case opHello:
		var h struct {
			Interval int64 `json:"heartbeat_interval"`
		}
		if err := json.Unmarshal(p.D, &h); err != nil || h.Interval < 1000 || h.Interval > 600_000 {
			return errors.New("hello without a usable heartbeat interval")
		}
		c.hello = nil
		c.interval = time.Duration(h.Interval) * time.Millisecond
		// The first heartbeat comes at a random point of the first interval.
		c.beat = g.clock.After(time.Duration(g.rng.Float64() * float64(c.interval)))
		c.acked = true
	case opHeartbeatAck:
		c.acked = true
		s.lastAck = g.clock.Now()
	case opHeartbeat:
		if err := c.heartbeat(); err != nil {
			return err
		}
		if c.interval > 0 {
			c.beat = g.clock.After(c.interval)
		}
	case opReconnect:
		return errReconnect
	case opInvalidSession:
		var canResume bool
		_ = json.Unmarshal(p.D, &canResume)
		c.ready = false
		resume := canResume && s.resumable(g.clock.Now())
		if !resume {
			s.reset()
			if !g.identifyAllowed(ctx) {
				return ctx.Err()
			}
		}
		if err := c.identifyOrResume(resume); err != nil {
			return err
		}
	case opDispatch:
		if err := c.dispatch(ctx, p, bo); err != nil {
			return err
		}
	}
	// Timers of a sleeping machine fire late; the web client catches up.
	if !c.lastBeat.IsZero() && g.clock.Now().Sub(c.lastBeat) > c.interval+5*time.Second {
		return c.heartbeat()
	}
	return nil
}

func (c *conn) established(bo *backoff) {
	c.ready = true
	c.s.lastAck = c.g.clock.Now()
	bo.succeed()
	c.g.setStatus("")
}

func (c *conn) dispatch(ctx context.Context, p payload, bo *backoff) error {
	g, s := c.g, c.s
	switch p.T {
	case "READY":
		var r struct {
			SessionID        string `json:"session_id"`
			ResumeGatewayURL string `json:"resume_gateway_url"`
			User             struct {
				ID string `json:"id"`
			} `json:"user"`
			UserSettingsProto string `json:"user_settings_proto"`
		}
		if err := json.Unmarshal(p.D, &r); err != nil || r.SessionID == "" {
			return errors.New("READY without a session")
		}
		s.id, s.resumeURL, s.selfID = r.SessionID, g.resumeURL(r.ResumeGatewayURL), r.User.ID
		s.status = ""
		c.established(bo)
		g.log.Info("discord gateway connected")
		return c.afterReady(settingsStatus(r.UserSettingsProto, g.clock.Now()))
	case "RESUMED":
		c.established(bo)
		g.log.Info("discord gateway resumed")
	case "USER_SETTINGS_PROTO_UPDATE":
		return c.settingsUpdate(p.D)
	case "MESSAGE_CREATE":
		g.storeCreated(ctx, p.D, s.selfID)
	case "MESSAGE_UPDATE":
		g.storeEdit(ctx, p.D)
	case "MESSAGE_DELETE":
		var m struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(p.D, &m) == nil && isSnowflake(m.ID) {
			if err := g.store.DeleteMessage(ctx, m.ID); err != nil {
				g.log.Error("store deleted message", "id", m.ID, "err", err)
			}
		}
	case "CHANNEL_CREATE", "CHANNEL_UPDATE":
		g.storeDM(ctx, p.D)
	}
	return nil
}

// settingsUpdate follows the owner changing their status in another client.
func (c *conn) settingsUpdate(d json.RawMessage) error {
	var u struct {
		Settings struct {
			Type  int    `json:"type"`
			Proto string `json:"proto"`
		} `json:"settings"`
	}
	if json.Unmarshal(d, &u) == nil && u.Settings.Type == 1 {
		if status := settingsStatus(u.Settings.Proto, c.g.clock.Now()); status != "" && status != c.s.status {
			return c.presence(status)
		}
	}
	return nil
}

// storeDM keeps DMs and group DMs current; guild channels come from sync.
func (g *gateway) storeDM(ctx context.Context, d json.RawMessage) {
	var ch apiChannel
	if json.Unmarshal(d, &ch) != nil || (ch.Type != channelDM && ch.Type != channelGroupDM) || !isSnowflake(ch.ID) {
		return
	}
	if cc, ok := channelContainer(&ch); ok {
		if err := g.store.UpsertContainer(ctx, cc); err != nil {
			g.log.Error("store chat", "id", ch.ID, "err", err)
		}
	}
}

// afterReady sends what an idle tab sends once its session is up: no voice,
// the account's own presence, and the analytics session.
func (c *conn) afterReady(status string) error {
	if err := c.send(opVoiceState, struct {
		GuildID   *string `json:"guild_id"`
		ChannelID *string `json:"channel_id"`
		SelfMute  bool    `json:"self_mute"`
		SelfDeaf  bool    `json:"self_deaf"`
		SelfVideo bool    `json:"self_video"`
		Flags     int     `json:"flags"`
	}{SelfMute: true}); err != nil {
		return err
	}
	// The status is only ever the account's own setting.
	if status != "" {
		if err := c.presence(status); err != nil {
			return err
		}
	}
	return c.timeSpent()
}

func (c *conn) presence(status string) error {
	if err := c.send(opPresence, struct {
		Status     string     `json:"status"`
		Since      int64      `json:"since"`
		Activities []struct{} `json:"activities"`
		AFK        bool       `json:"afk"`
	}{Status: status, Activities: []struct{}{}}); err != nil {
		return err
	}
	c.s.status = status
	return nil
}

// timeSpent tells the session about the tab's analytics session, followed by
// a heartbeat, as the web client does.
func (c *conn) timeSpent() error {
	t := c.g.currentTab()
	if t.heartbeatID == "" {
		return nil
	}
	if err := c.send(opTimeSpent, struct {
		InitializationTimestamp int64  `json:"initialization_timestamp"`
		SessionID               string `json:"session_id"`
		ClientLaunchID          string `json:"client_launch_id"`
	}{t.heartbeatStart.UnixMilli(), t.heartbeatID, t.launchID}); err != nil {
		return err
	}
	c.s.announced = t.heartbeatID
	return c.heartbeat()
}

// tabChanged announces a new analytics session the worker started.
func (c *conn) tabChanged() error {
	if c.ready && c.g.currentTab().heartbeatID != c.s.announced {
		return c.timeSpent()
	}
	return nil
}

// resumeURL keeps a resume_gateway_url only on the gateway's own domain, so
// the token never goes elsewhere.
func (g *gateway) resumeURL(raw string) string {
	u, err := url.Parse(raw)
	base, berr := url.Parse(g.url)
	if err != nil || berr != nil || u.Scheme != base.Scheme || u.User != nil || u.Host == "" {
		return ""
	}
	host, bh := u.Hostname(), base.Hostname()
	if _, parent, ok := strings.Cut(bh, "."); host != bh && (!ok || !strings.HasSuffix(host, "."+parent)) {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// gwMessage is a MESSAGE_CREATE or MESSAGE_UPDATE.
type gwMessage struct {
	apiMessage
	GuildID         string     `json:"guild_id"`
	ChannelType     int        `json:"channel_type"`
	EditedTimestamp *time.Time `json:"edited_timestamp"`
}

func (g *gateway) storeCreated(ctx context.Context, d json.RawMessage, selfID string) {
	var m gwMessage
	if json.Unmarshal(d, &m) != nil || !isSnowflake(m.ID) || !isSnowflake(m.ChannelID) {
		return
	}
	ch := apiChannel{ID: m.ChannelID, Type: m.ChannelType, GuildID: m.GuildID, LastMessageID: m.ID}
	c, ok := channelContainer(&ch)
	if !ok {
		return
	}
	if c.Kind == kindDM && m.Author != nil && m.Author.ID != selfID && isSnowflake(m.Author.ID) {
		c.Name = displayName(m.Author)
		c.Participants = []store.Participant{{ID: m.Author.ID, Name: c.Name}}
	}
	if err := g.store.UpsertContainer(ctx, c); err != nil {
		g.log.Error("store chat", "id", c.ID, "err", err)
		return
	}
	msg, ok := toMessage(&m.apiMessage, selfID)
	if !ok {
		return
	}
	if err := g.store.UpsertMessage(ctx, msg); err != nil {
		g.log.Error("store message", "id", msg.ID, "err", err)
	}
}

// storeEdit applies edits to stored messages; updates that only resolve an
// embed carry no edited_timestamp.
func (g *gateway) storeEdit(ctx context.Context, d json.RawMessage) {
	var m gwMessage
	var has struct {
		Content *string `json:"content"`
	}
	if json.Unmarshal(d, &m) != nil || json.Unmarshal(d, &has) != nil ||
		has.Content == nil || m.EditedTimestamp == nil || !isSnowflake(m.ID) {
		return
	}
	if err := g.store.EditMessage(ctx, m.ID, m.Content, *m.EditedTimestamp); err != nil {
		g.log.Error("store edit", "id", m.ID, "err", err)
	}
}

// settingsStatus reads the status setting from a base64 PreloadedUserSettings
// (status_settings = 11, its status = 1, a StringValue). It returns "" when
// the setting is absent, unknown or expired, so nothing is sent.
func settingsStatus(b64 string, now time.Time) string {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}
	settings, ok := protoField(b, 11)
	if !ok {
		return ""
	}
	if exp, ok := protoField(settings, 4); ok && len(exp) == 8 {
		if ms := binary.LittleEndian.Uint64(exp); ms > 0 && !time.UnixMilli(int64(ms)).After(now) {
			return ""
		}
	}
	wrapped, ok := protoField(settings, 1)
	if !ok {
		return ""
	}
	v, _ := protoField(wrapped, 1)
	switch s := string(v); s {
	case "online", "idle", "dnd", "invisible":
		return s
	}
	return ""
}

// protoField returns the last value of field num in a protobuf message: the
// bytes of a length-delimited field or the 8 bytes of a fixed64.
func protoField(b []byte, num int) ([]byte, bool) {
	var val []byte
	found := false
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, false
		}
		b = b[n:]
		var v []byte
		switch key & 7 {
		case 0:
			if _, n = binary.Uvarint(b); n <= 0 {
				return nil, false
			}
			b = b[n:]
			continue
		case 1:
			if len(b) < 8 {
				return nil, false
			}
			v, b = b[:8], b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return nil, false
			}
			v, b = b[n:n+int(l)], b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return nil, false
			}
			b = b[4:]
			continue
		default:
			return nil, false
		}
		if int(key>>3) == num {
			val, found = v, true
		}
	}
	return val, found
}
