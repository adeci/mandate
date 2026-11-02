package console

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

//go:embed pages.html
var pagesHTML string

//go:embed static
var staticFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"when":    func(t time.Time) string { return t.Local().Format(stamp) },
	"clock":   func(t time.Time) string { return t.Local().Format("15:04") },
	"iso":     func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"shortID": shortID,
	"visible": visible,
	"tone":    tone,
	"word":    statusWord,
}).Parse(pagesHTML))

// csp allows only the embedded script and stylesheet: no inline code, no
// framing, forms only to this origin.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Handler serves the console over HTTPS. Everything but login and static
// files needs a session.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login/options", s.loginOptions)
	mux.HandleFunc("POST /login", s.login)
	mux.Handle("GET /static/", http.StripPrefix("/static/", files))
	mux.Handle("GET /manifest.webmanifest", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		files.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /{$}", s.page(s.nowPage))
	mux.HandleFunc("GET /now.json", s.api(s.nowJSON))
	mux.HandleFunc("GET /agents", s.page(s.agentsPage))
	mux.HandleFunc("GET /agents.json", s.api(s.agentsJSON))
	mux.HandleFunc("GET /r/{id}", s.page(s.requestPage))
	mux.HandleFunc("POST /r/{id}/challenge", s.api(s.challenge))
	mux.HandleFunc("POST /r/{id}/approve", s.api(s.approve))
	mux.HandleFunc("POST /r/{id}/deny", s.api(s.deny))
	mux.HandleFunc("POST /r/{id}/delete", s.api(s.remove))
	mux.HandleFunc("GET /connections", s.page(s.connectionsPage))
	mux.HandleFunc("GET /connections/{name}/health", s.api(s.connectionHealth))
	mux.HandleFunc("POST /connections/{name}/setup", s.api(s.setup))
	mux.HandleFunc("POST /connections/{name}/unlink", s.api(s.unlink))
	mux.HandleFunc("GET /keys", s.page(s.keysPage))
	mux.HandleFunc("GET /access", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /access/chats", s.api(s.searchChats))
	mux.HandleFunc("GET /access/chats/tree", s.api(s.chatTreeHandler))
	mux.HandleFunc("POST /access/challenge", s.api(s.accessChallenge))
	mux.HandleFunc("POST /access/approve", s.api(s.accessApprove))
	mux.HandleFunc("POST /access/agents/{id}/delete", s.api(s.deleteAgent))
	mux.HandleFunc("POST /access/grants/{id}/close", s.api(s.closeGrant))
	mux.HandleFunc("POST /access/requests/{id}/deny", s.api(s.denyAccess))
	mux.HandleFunc("POST /access/stop-all", s.api(s.stopAll))
	mux.HandleFunc("POST /keys/options", s.api(s.registerOptions))
	mux.HandleFunc("POST /keys", s.api(s.addKey))
	mux.HandleFunc("POST /keys/remove", s.api(s.removeKey))
	mux.HandleFunc("POST /keys/changes/{id}/challenge", s.api(s.keyChallenge))
	mux.HandleFunc("POST /keys/changes/{id}/approve", s.api(s.approveKeyChange))
	mux.HandleFunc("POST /keys/changes/{id}/discard", s.api(s.discardKeyChange))
	mux.HandleFunc("GET /session", s.api(func(w http.ResponseWriter, _ *http.Request, _ session) {
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /version", s.api(func(w http.ResponseWriter, r *http.Request, _ session) {
		connector.WriteJSON(w, http.StatusOK, map[string]string{"version": s.version(r.Context())})
	}))
	mux.HandleFunc("POST /logout", s.api(s.logout))

	cop := http.NewCrossOriginProtection()
	// Behind a proxy the Host header may not match the origin browsers see.
	if err := cop.AddTrustedOrigin(s.cfg.Origin); err != nil {
		s.cfg.Log.Warn("trust origin for cross-origin checks", "origin", s.cfg.Origin, "err", err)
	}
	return securityHeaders(cop.Handler(mux))
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", csp)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Strict-Transport-Security", "max-age=63072000")
		hd.Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// page is what the layout needs besides the page's own data.
type page struct {
	Title string
	Nav   string
	// Waiting counts what waits for the owner, Agents the agents and
	// KeyChanges the key changes waiting for a trusted key.
	Waiting    int
	Agents     int
	KeyChanges int
	// Attention counts connectors that need a look, in the worst one's tone.
	Attention     int
	AttentionTone string
	Who           string
	// Passkey sessions may open an unlock window.
	Passkey bool
	Version string
	Data    any
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		s.cfg.Log.Error("render page", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) renderString(name string, data any) (string, error) {
	var buf bytes.Buffer
	err := s.tmpl.ExecuteTemplate(&buf, name, data)
	return buf.String(), err
}

func (s *Server) owner(w http.ResponseWriter, r *http.Request, sess session, name, title, nav string, data any) {
	ctx := r.Context()
	now := s.now()
	p := page{Title: title, Nav: nav, Who: sess.who(), Passkey: sess.key.CredentialID != "", Version: sess.version, Data: data}
	proposals, err := s.db.pendingCount(ctx)
	if err != nil {
		s.cfg.Log.Error("count pending", "err", err)
	}
	requests, err := s.db.pendingAccessCount(ctx, now)
	if err != nil {
		s.cfg.Log.Error("count access requests", "err", err)
	}
	p.Waiting = proposals + requests
	if agents, err := s.db.agents(ctx); err == nil {
		p.Agents = len(agents)
	} else {
		s.cfg.Log.Error("count agents", "err", err)
	}
	if changes, err := s.db.keyChanges(ctx, now); err == nil {
		p.KeyChanges = len(changes)
	} else {
		s.cfg.Log.Error("count key changes", "err", err)
	}
	_, p.Attention, p.AttentionTone = s.health()
	s.render(w, http.StatusOK, name, p)
}

// version changes whenever something a page shows may have, so open pages
// can reload after a decision made on another device or a connector's
// health changes.
func (s *Server) version(ctx context.Context) string {
	v, err := s.db.version(ctx, s.now())
	if err != nil {
		s.cfg.Log.Error("console version", "err", err)
	}
	n, head := s.keys.Head()
	states, _, _ := s.health()
	return v + "." + strconv.Itoa(n) + "." + head + states
}

func (s *Server) errorPage(w http.ResponseWriter, status int, msg string) {
	s.render(w, status, "error", page{Title: "Error", Data: msg})
}

type loginView struct {
	Passkeys bool
	Next     string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	next := localPath(r.URL.Query().Get("next"))
	if _, ok := s.session(r); ok {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", page{Title: "Sign in", Data: loginView{Passkeys: len(s.keys.Trusted()) > 0, Next: next}})
}

const stamp = "2006-01-02 15:04"

type fieldView struct {
	Label, Param string
	Value        template.HTML
	Raw          string
	Edit         bool
	// Hidden counts the characters Value marks, or the edit box hides.
	Hidden int
}

// fieldViews renders display fields; params named in editable become
// textareas holding args' value, which the display may render differently.
func fieldViews(display []approval.Field, args map[string]string, editable map[string]bool) []fieldView {
	out := make([]fieldView, 0, len(display))
	for _, f := range display {
		v := fieldView{Label: f.Label, Param: f.Param, Value: visible(f.Value), Raw: f.Value, Hidden: countHidden(f.Value)}
		if f.Param != "" && editable[f.Param] {
			v.Edit, v.Raw = true, args[f.Param]
			v.Hidden = max(v.Hidden, countHidden(v.Raw))
		}
		out = append(out, v)
	}
	return out
}

type requestView struct {
	proposal.Proposal
	Title      string
	Fields     []fieldView
	Pending    bool
	CanApprove bool
	Settled    bool
	DecidedAt  time.Time
	DecidedBy  string
	AgentName  string
	// AgentDeleted means the agent was deleted after proposing this.
	AgentDeleted bool
}

func (s *Server) requestPage(w http.ResponseWriter, r *http.Request, sess session) {
	pr, err := s.load(r.Context(), r.PathValue("id"))
	var ce *connector.Error
	if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
		s.errorPage(w, http.StatusNotFound, "No such request.")
		return
	}
	if err != nil {
		s.cfg.Log.Error("load proposal", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load the request.")
		return
	}
	v := requestView{
		Proposal:   pr.Proposal,
		Title:      pr.Connector + " " + pr.Op,
		Pending:    pr.Status == proposal.Pending,
		CanApprove: pr.Status == proposal.Pending && len(s.keys.Trusted()) > 0,
		Settled:    pr.Status != proposal.Pending && pr.Status != proposal.Executing,
		DecidedAt:  pr.DecidedAt,
		DecidedBy:  pr.DecidedBy,
		AgentName:  pr.AgentName,
	}
	if v.Pending && s.agentGone(r.Context(), pr) != nil {
		v.AgentDeleted, v.CanApprove = true, false
	}
	editable := map[string]bool{}
	if c, ok := s.conns[pr.Connector]; ok && v.Pending {
		// Without the spec nothing is editable; approving as proposed still works.
		if op, err := c.op(r.Context(), pr.Op, s.now()); err == nil {
			for _, p := range op.Params {
				editable[p.Name] = p.Editable
			}
		} else {
			s.cfg.Log.Warn("load op spec", "connector", pr.Connector, "err", err)
		}
	}
	v.Fields = fieldViews(pr.Display, pr.Args, editable)
	s.owner(w, r, sess, "request", v.Title, "inbox", v)
}

func (s *Server) connectionsPage(w http.ResponseWriter, r *http.Request, sess session) {
	conns := s.connections(r.Context())
	// Checking them may have changed what the page began with.
	sess.version = s.version(r.Context())
	s.owner(w, r, sess, "connections", "Connections", "connections", conns)
}

// maxMarks is how many combining marks in a row pass unmarked: enough for
// accented letters; longer runs stack over text or carry a payload.
const maxMarks = 2

// hiddenRunes yields the runes that render as nothing, look like a plain
// space, or reorder text around them, with their byte offsets: controls,
// format characters such as zero-width spaces and bidi overrides, every
// separator but the space, the braille blank, variation selectors other
// than an emoji's own, other default-ignorable code points, and runs of more
// than maxMarks combining marks.
func hiddenRunes(s string) iter.Seq2[int, rune] {
	return func(yield func(int, rune) bool) {
		var prev rune
		// run counts down the rest of an overlong run of marks.
		run := 0
		for i, r := range s {
			if run == 0 && unicode.Is(unicode.M, r) {
				if n := leadingMarks(s[i:]); n > maxMarks {
					run = n
				}
			}
			mark := run > 0 || hidden(r, prev)
			run = max(run-1, 0)
			if mark && !yield(i, r) {
				return
			}
			prev = r
		}
	}
}

func hidden(r, prev rune) bool {
	switch {
	case r == '\n' || r == '\t' || r == ' ':
		return false
	case unicode.Is(unicode.Variation_Selector, r):
		// VS1-VS16 pick an emoji's text or emoji style; one may follow it.
		return r < 0xFE00 || r > 0xFE0F || !emoji(prev)
	}
	return r == '\u2800' || unicode.IsControl(r) ||
		unicode.In(r, unicode.Cf, unicode.Z, unicode.Other_Default_Ignorable_Code_Point)
}

// emoji approximates the characters a variation selector may style:
// symbols, and the digits, # and * of keycaps.
func emoji(r rune) bool {
	return unicode.Is(unicode.So, r) || '0' <= r && r <= '9' || r == '#' || r == '*'
}

func leadingMarks(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.Is(unicode.M, r) {
			break
		}
		n++
	}
	return n
}

func countHidden(s string) int {
	n := 0
	for range hiddenRunes(s) {
		n++
	}
	return n
}

// visible escapes s and shows each hidden rune as a marked code point, so the
// owner sees every character that will be sent.
func visible(s string) template.HTML {
	var b strings.Builder
	start := 0
	for i, r := range hiddenRunes(s) {
		b.WriteString(html.EscapeString(s[start:i]))
		fmt.Fprintf(&b, `<span class="cp">U+%04X</span>`, r)
		start = i + utf8.RuneLen(r)
	}
	b.WriteString(html.EscapeString(s[start:]))
	return template.HTML(b.String())
}

// tone is the status label's emphasis in the style's terms.
func tone(status any) string {
	switch fmt.Sprint(status) {
	case "done", "ok":
		return "ok"
	case "pending", "executing", "setup", "connecting":
		return "wait"
	case "rejected", "expired", "resting":
		return "muted"
	case "failed", "unknown", "down":
		return "alert"
	}
	return ""
}

func left(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d <= 0:
		return "expired"
	case d < time.Hour:
		return fmt.Sprintf("%dm left", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm left", int(d.Hours()), int(d.Minutes())%60)
	}
}
