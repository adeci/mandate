package gateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/peer"
	"github.com/adeci/mandate/record"
	"github.com/adeci/mandate/store"
)

// fakeConn is a connector over a store the test fills.
type fakeConn struct {
	st       *store.Store
	prepares atomic.Int32
}

func (*fakeConn) Spec() connector.Spec {
	return connector.Spec{Name: "chat", Ops: []connector.Op{
		{Name: "fetch", Kind: connector.Read, Params: []connector.Param{
			{Name: "chat", Required: true, Container: true},
		}},
		{Name: "send", Kind: connector.Write, Params: []connector.Param{
			{Name: "chat", Required: true, Container: true},
			{Name: "text", Required: true, Editable: true},
			{Name: "seq", Message: true},
		}},
	}}
}

func (*fakeConn) Health(context.Context) connector.Health {
	return connector.Health{State: connector.Healthy}
}

func (*fakeConn) Setup(context.Context, string, map[string]string) (connector.Step, error) {
	return connector.Step{Done: true}, nil
}

func (f *fakeConn) Prepare(ctx context.Context, _ string, args map[string]string) (connector.Prepared, error) {
	f.prepares.Add(1)
	p := connector.Prepared{Args: args, Display: []record.Field{{Label: "Text", Value: args["text"], Param: "text"}}, Container: args["chat"]}
	if args["seq"] != "" {
		if err := f.st.DB().QueryRowContext(ctx, `SELECT id FROM messages WHERE seq = ?`, args["seq"]).Scan(&p.Message); err != nil {
			return p, connector.Invalid("no message %s", args["seq"])
		}
	}
	return p, nil
}

func (*fakeConn) Execute(context.Context, record.Write) connector.Result {
	return connector.Result{Status: connector.Failed}
}

func (f *fakeConn) Read(ctx context.Context, _ string, args map[string]string) (connector.ReadResult, error) {
	var res connector.ReadResult
	rows, err := f.st.DB().QueryContext(ctx, `SELECT id FROM messages WHERE container_id = ? ORDER BY seq`, args["chat"])
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return res, err
		}
		res.MessageIDs = append(res.MessageIDs, id)
	}
	return res, rows.Err()
}

// fakeOwner is the owner side's /internal/ routes, answering a fixed ID.
type fakeOwner struct {
	mu        sync.Mutex
	requests  []ownerRequest
	proposals []json.RawMessage
}

const fixedID = "AAAAAAAAAAAAAAAAAAAAAAAAAA"

func (o *fakeOwner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "POST /internal/requests":
		var req ownerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		o.requests = append(o.requests, req)
		writeJSON(w, http.StatusOK, map[string]string{"id": fixedID})
	case "GET /internal/requests/" + fixedID:
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
	case "POST /internal/proposals":
		b, _ := io.ReadAll(r.Body)
		o.proposals = append(o.proposals, b)
		writeJSON(w, http.StatusOK, map[string]string{"id": fixedID})
	case "POST /internal/activity":
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// got is what the owner side was sent so far.
func (o *fakeOwner) got() fakeOwner {
	o.mu.Lock()
	defer o.mu.Unlock()
	return fakeOwner{requests: o.requests, proposals: o.proposals}
}

type device struct {
	priv *ecdsa.PrivateKey
	key  record.Key
}

func newDevice(t *testing.T) device {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := record.P256Key(&priv.PublicKey, "phone")
	if err != nil {
		t.Fatal(err)
	}
	return device{priv, k}
}

func (d device) sign(t *testing.T, r record.Record) record.Envelope {
	t.Helper()
	e, err := record.New(r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := record.Sign(&e, d.priv); err != nil {
		t.Fatal(err)
	}
	return e
}

func at(d time.Duration) record.Time {
	return record.Time{Time: time.Now().Add(d).UTC().Truncate(time.Second)}
}

// env is a gateway serving one connector, "chat", whose store holds:
//
//	fam   group, Sam (+1) wrote 10 days ago and an hour ago, a login code
//	work  group, Kim (+2) wrote an hour ago
//	secret group, never shared in tests that say so
//	dm    a DM with Eve (+3), whose ID is not hers
type env struct {
	t        *testing.T
	sock     string
	dir      string
	conn     *fakeConn
	owner    *fakeOwner
	phone    device
	keys     []record.Envelope
	identity string
	token    string
	client   *http.Client
	stop     func()
}

func newEnv(t *testing.T, front, owner uint32) *env {
	t.Helper()
	ctx := t.Context()
	// Socket paths are short; t.TempDir's can be too long.
	sockDir, err := os.MkdirTemp("", "gw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	dir := t.TempDir()
	connDir := filepath.Join(dir, "conn")
	if err := os.MkdirAll(filepath.Join(connDir, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(connDir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, c := range []store.Container{
		{ID: "fam", Kind: "group", Name: "Family", IsGroup: true},
		{ID: "work", Kind: "group", Name: "Work", IsGroup: true},
		{ID: "secret", Kind: "group", Name: "Secret", IsGroup: true},
		{ID: "dm", Kind: "dm", Name: "~Eve", Participants: []store.Participant{{ID: "+3", Name: "~Eve"}}},
	} {
		if err := st.UpsertContainer(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	for _, m := range []store.Message{
		{ID: "f-old", ContainerID: "fam", SenderID: "+1", SenderName: "~Sam", SentAt: now.Add(-10 * 24 * time.Hour), Body: "old news"},
		{ID: "f-new", ContainerID: "fam", SenderID: "+1", SenderName: "~Sam", SentAt: now.Add(-time.Hour), Body: "see you at 6?"},
		{ID: "f-eve", ContainerID: "fam", SenderID: "+3", SenderName: "~Eve", SentAt: now.Add(-time.Hour), Body: "eve says hi"},
		{ID: "f-code", ContainerID: "fam", SenderID: "+1", SenderName: "~Sam", SentAt: now.Add(-time.Hour), Body: "Your login code is 482913"},
		{ID: "w-new", ContainerID: "work", SenderID: "+2", SenderName: "Kim", SentAt: now.Add(-time.Hour), Body: "standup moved"},
		{ID: "s-new", ContainerID: "secret", SenderID: "+2", SenderName: "Kim", SentAt: now.Add(-time.Hour), Body: "the secret"},
		{ID: "d-new", ContainerID: "dm", SenderID: "+3", SenderName: "~Eve", SentAt: now.Add(-time.Hour), Body: "private"},
	} {
		if err := st.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t, dir: dir, conn: &fakeConn{st: st}, owner: &fakeOwner{}, phone: newDevice(t)}

	self := uint32(os.Getgid())
	log := slog.New(slog.DiscardHandler)
	srv, err := connector.NewServer(e.conn, "chat", connDir, self, self, log)
	if err != nil {
		t.Fatal(err)
	}
	connSock := filepath.Join(sockDir, "c")
	serveUntilCleanup(t, connSock, func(ctx context.Context, ln net.Listener) error { return srv.Serve(ctx, ln) })
	ownerSock := filepath.Join(sockDir, "o")
	serveUntilCleanup(t, ownerSock, func(ctx context.Context, ln net.Listener) error {
		s := &http.Server{Handler: e.owner}
		go func() { <-ctx.Done(); s.Close() }()
		if err := s.Serve(ln); err != http.ErrServerClosed {
			return err
		}
		return nil
	})

	e.sock = filepath.Join(sockDir, "g")
	e.start(front, owner, ownerSock, connSock)
	e.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", e.sock)
	}}}

	e.token = strings.Repeat("ab", 32)
	e.identity = record.NewID()
	return e
}

// start runs a gateway on e.sock with its state in e.dir.
func (e *env) start(front, owner uint32, ownerSock, connSock string) {
	e.t.Helper()
	g, err := newGateway(options{
		stateDir: e.dir, ownerSocket: ownerSock, front: front, owner: owner, log: slog.New(slog.DiscardHandler),
		connectors: map[string]paths{"chat": {socket: connSock, store: filepath.Join(e.dir, "conn", "data.db")}},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.stop = serveUntilCleanup(e.t, e.sock, g.serve)
}

func serveUntilCleanup(t *testing.T, sock string, serve func(context.Context, net.Listener) error) func() {
	t.Helper()
	ln, err := peer.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln) }()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(stop)
	return stop
}

// push sends the live set: a key log of the phone, the identity, grants
// and never-share entries.
func (e *env) push(grants []record.Envelope, never ...neverShare) {
	e.t.Helper()
	if s := e.pushStatus(grants, never...); s != http.StatusNoContent {
		e.t.Fatalf("push: %d", s)
	}
}

func (e *env) pushStatus(grants []record.Envelope, never ...neverShare) int {
	e.t.Helper()
	if e.keys == nil {
		e.keys = []record.Envelope{e.phone.sign(e.t, record.KeyEntry{Seq: 0, Op: "add", Key: e.phone.key, At: at(0)})}
	}
	h := sha256.Sum256([]byte(e.token))
	id := e.phone.sign(e.t, record.Identity{ID: e.identity, TokenSHA256: hex.EncodeToString(h[:]), CreatedAt: at(-48 * time.Hour)})
	set := liveSet{Keys: e.keys, Identities: []record.Envelope{id}, Grants: grants, NeverShare: never}
	status, _ := e.call(http.MethodPut, "/internal/live", set)
	return status
}

// grant signs a grant for the identity on chat.
func (e *env) grant(by device, chats []string, horizon time.Duration, from, until time.Duration) record.Envelope {
	return by.sign(e.t, record.Grant{
		ID: record.NewID(), Identity: e.identity, Connector: "chat", Chats: chats,
		Horizon: int(horizon / time.Second), From: at(from), Until: at(until), CreatedAt: at(from),
	})
}

func (e *env) call(method, path string, body any) (int, string) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://gateway"+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// rows reads a table route and answers its bodies (or chat IDs) as text.
func (e *env) rows(path string) (int, []string) {
	e.t.Helper()
	status, body := e.call(http.MethodGet, path, nil)
	if status != http.StatusOK {
		return status, nil
	}
	var t table
	if err := json.Unmarshal([]byte(body), &t); err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, r := range t.Rows {
		out = append(out, r[len(r)-1].(string))
		if t.Columns[0] == "id" {
			out[len(out)-1] = r[0].(string)
		}
	}
	return status, out
}

func eq(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

func self() uint32 { return uint32(os.Getgid()) }

func TestGrants(t *testing.T) {
	e := newEnv(t, self(), self())
	e.push(nil)
	if s, _ := e.rows("/agent/v1/chat/messages?chat=fam"); s != http.StatusForbidden {
		t.Fatalf("before any grant: %d", s)
	}
	fam := e.grant(e.phone, []string{"fam"}, 48*time.Hour, -time.Hour, time.Hour)
	e.push([]record.Envelope{fam})
	if _, got := e.rows("/agent/v1/chat/chats"); !eq(got, []string{"fam"}) {
		t.Errorf("chats under a fam grant: %v", got)
	}
	// The horizon cuts off the old message.
	if _, got := e.rows("/agent/v1/chat/messages?chat=fam"); strings.Contains(strings.Join(got, "|"), "old news") || len(got) != 3 {
		t.Errorf("fam within 48h: %v", got)
	}
	if s, _ := e.rows("/agent/v1/chat/messages?chat=work"); s != http.StatusNotFound {
		t.Errorf("ungranted work: %d", s)
	}

	// The union: work by another grant, fam's whole history by a third.
	work := e.grant(e.phone, []string{"work"}, 0, -time.Hour, time.Hour)
	famAll := e.grant(e.phone, []string{"fam"}, 0, -time.Hour, time.Hour)
	e.push([]record.Envelope{fam, work, famAll})
	if _, got := e.rows("/agent/v1/chat/messages?chat=work"); !eq(got, []string{"standup moved"}) {
		t.Errorf("work: %v", got)
	}
	if _, got := e.rows("/agent/v1/chat/messages?chat=fam"); len(got) != 4 || got[3] != "old news" {
		t.Errorf("fam with whole history: %v", got)
	}

	// Closing a grant takes it out of the live set.
	e.push([]record.Envelope{fam})
	if s, _ := e.rows("/agent/v1/chat/messages?chat=work"); s != http.StatusNotFound {
		t.Errorf("work after its grant closed: %d", s)
	}
	e.push(nil)
	if s, _ := e.rows("/agent/v1/chat/messages?chat=fam"); s != http.StatusForbidden {
		t.Errorf("after every grant closed: %d", s)
	}

	// A grant whose window ended reads nothing.
	ended := e.grant(e.phone, nil, 0, -3*time.Hour, -2*time.Hour)
	e.push([]record.Envelope{ended})
	if s, _ := e.rows("/agent/v1/chat/chats"); s != http.StatusForbidden {
		t.Errorf("ended grant: %d", s)
	}
}

func TestLiveSurvivesRestart(t *testing.T) {
	e := newEnv(t, self(), self())
	e.push([]record.Envelope{e.grant(e.phone, []string{"work"}, 0, -time.Hour, time.Hour)})
	e.stop()
	sockDir := filepath.Dir(e.sock)
	e.start(self(), self(), filepath.Join(sockDir, "o"), filepath.Join(sockDir, "c"))
	if _, got := e.rows("/agent/v1/chat/messages?chat=work"); !eq(got, []string{"standup moved"}) {
		t.Errorf("after restart: %v", got)
	}
}

func TestUntrustedKey(t *testing.T) {
	e := newEnv(t, self(), self())
	stranger := newDevice(t)
	e.push([]record.Envelope{e.grant(stranger, nil, 0, -time.Hour, time.Hour)})
	if s, _ := e.rows("/agent/v1/chat/chats"); s != http.StatusForbidden {
		t.Errorf("grant by an untrusted key: %d", s)
	}
}

func TestNeverShare(t *testing.T) {
	e := newEnv(t, self(), self())
	e.push([]record.Envelope{e.grant(e.phone, nil, 0, -time.Hour, time.Hour)},
		neverShare{Connector: "chat", Kind: "chat", ID: "secret"},
		neverShare{Connector: "chat", Kind: "person", ID: "+3"})
	if _, got := e.rows("/agent/v1/chat/chats"); !eq(got, []string{"fam", "work"}) && !eq(got, []string{"work", "fam"}) {
		t.Errorf("chats: %v", got)
	}
	for _, chat := range []string{"secret", "dm"} {
		if s, _ := e.rows("/agent/v1/chat/messages?chat=" + chat); s != http.StatusNotFound {
			t.Errorf("never-shared %s: %d", chat, s)
		}
	}
	if _, got := e.rows("/agent/v1/chat/search?text=e"); strings.Contains(strings.Join(got, "|"), "eve") ||
		strings.Contains(strings.Join(got, "|"), "secret") {
		t.Errorf("search shows never-shared: %v", got)
	}
}

func TestLoginCodesHidden(t *testing.T) {
	e := newEnv(t, self(), self())
	e.push([]record.Envelope{e.grant(e.phone, []string{"fam"}, 0, -time.Hour, time.Hour)})
	_, got := e.rows("/agent/v1/chat/messages?chat=fam")
	if strings.Contains(strings.Join(got, "|"), "482913") || !strings.Contains(strings.Join(got, "|"), "[login code hidden]") {
		t.Errorf("messages: %v", got)
	}
	if _, got := e.rows("/agent/v1/chat/search?text=482913"); len(got) != 0 {
		t.Errorf("search finds the code: %v", got)
	}
}

func TestNotFoundIsTheSame(t *testing.T) {
	e := newEnv(t, self(), self())
	e.push([]record.Envelope{e.grant(e.phone, []string{"fam", "secret"}, 0, -time.Hour, time.Hour)},
		neverShare{Connector: "chat", Kind: "chat", ID: "secret"})
	answer := func(chat string) string {
		s, body := e.call(http.MethodGet, "/agent/v1/chat/messages?chat="+chat, nil)
		if s != http.StatusNotFound {
			t.Errorf("%s: %d", chat, s)
		}
		return strings.ReplaceAll(body, chat, "X")
	}
	unknown := answer("nope")
	for _, chat := range []string{"secret", "work"} {
		if got := answer(chat); got != unknown {
			t.Errorf("%s answers %s, unknown %s", chat, got, unknown)
		}
	}
	// And so before the connector is asked to write.
	for _, chat := range []string{"nope", "secret", "work"} {
		s, body := e.call(http.MethodPost, "/agent/v1/chat/send", opRequest{Args: map[string]string{"chat": chat, "text": "hi"}})
		if got := strings.ReplaceAll(body, chat, "X"); s != http.StatusNotFound || got != unknown {
			t.Errorf("send to %s: %d %s", chat, s, got)
		}
	}
	if n := e.conn.prepares.Load(); n != 0 {
		t.Errorf("connector prepared %d refused writes", n)
	}
}

func TestRequestAnswerIgnoresMatches(t *testing.T) {
	e := newEnv(t, self(), self())
	e.push(nil, neverShare{Connector: "chat", Kind: "chat", ID: "secret"})
	var answers []string
	for _, chats := range [][]string{{"fam"}, {"nothing like it"}, {"secret"}} {
		s, body := e.call(http.MethodPost, "/agent/v1/access/requests", map[string]any{
			"connector": "chat", "chats": chats, "reason": "summarize",
		})
		if s != http.StatusAccepted {
			t.Fatalf("request %v: %d %s", chats, s, body)
		}
		answers = append(answers, body)
	}
	if answers[0] != answers[1] || answers[1] != answers[2] {
		t.Errorf("answers differ: %q", answers)
	}
	got := e.owner.got().requests
	if len(got) != 3 || len(got[0].Terms[0].Chats) != 1 || got[0].Terms[0].Chats[0].ID != "fam" ||
		len(got[1].Terms[0].Chats) != 0 || len(got[2].Terms[0].Chats) != 0 {
		t.Errorf("owner side got %+v", got)
	}
	// A fourth while three wait is refused before matching.
	s, _ := e.call(http.MethodPost, "/agent/v1/access/requests", map[string]any{"connector": "chat", "chats": []string{"fam"}, "reason": "x"})
	if n := len(e.owner.got().requests); s != http.StatusTooManyRequests || n != 3 {
		t.Errorf("fourth waiting request: %d, owner side got %d", s, n)
	}
}

func seqOf(t *testing.T, e *env, id string) string {
	t.Helper()
	var seq string
	if err := e.conn.st.DB().QueryRow(`SELECT seq FROM messages WHERE id = ?`, id).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestProposalChecks(t *testing.T) {
	e := newEnv(t, self(), self())
	send := func(args map[string]string) int {
		s, _ := e.call(http.MethodPost, "/agent/v1/chat/send", opRequest{Args: args, Note: "reply"})
		return s
	}
	// Ended 30 minutes ago: messages sent before its end may be answered,
	// for a day.
	ended := e.grant(e.phone, []string{"fam"}, 0, -3*time.Hour, -30*time.Minute)
	e.push([]record.Envelope{ended})
	if s := send(map[string]string{"chat": "fam", "text": "on my way", "seq": seqOf(t, e, "f-new")}); s != http.StatusAccepted {
		t.Errorf("reply under a grant ended 30m ago: %d", s)
	}
	if s := send(map[string]string{"chat": "fam", "text": "x", "seq": seqOf(t, e, "f-code")}); s != http.StatusNotFound {
		t.Errorf("reply to a login code: %d", s)
	}
	if s := send(map[string]string{"chat": "work", "text": "x"}); s != http.StatusNotFound {
		t.Errorf("ungranted chat: %d", s)
	}
	if s, _ := e.call(http.MethodPost, "/agent/v1/chat/fetch", opRequest{Args: map[string]string{"chat": "fam"}}); s != http.StatusForbidden {
		t.Errorf("read op under an ended grant: %d", s)
	}

	// A message sent after the grant's end is out of reach.
	late := e.grant(e.phone, []string{"fam"}, 0, -4*time.Hour, -2*time.Hour)
	e.push([]record.Envelope{late})
	if s := send(map[string]string{"chat": "fam", "text": "x", "seq": seqOf(t, e, "f-new")}); s != http.StatusNotFound {
		t.Errorf("message sent after the grant ended: %d", s)
	}

	// Over a day after the end, nothing.
	old := e.grant(e.phone, []string{"fam"}, 0, -30*time.Hour, -25*time.Hour)
	e.push([]record.Envelope{old})
	if s := send(map[string]string{"chat": "fam", "text": "x"}); s != http.StatusForbidden {
		t.Errorf("grant ended 25h ago: %d", s)
	}
	if n := len(e.owner.got().proposals); n != 1 {
		t.Errorf("owner side got %d proposals, want 1", n)
	}
}

func TestCallerGroups(t *testing.T) {
	for _, tt := range []struct {
		name         string
		front, owner uint32
	}{
		{"front", self(), self() + 1},
		{"owner", self() + 1, self()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.front, tt.owner)
			if s := e.pushStatus(nil); (s == http.StatusForbidden) != (tt.owner != self()) {
				t.Errorf("PUT /internal/live: %d", s)
			}
			if s, _ := e.call(http.MethodGet, "/agent/v1/connectors", nil); (s == http.StatusForbidden) != (tt.front != self()) {
				t.Errorf("GET /agent/v1/connectors: %d", s)
			}
		})
	}
}
