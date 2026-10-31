package connector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/adeci/mandate/approval"
)

const maxBody = 1 << 20

// Server serves a Connector. Writes run only with an approval the server
// verifies itself, so nothing that can reach the socket can make it act alone.
type Server struct {
	Connector Connector
	Verifier  *approval.Verifier
	Seen      *approval.Seen
	Log       *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /spec", s.spec)
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /setup", s.setup)
	mux.HandleFunc("POST /read", s.read)
	mux.HandleFunc("POST /prepare", s.prepare)
	mux.HandleFunc("POST /execute", s.execute)
	f, ok := s.Connector.(Forwarder)
	if !ok {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Route on the raw request target: ServeMux would clean "..", "//"
		// and redirect instead of refusing, and the decoded URL loses %2F.
		raw, _, _ := strings.Cut(r.RequestURI, "?")
		rest, ok := strings.CutPrefix(raw, "/forward/")
		if !ok {
			mux.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			WriteError(w, &Error{Status: http.StatusMethodNotAllowed, Message: "raw reads are GET or HEAD"})
			return
		}
		f.Forward(w, r, rest)
	})
}

func (s *Server) Serve(ctx context.Context, path string) error {
	return ServeUnix(ctx, path, s.Handler())
}

// ServeUnix serves h on a Unix socket at path until ctx is done.
func ServeUnix(ctx context.Context, path string, h http.Handler) error {
	ln, err := ListenUnix(path)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
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
	if s.Verifier == nil {
		WriteError(w, &Error{Status: http.StatusForbidden, Message: "this connector has no approvers, so it refuses writes"})
		return
	}
	rec, err := s.Verifier.Verify(a)
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
	if err := s.Seen.Use(a.Nonce, a.IssuedAt); err != nil {
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
