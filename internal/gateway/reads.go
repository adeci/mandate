package gateway

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/record"
)

const (
	maxLimit = 500
	maxWait  = 900
	maxNote  = 1000
)

// conn answers the configured connector name, or refuses with 404.
func (g *gateway) conn(w http.ResponseWriter, name string) (*conn, bool) {
	c, ok := g.conns[name]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no connector %q; list them with: mandate connectors", name))
	}
	return c, ok
}

// scoped answers the rules of cl's grants on c that pick keeps, or refuses
// with how to ask for one.
func (g *gateway) scoped(w http.ResponseWriter, cl *caller, c *conn, pick func([]record.Grant, time.Time) []rule) ([]rule, bool) {
	now := time.Now()
	grants := cl.live.grantsOn(cl.identity, c.name)
	if rules := pick(grants, now); len(rules) > 0 {
		return rules, true
	}
	cmd := fmt.Sprintf("mandate request %s --reason WHY", c.name)
	msg := fmt.Sprintf("no read access to %s now", c.name)
	if next := accessNow(grants, now).Next; !next.IsZero() {
		msg += "; a grant opens " + next.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusForbidden, struct {
		Error   string `json:"error"`
		Request string `json:"request"`
	}{msg + "; ask the owner with: " + cmd, cmd})
	return nil, false
}

// unknownChat is the one answer for a chat the agent may not name: unknown,
// never-shared or outside its grants.
func unknownChat(w http.ResponseWriter, c *conn, id string) {
	writeError(w, http.StatusNotFound, fmt.Sprintf("no chat %q; find chats with: mandate %s chats --search TEXT", id, c.name))
}

func intParam(q url.Values, key string, def, lo, hi int64) (int64, error) {
	v := q.Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d", key, lo, hi)
	}
	return n, nil
}

// read answers GET {c}/chats, messages, search and media.
func (g *gateway) read(w http.ResponseWriter, r *http.Request, cl *caller) {
	c, ok := g.conn(w, r.PathValue("connector"))
	if !ok {
		return
	}
	verb := r.PathValue("verb")
	if !slices.Contains([]string{"chats", "messages", "search", "media"}, verb) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no read %q: reads are chats, messages, search and media", verb))
		return
	}
	scope, ok := g.scoped(w, cl, c, readScope)
	if !ok {
		return
	}
	q := r.URL.Query()
	defLimit := int64(20)
	if verb == "search" {
		defLimit = 50
	}
	limit, err := intParam(q, "limit", defLimit, 1, maxLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := intParam(q, "before", 0, 0, math.MaxInt64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	chat := q.Get("chat")
	var run func(context.Context, *view) (*table, error)
	switch verb {
	case "chats":
		run = func(ctx context.Context, v *view) (*table, error) { return v.chats(ctx, q.Get("search"), limit) }
	case "messages":
		if chat == "" {
			writeError(w, http.StatusBadRequest, "chat is required")
			return
		}
		run = func(ctx context.Context, v *view) (*table, error) { return v.messages(ctx, chat, before, limit) }
	case "search":
		text, since := q.Get("text"), q.Get("since")
		if text == "" {
			writeError(w, http.StatusBadRequest, "text is required")
			return
		}
		if _, err := time.Parse(time.DateOnly, since); since != "" && err != nil {
			writeError(w, http.StatusBadRequest, "since must be YYYY-MM-DD")
			return
		}
		run = func(ctx context.Context, v *view) (*table, error) {
			return v.search(ctx, text, chat, since, before, limit)
		}
	case "media":
		g.media(w, r, cl, c, scope)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	v, err := g.view(ctx, cl, c, scope)
	if err != nil {
		unavailable(w, cl, "open store", err)
		return
	}
	if chat != "" {
		if ok, err := v.chatVisible(ctx, chat); err != nil || !ok {
			if err != nil {
				unavailable(w, cl, "chat lookup", err)
			} else {
				unknownChat(w, c, chat)
			}
			return
		}
	}
	t, err := run(ctx, v)
	if err != nil {
		unavailable(w, cl, verb, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
	var chats []chatRef
	switch verb {
	case "messages":
		chats = g.chatNames(ctx, cl, v, []string{chat})
	case "search":
		chats = rowChats(t)
	}
	g.record(cl, c, verb, chats, len(t.Rows))
}

// chatNames names chats as v shows them; it only labels activity, so a
// failed lookup is logged and leaves the names empty.
func (g *gateway) chatNames(ctx context.Context, cl *caller, v *view, ids []string) []chatRef {
	names, err := v.names(ctx, ids)
	if err != nil {
		cl.log.Warn("chat names", "err", err)
	}
	out := make([]chatRef, len(ids))
	for i, id := range ids {
		out[i] = chatRef{id, names[id]}
	}
	return out
}

// rowChats lists the chats of a search table's rows.
func rowChats(t *table) []chatRef {
	var out []chatRef
	for _, row := range t.Rows {
		id, _ := row[2].(string)
		name, _ := row[3].(string)
		if !slices.ContainsFunc(out, func(c chatRef) bool { return c.ID == id }) {
			out = append(out, chatRef{id, name})
		}
	}
	return out
}

func (g *gateway) record(cl *caller, c *conn, verb string, chats []chatRef, count int) {
	if chats == nil {
		chats = []chatRef{}
	}
	g.activity.add(entry{At: time.Now().UTC().Truncate(time.Second), Identity: cl.identity, Connector: c.name, Verb: verb, Chats: chats, Count: count})
}

// healthy refuses while the connector cannot serve the agent.
func (g *gateway) healthy(w http.ResponseWriter, r *http.Request, cl *caller, c *conn) bool {
	h, err := c.health(r.Context())
	if err != nil {
		cl.log.Error("connector health", "connector", c.name, "err", err)
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s is not answering; try again in a minute, and tell the owner if it persists", c.name))
		return false
	}
	detail := cmp.Or(h.Detail, string(h.State))
	switch h.State {
	case connector.Healthy:
		return true
	case connector.Resting:
		retry := 60
		if !h.Until.IsZero() {
			retry = max(int(math.Ceil(time.Until(h.Until).Seconds())), 1)
		}
		writeJSON(w, http.StatusTooManyRequests, connector.Error{Message: fmt.Sprintf("%s is resting: %s", c.name, detail), RetryAfter: retry})
	case connector.Connecting:
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s is connecting; try again in a minute", c.name))
	default:
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s needs the owner: %s", c.name, detail))
	}
	return false
}

func (g *gateway) media(w http.ResponseWriter, r *http.Request, cl *caller, c *conn, scope []rule) {
	q := r.URL.Query()
	chat := q.Get("chat")
	seq, err := strconv.ParseInt(q.Get("seq"), 10, 64)
	if chat == "" || err != nil || seq < 1 {
		writeError(w, http.StatusBadRequest, "chat and seq are required: the chat's id and the message's seq")
		return
	}
	index, err := intParam(q, "index", 0, 0, math.MaxInt32)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	v, err := g.view(ctx, cl, c, scope)
	if err != nil {
		unavailable(w, cl, "open store", err)
		return
	}
	f, err := v.file(ctx, chat, seq, index)
	if err != nil {
		unavailable(w, cl, "media lookup", err)
		return
	}
	switch {
	case f == nil || f.hidden:
		writeError(w, http.StatusNotFound, fmt.Sprintf("no message %d in chat %q; see mandate %s messages", seq, chat, c.name))
		return
	case f.count == 0 && !f.marked:
		writeError(w, http.StatusNotFound, fmt.Sprintf("message %d has no files", seq))
		return
	case f.count > 0 && index >= f.count:
		writeError(w, http.StatusNotFound, fmt.Sprintf("message %d has %d files: index 0 to %d", seq, f.count, f.count-1))
		return
	case f.size > maxMedia:
		writeError(w, http.StatusRequestEntityTooLarge, tooLarge(f.size))
		return
	}
	spec, err := c.getSpec(r.Context())
	if err != nil {
		unavailable(w, cl, "connector spec", err)
		return
	}
	if !spec.Media {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s serves no files", c.name))
		return
	}
	if !g.healthy(w, r, cl, c) {
		return
	}
	mctx, mcancel := context.WithTimeout(r.Context(), mediaTimeout)
	defer mcancel()
	resp, err := c.client.Media(mctx, f.message, int(index), maxMedia)
	if err != nil {
		relayConnector(w, cl, c, err)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.ContentLength < 0:
		unavailable(w, cl, "media", errors.New("connector sent a file without a length"))
		return
	case resp.ContentLength > maxMedia:
		writeError(w, http.StatusRequestEntityTooLarge, tooLarge(resp.ContentLength))
		return
	}
	w.Header().Set("Content-Type", cmp.Or(resp.Header.Get("Content-Type"), "application/octet-stream"))
	w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	if d := resp.Header.Get("Content-Disposition"); d != "" {
		w.Header().Set("Content-Disposition", d)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		cl.log.Warn("media", "connector", c.name, "err", err)
		return
	}
	g.record(cl, c, "media", g.chatNames(ctx, cl, v, []string{chat}), 1)
}

func tooLarge(size int64) string {
	return fmt.Sprintf("the file is %.1f MiB, over the %d MiB limit for files", float64(size)/(1<<20), maxMedia>>20)
}

// relayConnector passes a connector's refusal through; anything else means
// it is unreachable.
func relayConnector(w http.ResponseWriter, cl *caller, c *conn, err error) {
	if e, ok := errors.AsType[*connector.Error](err); ok {
		writeJSON(w, e.Status, e)
		return
	}
	cl.log.Error("connector call", "connector", c.name, "err", err)
	writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s is not answering; try again in a minute, and tell the owner if it persists", c.name))
}

type opRequest struct {
	Args map[string]string `json:"args"`
	Note string            `json:"note,omitempty"`
}

// op runs a read op or proposes a write op.
func (g *gateway) op(w http.ResponseWriter, r *http.Request, cl *caller) {
	c, ok := g.conn(w, r.PathValue("connector"))
	if !ok {
		return
	}
	var req opRequest
	if !decode(w, r, &req, maxBody) {
		return
	}
	spec, err := c.getSpec(r.Context())
	if err != nil {
		relayConnector(w, cl, c, err)
		return
	}
	op, ok := spec.Op(r.PathValue("op"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no op %q; list %s's ops with: mandate connectors", r.PathValue("op"), c.name))
		return
	}
	if err := checkArgs(op, req.Args); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if utf8.RuneCountInString(req.Note) > maxNote {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("note is over %d characters", maxNote))
		return
	}
	// What a write may target or answer is what the agent may read, or
	// could while a window open in the last day lasted.
	pick := readScope
	if op.Kind == connector.Write {
		pick = writeScope
	}
	scope, ok := g.scoped(w, cl, c, pick)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	v, err := g.view(ctx, cl, c, scope)
	if err != nil {
		unavailable(w, cl, "open store", err)
		return
	}
	chats, ok := g.checkTargets(ctx, w, cl, c, v, op, req.Args)
	if !ok {
		return
	}
	cancel()
	if op.Kind == connector.Write {
		g.propose(w, r, cl, c, op, req)
		return
	}
	if !g.healthy(w, r, cl, c) {
		return
	}
	g.readOp(w, r, cl, c, op, req.Args, chats)
}

func checkArgs(op connector.Op, args map[string]string) error {
	for _, p := range op.Params {
		if p.Required && args[p.Name] == "" {
			return fmt.Errorf("%s needs %s", op.Name, p.Name)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(args)) {
		if !slices.ContainsFunc(op.Params, func(p connector.Param) bool { return p.Name == k }) {
			return fmt.Errorf("%s takes no %s", op.Name, k)
		}
	}
	return nil
}

// checkTargets refuses unless every container arg is a chat v shows and
// every message arg a message v shows in the op's container chat, before
// the connector looks either up. It answers the container chats.
func (g *gateway) checkTargets(ctx context.Context, w http.ResponseWriter, cl *caller, c *conn, v *view, op connector.Op, args map[string]string) ([]string, bool) {
	var chats []string
	var seqs []int64
	for _, p := range op.Params {
		a := args[p.Name]
		switch {
		case a == "":
		case p.Container:
			chats = append(chats, a)
		case p.Message:
			seq, err := strconv.ParseInt(a, 10, 64)
			if err != nil || seq < 1 {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a message's seq", p.Name))
				return nil, false
			}
			seqs = append(seqs, seq)
		}
	}
	for _, id := range chats {
		ok, err := v.chatVisible(ctx, id)
		if err != nil {
			unavailable(w, cl, "chat lookup", err)
			return nil, false
		}
		if !ok {
			unknownChat(w, c, id)
			return nil, false
		}
	}
	for _, seq := range seqs {
		ok := false
		if len(chats) > 0 {
			var err error
			if ok, err = v.seqVisible(ctx, chats[0], seq); err != nil {
				unavailable(w, cl, "message lookup", err)
				return nil, false
			}
		}
		if !ok {
			noMessage(w, c, op, args)
			return nil, false
		}
	}
	return chats, true
}

// noMessage is the one answer for a message the agent may not name:
// unknown, hidden, deleted or past the horizon. It repeats only what the
// agent passed.
func noMessage(w http.ResponseWriter, c *conn, op connector.Op, args map[string]string) {
	var passed []string
	for _, p := range op.Params {
		if a := args[p.Name]; a != "" && (p.Container || p.Message) {
			passed = append(passed, p.Name+"="+a)
		}
	}
	writeError(w, http.StatusNotFound, fmt.Sprintf("no message at %s; see mandate %s messages", strings.Join(passed, " "), c.name))
}

// relayOp relays a connector's refusal of op. For an op that names
// messages, a refusal that depends on what is stored (no such seq, deleted)
// answers as noMessage, so it tells nothing the agent may not read.
func relayOp(w http.ResponseWriter, cl *caller, c *conn, op connector.Op, args map[string]string, err error) {
	if e, ok := errors.AsType[*connector.Error](err); ok && (e.Status == http.StatusBadRequest || e.Status == http.StatusGone) &&
		slices.ContainsFunc(op.Params, func(p connector.Param) bool { return p.Message }) {
		noMessage(w, c, op, args)
		return
	}
	relayConnector(w, cl, c, err)
}

// readOp runs a connector's read op and answers what it stored, as far as
// the grants open once it is done show it.
func (g *gateway) readOp(w http.ResponseWriter, r *http.Request, cl *caller, c *conn, op connector.Op, args map[string]string, chats []string) {
	rctx, rcancel := context.WithTimeout(r.Context(), readTimeout)
	res, err := c.client.Read(rctx, op.Name, args)
	rcancel()
	if err != nil {
		relayOp(w, cl, c, op, args, err)
		return
	}
	cl, scope, ok := g.rescope(w, cl, c, readScope)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	v, err := g.view(ctx, cl, c, scope)
	if err != nil {
		unavailable(w, cl, "open store", err)
		return
	}
	t, err := v.byID(ctx, res.MessageIDs)
	if err != nil {
		unavailable(w, cl, "read back", err)
		return
	}
	if t.Containers, err = v.visibleCount(ctx, res.ContainerIDs); err != nil {
		unavailable(w, cl, "count chats", err)
		return
	}
	// The connector's more past rows filtered out would count what the
	// agent may not see, page by page.
	t.More = res.More && len(t.Rows) == len(res.MessageIDs) && t.Containers == len(res.ContainerIDs)
	writeJSON(w, http.StatusOK, t)
	named := rowChats(t)
	if len(named) == 0 && len(chats) > 0 {
		named = g.chatNames(ctx, cl, v, chats[:1])
	}
	g.record(cl, c, op.Name, named, len(t.Rows))
}

// rescope reloads the live set after a connector call, so that a grant
// closed or a chat never-shared meanwhile counts.
func (g *gateway) rescope(w http.ResponseWriter, cl *caller, c *conn, pick func([]record.Grant, time.Time) []rule) (*caller, []rule, bool) {
	cl = &caller{identity: cl.identity, live: g.live.Load(), log: cl.log}
	scope, ok := g.scoped(w, cl, c, pick)
	return cl, scope, ok
}

// propose prepares a write and hands it to the owner side. The chat and
// message the connector resolved must be ones the grants show, like the args.
func (g *gateway) propose(w http.ResponseWriter, r *http.Request, cl *caller, c *conn, op connector.Op, req opRequest) {
	pctx, pcancel := context.WithTimeout(r.Context(), callTimeout)
	prepared, err := c.client.Prepare(pctx, op.Name, req.Args)
	pcancel()
	if err != nil {
		relayOp(w, cl, c, op, req.Args, err)
		return
	}
	cl, scope, ok := g.rescope(w, cl, c, writeScope)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	v, err := g.view(ctx, cl, c, scope)
	if err != nil {
		unavailable(w, cl, "open store", err)
		return
	}
	// A chat readable through its mailbox's grant names the mailbox, which
	// the owner side finds in the grant.
	var mailbox string
	if prepared.Container != "" {
		rows, err := v.query(ctx, `SELECT mailbox FROM chats WHERE id = ?4`, prepared.Container)
		if err != nil {
			unavailable(w, cl, "chat lookup", err)
			return
		}
		if len(rows) == 0 {
			cl.log.Info("proposal refused", "connector", c.name, "why", "prepared chat")
			noMessage(w, c, op, req.Args)
			return
		}
		mailbox, _ = rows[0][0].(string)
	}
	if prepared.Message != "" {
		ok, err := v.messageVisible(ctx, prepared.Message)
		if err != nil {
			unavailable(w, cl, "message lookup", err)
			return
		}
		if !ok {
			cl.log.Info("proposal refused", "connector", c.name, "why", "prepared message")
			noMessage(w, c, op, req.Args)
			return
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	err = g.toOwner.do(r.Context(), http.MethodPost, "/internal/proposals", struct {
		Identity  string             `json:"identity"`
		Connector string             `json:"connector"`
		Op        string             `json:"op"`
		Prepared  connector.Prepared `json:"prepared"`
		Mailbox   string             `json:"mailbox,omitempty"`
		Note      string             `json:"note,omitempty"`
	}{cl.identity, c.name, op.Name, prepared, mailbox, req.Note}, &created)
	if err != nil {
		relay(w, cl, "submit proposal", err)
		return
	}
	cl.log.Info("proposed", "connector", c.name, "op", op.Name, "id", created.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": created.ID, "status": "pending"})
}

var recordID = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// waitParam reads wait=SECONDS, capped at maxWait.
func waitParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := intParam(r.URL.Query(), "wait", 0, 0, math.MaxInt32)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return 0, false
	}
	return int(min(n, maxWait)), true
}

func (g *gateway) proposalStatus(w http.ResponseWriter, r *http.Request, cl *caller) {
	id := r.PathValue("id")
	wait, ok := waitParam(w, r)
	if !ok {
		return
	}
	if !recordID.MatchString(id) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no proposal %q of yours", id))
		return
	}
	// Only these fields: what the owner was shown, and the note, stay there.
	var st struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Result *struct {
			Ref    string `json:"ref,omitempty"`
			Detail string `json:"detail,omitempty"`
			Output string `json:"output,omitempty"`
		} `json:"result,omitempty"`
	}
	ctx, cancel := g.longPoll(r.Context())
	defer cancel()
	if err := g.toOwner.do(ctx, http.MethodGet, "/internal/proposals/"+id+query(cl.identity, wait), nil, &st); err != nil {
		relay(w, cl, "proposal status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
