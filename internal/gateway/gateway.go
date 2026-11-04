// Package gateway serves the agent API on the gateway socket: reads through
// the grants and never-share filter, access requests and write proposals,
// relayed to the owner side. The owner side pushes the live set to it.
package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ncruces/go-sqlite3"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/config"
	"github.com/adeci/mandate/internal/peer"
	"github.com/adeci/mandate/store"
)

const (
	maxBody = 1 << 20
	// maxMedia is the largest file agents may fetch.
	maxMedia = 100 << 20
)

// Serve serves the gateway socket until ctx ends.
func Serve(ctx context.Context, cfg *config.Config) error {
	front, err := peer.LookupGID("mandate-front")
	if err != nil {
		return fmt.Errorf("group mandate-front: %w", err)
	}
	owner, err := peer.LookupGID("mandate-owner")
	if err != nil {
		return fmt.Errorf("group mandate-owner: %w", err)
	}
	conns := map[string]paths{}
	for name := range cfg.Connectors {
		conns[name] = paths{
			socket: config.ConnectorSocket(name),
			store:  filepath.Join(config.ConnectorDir(name), "data.db"),
		}
	}
	g, err := newGateway(options{
		stateDir:    config.StateDir("gateway"),
		ownerSocket: config.Socket("owner"),
		connectors:  conns,
		front:       front,
		owner:       owner,
		log:         slog.Default(),
	})
	if err != nil {
		return err
	}
	ln, err := peer.Listen(config.Socket("gateway"))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return g.serve(ctx, ln)
}

type paths struct{ socket, store string }

type options struct {
	stateDir    string
	ownerSocket string
	connectors  map[string]paths
	front       uint32 // group of /agent/ callers
	owner       uint32 // group of /internal/ callers
	log         *slog.Logger
}

type gateway struct {
	log      *slog.Logger
	stateDir string
	front    uint32
	owner    uint32

	conns    map[string]*conn
	toOwner  *ownerClient
	activity *activity

	liveMu sync.Mutex // serializes pushes
	live   atomic.Pointer[live]

	// stopping is done once shutdown begins; long polls, which never go
	// idle, end on it.
	stopping context.Context
	stop     context.CancelFunc
}

func newGateway(o options) (*gateway, error) {
	l, err := loadLive(filepath.Join(o.stateDir, "live.json"), o.log)
	if err != nil {
		return nil, err
	}
	toOwner := dialOwner(o.ownerSocket)
	g := &gateway{
		log: o.log, stateDir: o.stateDir, front: o.front, owner: o.owner,
		conns:    map[string]*conn{},
		toOwner:  toOwner,
		activity: &activity{owner: toOwner, log: o.log},
	}
	g.live.Store(l)
	g.stopping, g.stop = context.WithCancel(context.Background())
	for name, p := range o.connectors {
		g.conns[name] = &conn{name: name, client: connector.Dial(name, p.socket), store: p.store}
	}
	return g, nil
}

// longPoll is ctx, also done once shutdown begins.
func (g *gateway) longPoll(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.stopping, cancel)
	return ctx, func() { stop(); cancel() }
}

func (g *gateway) serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:     g.handler(),
		ConnContext: peer.ConnContext,
		// No write timeout: long polls last up to 900 seconds.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	srv.RegisterOnShutdown(g.stop)
	actx, stopActivity := context.WithCancel(context.Background())
	activityDone := make(chan struct{})
	go func() { g.activity.run(actx); close(activityDone) }()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	var err error
	select {
	case <-ctx.Done():
		// Long polls end on g.stopping; the rest gets a few seconds.
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(shutdown) != nil {
			srv.Close()
		}
	case err = <-errc:
		err = fmt.Errorf("serve: %w", err)
	}
	stopActivity()
	<-activityDone
	for _, c := range g.conns {
		c.close()
	}
	return err
}

func (g *gateway) handler() http.Handler {
	agent := http.NewServeMux()
	agent.Handle("POST /agent/v1/join", g.toOwner.joinRelay())
	agent.Handle("GET /agent/v1/join/{id}", g.toOwner.joinRelay())
	agent.Handle("POST /agent/v1/join/{id}", g.toOwner.joinRelay())
	route := func(pattern string, h func(http.ResponseWriter, *http.Request, *caller)) {
		agent.Handle(pattern, g.authed(h))
	}
	route("GET /agent/v1/connectors", g.listConnectors)
	route("GET /agent/v1/access", g.listAccess)
	route("POST /agent/v1/access/requests", g.requestAccess)
	route("GET /agent/v1/access/requests/{id}", g.requestStatus)
	route("GET /agent/v1/proposals/{id}", g.proposalStatus)
	route("GET /agent/v1/{connector}/{verb}", g.read)
	route("POST /agent/v1/{connector}/{op}", g.op)

	internal := http.NewServeMux()
	internal.HandleFunc("PUT /internal/live", g.putLive)

	mux := http.NewServeMux()
	mux.Handle("/agent/", peer.Allow(g.log, agent, g.front))
	mux.Handle("/internal/", peer.Allow(g.log, internal, g.owner))
	return mux
}

// caller is an agent request's identity and the live set it was let in by.
type caller struct {
	identity string
	live     *live
	log      *slog.Logger
}

func (g *gateway) authed(h func(http.ResponseWriter, *http.Request, *caller)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := g.live.Load()
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		id := ""
		if ok {
			id = l.identity(token)
		}
		if id == "" {
			g.log.Warn("unknown token", "addr", r.Header.Get("X-Forwarded-For"), "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "unknown or revoked token; log in again with: mandate login")
			return
		}
		h(w, r, &caller{identity: id, live: l, log: g.log.With("identity", id)})
	})
}

// conn is a configured connector: its socket and its store.
type conn struct {
	name   string
	client *connector.Client
	store  string

	mu     sync.Mutex
	db     *sql.DB
	spec   connector.Spec
	specAt time.Time
}

const (
	specTTL     = 30 * time.Second
	quickCall   = 3 * time.Second
	callTimeout = 30 * time.Second
	// A connector may pace reads and wait for a slot.
	readTimeout  = 2 * time.Minute
	mediaTimeout = 10 * time.Minute
	queryTimeout = 10 * time.Second
)

// getSpec caches only successes, so a connector that comes back is listed
// again on the next call.
func (c *conn) getSpec(ctx context.Context) (connector.Spec, error) {
	c.mu.Lock()
	s, at := c.spec, c.specAt
	c.mu.Unlock()
	if time.Since(at) < specTTL {
		return s, nil
	}
	ctx, cancel := context.WithTimeout(ctx, quickCall)
	defer cancel()
	s, err := c.client.Spec(ctx)
	if err != nil {
		return connector.Spec{}, err
	}
	c.mu.Lock()
	c.spec, c.specAt = s, time.Now()
	c.mu.Unlock()
	return s, nil
}

func (c *conn) health(ctx context.Context) (connector.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, quickCall)
	defer cancel()
	return c.client.Health(ctx)
}

// storeDB opens the store once it exists; until then it is nil, and reads
// find nothing.
func (c *conn) storeDB(ctx context.Context) (*sql.DB, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db != nil {
		return c.db, nil
	}
	if _, err := os.Stat(c.store); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	db, err := store.OpenReadOnly(ctx, c.store, func(conn *sqlite3.Conn) error {
		return conn.CreateFunction("login_code", 1, sqlite3.DETERMINISTIC|sqlite3.INNOCUOUS,
			func(ctx sqlite3.Context, arg ...sqlite3.Value) {
				ctx.ResultBool(loginCode(arg[0].Text()))
			})
	})
	if err != nil {
		return nil, err
	}
	c.db = db
	return db, nil
}

func (c *conn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db != nil {
		c.db.Close()
	}
}

// view opens the connector's store as scope shows it to cl.
func (g *gateway) view(ctx context.Context, cl *caller, c *conn, scope []rule) (*view, error) {
	db, err := c.storeDB(ctx)
	if err != nil {
		return nil, err
	}
	return newView(db, cl.live.hidden[c.name], scope)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, connector.Error{Message: msg})
}

// unavailable answers a failure the agent can only report.
func unavailable(w http.ResponseWriter, cl *caller, what string, err error) {
	cl.log.Error(what, "err", err)
	writeError(w, http.StatusServiceUnavailable, "the gateway could not answer; try again, and tell the owner if it persists")
}

// decode strictly decodes the request body into v, answering the refusal
// itself when it fails.
func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data after the JSON body")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return false
	}
	return true
}

// sortedConns lists the connectors by name.
func (g *gateway) sortedConns() []*conn {
	out := make([]*conn, 0, len(g.conns))
	for _, c := range g.conns {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *conn) int { return strings.Compare(a.name, b.name) })
	return out
}
