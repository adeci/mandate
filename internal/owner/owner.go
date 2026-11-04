// Package owner is mandate's owner side: the owner API of
// docs/spec/owner-api.md for the app, the setup page, and the internal API
// the gateway calls. It holds every decision the owner makes and pushes the
// live set to the gateway.
package owner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/config"
	"github.com/adeci/mandate/internal/peer"
	"github.com/adeci/mandate/record"
)

const maxBody = 1 << 20

func Serve(ctx context.Context, cfg *config.Config) error {
	front, err := peer.LookupGID("mandate-front")
	if err != nil {
		return fmt.Errorf("group mandate-front: %w", err)
	}
	gateway, err := peer.LookupGID("mandate-gateway")
	if err != nil {
		return fmt.Errorf("group mandate-gateway: %w", err)
	}
	conns := map[string]*connector.Client{}
	for name := range cfg.Connectors {
		conns[name] = connector.Dial(name, config.ConnectorSocket(name))
	}
	s, err := newServer(config.StateDir("owner"), conns, config.Socket("gateway"), slog.Default())
	if err != nil {
		return err
	}
	s.certVerified = cfg.TLS.Mode != "own"
	_, s.agentPort, _ = net.SplitHostPort(cmp.Or(cfg.AgentListen, cfg.Listen))
	ln, err := peer.Listen(config.Socket("owner"))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return s.serve(ctx, ln, front, gateway)
}

type Server struct {
	dir     string
	conns   map[string]*connector.Client
	gateway *http.Client
	log     *slog.Logger
	now     func() time.Time

	mu         sync.Mutex
	st         state
	keys       *record.Log
	challenges map[string]*challenge // by nonce
	joins      map[string]*joining   // by hex ID
	joinStarts map[string][]time.Time
	secret     []byte // shown on the setup page, spent by pairing
	pushedKeys map[string]string
	health     map[string]connector.HealthState
	// wake is closed and replaced on every change, for long polls.
	wake chan struct{}
	subs map[chan event]struct{}

	push      chan struct{}
	executing sync.WaitGroup
	// stopping is done once shutdown begins; streams and long polls, which
	// never go idle, end on it.
	stopping context.Context
	stop     context.CancelFunc

	names chatNames

	// certVerified: the TLS certificate chains to a CA (mode acme or cert),
	// so the app checks it and people compare no key.
	certVerified bool
	// agentPort is the port agents log in on.
	agentPort string
}

func newServer(dir string, conns map[string]*connector.Client, gatewaySock string, log *slog.Logger) (*Server, error) {
	st, err := loadState(dir)
	if err != nil {
		return nil, err
	}
	keys, err := record.NewLog(st.Keys)
	if err != nil {
		return nil, fmt.Errorf("owner key log: %w", err)
	}
	s := &Server{
		dir:   dir,
		conns: conns,
		gateway: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", gatewaySock)
			},
		}},
		log:        log,
		now:        func() time.Time { return time.Now().UTC().Truncate(time.Second) },
		st:         st,
		keys:       keys,
		challenges: map[string]*challenge{},
		joins:      map[string]*joining{},
		joinStarts: map[string][]time.Time{},
		pushedKeys: map[string]string{},
		health:     map[string]connector.HealthState{},
		wake:       make(chan struct{}),
		subs:       map[chan event]struct{}{},
		push:       make(chan struct{}, 1),
	}
	s.stopping, s.stop = context.WithCancel(context.Background())
	// A write that was running when mandate stopped may have reached the
	// service; it is never run again.
	stale := false
	for _, p := range st.Proposals {
		if p.Status == "executing" {
			p.Status, p.DecidedAt = "unknown", s.now()
			p.Result = &connector.Result{Status: connector.Unknown, Detail: "mandate stopped while the write ran"}
			stale = true
		}
	}
	if stale {
		if err := saveState(dir, &s.st); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) serve(ctx context.Context, ln net.Listener, front, gateway uint32) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := &http.Server{
		Handler:           s.handler(front, gateway),
		ConnContext:       peer.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
	}
	srv.RegisterOnShutdown(s.stop)
	done := make(chan struct{})
	go func() { s.loop(ctx); close(done) }()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	var err error
	select {
	case <-ctx.Done():
		// Streams and long polls end on s.stopping; the rest gets a few
		// seconds.
		shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		if srv.Shutdown(shutdown) != nil {
			srv.Close()
		}
		cancelShutdown()
	case err = <-errc:
		err = fmt.Errorf("serve: %w", err)
	}
	cancel()
	<-done
	s.executing.Wait()
	return err
}

// serverInfo tells the app what agents log in with.
func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, map[string]string{"agent_url": "https://" + net.JoinHostPort(hostOnly(r.Host), s.agentPort)})
}

func (s *Server) handler(front, gateway uint32) http.Handler {
	app := http.NewServeMux()
	app.HandleFunc("GET /setup", s.setupPage)
	app.HandleFunc("POST /setup", s.setupPassword)
	app.HandleFunc("GET /setup/{password}", s.setupLink)
	app.HandleFunc("POST /owner/v1/pair", s.pair)
	app.HandleFunc("POST /owner/v1/join", s.joinStart("device"))
	app.HandleFunc("GET /owner/v1/join/{id}", s.joinPoll("device"))
	app.HandleFunc("POST /owner/v1/join/{id}", s.joinAnswer("device"))
	authed := func(pattern string, h http.HandlerFunc) { app.HandleFunc(pattern, s.device(h)) }
	authed("GET /owner/v1/server", s.serverInfo)
	authed("GET /owner/v1/now", s.getNow)
	authed("GET /owner/v1/activity", s.getActivity)
	authed("GET /owner/v1/stream", s.stream)
	authed("POST /owner/v1/signed", s.signed)
	authed("POST /owner/v1/requests/{id}/challenge", s.requestChallenge)
	authed("POST /owner/v1/requests/{id}/deny", s.denyRequest)
	authed("GET /owner/v1/proposals/{id}", s.getProposal)
	authed("POST /owner/v1/proposals/{id}/challenge", s.proposalChallenge)
	authed("POST /owner/v1/proposals/{id}/deny", s.denyProposal)
	authed("POST /owner/v1/grants/challenge", s.grantChallenge)
	authed("POST /owner/v1/grants/{id}/extend/challenge", s.extendChallenge)
	authed("POST /owner/v1/grants/{id}/resume/challenge", s.resumeChallenge)
	authed("POST /owner/v1/grants/{id}/close", s.closeGrant)
	authed("POST /owner/v1/stop-all", s.stopAll)
	authed("GET /owner/v1/exchanges", s.listJoins)
	authed("POST /owner/v1/exchanges", s.openJoin)
	authed("DELETE /owner/v1/exchanges/{id}", s.cancelJoin)
	authed("POST /owner/v1/exchanges/{id}/challenge", s.joinChallenge)
	authed("GET /owner/v1/identities", s.listIdentities)
	authed("PATCH /owner/v1/identities/{id}", s.renameIdentity)
	authed("DELETE /owner/v1/identities/{id}", s.deleteIdentity)
	authed("GET /owner/v1/connectors", s.listConnectors)
	authed("GET /owner/v1/connectors/{name}/setup", s.connectorSetup)
	authed("POST /owner/v1/connectors/{name}/setup", s.connectorSetup)
	authed("POST /owner/v1/connectors/{name}/unlink", s.connectorUnlink)
	authed("GET /owner/v1/connectors/{name}/chats", s.connectorChats)
	authed("GET /owner/v1/never-share", s.listNeverShare)
	authed("PUT /owner/v1/never-share/{connector}/{kind}/{id}", s.editNeverShare)
	authed("DELETE /owner/v1/never-share/{connector}/{kind}/{id}", s.editNeverShare)
	authed("GET /owner/v1/keys", s.getKeys)
	authed("POST /owner/v1/keys/{id}/remove/challenge", s.removeKeyChallenge)

	internal := http.NewServeMux()
	internal.HandleFunc("POST /internal/requests", s.postRequest)
	internal.HandleFunc("GET /internal/requests/{id}", s.requestStatus)
	internal.HandleFunc("POST /internal/proposals", s.postProposal)
	internal.HandleFunc("GET /internal/proposals/{id}", s.proposalStatus)
	internal.HandleFunc("POST /internal/activity", s.postActivity)
	internal.HandleFunc("POST /internal/join", s.joinStart("agent"))
	internal.HandleFunc("GET /internal/join/{id}", s.joinPoll("agent"))
	internal.HandleFunc("POST /internal/join/{id}", s.joinAnswer("agent"))

	mux := http.NewServeMux()
	mux.Handle("/owner/", peer.Allow(s.log, app, front))
	mux.Handle("/setup", peer.Allow(s.log, app, front))
	mux.Handle("/setup/", peer.Allow(s.log, app, front))
	mux.Handle("/internal/", peer.Allow(s.log, internal, gateway))
	return mux
}

func (s *Server) device(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bytes.CutPrefix([]byte(r.Header.Get("Authorization")), []byte("Bearer "))
		if ok {
			sum := tokenSHA256(string(token))
			s.mu.Lock()
			ok = false
			for _, k := range s.keys.Trusted() {
				if s.st.Devices[k.ID] == sum {
					ok = true
					break
				}
			}
			s.mu.Unlock()
		}
		if !ok {
			fail(w, http.StatusUnauthorized, "missing or unknown device token")
			return
		}
		h(w, r)
	}
}

// The caller holds s.mu.
func (s *Server) commit(what ...string) error {
	if err := saveState(s.dir, &s.st); err != nil {
		s.log.Error("save state", "err", err)
		return err
	}
	s.changed(what...)
	return nil
}

// changed wakes long polls, sends the app's changed events and schedules a
// push of the live set. The caller holds s.mu.
func (s *Server) changed(what ...string) {
	close(s.wake)
	s.wake = make(chan struct{})
	for _, w := range what {
		s.publish("changed", map[string]string{"what": w})
	}
	select {
	case s.push <- struct{}{}:
	default:
	}
}

// cond runs with s.mu held.
func (s *Server) await(ctx context.Context, wait time.Duration, cond func() bool) {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		s.mu.Lock()
		ok, wake := cond(), s.wake
		s.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-wake:
		case <-t.C:
			return
		case <-ctx.Done():
			return
		case <-s.stopping.Done():
			return
		}
	}
}

func waitParam(r *http.Request) (time.Duration, bool) {
	v := r.URL.Query().Get("wait")
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(min(n, 900)) * time.Second, true
}

var codes = map[int]string{
	http.StatusBadRequest:      "invalid",
	http.StatusUnauthorized:    "token",
	http.StatusForbidden:       "forbidden",
	http.StatusNotFound:        "not_found",
	http.StatusConflict:        "conflict",
	http.StatusGone:            "expired",
	http.StatusTooManyRequests: "rate_limited",
	http.StatusBadGateway:      "connector",
}

type apiError struct {
	Error        string `json:"error"`
	Code         string `json:"code"`
	AttemptsLeft *int   `json:"attempts_left,omitempty"`
	RetryAfter   int    `json:"retry_after_seconds,omitempty"`
}

func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, apiError{Error: msg, Code: codes[status]})
}

func failErr(w http.ResponseWriter, err error) {
	if e, ok := errors.AsType[*httpError](err); ok {
		fail(w, e.status, e.msg)
		return
	}
	fail(w, http.StatusInternalServerError, "internal error")
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func errorf(status int, format string, a ...any) error {
	return &httpError{status, fmt.Sprintf(format, a...)}
}

// connectorError maps a connector's refusal for the app: what the owner
// can fix keeps its status, anything else is the connector's failure.
func connectorError(err error) error {
	if e, ok := errors.AsType[*connector.Error](err); ok {
		switch e.Status {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict:
			return errorf(e.Status, "%s", e.Message)
		}
		return errorf(http.StatusBadGateway, "%s", e.Message)
	}
	return errorf(http.StatusBadGateway, "%v", err)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads a JSON body into v, refusing unknown fields. An empty body
// leaves v as it is.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "body: "+err.Error())
		return false
	}
	return true
}
