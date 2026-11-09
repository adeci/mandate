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
	"strings"
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
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Parents are the chats it is in, the outermost first.
	Parents []Parent `json:"parents"`
	// LastActivity is a shelf's newest chat's: a guild's, space's, forum's
	// or mailbox's.
	LastActivity time.Time `json:"last_activity,omitzero"`
	// Opens says it holds chats: a shelf, or a chat with threads.
	Opens bool `json:"opens,omitempty"`
	// Members counts a group's stored members and the owner.
	Members int `json:"members,omitempty"`
	// Threads counts the chats filed directly in a mailbox.
	Threads int `json:"threads,omitempty"`
	// Icon is the version of the chat's picture, for GET /icon; absent when
	// it has none.
	Icon string `json:"icon,omitempty"`
	// Handle is a DM's person's @name or number, as the service shows it.
	Handle string `json:"handle,omitempty"`
	// Participants are a DM's person, and a group's members when listed by
	// ID.
	Participants []store.Participant `json:"participants,omitempty"`
	// Hidden is set by the owner side, which knows never-share: the chat,
	// a chat it is in, or a DM's person is never shared.
	Hidden bool `json:"hidden,omitempty"`
}

type Parent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Icon string `json:"icon,omitempty"`
}

// ContainerQuery is what GET /containers lists:
//   - IDs: exactly those chats, in that order;
//   - Q: the shelves whose own name has every word, as headings, then the
//     chats Q finds as an agent's request term would, all under Parent at
//     any depth if it is set;
//   - otherwise: the shelves in Parent, then up to Limit of its other
//     chats; no Parent is the top level.
//
// Shelves, the guilds, spaces, forums and mailboxes a chat is filed in, are
// never cut by Limit, up to the 500 a call returns. Each list is the newest
// first.
type ContainerQuery struct {
	Q      string
	Parent string
	IDs    []string
	Limit  int
}

const maxContainers = 500

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := ContainerQuery{Q: v.Get("q"), Parent: v.Get("parent"), Limit: 50}
	if ids := v.Get("ids"); ids != "" {
		q.IDs = strings.Split(ids, ",")
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxContainers {
			writeError(w, Invalid("limit must be from 1 to %d", maxContainers))
			return
		}
		q.Limit = n
	}
	if len(q.IDs) > maxContainers {
		writeError(w, Invalid("ids lists at most %d chats", maxContainers))
		return
	}
	out, err := s.listContainers(r.Context(), q)
	if err != nil {
		writeError(w, fmt.Errorf("list chats: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// pickerChats is every chat with its path, a DM's handle, parents, last
// activity and counts, as candidates for store.Matching. A shelf is as
// recent as the newest chat in it, at any depth.
const pickerChats = `WITH tree AS (
  SELECT c.id, c.kind, c.parent_id, c.name AS own, c.is_group, c.participants_json, COALESCE(c.icon, '') AS icon,
         p1.id AS p1, COALESCE(p1.name, '') AS p1_name, COALESCE(p1.kind, '') AS p1_kind, COALESCE(p1.icon, '') AS p1_icon,
         p2.id AS p2, COALESCE(p2.name, '') AS p2_name, COALESCE(p2.kind, '') AS p2_kind, COALESCE(p2.icon, '') AS p2_icon,
         p3.id AS p3, COALESCE(p3.name, '') AS p3_name, COALESCE(p3.kind, '') AS p3_kind, COALESCE(p3.icon, '') AS p3_icon,
         concat_ws(' / ', p3.name, p2.name, p1.name, c.name) AS path,
         NULLIF(max(COALESCE(c.last_message_at, ''), COALESCE((SELECT max(sent_at) FROM messages WHERE container_id = c.id), '')), '') AS own_at
  FROM containers AS c
  LEFT JOIN containers AS p1 ON p1.id = c.parent_id
  LEFT JOIN containers AS p2 ON p2.id = p1.parent_id
  LEFT JOIN containers AS p3 ON p3.id = p2.parent_id),
inside(id, at) AS (
  SELECT id, max(at) FROM (
    SELECT p1 AS id, own_at AS at FROM tree WHERE p1 IS NOT NULL
    UNION ALL SELECT p2, own_at FROM tree WHERE p2 IS NOT NULL
    UNION ALL SELECT p3, own_at FROM tree WHERE p3 IS NOT NULL)
  GROUP BY id),
kids(id, n, threads) AS (
  SELECT parent_id, count(*), sum(kind != 'mailbox') FROM containers WHERE parent_id IS NOT NULL GROUP BY parent_id),
candidates AS (
  SELECT t.*, t.kind IN ` + store.HolderKinds + ` OR t.kind = 'mailbox' AS shelf,
         t.kind IN ` + store.HolderKinds + ` OR t.kind = 'mailbox' OR k.n IS NOT NULL AS opens,
         iif(t.kind = 'mailbox', COALESCE(k.threads, 0), 0) AS threads,
         iif(t.kind = 'dm', t.participants_json ->> '$[0].handle') AS handle,
         iif(t.kind IN ` + store.HolderKinds + ` OR t.kind = 'mailbox',
           NULLIF(max(COALESCE(t.own_at, ''), COALESCE(i.at, '')), ''), t.own_at) AS at
  FROM tree AS t LEFT JOIN inside AS i ON i.id = t.id LEFT JOIN kids AS k ON k.id = t.id)`

const pickerColumns = `SELECT c.id, c.kind, COALESCE(c.own, ''), c.at, c.opens, c.threads, c.kind = 'group' OR c.is_group, c.participants_json, c.icon,
  c.p1, c.p1_name, c.p1_kind, c.p1_icon, c.p2, c.p2_name, c.p2_kind, c.p2_icon, c.p3, c.p3_name, c.p3_kind, c.p3_icon`

// under keeps the chats in ?2 at any depth.
const under = `(c.p1 = ?2 OR c.p2 = ?2 OR c.p3 = ?2)`

func (s *Server) listContainers(ctx context.Context, q ContainerQuery) ([]Container, error) {
	out := []Container{}
	path := filepath.Join(s.dir, "data.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, store.ReadTimeout)
	defer cancel()
	db, err := store.OpenReadOnly(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	list := func(query string, args ...any) error {
		cs, err := scanContainers(ctx, db, q.IDs != nil, pickerChats+query, args...)
		out = append(out, cs...)
		return err
	}
	switch {
	case q.IDs != nil:
		b, err := json.Marshal(q.IDs)
		if err != nil {
			return nil, err
		}
		if err := list("\n"+pickerColumns+` FROM candidates AS c WHERE c.id IN (SELECT value FROM json_each(?1))`, string(b)); err != nil {
			return nil, err
		}
		pos := make(map[string]int, len(q.IDs))
		for i, id := range q.IDs {
			pos[id] = i
		}
		slices.SortFunc(out, func(a, b Container) int { return pos[a.ID] - pos[b.ID] })
		return out, nil
	case q.Q != "":
		words, err := json.Marshal(store.Words(q.Q))
		if err != nil {
			return nil, err
		}
		if err := list("\n"+pickerColumns+` FROM candidates AS c
WHERE c.kind IN `+store.HolderKinds+` AND (?2 = '' OR `+under+`) AND json_array_length(?1) > 0
  AND NOT EXISTS (SELECT 1 FROM json_each(?1) AS w WHERE instr(lower(COALESCE(c.own, '')), w.value) = 0)
ORDER BY c.at DESC NULLS LAST, c.id LIMIT ?3`, string(words), q.Parent, maxContainers); err != nil {
			return nil, err
		}
		terms, err := store.Terms([]string{q.Q})
		if err != nil {
			return nil, err
		}
		// A holder the term names exactly is already among the headings.
		err = list(", "+store.Matching("?1", "?4")+"\n"+pickerColumns+` FROM matched AS m JOIN candidates AS c ON c.id = m.id
WHERE c.kind NOT IN `+store.HolderKinds+` AND (?2 = '' OR `+under+`)
ORDER BY m.n LIMIT ?3`, terms, q.Parent, q.Limit, time.Now().UTC().Format(time.RFC3339Nano))
		return out, err
	}
	in := `COALESCE(c.parent_id, '') = ?1`
	if err := list("\n"+pickerColumns+` FROM candidates AS c WHERE c.shelf AND `+in+`
ORDER BY c.at DESC NULLS LAST, c.id LIMIT ?2`, q.Parent, maxContainers); err != nil {
		return nil, err
	}
	err = list("\n"+pickerColumns+` FROM candidates AS c WHERE NOT c.shelf AND `+in+`
ORDER BY c.at DESC NULLS LAST, c.id LIMIT ?2`, q.Parent, q.Limit)
	return out, err
}

// scanContainers reads pickerColumns rows; members keeps groups'
// participants.
func scanContainers(ctx context.Context, db *sql.DB, members bool, query string, args ...any) ([]Container, error) {
	rs, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Container
	for rs.Next() {
		var (
			c        Container
			active   sql.NullString
			group    bool
			parts    string
			icon     string
			ps       [3]Parent
			parentID [3]sql.NullString
		)
		if err := rs.Scan(&c.ID, &c.Kind, &c.Name, &active, &c.Opens, &c.Threads, &group, &parts, &icon,
			&parentID[0], &ps[0].Name, &ps[0].Kind, &ps[0].Icon, &parentID[1], &ps[1].Name, &ps[1].Kind, &ps[1].Icon,
			&parentID[2], &ps[2].Name, &ps[2].Kind, &ps[2].Icon); err != nil {
			return nil, err
		}
		c.Icon = iconVersion(icon)
		c.Parents = []Parent{}
		for i := 2; i >= 0; i-- {
			if parentID[i].Valid {
				ps[i].ID, ps[i].Icon = parentID[i].String, iconVersion(ps[i].Icon)
				c.Parents = append(c.Parents, ps[i])
			}
		}
		if c.Kind == "dm" || group {
			if err := json.Unmarshal([]byte(parts), &c.Participants); err != nil {
				return nil, fmt.Errorf("participants of chat %s: %w", c.ID, err)
			}
		}
		if c.Kind == "dm" && len(c.Participants) > 0 {
			c.Handle = c.Participants[0].Handle
		}
		if group {
			if n := len(c.Participants); n > 0 {
				c.Members = n + 1
			}
			if !members {
				c.Participants = nil
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
