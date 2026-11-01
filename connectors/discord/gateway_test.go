package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	testSession  = "0f3bb1c9a3d9c1e2f4a5b6c7d8e9f001"
	testInterval = 41250 * time.Millisecond
)

// clientFrame is what the connector sent.
type clientFrame struct {
	Op  int             `json:"op"`
	D   json.RawMessage `json:"d"`
	raw string
}

// fakeGateway speaks the gateway protocol: HELLO on connect, READY after
// IDENTIFY, RESUMED after RESUME, an ACK per heartbeat. react runs first and
// may take over a frame.
type fakeGateway struct {
	t     *testing.T
	srv   *fhttp.Server
	url   string
	conns chan *fakeConn

	mu     sync.Mutex
	react  func(c *fakeConn, f clientFrame) bool
	ready  func() map[string]any
	resume string // resume_gateway_url in READY
}

type fakeConn struct {
	ws     *websocket.Conn
	header fhttp.Header
	query  string
	frames chan clientFrame
	closed chan int // the close code the client sent, -1 if it just went away

	wmu sync.Mutex
	buf bytes.Buffer
	zw  *zlib.Writer
	seq int64
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	f := &fakeGateway{t: t, conns: make(chan *fakeConn, 16)}
	f.ready = func() map[string]any {
		return map[string]any{
			"v": 9, "session_id": testSession, "resume_gateway_url": f.resumeURL(),
			"user":                map[string]any{"id": selfUser, "username": "me"},
			"user_settings_proto": settingsProto("invisible", 0),
			"sessions":            []map[string]any{{"status": "invisible", "session_id": "x"}},
			"private_channels":    []any{},
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.srv = &fhttp.Server{Handler: f}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	f.url = "ws://" + ln.Addr().String()
	return f
}

func (f *fakeGateway) resumeURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resume != "" {
		return f.resume
	}
	return f.url
}

func (f *fakeGateway) setReact(r func(c *fakeConn, fr clientFrame) bool) {
	f.mu.Lock()
	f.react = r
	f.mu.Unlock()
}

func (f *fakeGateway) ServeHTTP(w fhttp.ResponseWriter, r *fhttp.Request) {
	up := websocket.Upgrader{CheckOrigin: func(r *fhttp.Request) bool { return r.Header.Get("Origin") == "https://discord.com" }}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		f.t.Errorf("upgrade: %v", err)
		return
	}
	c := &fakeConn{
		ws: ws, header: r.Header.Clone(), query: r.URL.RawQuery,
		frames: make(chan clientFrame, 256), closed: make(chan int, 1),
	}
	c.zw = zlib.NewWriter(&c.buf)
	f.conns <- c
	c.send(opHello, "", map[string]any{"heartbeat_interval": testInterval.Milliseconds(), "_trace": []string{"fake"}})
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			code := -1
			if ce, ok := errors.AsType[*websocket.CloseError](err); ok {
				code = ce.Code
			}
			c.closed <- code
			ws.Close()
			return
		}
		fr := clientFrame{raw: string(data)}
		if err := json.Unmarshal(data, &fr); err != nil {
			f.t.Errorf("client frame %q: %v", data, err)
		}
		// The answer goes out before the test sees the frame, so whatever
		// the test sends next reaches the client after it.
		f.mu.Lock()
		react := f.react
		f.mu.Unlock()
		if react == nil || !react(c, fr) {
			switch fr.Op {
			case opIdentify:
				c.dispatch("READY", f.ready())
			case opResume:
				c.dispatch("RESUMED", map[string]any{"_trace": []string{"fake"}})
			case opQoSHeartbeat, opHeartbeat:
				c.send(opHeartbeatAck, "", nil)
			}
		}
		c.frames <- fr
	}
}

// synced returns once the client handled everything sent before: a status
// change it answers with op 3 comes last.
func (c *fakeConn) synced(t *testing.T, status string) {
	t.Helper()
	c.dispatch("USER_SETTINGS_PROTO_UPDATE", map[string]any{"settings": map[string]any{"type": 1, "proto": settingsProto(status, 0)}, "partial": true})
	if fr := c.next(t); fr.Op != opPresence || !strings.Contains(fr.raw, `"status":"`+status+`"`) {
		t.Fatalf("sync got %s", fr.raw)
	}
}

// send writes one payload into the connection's zlib stream.
func (c *fakeConn) send(op int, t string, d any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	p := map[string]any{"op": op, "d": d, "s": nil, "t": nil}
	if op == opDispatch {
		c.seq++
		p["s"], p["t"] = c.seq, t
	}
	b, _ := json.Marshal(p)
	c.buf.Reset()
	c.zw.Write(b)
	c.zw.Flush() // ends with the 00 00 ff ff sync marker, as Discord's frames do
	_ = c.ws.WriteMessage(websocket.BinaryMessage, c.buf.Bytes())
}

func (c *fakeConn) dispatch(t string, d any) { c.send(opDispatch, t, d) }

func (c *fakeConn) closeWith(code int) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
}

func (f *fakeGateway) conn(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no gateway connection")
		return nil
	}
}

func (c *fakeConn) next(t *testing.T) clientFrame {
	t.Helper()
	select {
	case fr := <-c.frames:
		return fr
	case <-time.After(5 * time.Second):
		t.Fatal("no frame from the client")
		return clientFrame{}
	}
}

// ops reads n frames and returns their opcodes.
func (c *fakeConn) ops(t *testing.T, n int) ([]int, []clientFrame) {
	t.Helper()
	var ops []int
	var frs []clientFrame
	for range n {
		fr := c.next(t)
		ops = append(ops, fr.Op)
		frs = append(frs, fr)
	}
	return ops, frs
}

func (c *fakeConn) quiet(t *testing.T) {
	t.Helper()
	select {
	case fr := <-c.frames:
		t.Errorf("unexpected frame %s", fr.raw)
	case <-time.After(100 * time.Millisecond):
	}
}

func testDial(ctx context.Context, url string, h fhttp.Header) (*websocket.Conn, error) {
	c, _, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).DialContext(ctx, url, h)
	return c, err
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settingsProto is a PreloadedUserSettings with only status_settings set.
func settingsProto(status string, expiresMS uint64) string {
	field := func(num int, b []byte) []byte {
		out := binary.AppendUvarint(nil, uint64(num<<3|2))
		out = binary.AppendUvarint(out, uint64(len(b)))
		return append(out, b...)
	}
	inner := field(1, field(1, []byte(status)))
	if expiresMS > 0 {
		inner = binary.AppendUvarint(inner, 4<<3|1)
		inner = binary.LittleEndian.AppendUint64(inner, expiresMS)
	}
	return base64.StdEncoding.EncodeToString(append(field(2, []byte("x")), field(11, inner)...))
}

func TestGatewayIdentifiesAsAnIdleTab(t *testing.T) {
	h := newHarness(t, func(s *state) {
		s.HeartbeatSessionID, s.HeartbeatStartedAt, s.HeartbeatUsedAt = "1ed19025-eebd-4af4-8e95-fdd543da0fd8", noon.Add(-time.Minute), noon.Add(-time.Minute)
	})
	h.start()
	// Like a browser restoring the tab: the app page, after the app-open
	// delay, then the gateway.
	if len(h.startup) != 1 || h.startup[0].path != "/channels/@me" || !h.startup[0].at.Equal(noon.Add(2*time.Second)) {
		t.Errorf("startup requests %+v", h.startup)
	}
	c := h.gw.conn(t)
	if c.query != gatewayQuery {
		t.Errorf("query %q", c.query)
	}
	for k, v := range map[string]string{
		"User-Agent": userAgent, "Origin": "https://discord.com", "Pragma": "no-cache", "Cache-Control": "no-cache",
		"Accept-Language": "en-US,en;q=0.9", "Cookie": "", "Sec-Ch-Ua": "", "Authorization": "",
	} {
		if got := c.header.Get(k); got != v {
			t.Errorf("handshake %s = %q, want %q", k, got, v)
		}
	}

	identify := c.next(t)
	if identify.Op != opIdentify {
		t.Fatalf("first frame %s", identify.raw)
	}
	var d struct {
		Token        string          `json:"token"`
		Capabilities int             `json:"capabilities"`
		Properties   json.RawMessage `json:"properties"`
		ClientState  json.RawMessage `json:"client_state"`
		Presence     json.RawMessage `json:"presence"`
	}
	if err := json.Unmarshal(identify.D, &d); err != nil {
		t.Fatal(err)
	}
	if d.Token != testToken || d.Capabilities != 1767421 || string(d.ClientState) != `{"guild_versions":{}}` || d.Presence != nil {
		t.Errorf("identify %s", identify.raw)
	}
	var props struct {
		LaunchID string `json:"client_launch_id"`
	}
	_ = json.Unmarshal(d.Properties, &props)
	s := h.state()
	if props.LaunchID == s.LaunchID || len(props.LaunchID) != 36 {
		t.Errorf("fast connect launch %q, the app's is %q", props.LaunchID, s.LaunchID)
	}
	want := `{"os":"Linux","browser":"Chrome","device":"","device_model_name":"","system_locale":"en-US",` +
		`"has_client_mods":false,"browser_user_agent":"` + userAgent + `","browser_version":"156.0.0.0",` +
		`"os_version":"","referrer":"","referring_domain":"","referrer_current":"","referring_domain_current":"",` +
		`"release_channel":"stable","client_build_number":657850,"client_event_source":null,` +
		`"client_launch_id":"` + props.LaunchID + `","is_fast_connect":true,"installation_id":"` + testInstallation + `"}`
	if string(d.Properties) != want {
		t.Errorf("properties\n got %s\nwant %s", d.Properties, want)
	}

	// After READY: no voice, the account's own status, the analytics
	// session and a heartbeat; nothing else while idle.
	ops, frs := c.ops(t, 4)
	if want := []int{opVoiceState, opPresence, opTimeSpent, opQoSHeartbeat}; !slices.Equal(ops, want) {
		t.Fatalf("after READY sent ops %v, want %v", ops, want)
	}
	for i, want := range []string{
		`{"op":4,"d":{"guild_id":null,"channel_id":null,"self_mute":true,"self_deaf":false,"self_video":false,"flags":0}}`,
		`{"op":3,"d":{"status":"invisible","since":0,"activities":[],"afk":false}}`,
		`{"op":41,"d":{"initialization_timestamp":` + itoa(noon.Add(-time.Minute).UnixMilli()) +
			`,"session_id":"1ed19025-eebd-4af4-8e95-fdd543da0fd8","client_launch_id":"` + s.LaunchID + `"}}`,
		`{"op":40,"d":{"seq":1,"qos":{"active":false,"ver":31,"reasons":[]}}}`,
	} {
		if frs[i].raw != want {
			t.Errorf("frame %d\n got %s\nwant %s", i, frs[i].raw, want)
		}
	}
	c.quiet(t)

	// The owner switching their status elsewhere is followed; a status the
	// connector cannot read is never replaced by one of its own.
	c.dispatch("USER_SETTINGS_PROTO_UPDATE", map[string]any{"settings": map[string]any{"type": 1, "proto": settingsProto("dnd", 0)}, "partial": false})
	if fr := c.next(t); fr.raw != `{"op":3,"d":{"status":"dnd","since":0,"activities":[],"afk":false}}` {
		t.Errorf("status change sent %s", fr.raw)
	}
	c.dispatch("USER_SETTINGS_PROTO_UPDATE", map[string]any{"settings": map[string]any{"type": 1, "proto": settingsProto("bogus", 0)}, "partial": false})
	c.quiet(t)
	if got := h.health(); got.State != connector.Healthy || got.Detail != "" {
		t.Errorf("health while connected %+v", got)
	}
}

func TestSettingsStatus(t *testing.T) {
	tests := []struct {
		name, proto, want string
	}{
		{"invisible", settingsProto("invisible", 0), "invisible"},
		{"unexpired", settingsProto("dnd", uint64(noon.Add(time.Hour).UnixMilli())), "dnd"},
		{"expired", settingsProto("dnd", uint64(noon.Add(-time.Hour).UnixMilli())), ""},
		{"unknown value", settingsProto("streaming", 0), ""},
		{"no status settings", base64.StdEncoding.EncodeToString([]byte{0x12, 0x01, 'x'}), ""},
		{"truncated", base64.StdEncoding.EncodeToString([]byte{0x5a, 0x09, 0x0a}), ""},
		{"not base64", "%%%", ""},
	}
	for _, tt := range tests {
		if got := settingsStatus(tt.proto, noon); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestGatewayHeartbeats(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	c := h.gw.conn(t)
	c.ops(t, 5) // identify and what follows READY
	// The first heartbeat comes at a random point of the first interval
	// (half of it with this rng), then one per interval.
	first := time.Duration(0.5 * float64(testInterval))
	eventually(t, "first heartbeat timer", func() bool { return h.clock.pending(first) })
	h.clock.Advance(first - time.Millisecond)
	c.quiet(t)
	h.clock.Advance(time.Millisecond)
	if fr := c.next(t); fr.Op != opQoSHeartbeat || !strings.Contains(fr.raw, `"qos":{"active":false,"ver":31,"reasons":[]}`) {
		t.Fatalf("first heartbeat %s", fr.raw)
	}
	c.synced(t, "dnd")
	for _, status := range []string{"invisible", "dnd"} {
		eventually(t, "next heartbeat timer", func() bool { return h.clock.pending(testInterval) })
		h.clock.Advance(testInterval)
		if fr := c.next(t); fr.Op != opQoSHeartbeat {
			t.Fatalf("heartbeat %s", fr.raw)
		}
		c.synced(t, status)
	}

	// A heartbeat Discord does not acknowledge ends the connection, which
	// is resumed on the next one.
	h.gw.setReact(func(_ *fakeConn, fr clientFrame) bool { return fr.Op == opQoSHeartbeat })
	eventually(t, "next heartbeat timer", func() bool { return h.clock.pending(testInterval) })
	h.clock.Advance(testInterval)
	c.next(t)
	c.synced(t, "invisible")
	eventually(t, "next heartbeat timer", func() bool { return h.clock.pending(testInterval) })
	h.clock.Advance(testInterval)
	if code := <-c.closed; code != closeResume {
		t.Errorf("closed with %d, want 4000", code)
	}
	h.gw.setReact(nil)
	eventually(t, "reconnect timer", func() bool { return h.clock.pending(2 * time.Second) })
	h.clock.Advance(2 * time.Second)
	if fr := h.gw.conn(t).next(t); fr.Op != opResume {
		t.Errorf("after a missed ack sent %s, want a resume", fr.raw)
	}
}

func TestGatewayResumesAndReidentifies(t *testing.T) {
	h := newHarness(t, nil)
	resume := newFakeGateway(t)
	h.gw.resume = resume.url
	h.start()
	c := h.gw.conn(t)
	c.ops(t, 5)
	c.dispatch("TYPING_START", map[string]any{"channel_id": dmID})
	c.dispatch("PRESENCE_UPDATE", map[string]any{"status": "online"})
	c.synced(t, "dnd") // seq 4
	c.ws.Close()       // the connection drops without a close frame
	if got := h.health(); got.State != connector.Healthy {
		t.Errorf("health while reconnecting %+v", got)
	}
	eventually(t, "reconnect timer", func() bool { return h.clock.pending(2 * time.Second) })
	if got := h.health(); got.Detail != "live updates reconnecting" {
		t.Errorf("health detail while reconnecting %q", got.Detail)
	}
	h.clock.Advance(2 * time.Second)

	rc := resume.conn(t)
	fr := rc.next(t)
	if want := `{"op":6,"d":{"token":"` + testToken + `","session_id":"` + testSession + `","seq":4}}`; fr.raw != want {
		t.Errorf("resume\n got %s\nwant %s", fr.raw, want)
	}
	eventually(t, "resumed", func() bool { return h.health().Detail == "" })
	select {
	case c := <-h.gw.conns:
		t.Errorf("reconnected to the first URL as well: %s", c.query)
	default:
	}

	// An invalid session is identified anew on the same connection, now by
	// the app rather than the page's fast-connect script.
	rc.send(opInvalidSession, "", false)
	fr = rc.next(t)
	var d struct {
		Properties struct {
			LaunchID string `json:"client_launch_id"`
			Fast     bool   `json:"is_fast_connect"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(fr.D, &d); err != nil || fr.Op != opIdentify {
		t.Fatalf("after op 9 sent %s", fr.raw)
	}
	if d.Properties.Fast || d.Properties.LaunchID != h.state().LaunchID {
		t.Errorf("re-identify properties %+v", d.Properties)
	}
}

func TestGatewayRejectedTokenIsDiscarded(t *testing.T) {
	h := newHarness(t, nil)
	h.gw.setReact(func(c *fakeConn, fr clientFrame) bool {
		if fr.Op == opIdentify {
			c.closeWith(closeAuthFailed)
			return true
		}
		return false
	})
	h.start()
	if code := <-h.gw.conn(t).closed; code != closeAuthFailed {
		t.Errorf("connection ended with %d", code)
	}
	eventually(t, "token discarded", func() bool { return h.storedToken() == "" })
	got := h.health()
	if got.State != connector.NeedsSetup || !strings.Contains(got.Detail, "4004") || !strings.HasSuffix(got.Detail, fixToken) {
		t.Errorf("health %+v", got)
	}
	if n := len(h.discord.all()); n != 0 {
		t.Errorf("%d REST requests", n)
	}
	select {
	case c := <-h.gw.conns:
		t.Errorf("reconnected after 4004 (%s)", c.query)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestGatewayStoresLiveMessages(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	c := h.gw.conn(t)
	c.ops(t, 5)
	ctx := context.Background()
	c.dispatch("MESSAGE_CREATE", map[string]any{
		"id": msg1, "channel_id": dmID, "channel_type": channelDM, "type": 0, "content": secretBody,
		"timestamp": "2026-10-29T15:59:00.000000+00:00", "edited_timestamp": nil, "nonce": "1",
		"author": map[string]any{"id": aliceUser, "username": "alice", "global_name": "Alice A"},
	})
	c.dispatch("MESSAGE_CREATE", map[string]any{
		"id": msg2, "channel_id": chanGeneral, "guild_id": guildID, "channel_type": 0, "type": 0, "content": "in a server",
		"timestamp": "2026-10-29T16:00:00.000000+00:00", "author": map[string]any{"id": selfUser, "username": "me"},
	})
	eventually(t, "messages stored", func() bool {
		var n int
		_ = h.st.DB().QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&n)
		return n == 2
	})
	if row := h.queryRow(`SELECT count(*), max(id), max(body), max(sender_name), max(is_from_me) FROM messages WHERE container_id = ?`, dmID); row[0] != int64(1) ||
		row[1] != msg1 || row[2] != secretBody || row[3] != "Alice A" || row[4] != int64(0) {
		t.Fatalf("DM messages %v", row)
	}
	if row := h.queryRow(`SELECT kind, name, last_message_at FROM containers WHERE id = ?`, dmID); row[0] != kindDM || row[1] != "Alice A" ||
		row[2] != snowflakeTimeString(msg1) {
		t.Errorf("DM row %v", row)
	}
	if row := h.queryRow(`SELECT kind, parent_id FROM containers WHERE id = ?`, chanGeneral); row[0] != kindChannel || row[1] != guildID {
		t.Errorf("channel row %v", row)
	}
	if row := h.queryRow(`SELECT is_from_me FROM messages WHERE id = ?`, msg2); row[0] != int64(1) {
		t.Errorf("own message %v", row)
	}

	// An embed resolving is no edit; a real one is; deletes keep the row.
	c.dispatch("MESSAGE_UPDATE", map[string]any{"id": msg1, "channel_id": dmID, "content": "ignored", "edited_timestamp": nil})
	c.dispatch("MESSAGE_UPDATE", map[string]any{"id": msg1, "channel_id": dmID, "content": "edited body", "edited_timestamp": "2026-10-29T16:01:00.000000+00:00"})
	c.dispatch("MESSAGE_DELETE", map[string]any{"id": msg2, "channel_id": chanGeneral, "guild_id": guildID})
	c.dispatch("CHANNEL_CREATE", map[string]any{
		"id": groupID, "type": channelGroupDM, "last_message_id": msg3,
		"recipients": []map[string]any{{"id": aliceUser, "username": "alice"}, {"id": "1000000000000000003", "username": "bob"}},
	})
	c.dispatch("CHANNEL_CREATE", map[string]any{"id": otherChan, "type": 0, "guild_id": guildID, "name": "skipped"})
	eventually(t, "group stored", func() bool {
		var n int
		_ = h.st.DB().QueryRowContext(ctx, `SELECT count(*) FROM containers WHERE id = ?`, groupID).Scan(&n)
		return n == 1
	})
	if row := h.queryRow(`SELECT body, edited_at IS NOT NULL FROM messages WHERE id = ?`, msg1); row[0] != "edited body" || row[1] != int64(1) {
		t.Errorf("edited %v", row)
	}
	if row := h.queryRow(`SELECT deleted FROM messages WHERE id = ?`, msg2); row[0] != int64(1) {
		t.Errorf("deleted %v", row)
	}
	if row := h.queryRow(`SELECT kind, name FROM containers WHERE id = ?`, groupID); row[0] != kindGroup || row[1] != "alice, bob" {
		t.Errorf("group %v", row)
	}
	if row := h.queryRow(`SELECT count(*) FROM containers WHERE id = ?`, otherChan); row[0] != int64(0) {
		t.Error("guild channel created from CHANNEL_CREATE")
	}
	if n := len(h.discord.all()); n != 0 {
		t.Errorf("%d REST requests", n)
	}
}

func snowflakeTimeString(id string) string {
	at, _ := snowflakeTime(id)
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}

func TestGatewayOpensDMBeforeFetching(t *testing.T) {
	h := newHarness(t, func(s *state) { s.SelfID, s.SelfCheckedAt = selfUser, noon })
	if err := h.st.UpsertContainer(context.Background(), store.Container{ID: dmID, Kind: kindDM, Name: "Alice A"}); err != nil {
		t.Fatal(err)
	}
	h.start()
	c := h.gw.conn(t)
	c.ops(t, 5)
	path := api("/channels/" + dmID + "/messages")
	opened := make(chan string, 2)
	h.discord.handle(path, func(w http.ResponseWriter, _ *http.Request, _ int) {
		select {
		case fr := <-c.frames:
			opened <- fr.raw
		case <-time.After(time.Second):
			opened <- "nothing"
		}
		connector.WriteJSON(w, 200, dmMessagesBody)
	})
	h.callOK("fetch", map[string]string{"channel_id": dmID}, &connector.ReadResult{})
	if got := <-opened; got != `{"op":13,"d":{"channel_id":"`+dmID+`"}}` {
		t.Errorf("before the messages request the gateway got %s, want op 13", got)
	}
	// Scrolling in an open DM sends nothing more.
	seq := h.queryRow(`SELECT seq FROM messages WHERE id = ?`, msg4)[0].(int64)
	h.callOK("fetch", map[string]string{"channel_id": dmID, "before": itoa(seq)}, &connector.ReadResult{})
	c.quiet(t)
}

// Resume URLs come from Discord; the token only ever goes to its gateway.
func TestResumeURL(t *testing.T) {
	g := &gateway{url: defaultGatewayURL}
	for raw, want := range map[string]string{
		"wss://gateway-us-east1-d.discord.gg":      "wss://gateway-us-east1-d.discord.gg",
		"wss://gateway.discord.gg/":                "wss://gateway.discord.gg",
		"wss://evil.example":                       "",
		"wss://discord.gg.evil.example":            "",
		"ws://gateway-us-east1-d.discord.gg":       "",
		"wss://user@gateway-us-east1-d.discord.gg": "",
		"": "",
	} {
		if got := g.resumeURL(raw); got != want {
			t.Errorf("resumeURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// stop ends the connection with a close frame and leaves no goroutine behind.
func TestGatewayStopsCleanly(t *testing.T) {
	before := runtime.NumGoroutine()
	f := newFakeGateway(t)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	g := newGateway(f.url, testDial, realClock{}, fixedRand(0.5), st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.start(tab{token: testToken, buildNumber: defaultBuildNumber, launchID: newUUID()})
	c := f.conn(t)
	c.next(t)
	eventually(t, "connected", func() bool { return g.health() == "" })
	g.stop(websocket.CloseGoingAway)
	if code := <-c.closed; code != websocket.CloseGoingAway {
		t.Errorf("closed with %d, want 1001", code)
	}
	g.stop(websocket.CloseGoingAway) // stopping twice is fine
	f.srv.Close()
	st.Close()
	eventually(t, "goroutines to exit", func() bool { return runtime.NumGoroutine() <= before })
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
