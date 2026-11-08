package connector

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	"time"

	"github.com/adeci/mandate/internal/peer"
	"github.com/adeci/mandate/record"
	"github.com/adeci/mandate/store"
)

const maxBody = 1 << 20

// Server serves a Connector to the gateway and owner groups.
type Server struct {
	c              Connector
	name           string
	dir            string
	gateway, owner uint32
	log            *slog.Logger

	mu    sync.Mutex // guards keys
	keys  *record.Log
	spent *spent

	executing sync.WaitGroup
	fetching  atomic.Int32
	icons     atomic.Int32 // pictures fetching
}

// NewServer serves c under name, with its state in dir, where dir/private
// exists. Only callers whose primary group is gateway or owner may call it,
// each on its own routes.
func NewServer(c Connector, name, dir string, gateway, owner uint32, log *slog.Logger) (*Server, error) {
	keys, err := loadKeys(filepath.Join(dir, "private", keyLogFile))
	if err != nil {
		return nil, err
	}
	sp, err := openSpent(filepath.Join(dir, "private", "spent"))
	if err != nil {
		return nil, err
	}
	return &Server{c: c, name: name, dir: dir, gateway: gateway, owner: owner, log: log, keys: keys, spent: sp}, nil
}

// Serve serves on ln until ctx is done, then waits for writes in flight.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	defer s.spent.close()
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext:       peer.ConnContext,
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

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	both := []uint32{s.gateway, s.owner}
	route := func(pattern string, gids []uint32, h http.HandlerFunc) {
		mux.Handle(pattern, peer.Allow(s.log, h, gids...))
	}
	route("GET /spec", both, s.spec)
	route("GET /health", both, s.health)
	route("GET /keys", both, s.getKeys)
	route("POST /prepare", both, s.prepare)
	route("POST /read", []uint32{s.gateway}, s.read)
	route("POST /media", []uint32{s.gateway}, s.media)
	route("POST /setup", []uint32{s.owner}, s.setup)
	route("POST /unlink", []uint32{s.owner}, s.unlink)
	route("POST /execute", []uint32{s.owner}, s.execute)
	route("PUT /keys", []uint32{s.owner}, s.putKeys)
	route("GET /containers", []uint32{s.owner}, s.containers)
	route("GET /icon", []uint32{s.owner}, s.icon)
	return mux
}

func (s *Server) spec(w http.ResponseWriter, _ *http.Request) {
	spec := s.c.Spec()
	spec.Protocol = ProtocolVersion
	spec.Name = s.name
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
	if op, ok := s.c.Spec().Op(wr.Op); wr.Connector != s.name || !ok || op.Kind != Write {
		refuse(http.StatusForbidden, fmt.Sprintf("approval is for %s %s, not a write op of %s", wr.Connector, wr.Op, s.name))
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
	if err := saveKeys(filepath.Join(s.dir, "private", keyLogFile), &next); err != nil {
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

// icon serves a chat's picture, fetched from the service as its stored icon
// names: 404 for a chat without one. The answer names the picture's
// version in Mandate-Icon, as the listing does.
func (s *Server) icon(w http.ResponseWriter, r *http.Request) {
	f, ok := s.c.(IconFetcher)
	if !ok {
		writeError(w, &Error{Status: http.StatusNotFound, Message: "this connector serves no pictures"})
		return
	}
	chat := r.URL.Query().Get("chat")
	if chat == "" {
		writeError(w, Invalid("a chat is required"))
		return
	}
	ref, err := s.iconRef(r.Context(), chat)
	if err != nil {
		writeError(w, fmt.Errorf("read the chat's icon: %w", err))
		return
	}
	if ref == "" {
		writeError(w, &Error{Status: http.StatusNotFound, Message: "the chat has no picture"})
		return
	}
	if s.icons.Add(1) > maxFetches {
		s.icons.Add(-1)
		writeError(w, &Error{Status: http.StatusTooManyRequests, Message: "other pictures are downloading", RetryAfter: 1})
		return
	}
	defer s.icons.Add(-1)
	ic, err := f.Icon(r.Context(), chat, ref)
	switch {
	case err != nil:
		writeError(w, err)
		return
	case ic.Type != "image/png" && ic.Type != "image/jpeg":
		writeError(w, fmt.Errorf("the picture is %q, not PNG or JPEG", ic.Type))
		return
	case len(ic.Body) > MaxIcon:
		writeError(w, fmt.Errorf("the picture is larger than %d bytes", MaxIcon))
		return
	}
	h := w.Header()
	h.Set("Content-Type", ic.Type)
	h.Set("Content-Length", strconv.Itoa(len(ic.Body)))
	h.Set("Mandate-Icon", iconVersion(ref))
	_, _ = w.Write(ic.Body)
}

// iconRef is the chat's stored icon, "" for none.
func (s *Server) iconRef(ctx context.Context, chat string) (string, error) {
	path := filepath.Join(s.dir, "data.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	db, err := store.OpenReadOnly(ctx, path, nil)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var ref sql.NullString
	err = db.QueryRowContext(ctx, `SELECT icon FROM containers WHERE id = ?1`, chat).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return ref.String, err
}

// iconVersion is a short hash of an icon ref: it changes with the picture
// and tells nothing of the ref, which never leaves the connector.
func iconVersion(ref string) string {
	if ref == "" {
		return ""
	}
	h := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(h[:6])
}

// Container is a stored chat as GET /containers lists it.
type Container struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	ParentID string `json:"parent_id"`
	// ParentName is the parent's path, "Inbox / Receipts", for showing the
	// chat as "Inbox / Receipts / Your order"; empty at the top.
	ParentName   string    `json:"parent_name,omitempty"`
	LastActivity time.Time `json:"last_activity,omitzero"`
	// Participants is set for DMs only.
	Participants []store.Participant `json:"participants,omitempty"`
	// Icon is the version of the chat's picture, for GET /icon; absent when
	// it has none.
	Icon string `json:"icon,omitempty"`
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

// pickerChats is every chat with its path and last activity, for
// store.Matching.
const pickerChats = `WITH candidates AS (
  SELECT c.id, c.kind, c.parent_id, c.name AS own,
         concat_ws(' / ', p3.name, p2.name, p1.name, c.name) AS path,
         concat_ws(' / ', p3.name, p2.name, p1.name) AS parent_path,
         NULLIF(max(COALESCE(c.last_message_at, ''), COALESCE((SELECT max(sent_at) FROM messages WHERE container_id = c.id), '')), '') AS at
  FROM containers AS c
  LEFT JOIN containers AS p1 ON p1.id = c.parent_id
  LEFT JOIN containers AS p2 ON p2.id = p1.parent_id
  LEFT JOIN containers AS p3 ON p3.id = p2.parent_id)`

// listContainers lists the chats under parent if it is set: all of them,
// the most recently active first, or those q finds as an agent's request
// term would.
func (s *Server) listContainers(ctx context.Context, q, parent string, limit int) ([]Container, error) {
	out := []Container{}
	path := filepath.Join(s.dir, "data.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	db, err := store.OpenReadOnly(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	const columns = `SELECT c.id, c.kind, COALESCE(c.parent_id, ''), COALESCE(c.name, ''), cand.parent_path, c.participants_json, cand.at, COALESCE(c.icon, '')`
	query := pickerChats + `
` + columns + ` FROM candidates AS cand JOIN containers AS c ON c.id = cand.id
WHERE ?2 = '' OR c.parent_id = ?2
ORDER BY cand.at DESC NULLS LAST, c.id LIMIT ?3`
	var terms string
	if q != "" {
		if terms, err = store.Terms([]string{q}); err != nil {
			return nil, err
		}
		query = pickerChats + `, ` + store.Matching("?1") + `
` + columns + ` FROM matched AS m JOIN containers AS c ON c.id = m.id JOIN candidates AS cand ON cand.id = m.id
WHERE ?2 = '' OR c.parent_id = ?2
ORDER BY m.n LIMIT ?3`
	}
	rs, err := db.QueryContext(ctx, query, terms, parent, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var (
			c      Container
			parts  string
			active sql.NullString
			ref    string
		)
		if err := rs.Scan(&c.ID, &c.Kind, &c.ParentID, &c.Name, &c.ParentName, &parts, &active, &ref); err != nil {
			return nil, err
		}
		c.Icon = iconVersion(ref)
		if c.Kind == "dm" {
			if err := json.Unmarshal([]byte(parts), &c.Participants); err != nil {
				return nil, fmt.Errorf("participants of chat %s: %w", c.ID, err)
			}
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
