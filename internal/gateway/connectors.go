package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const (
	// readTimeout bounds a connector read. Paced reads (Discord) wait up to
	// 90 s for a slot before answering 429.
	readTimeout = 120 * time.Second
	callTimeout = 30 * time.Second
	specTimeout = 3 * time.Second
	specTTL     = 30 * time.Second

	// maxWait caps an action long-poll; pollInterval is how often it asks the
	// console meanwhile.
	maxWait      = 900 * time.Second
	pollInterval = time.Second
)

type connectors struct {
	// calls serves spec and prepare; reads get their own longer timeout.
	calls, reads map[string]*connector.Client

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
		cache: map[string]cachedSpec{},
	}
	for name, socket := range sockets {
		c.calls[name] = connector.Dial(socket, callTimeout)
		c.reads[name] = connector.Dial(socket, readTimeout)
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
	Health connector.Health `json:"health"`
}

// list describes the connectors that answer, by name.
func (c *connectors) list(ctx context.Context) []connectorInfo {
	out := []connectorInfo{}
	for _, name := range slices.Sorted(maps.Keys(c.calls)) {
		s, err := c.spec(ctx, name)
		if err != nil {
			slog.Warn("connector spec", "connector", name, "err", err)
			continue
		}
		hl, err := c.health(ctx, name)
		if err != nil {
			slog.Warn("connector health", "connector", name, "err", err)
			hl = connector.Health{State: connector.Broken, Detail: "not answering"}
		}
		out = append(out, connectorInfo{Spec: s, Health: hl})
	}
	return out
}

// lookup resolves connector and op, checks args against the op's params, and
// refuses connectors that are not healthy before they are called. Without a
// source there are no filtered views to check containers against, so
// container values are refused outright.
func (h *handler) lookup(w http.ResponseWriter, r *http.Request, name, opName string, kind connector.Kind, args map[string]string) (connector.Op, bool) {
	if _, ok := h.connectors.calls[name]; !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no connector %q", name))
		return connector.Op{}, false
	}
	spec, err := h.connectors.spec(r.Context(), name)
	if err != nil {
		slog.Error("connector spec", "connector", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, unavailable("connector "+name))
		return connector.Op{}, false
	}
	op, ok := spec.Op(opName)
	if !ok || op.Kind != kind {
		writeError(w, http.StatusNotFound, fmt.Sprintf("connector %s has no %s op %q; see mandate guide for its ops", name, kind, opName))
		return connector.Op{}, false
	}
	if err := checkArgs(op, args); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return connector.Op{}, false
	}
	if _, ok := h.cfg.Sources[name]; !ok {
		for _, p := range op.Params {
			if p.Container && args[p.Name] != "" {
				writeError(w, http.StatusForbidden, noSource(name))
				return connector.Op{}, false
			}
		}
	}
	return op, h.healthy(w, r, name)
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
		writeJSON(w, http.StatusTooManyRequests, connector.Error{
			Message: fmt.Sprintf("%s is resting: %s", name, detail), RetryAfter: retry,
		})
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

func noSource(name string) string {
	return fmt.Sprintf("connector %s has no data source on this gateway, so it cannot target chats or channels", name)
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
		writeJSON(w, e.Status, e)
		return
	}
	slog.Error("call failed", "peer", who, "err", err)
	writeError(w, http.StatusServiceUnavailable, unavailable(who))
}

// relayConnector is relay for connector refusals, which may list candidates
// for an ambiguous name. Hidden candidates are dropped; if none is left the
// answer is the one for an unknown target, so a hidden match is not revealed.
func (h *handler) relayConnector(w http.ResponseWriter, name string, err error) {
	e, ok := errors.AsType[*connector.Error](err)
	if !ok || len(e.Candidates) == 0 {
		relay(w, "connector "+name, err)
		return
	}
	var visible []connector.Candidate
	if _, ok := h.cfg.Sources[name]; ok {
		for _, c := range e.Candidates {
			hidden, err := h.hidden(name, append([]string{c.ID}, c.Aliases...))
			if err != nil {
				slog.Error("exposure check", "connector", name, "err", err)
				writeError(w, http.StatusServiceUnavailable, "gateway unavailable")
				return
			}
			if !hidden {
				visible = append(visible, c)
			}
		}
	}
	if len(visible) == 0 {
		unknownTarget(w, name)
		return
	}
	labels := make([]string, len(visible))
	for i, c := range visible {
		labels[i] = fmt.Sprintf("%s (%s)", c.Label, c.ID)
	}
	writeJSON(w, e.Status, connector.Error{
		Message:    fmt.Sprintf("%s: %s; pass one of these IDs instead", e.Message, strings.Join(labels, ", ")),
		Candidates: visible,
	})
}

func unknownTarget(w http.ResponseWriter, name string) {
	writeError(w, http.StatusNotFound,
		fmt.Sprintf("%s target is unknown or hidden; look it up in %s_containers and pass its id", name, name))
}

type readRequest struct {
	Args map[string]string `json:"args"`
}

func (h *handler) read(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	name, opName := r.PathValue("connector"), r.PathValue("op")
	var req readRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	op, ok := h.lookup(w, r, name, opName, connector.Read, req.Args)
	if !ok {
		return
	}
	var ids []string
	for _, p := range op.Params {
		if id := req.Args[p.Name]; p.Container && id != "" {
			ids = append(ids, id)
		}
	}
	if !h.visible(r.Context(), w, name, ids) {
		return
	}

	raw, err := h.connectors.reads[name].Read(r.Context(), opName, req.Args)
	attrs := []any{"remote", r.RemoteAddr, "connector", name, "op", opName, "duration_ms", time.Since(start).Milliseconds()}
	if err != nil {
		slog.Info("read", append(attrs, "err", err.Error())...)
		h.relayConnector(w, name, err)
		return
	}
	var res connector.ReadResult
	if err := json.Unmarshal(raw, &res); err != nil {
		slog.Error("decode read result", "connector", name, "err", err)
		writeError(w, http.StatusBadGateway, "connector returned an invalid read result")
		return
	}
	slog.Info("read", append(attrs, "messages", len(res.MessageIDs), "containers", res.Containers)...)
	if len(res.MessageIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]int{"containers": res.Containers, "total_results": res.TotalResults})
		return
	}
	h.answer(w, r, name, res)
}

// visible answers 404 unless every id is in the source's containers view.
// Unknown and hidden look the same to the client.
func (h *handler) visible(ctx context.Context, w http.ResponseWriter, source string, ids []string) bool {
	if len(ids) == 0 {
		return true
	}
	q, err := h.open()
	if err != nil {
		slog.Error("open query connection", "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable")
		return false
	}
	defer q.close()
	view := source + "_containers"
	for _, id := range ids {
		res, err := q.run(ctx, `SELECT 1 FROM `+sqlite3.QuoteIdentifier(view)+` WHERE id = ?1`, []any{id}, 1)
		if err == nil && len(res.Rows) == 1 {
			continue
		}
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("%s container %s is unknown or hidden; check %s (a sync read op may add it)", source, id, view))
		return false
	}
	return true
}

// answer reads the connector's message IDs back through the filtered view, so
// hidden senders and containers drop out and auth messages lose their body,
// as with /v1/query. Rows keep the connector's order.
func (h *handler) answer(w http.ResponseWriter, r *http.Request, source string, rr connector.ReadResult) {
	q, err := h.open()
	if err != nil {
		slog.Error("open query connection", "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable")
		return
	}
	defer q.close()

	params := make([]any, len(rr.MessageIDs))
	order := make(map[string]int, len(rr.MessageIDs))
	for i, id := range rr.MessageIDs {
		params[i] = id
		order[id] = i
	}
	sql := `SELECT * FROM ` + sqlite3.QuoteIdentifier(source+"_messages") +
		` WHERE id IN (?` + strings.Repeat(",?", len(params)-1) + `)`
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	res, err := q.run(ctx, sql, params, len(params))
	if err != nil {
		slog.Error("read back messages", "source", source, "err", err)
		writeError(w, http.StatusInternalServerError, "read back messages")
		return
	}
	idCol := slices.Index(res.Columns, "id")
	if idCol < 0 {
		writeError(w, http.StatusInternalServerError, "view has no id column")
		return
	}
	slices.SortFunc(res.Rows, func(a, b []any) int {
		return order[a[idCol].(string)] - order[b[idCol].(string)]
	})
	writeJSON(w, http.StatusOK, struct {
		*queryResult
		TotalResults int `json:"total_results,omitempty"`
	}{res, rr.TotalResults})
}

func (h *handler) propose(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if h.console == nil {
		writeError(w, http.StatusNotFound, "proposals are not enabled on this gateway")
		return
	}
	var req proposal.Request
	if !decodeStrict(w, r, &req) {
		return
	}
	if _, ok := h.lookup(w, r, req.Connector, req.Op, connector.Write, req.Args); !ok {
		return
	}
	attrs := []any{"remote", r.RemoteAddr, "connector", req.Connector, "op", req.Op}
	prepared, err := h.connectors.calls[req.Connector].Prepare(r.Context(), req.Op, req.Args)
	if err != nil {
		slog.Info("propose", append(attrs, "err", err.Error())...)
		h.relayConnector(w, req.Connector, err)
		return
	}
	if prepared.Container != "" {
		if _, ok := h.cfg.Sources[req.Connector]; !ok {
			slog.Info("propose", append(attrs, "refused", "container without source")...)
			writeError(w, http.StatusForbidden, noSource(req.Connector))
			return
		}
		hidden, err := h.hidden(req.Connector, append([]string{prepared.Container}, prepared.Aliases...))
		if err != nil {
			slog.Error("exposure check", "connector", req.Connector, "err", err)
			writeError(w, http.StatusServiceUnavailable, "gateway unavailable")
			return
		}
		if hidden {
			slog.Info("propose", append(attrs, "refused", "hidden container")...)
			unknownTarget(w, req.Connector)
			return
		}
	}
	p, err := h.console.Submit(r.Context(), proposal.Request{
		Connector: req.Connector, Op: req.Op, Args: prepared.Args, Note: req.Note,
	})
	if err != nil {
		slog.Info("propose", append(attrs, "err", err.Error())...)
		relay(w, "console", err)
		return
	}
	slog.Info("propose", append(attrs, "id", p.ID, "duration_ms", time.Since(start).Milliseconds())...)
	writeJSON(w, http.StatusAccepted, p)
}

// hidden mirrors the containers view's filter (the id or its parent is
// hidden) against the hide lists directly: a write may target a chat with no
// stored row yet, such as a new WhatsApp contact. A DM's container is the
// other person, so hidden senders count as hidden targets too. ids are one
// container's IDs; any of them being hidden hides it.
func (h *handler) hidden(source string, ids []string) (bool, error) {
	pol, err := h.policy.get()
	if err != nil {
		return false, err
	}
	exp := pol.sources[source]
	for _, id := range ids {
		if slices.Contains(exp.HideContainers, id) || slices.Contains(exp.HideSenders, id) {
			return true, nil
		}
	}
	path := h.cfg.Sources[source]
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	conn, err := sqlite3.OpenFlags(path, sqlite3.OPEN_READONLY)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", source, err)
	}
	defer conn.Close()
	if err := conn.BusyTimeout(time.Second); err != nil {
		return false, err
	}
	stmt, _, err := conn.Prepare(`SELECT parent_id FROM containers WHERE id = ?1`)
	if err != nil {
		return false, fmt.Errorf("look up %s container: %w", source, err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if err := stmt.BindText(1, id); err != nil {
			return false, err
		}
		var parent string
		if stmt.Step() {
			parent = stmt.ColumnText(0)
		}
		if err := stmt.Err(); err != nil {
			return false, fmt.Errorf("look up %s container: %w", source, err)
		}
		if err := stmt.Reset(); err != nil {
			return false, err
		}
		if parent != "" && slices.Contains(exp.HideContainers, parent) {
			return true, nil
		}
	}
	return false, nil
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
			writeJSON(w, http.StatusOK, p)
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
