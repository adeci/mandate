package connector

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/adeci/mandate/record"
	"github.com/adeci/mandate/store"
)

const maxBody = 1 << 20

// Server serves a Connector to the gateway and owner users.
type Server struct {
	c              Connector
	dir            string
	gateway, owner uint32
	log            *slog.Logger

	mu    sync.Mutex // guards keys
	keys  *record.Log
	spent *spent

	executing sync.WaitGroup
	fetching  atomic.Int32
}

// NewServer serves c with its state in dir, where dir/private exists. Only
// callers whose primary group is gateway or owner may call it, each on its
// own routes.
func NewServer(c Connector, dir string, gateway, owner uint32, log *slog.Logger) (*Server, error) {
	keys, err := loadKeys(filepath.Join(dir, "private", "keys.json"))
	if err != nil {
		return nil, err
	}
	sp, err := openSpent(filepath.Join(dir, "private", "spent"))
	if err != nil {
		return nil, err
	}
	return &Server{c: c, dir: dir, gateway: gateway, owner: owner, log: log, keys: keys, spent: sp}, nil
}

// Serve serves on ln until ctx is done, then waits for writes in flight.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	defer s.spent.close()
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			gid, err := peerGID(c)
			if err != nil {
				s.log.Warn("peer credentials", "err", err)
				return ctx
			}
			return context.WithValue(ctx, peerKey{}, gid)
		},
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdown)
	// Shutdown gives up on slow reads, but a write runs to its end.
	s.executing.Wait()
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// class is who may call a route.
type class uint8

const (
	gateway class = 1 << iota
	owner
)

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, who class, h http.HandlerFunc) {
		mux.HandleFunc(pattern, s.allow(who, h))
	}
	route("GET /spec", gateway|owner, s.spec)
	route("GET /health", gateway|owner, s.health)
	route("GET /keys", gateway|owner, s.getKeys)
	route("POST /prepare", gateway|owner, s.prepare)
	route("POST /read", gateway, s.read)
	route("POST /media", gateway, s.media)
	route("POST /setup", owner, s.setup)
	route("POST /unlink", owner, s.unlink)
	route("POST /execute", owner, s.execute)
	route("PUT /keys", owner, s.putKeys)
	route("GET /containers", owner, s.containers)
	return mux
}

type peerKey struct{}

func (s *Server) allow(who class, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		gid, ok := r.Context().Value(peerKey{}).(uint32)
		var c class
		if ok && gid == s.gateway {
			c |= gateway
		}
		if ok && gid == s.owner {
			c |= owner
		}
		if c&who == 0 {
			s.log.Warn("caller refused", "gid", gid, "known", ok, "method", r.Method, "path", r.URL.Path)
			writeError(w, &Error{Status: http.StatusForbidden, Message: "this caller may not use " + r.Method + " " + r.URL.Path})
			return
		}
		h(w, r)
	}
}

// peerGID is the primary group of the process at the other end of a Unix
// socket, as the kernel recorded it at connect.
func peerGID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("%T is not a Unix socket", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Gid, nil
}

func (s *Server) spec(w http.ResponseWriter, _ *http.Request) {
	spec := s.c.Spec()
	spec.Protocol = ProtocolVersion
	_, spec.Media = s.c.(MediaFetcher)
	_, spec.Unlink = s.c.(Unlinker)
	writeJSON(w, http.StatusOK, spec)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	h := s.c.Health(r.Context())
	if !h.Until.IsZero() {
		h.Until = h.Until.UTC()
	}
	writeJSON(w, http.StatusOK, h)
}

type opRequest struct {
	Op   string            `json:"op"`
	Args map[string]string `json:"args"`
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	var req opRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	reader, ok := s.c.(Reader)
	op, known := s.c.Spec().Op(req.Op)
	if !ok || !known || op.Kind != Read {
		writeError(w, &Error{Status: http.StatusNotFound, Message: fmt.Sprintf("no read op %q", req.Op)})
		return
	}
	res, err := reader.Read(r.Context(), req.Op, req.Args)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) prepare(w http.ResponseWriter, r *http.Request) {
	var req opRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	op, ok := s.c.Spec().Op(req.Op)
	if !ok || op.Kind != Write {
		writeError(w, &Error{Status: http.StatusNotFound, Message: fmt.Sprintf("no write op %q", req.Op)})
		return
	}
	p, err := s.c.Prepare(r.Context(), req.Op, req.Args)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request) {
	s.executing.Add(1)
	defer s.executing.Done()
	var e record.Envelope
	if !decodeJSON(w, r, &e) {
		return
	}
	refuse := func(status int, msg string) {
		s.log.Warn("approval refused", "err", msg)
		writeError(w, &Error{Status: status, Message: msg})
	}
	if e.Kind != "write" {
		refuse(http.StatusForbidden, "not a write approval")
		return
	}
	s.mu.Lock()
	rec, err := s.keys.Verify(e, time.Now())
	s.mu.Unlock()
	if err != nil {
		refuse(http.StatusForbidden, "approval: "+err.Error())
		return
	}
	wr := rec.(record.Write)
	spec := s.c.Spec()
	if op, ok := spec.Op(wr.Op); wr.Connector != spec.Name || !ok || op.Kind != Write {
		refuse(http.StatusForbidden, fmt.Sprintf("approval is for %s %s, not a write op of %s", wr.Connector, wr.Op, spec.Name))
		return
	}
	// A record whose display differs from what its args render now showed
	// the owner something other than what would run.
	p, err := s.c.Prepare(r.Context(), wr.Op, wr.Args)
	if err != nil {
		writeError(w, err)
		return
	}
	if !maps.Equal(p.Args, wr.Args) || !slices.Equal(p.Display, wr.Display) {
		refuse(http.StatusConflict, "the approved write no longer renders as approved; propose it again")
		return
	}
	if err := s.spent.spend(wr.ID, e.Nonce); errors.Is(err, errSpent) {
		refuse(http.StatusConflict, err.Error())
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	// Past the spend, the write runs even if the owner hangs up.
	res := s.c.Execute(context.WithoutCancel(r.Context()), wr)
	s.log.Info("executed", "id", wr.ID, "op", wr.Op, "status", res.Status, "ref", res.Ref)
	writeJSON(w, http.StatusOK, res)
}

// KeysHead is how far a key log goes.
type KeysHead struct {
	Length int    `json:"length"`
	Head   string `json:"head"`
}

func (s *Server) getKeys(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	n, head := s.keys.Head()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, KeysHead{n, head})
}

func (s *Server) putKeys(w http.ResponseWriter, r *http.Request) {
	var entries []record.Envelope
	if !decodeJSON(w, r, &entries) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Extend replaces the log's slices rather than changing them, so the
	// copy leaves s.keys alone until the new log is on disk.
	next := *s.keys
	if err := next.Extend(entries); err != nil {
		s.log.Warn("key log refused", "err", err)
		writeError(w, &Error{Status: http.StatusConflict, Message: "key log refused: " + err.Error()})
		return
	}
	if err := saveKeys(filepath.Join(s.dir, "private", "keys.json"), &next); err != nil {
		writeError(w, fmt.Errorf("save key log: %w", err))
		return
	}
	*s.keys = next
	n, head := next.Head()
	s.log.Info("key log updated", "length", n, "head", head)
	writeJSON(w, http.StatusOK, KeysHead{n, head})
}

type setupRequest struct {
	Step   string            `json:"step"`
	Values map[string]string `json:"values"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	step, err := s.c.Setup(r.Context(), req.Step, req.Values)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, step)
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request) {
	u, ok := s.c.(Unlinker)
	if !ok {
		writeError(w, &Error{Status: http.StatusNotFound, Message: "this connector cannot be unlinked"})
		return
	}
	if err := u.Unlink(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	s.log.Info("unlinked")
	writeJSON(w, http.StatusOK, struct{}{})
}

type mediaRequest struct {
	Message string `json:"message"`
	Index   int    `json:"index"`
	Limit   int64  `json:"limit"`
}

// maxFetches bounds the files downloading at once, since some services
// only hand them over whole.
const maxFetches = 2

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	var req mediaRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m, ok := s.c.(MediaFetcher)
	if !ok {
		writeError(w, &Error{Status: http.StatusNotFound, Message: "this connector serves no media"})
		return
	}
	if req.Message == "" || req.Index < 0 || req.Limit <= 0 {
		writeError(w, Invalid("a message, an index from 0 and a positive limit are required"))
		return
	}
	if s.fetching.Add(1) > maxFetches {
		s.fetching.Add(-1)
		writeError(w, &Error{Status: http.StatusTooManyRequests, Message: "other files are downloading", RetryAfter: 5})
		return
	}
	defer s.fetching.Add(-1)
	f, err := m.Media(r.Context(), req.Message, req.Index, req.Limit)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Body.Close()
	if f.Size > req.Limit {
		writeError(w, TooLarge(req.Limit))
		return
	}
	h := w.Header()
	h.Set("Content-Type", cmp.Or(f.Type, "application/octet-stream"))
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	if d := mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}); f.Name != "" && d != "" {
		h.Set("Content-Disposition", d)
	}
	if _, err := io.CopyN(w, f.Body, f.Size); err != nil {
		s.log.Warn("send media", "message", req.Message, "index", req.Index, "err", err)
	}
}

// Container is a stored chat as GET /containers lists it.
type Container struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	ParentID     string    `json:"parent_id"`
	LastActivity time.Time `json:"last_activity,omitzero"`
}

const maxContainers = 500

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxContainers {
			writeError(w, Invalid("limit must be from 1 to %d", maxContainers))
			return
		}
		limit = n
	}
	out, err := s.listContainers(r.Context(), q.Get("q"), q.Get("parent"), limit)
	if err != nil {
		writeError(w, fmt.Errorf("list chats: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// listContainers lists the chats whose name contains q (any case) or whose
// ID is q, under parent if it is set, most recently active first.
func (s *Server) listContainers(ctx context.Context, q, parent string, limit int) ([]Container, error) {
	out := []Container{}
	path := filepath.Join(s.dir, "data.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	db, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rs, err := db.QueryContext(ctx, `
SELECT id, kind, COALESCE(parent_id, ''), COALESCE(name, ''),
       max(COALESCE(last_message_at, ''), COALESCE((SELECT max(sent_at) FROM messages WHERE container_id = c.id), '')) AS active
FROM containers AS c
WHERE (?1 = '' OR id = ?1 OR instr(lower(COALESCE(name, '')), lower(?1)))
  AND (?2 = '' OR parent_id = ?2)
ORDER BY active DESC, id
LIMIT ?3`, q, parent, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var (
			c      Container
			active sql.NullString
		)
		if err := rs.Scan(&c.ID, &c.Kind, &c.ParentID, &c.Name, &active); err != nil {
			return nil, err
		}
		if active.String != "" {
			if c.LastActivity, err = time.Parse(time.RFC3339Nano, active.String); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	return out, rs.Err()
}

// decodeJSON strictly decodes the request body into v, answering the
// refusal itself when it fails.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, &Error{Status: status, Message: "invalid request: " + err.Error()})
		return false
	}
	return true
}

// writeError answers with err as an *Error, keeping its status (400 if
// unset); any other error is a 500.
func writeError(w http.ResponseWriter, err error) {
	e, ok := errors.AsType[*Error](err)
	if !ok {
		e = &Error{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	status := cmp.Or(e.Status, http.StatusBadRequest)
	writeJSON(w, status, e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}
