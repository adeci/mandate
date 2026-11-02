package connector

import (
	"cmp"
	"context"
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
	"os/user"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/store"
)

const maxBody = 1 << 20

// Server serves a Connector. Writes run only with an approval it verifies
// itself against Keys; with nil Keys every write is refused.
type Server struct {
	Connector Connector
	Keys      *approval.Keys
	Seen      *approval.Seen
	// Peers limits who may call; nil lets anyone who can open the socket
	// call every route.
	Peers *Peers
	Log   *slog.Logger
	// Data is the connector's message store, which GET /containers lists
	// for the console; empty for a connector that keeps no messages.
	Data string
	// SocketGroup, if set, owns the socket Serve listens on.
	SocketGroup string

	mediaFetches atomic.Int32
}

// Peers names the uids that may call a Server over its socket. Each route
// answers one of them or both, as callers says.
type Peers struct {
	Console, Gateway uint32
}

// callers says who may use a route. Only the gateway, which enforces
// agents' grants, reads stored messages, files and forwarded APIs; only the
// console, which holds the owner's approvals, changes the connector or lists
// its chats.
type callers int

const (
	gatewayOnly callers = iota
	either
	consoleOnly
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /spec", s.allow(either, s.spec))
	mux.HandleFunc("GET /health", s.allow(either, s.health))
	mux.HandleFunc("POST /setup", s.allow(consoleOnly, s.setup))
	mux.HandleFunc("POST /unlink", s.allow(consoleOnly, s.unlink))
	mux.HandleFunc("POST /read", s.allow(gatewayOnly, s.read))
	mux.HandleFunc("POST /prepare", s.allow(either, s.prepare))
	mux.HandleFunc("POST /execute", s.allow(consoleOnly, s.execute))
	mux.HandleFunc("GET /keys", s.allow(either, s.keysHead))
	mux.HandleFunc("PUT /keys", s.allow(consoleOnly, s.putKeys))
	mux.HandleFunc("POST /media", s.allow(gatewayOnly, s.media))
	if s.Data != "" {
		mux.HandleFunc("GET /containers", s.allow(consoleOnly, s.containers))
	}
	f, ok := s.Connector.(Forwarder)
	if !ok {
		return mux
	}
	forward := s.allow(gatewayOnly, func(w http.ResponseWriter, r *http.Request) {
		// Route on the raw request target: ServeMux would clean "..", "//"
		// and redirect instead of refusing, and the decoded URL loses %2F.
		raw, _, _ := strings.Cut(r.RequestURI, "?")
		rest, _ := strings.CutPrefix(raw, "/forward/")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			WriteError(w, &Error{Status: http.StatusMethodNotAllowed, Message: "raw reads are GET or HEAD"})
			return
		}
		f.Forward(w, r, rest)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.RequestURI, "/forward/") {
			forward(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// allow lets who call h.
func (s *Server) allow(who callers, h http.HandlerFunc) http.HandlerFunc {
	if s.Peers == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := r.Context().Value(peerKey{}).(uint32)
		console, gateway := ok && uid == s.Peers.Console, ok && uid == s.Peers.Gateway
		allowed := console && who != gatewayOnly || gateway && who != consoleOnly
		if !allowed {
			s.Log.Warn("caller refused", "uid", uid, "known", ok, "method", r.Method, "path", r.URL.Path)
			WriteError(w, &Error{Status: http.StatusForbidden, Message: "this caller may not use " + r.Method + " " + r.URL.Path})
			return
		}
		h(w, r)
	}
}

type peerKey struct{}

// peerUID is the uid of the process at the other end of a Unix socket, as
// the kernel recorded it at connect.
func peerUID(c net.Conn) (uint32, error) {
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
	return cred.Uid, nil
}

func (s *Server) Serve(ctx context.Context, path string) error {
	ln, err := ListenUnix(path)
	if err != nil {
		return err
	}
	if s.SocketGroup != "" {
		if err := chgrp(path, s.SocketGroup); err != nil {
			ln.Close()
			return fmt.Errorf("--socket-group: %w", err)
		}
	}
	return serveOn(ctx, ln, s.Handler())
}

func chgrp(path, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	return os.Chown(path, -1, gid)
}

// ServeUnix serves h on a Unix socket at path until ctx is done. Requests
// carry the caller's uid for Server's Peers.
func ServeUnix(ctx context.Context, path string, h http.Handler) error {
	ln, err := ListenUnix(path)
	if err != nil {
		return err
	}
	return serveOn(ctx, ln, h)
}

func serveOn(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if uid, err := peerUID(c); err == nil {
				return context.WithValue(ctx, peerKey{}, uid)
			}
			return ctx
		},
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	case err := <-errc:
		return err
	}
}

// ListenUnix listens at path with mode 0660, replacing a stale socket.
func ListenUnix(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (s *Server) spec(w http.ResponseWriter, _ *http.Request) {
	spec := s.Connector.Spec()
	spec.Protocol = ProtocolVersion
	_, spec.Forward = s.Connector.(Forwarder)
	_, spec.Unlink = s.Connector.(Unlinker)
	_, spec.Media = s.Connector.(MediaFetcher)
	WriteJSON(w, http.StatusOK, spec)
}

type opRequest struct {
	Op   string            `json:"op"`
	Args map[string]string `json:"args"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	h := Health{State: Healthy}
	if c, ok := s.Connector.(Checker); ok {
		h = c.Health(r.Context())
	}
	WriteJSON(w, http.StatusOK, h)
}

type setupRequest struct {
	Args map[string]string `json:"args"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if !DecodeJSON(w, r, &req, maxBody) {
		return
	}
	c, ok := s.Connector.(Setupper)
	if !ok {
		WriteError(w, &Error{Status: http.StatusNotFound, Message: "this connector needs no setup"})
		return
	}
	step, err := c.Setup(r.Context(), req.Args)
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, step)
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request) {
	c, ok := s.Connector.(Unlinker)
	if !ok {
		WriteError(w, &Error{Status: http.StatusNotFound, Message: "this connector cannot be unlinked"})
		return
	}
	if err := c.Unlink(r.Context()); err != nil {
		WriteError(w, err)
		return
	}
	s.Log.Info("unlinked")
	WriteJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	var req opRequest
	if !DecodeJSON(w, r, &req, maxBody) {
		return
	}
	reader, ok := s.Connector.(Reader)
	op, known := s.Connector.Spec().Op(req.Op)
	if !ok || !known || op.Kind != Read {
		WriteError(w, &Error{Status: http.StatusNotFound, Message: fmt.Sprintf("no read op %q", req.Op)})
		return
	}
	out, err := reader.Read(r.Context(), req.Op, req.Args)
	if err != nil {
		WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

type mediaRequest struct {
	Message string `json:"message"`
	Index   int    `json:"index"`
	// Limit is the largest file the caller accepts; 0 means DefaultMediaLimit.
	Limit int64 `json:"limit,omitempty"`
}

// maxMediaFetches bounds the files a connector downloads at once, since
// some services only hand them over whole.
const maxMediaFetches = 2

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	var req mediaRequest
	if !DecodeJSON(w, r, &req, maxBody) {
		return
	}
	m, ok := s.Connector.(MediaFetcher)
	if !ok {
		WriteError(w, &Error{Status: http.StatusNotFound, Message: "this connector serves no media"})
		return
	}
	if req.Message == "" || req.Index < 0 || req.Limit < 0 {
		WriteError(w, Invalid("a message and an index from 0 are required"))
		return
	}
	if s.mediaFetches.Add(1) > maxMediaFetches {
		s.mediaFetches.Add(-1)
		WriteError(w, &Error{Status: http.StatusTooManyRequests, Message: "other files are downloading; try again shortly"})
		return
	}
	defer s.mediaFetches.Add(-1)
	ctx := r.Context()
	if req.Limit > 0 {
		ctx = WithMediaLimit(ctx, req.Limit)
	}
	f, err := m.Media(ctx, req.Message, req.Index)
	if err != nil {
		WriteError(w, err)
		return
	}
	defer f.Body.Close()
	if limit := MediaLimit(ctx); f.Size > limit {
		WriteError(w, TooLarge(limit))
		return
	}
	h := w.Header()
	h.Set("Content-Type", cmp.Or(f.Type, "application/octet-stream"))
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	if d := mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}); f.Name != "" && d != "" {
		h.Set("Content-Disposition", d)
	}
	if _, err := io.Copy(w, f.Body); err != nil {
		s.Log.Warn("send media", "message", req.Message, "index", req.Index, "err", err)
	}
}

func (s *Server) prepare(w http.ResponseWriter, r *http.Request) {
	var req opRequest
	if !DecodeJSON(w, r, &req, maxBody) {
		return
	}
	op, ok := s.Connector.Spec().Op(req.Op)
	if !ok || op.Kind != Write {
		WriteError(w, &Error{Status: http.StatusNotFound, Message: fmt.Sprintf("no write op %q", req.Op)})
		return
	}
	p, err := s.Connector.Prepare(r.Context(), req.Op, req.Args)
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, p)
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request) {
	var a approval.Approval
	if !DecodeJSON(w, r, &a, maxBody) {
		return
	}
	rec, err := (&approval.Verifier{Keys: s.Keys}).Verify(a)
	if err != nil {
		s.Log.Warn("approval rejected", "err", err)
		WriteError(w, &Error{Status: http.StatusForbidden, Message: err.Error()})
		return
	}
	spec := s.Connector.Spec()
	op, ok := spec.Op(rec.Op)
	if rec.Connector != spec.Name || !ok || op.Kind != Write {
		WriteError(w, &Error{Status: http.StatusForbidden, Message: "approval is for a different connector or op"})
		return
	}
	// Re-render from the approved args; a record whose display disagrees with
	// its args showed the owner something other than what would run.
	p, err := s.Connector.Prepare(r.Context(), rec.Op, rec.Args)
	if err != nil {
		WriteError(w, err)
		return
	}
	if !maps.Equal(p.Args, rec.Args) || !slices.Equal(p.Display, rec.Display) {
		WriteError(w, &Error{Status: http.StatusConflict, Message: "approved record no longer matches what would run; propose again"})
		return
	}
	if err := s.Seen.Use(a, rec); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, approval.ErrReplay) {
			status = http.StatusConflict
		}
		WriteError(w, &Error{Status: status, Message: err.Error()})
		return
	}
	res := s.Connector.Execute(r.Context(), rec)
	s.Log.Info("executed", "id", rec.ID, "op", rec.Op, "status", res.Status, "ref", res.Ref)
	WriteJSON(w, http.StatusOK, res)
}

// KeysHead is how far a key log goes; the console pushes its log to a
// connector whose head differs from its own.
type KeysHead struct {
	Length int    `json:"length"`
	Head   string `json:"head"`
}

var errNoKeyLog = &Error{Status: http.StatusServiceUnavailable, Message: "connector started without --rp-id and --origin; it has no keys"}

func (s *Server) keysHead(w http.ResponseWriter, _ *http.Request) {
	if s.Keys == nil {
		WriteError(w, errNoKeyLog)
		return
	}
	n, head := s.Keys.Head()
	WriteJSON(w, http.StatusOK, KeysHead{Length: n, Head: head})
}

// putKeys takes the console's whole log and keeps it only if it extends the
// one held here.
func (s *Server) putKeys(w http.ResponseWriter, r *http.Request) {
	if s.Keys == nil {
		WriteError(w, errNoKeyLog)
		return
	}
	var log []approval.SignedChange
	if !DecodeJSON(w, r, &log, maxBody) {
		return
	}
	if err := s.Keys.Extend(log); err != nil {
		s.Log.Warn("key log refused", "err", err)
		WriteError(w, &Error{Status: http.StatusConflict, Message: "key log refused: " + err.Error()})
		return
	}
	n, head := s.Keys.Head()
	s.Log.Info("key log updated", "length", n, "head", head)
	WriteJSON(w, http.StatusOK, KeysHead{Length: n, Head: head})
}

// Container is a chat as the connector's store keeps it, name included as
// stored (a "~" marks one not from the owner's address book).
type Container struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	ParentID     string    `json:"parent_id,omitempty"`
	LastActivity time.Time `json:"last_activity,omitzero"`
}

const (
	defaultContainers = 50
	maxContainers     = 500
)

// containers serves the console's chat picker, so the console needs no
// read access to the store itself.
func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := defaultContainers
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			WriteError(w, Invalid("limit must be a positive number"))
			return
		}
		limit = min(n, maxContainers)
	}
	listed, err := store.ListContainers(r.Context(), s.Data, q.Get("q"), q.Get("parent"), limit)
	if err != nil {
		WriteError(w, err)
		return
	}
	out := make([]Container, len(listed))
	for i, c := range listed {
		out[i] = Container{ID: c.ID, Name: c.Name, Kind: c.Kind, ParentID: c.ParentID, LastActivity: c.LastActivity}
	}
	WriteJSON(w, http.StatusOK, out)
}
