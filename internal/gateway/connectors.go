package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const (
	// Paced reads may wait up to 90 s for a slot before answering 429.
	readTimeout  = 120 * time.Second
	mediaTimeout = 10 * time.Minute
	callTimeout  = 30 * time.Second
	specTimeout  = 3 * time.Second
	specTTL      = 30 * time.Second

	maxWait      = 900 * time.Second
	pollInterval = time.Second

	maxLimit = 500
)

type connectors struct {
	// calls serves spec and prepare; reads and media get their own longer
	// timeouts.
	calls, reads, media map[string]*connector.Client

	mu    sync.Mutex
	cache map[string]cachedSpec
}

type cachedSpec struct {
	spec connector.Spec
	at   time.Time
}

func newConnectors(sockets map[string]string) *connectors {
	c := &connectors{
		calls: map[string]*connector.Client{},
		reads: map[string]*connector.Client{},
		media: map[string]*connector.Client{},
		cache: map[string]cachedSpec{},
	}
	for name, socket := range sockets {
		c.calls[name] = connector.Dial(socket, callTimeout)
		c.reads[name] = connector.Dial(socket, readTimeout)
		c.media[name] = connector.Dial(socket, mediaTimeout)
	}
	return c
}

// spec fetches lazily and caches only successes, so a connector that comes
// back up is listed again on the next call.
func (c *connectors) spec(ctx context.Context, name string) (connector.Spec, error) {
	c.mu.Lock()
	cached, ok := c.cache[name]
	c.mu.Unlock()
	if ok && time.Since(cached.at) < specTTL {
		return cached.spec, nil
	}
	ctx, cancel := context.WithTimeout(ctx, specTimeout)
	defer cancel()
	s, err := c.calls[name].Spec(ctx)
	if err != nil {
		return connector.Spec{}, err
	}
	// The console routes by the gateway's name and connectors check the record
	// against their own, so a mismatch would only fail at execution.
	if s.Name != name {
		return connector.Spec{}, fmt.Errorf("socket for %s serves connector %q", name, s.Name)
	}
	c.mu.Lock()
	c.cache[name] = cachedSpec{spec: s, at: time.Now()}
	c.mu.Unlock()
	return s, nil
}

func (c *connectors) health(ctx context.Context, name string) (connector.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, specTimeout)
	defer cancel()
	return c.calls[name].Health(ctx)
}

type connectorInfo struct {
	connector.Spec
	// Health is nil for a source served without a connector.
	Health *connector.Health `json:"health,omitempty"`
	// Source connectors answer chats, messages and search.
	Source bool `json:"source"`
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	names := slices.Concat(slices.Collect(maps.Keys(h.cfg.Sources)), slices.Collect(maps.Keys(h.connectors.calls)))
	slices.Sort(names)
	out := []connectorInfo{}
	for _, name := range slices.Compact(names) {
		_, source := h.cfg.Sources[name]
		info := connectorInfo{Spec: connector.Spec{Name: name, Ops: []connector.Op{}}, Source: source}
		if _, ok := h.connectors.calls[name]; ok {
			info.Health = &connector.Health{State: connector.Broken, Detail: "not answering"}
			s, err := h.connectors.spec(r.Context(), name)
			if pe, ok := errors.AsType[*connector.ProtocolError](err); ok {
				info.Health.Detail = pe.Error()
			} else if err != nil {
				slog.Warn("connector spec", "connector", name, "err", err)
			} else if hl, err := h.connectors.health(r.Context(), name); err != nil {
				slog.Warn("connector health", "connector", name, "err", err)
			} else {
				info.Spec, info.Health = s, &hl
			}
		}
		out = append(out, info)
	}
	connector.WriteJSON(w, http.StatusOK, out)
}

func (h *handler) spec(w http.ResponseWriter, r *http.Request, name string) (connector.Spec, bool) {
	if _, ok := h.connectors.calls[name]; !ok {
		msg := fmt.Sprintf("no connector %q; see mandate --help", name)
		if _, ok := h.cfg.Sources[name]; ok {
			msg = fmt.Sprintf("%s only has stored chats here; see mandate %s --help", name, name)
		}
		writeError(w, http.StatusNotFound, msg)
		return connector.Spec{}, false
	}
	spec, err := h.connectors.spec(r.Context(), name)
	if pe, ok := errors.AsType[*connector.ProtocolError](err); ok {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("connector %s %s; tell the owner", name, pe))
		return connector.Spec{}, false
	}
	if err != nil {
		slog.Error("connector spec", "connector", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, unavailable("connector "+name))
		return connector.Spec{}, false
	}
	return spec, true
}

func (h *handler) healthy(w http.ResponseWriter, r *http.Request, name string) bool {
	hl, err := h.connectors.health(r.Context(), name)
	if err != nil {
		slog.Error("connector health", "connector", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, unavailable("connector "+name))
		return false
	}
	detail := hl.Detail
	if detail == "" {
		detail = string(hl.State)
	}
	switch hl.State {
	case connector.Healthy:
		return true
	case connector.Resting:
		retry := max(int(math.Ceil(time.Until(hl.Until).Seconds())), 1)
		if hl.Until.IsZero() {
			retry = 60
		}
		connector.WriteJSON(w, http.StatusTooManyRequests, connector.Error{
			Message: fmt.Sprintf("%s is resting: %s", name, detail), RetryAfter: retry,
		})
		return false
	case connector.Connecting:
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s is %s; try again in a minute", name, detail))
		return false
	}
	console := "the console"
	if h.cfg.ConsoleURL != "" {
		console = strings.TrimRight(h.cfg.ConsoleURL, "/") + "/connections"
	}
	writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s needs the owner: %s. Ask them to check %s", name, detail, console))
	return false
}

func unavailable(who string) string {
	return who + " is unavailable; try again in a minute, and tell the owner if it persists"
}

// Unknown and hidden chats get the same answer.
func unknownChat(name, id string) string {
	return fmt.Sprintf("unknown chat %q: run mandate %s chats --search %s", id, name, shellWord(id))
}

func shellWord(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`*?[]#~;&|<>(){}!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func checkArgs(op connector.Op, args map[string]string) error {
	declared := map[string]bool{}
	for _, p := range op.Params {
		declared[p.Name] = true
		if p.Required && args[p.Name] == "" {
			return fmt.Errorf("%s: missing required param %q", op.Name, p.Name)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(args)) {
		if !declared[k] {
			return fmt.Errorf("%s: unknown param %q", op.Name, k)
		}
	}
	return nil
}

// relay passes a connector's or the console's refusal through with its status
// and message; anything else means the peer is unreachable.
func relay(w http.ResponseWriter, who string, err error) {
	if e, ok := errors.AsType[*connector.Error](err); ok {
		connector.WriteJSON(w, e.Status, e)
		return
	}
	slog.Error("call failed", "peer", who, "err", err)
	writeError(w, http.StatusServiceUnavailable, unavailable(who))
}

func intParam(q url.Values, key string, def, lo, hi int64) (int64, error) {
	v := q.Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("--%s must be a whole number from %d to %d", key, lo, hi)
	}
	return n, nil
}

func (h *handler) verb(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	name, verb := r.PathValue("connector"), r.PathValue("verb")
	if _, ok := h.cfg.Sources[name]; !ok {
		if _, ok := h.connectors.calls[name]; ok {
			writeError(w, http.StatusNotFound, fmt.Sprintf("%s keeps no chats or messages; see mandate %s --help", name, name))
		} else {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no connector %q; see mandate --help", name))
		}
		return
	}
	if verb == "media" {
		h.media(w, r, name)
		return
	}
	q := r.URL.Query()
	var chat string
	var run func(*source) (*table, error)
	limit, err := intParam(q, "limit", 20, 1, maxLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := intParam(q, "before", 0, 0, math.MaxInt64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch verb {
	case "chats":
		run = func(s *source) (*table, error) { return s.chats(q.Get("search"), limit) }
	case "messages":
		if chat = q.Get("chat"); chat == "" {
			writeError(w, http.StatusBadRequest, "chat is required")
			return
		}
		run = func(s *source) (*table, error) { return s.messages(chat, before, limit) }
	case "search":
		text, since := q.Get("text"), q.Get("since")
		if text == "" {
			writeError(w, http.StatusBadRequest, "text is required")
			return
		}
		if _, err := time.Parse(time.DateOnly, since); since != "" && err != nil {
			writeError(w, http.StatusBadRequest, "--since must be YYYY-MM-DD")
			return
		}
		if !q.Has("limit") {
			limit = 50
		}
		chat = q.Get("chat")
		run = func(s *source) (*table, error) { return s.search(text, chat, since, before, limit) }
	default:
		writeError(w, http.StatusNotFound, fmt.Sprintf("no read %q: reads are chats, messages and search; see mandate %s --help", verb, name))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	s, ok := h.source(ctx, w, name)
	if !ok {
		return
	}
	defer s.close()
	if chat != "" && !s.check(w, name, chat) {
		return
	}
	t, err := run(s)
	attrs := []any{"remote", r.RemoteAddr, "connector", name, "verb", verb, "duration_ms", time.Since(start).Milliseconds()}
	if err != nil {
		slog.Error("verb", append(attrs, "err", err)...)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable; try again, and tell the owner if it persists")
		return
	}
	slog.Info("verb", append(attrs, "rows", len(t.Rows))...)
	connector.WriteJSON(w, http.StatusOK, t)
}

func (h *handler) source(ctx context.Context, w http.ResponseWriter, name string) (*source, bool) {
	s, err := h.openSource(ctx, name)
	if err != nil {
		slog.Error("open source", "source", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable; tell the owner")
		return nil, false
	}
	return s, true
}

func (s *source) check(w http.ResponseWriter, name, chat string) bool {
	ok, err := s.visible(chat)
	if err != nil {
		slog.Error("chat lookup", "source", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable; tell the owner")
		return false
	}
	if !ok {
		writeError(w, http.StatusNotFound, unknownChat(name, chat))
	}
	return ok
}

// chatsVisible refuses unless every id is a visible chat of the connector's
// source. Without a source every id is refused.
func (h *handler) chatsVisible(w http.ResponseWriter, r *http.Request, name string, ids []string) bool {
	if len(ids) == 0 {
		return true
	}
	if _, ok := h.cfg.Sources[name]; !ok {
		writeError(w, http.StatusForbidden, fmt.Sprintf("connector %s has no data source on this gateway, so it cannot target chats or channels", name))
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	s, ok := h.source(ctx, w, name)
	if !ok {
		return false
	}
	defer s.close()
	for _, id := range ids {
		if !s.check(w, name, id) {
			return false
		}
	}
	return true
}

type opRequest struct {
	Args map[string]string `json:"args"`
	// Note is the agent's explanation for a write, shown to the owner.
	Note string `json:"note,omitempty"`
}

// op runs a read op or proposes a write op.
func (h *handler) op(w http.ResponseWriter, r *http.Request) {
	name, opName := r.PathValue("connector"), r.PathValue("op")
	var req opRequest
	if !connector.DecodeJSON(w, r, &req, maxRequestBytes) {
		return
	}
	spec, ok := h.spec(w, r, name)
	if !ok {
		return
	}
	op, ok := spec.Op(opName)
	if !ok {
		var cmds []string
		if _, ok := h.cfg.Sources[name]; ok {
			cmds = append(cmds, "chats", "messages", "search")
			if spec.Media {
				cmds = append(cmds, "media")
			}
		}
		for _, op := range spec.Ops {
			cmds = append(cmds, op.Name)
		}
		if spec.Forward {
			cmds = append(cmds, "get")
		}
		writeError(w, http.StatusNotFound, fmt.Sprintf("no command %q; %s commands: %s", opName, name, strings.Join(cmds, " ")))
		return
	}
	if op.Kind == connector.Write && h.console == nil {
		writeError(w, http.StatusNotFound, "proposals are not enabled on this gateway")
		return
	}
	if err := checkArgs(op, req.Args); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s; see mandate %s --help", err, name))
		return
	}
	var ids []string
	for _, p := range op.Params {
		if id := req.Args[p.Name]; p.Container && id != "" {
			ids = append(ids, id)
		}
	}
	if !h.chatsVisible(w, r, name, ids) || !h.healthy(w, r, name) {
		return
	}
	if op.Kind == connector.Read {
		h.read(w, r, name, opName, req.Args)
		return
	}
	h.propose(w, r, name, opName, req)
}

func (h *handler) read(w http.ResponseWriter, r *http.Request, name, op string, args map[string]string) {
	start := time.Now()
	raw, err := h.connectors.reads[name].Read(r.Context(), op, args)
	attrs := []any{"remote", r.RemoteAddr, "connector", name, "op", op, "duration_ms", time.Since(start).Milliseconds()}
	if err != nil {
		slog.Info("read", append(attrs, "err", err.Error())...)
		relay(w, "connector "+name, err)
		return
	}
	var res connector.ReadResult
	if err := json.Unmarshal(raw, &res); err != nil {
		slog.Error("decode read result", "connector", name, "err", err)
		writeError(w, http.StatusBadGateway, "connector returned an invalid read result")
		return
	}
	slog.Info("read", append(attrs, "messages", len(res.MessageIDs), "containers", len(res.ContainerIDs))...)
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	s, ok := h.source(ctx, w, name)
	if !ok {
		return
	}
	defer s.close()
	t, err := s.byID(res.MessageIDs)
	if err != nil {
		slog.Error("read back messages", "source", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable; tell the owner")
		return
	}
	if t.Containers, err = s.visibleCount(res.ContainerIDs); err != nil {
		slog.Error("count chats", "source", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable; tell the owner")
		return
	}
	t.More = res.More
	connector.WriteJSON(w, http.StatusOK, t)
}

func (h *handler) propose(w http.ResponseWriter, r *http.Request, name, op string, req opRequest) {
	start := time.Now()
	attrs := []any{"remote", r.RemoteAddr, "connector", name, "op", op}
	prepared, err := h.connectors.calls[name].Prepare(r.Context(), op, req.Args)
	if err != nil {
		slog.Info("propose", append(attrs, "err", err.Error())...)
		relay(w, "connector "+name, err)
		return
	}
	if prepared.Container != "" && !h.chatsVisible(w, r, name, []string{prepared.Container}) {
		slog.Info("propose", append(attrs, "refused", "prepared container")...)
		return
	}
	p, err := h.console.Submit(r.Context(), proposal.Request{
		Connector: name, Op: op, Args: prepared.Args, Note: req.Note,
	})
	if err != nil {
		slog.Info("propose", append(attrs, "err", err.Error())...)
		relay(w, "console", err)
		return
	}
	slog.Info("propose", append(attrs, "id", p.ID, "duration_ms", time.Since(start).Milliseconds())...)
	connector.WriteJSON(w, http.StatusAccepted, p)
}

var proposalID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// status answers a proposal's state. With wait=SECONDS it long-polls the
// console until the proposal is settled or the wait is over, so an agent can
// block in one call instead of spending a turn per poll.
func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	if h.console == nil {
		writeError(w, http.StatusNotFound, "proposals are not enabled on this gateway")
		return
	}
	id := r.PathValue("id")
	if !proposalID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid proposal id")
		return
	}
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "wait must be whole seconds, e.g. wait=600")
			return
		}
		wait = min(time.Duration(n)*time.Second, maxWait)
	}
	deadline := time.Now().Add(wait)
	for {
		p, err := h.console.Get(r.Context(), id)
		if err != nil {
			relay(w, "console", err)
			return
		}
		if settled(p.Status) || !time.Now().Before(deadline) {
			connector.WriteJSON(w, http.StatusOK, p)
			return
		}
		t := time.NewTimer(min(pollInterval, time.Until(deadline)))
		select {
		case <-r.Context().Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func settled(s proposal.Status) bool {
	return s != proposal.Pending && s != proposal.Executing
}
