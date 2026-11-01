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
	"maps"
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
	"visible": visible,
	"tone":    tone,
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

	mux.HandleFunc("GET /{$}", s.page(s.inboxPage))
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
		hd.Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// page is what the layout needs besides the page's own data.
type page struct {
	Title   string
	Nav     string
	Pending int
	Who     string
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
	n, err := s.db.pendingCount(r.Context())
	if err != nil {
		s.cfg.Log.Error("count pending", "err", err)
	}
	s.render(w, http.StatusOK, name, page{
		Title: title, Nav: nav, Pending: n, Who: sess.who(), Passkey: sess.key.CredentialID != "",
		Version: sess.version, Data: data,
	})
}

// version changes whenever something a page shows may have, so open pages
// can reload after a decision made on another device.
func (s *Server) version(ctx context.Context) string {
	v, err := s.db.version(ctx, s.now())
	if err != nil {
		s.cfg.Log.Error("console version", "err", err)
	}
	n, head := s.keys.Head()
	return v + "." + strconv.Itoa(n) + "." + head
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

type listItem struct {
	ID, Action string
	Who, What  template.HTML
	Status     proposal.Status
	When       string
}

// item is a request as one row: the first field not tied to a param (e.g.
// the recipient) and the first that is (e.g. the start of the message).
func (s *Server) item(pr row) listItem {
	var who, what string
	for _, f := range pr.Display {
		switch {
		case f.Param == "" && who == "":
			who = f.Value
		case f.Param != "" && what == "":
			what = f.Value
		}
	}
	it := listItem{
		ID: pr.ID, Action: pr.Connector + " " + pr.Op, Status: pr.Status,
		Who: visible(oneLine(who, 40)), What: visible(oneLine(what, 80)),
	}
	if pr.Status == proposal.Pending {
		it.When = left(pr.ExpiresAt.Sub(s.now()))
	} else {
		it.When = pr.DecidedAt.Local().Format(stamp)
	}
	return it
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n]) + "…"
	}
	return s
}

const (
	stamp    = "2006-01-02 15:04"
	pageSize = 25
)

type inboxView struct {
	Waiting, Done []listItem
	// The search form as submitted.
	Q, Connector, Status, From, To string
	Connectors                     []string
	Statuses                       []proposal.Status
	First, Last, Total             int
	Newer, Older                   string
}

var decidedStatuses = []proposal.Status{proposal.Done, proposal.Rejected, proposal.Expired, proposal.Failed, proposal.Unknown}

// inboxPage lists every waiting request, then one page of decided ones
// matching the search form.
func (s *Server) inboxPage(w http.ResponseWriter, r *http.Request, sess session) {
	ctx := r.Context()
	fail := func(err error) {
		s.cfg.Log.Error("load inbox", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load the inbox.")
	}
	if err := s.db.expireDue(ctx, s.now()); err != nil {
		s.cfg.Log.Error("expire proposals", "err", err)
	}
	form := r.URL.Query()
	v := inboxView{
		Q: form.Get("q"), Connector: form.Get("connector"), Status: form.Get("status"),
		From: form.Get("from"), To: form.Get("to"), Statuses: decidedStatuses,
	}
	q := search{Text: strings.TrimSpace(v.Q), Connector: v.Connector, Status: v.Status}
	if t, err := time.ParseInLocation(time.DateOnly, v.From, time.Local); err == nil {
		q.From = t
	}
	if t, err := time.ParseInLocation(time.DateOnly, v.To, time.Local); err == nil {
		q.To = t.AddDate(0, 0, 1)
	}
	n, _ := strconv.Atoi(form.Get("page"))
	n = max(n, 1)

	waiting, err := s.db.pending(ctx)
	if err != nil {
		fail(err)
		return
	}
	done, total, err := s.db.decided(ctx, q, pageSize, (n-1)*pageSize)
	if err != nil {
		fail(err)
		return
	}
	if v.Connectors, err = s.db.decidedConnectors(ctx); err != nil {
		fail(err)
		return
	}
	for _, pr := range waiting {
		v.Waiting = append(v.Waiting, s.item(pr))
	}
	for _, pr := range done {
		v.Done = append(v.Done, s.item(pr))
	}
	v.Total = total
	if len(done) > 0 {
		v.First = (n-1)*pageSize + 1
		v.Last = v.First + len(done) - 1
	}
	link := func(p int) string {
		u := maps.Clone(form)
		u.Set("page", strconv.Itoa(p))
		if p == 1 {
			u.Del("page")
		}
		if len(u) == 0 {
			return "/"
		}
		return "/?" + u.Encode()
	}
	if n > 1 {
		v.Newer = link(n - 1)
	}
	if v.Last > 0 && v.Last < total {
		v.Older = link(n + 1)
	}
	s.owner(w, r, sess, "inbox", "Inbox", "inbox", v)
}

type fieldView struct {
	Label, Param string
	Value        template.HTML
	Raw          string
	Edit         bool
	Hidden       bool
}

// fieldViews renders display fields; params named in editable become
// textareas holding args' value, which the display may render differently.
func fieldViews(display []approval.Field, args map[string]string, editable map[string]bool) []fieldView {
	out := make([]fieldView, 0, len(display))
	for _, f := range display {
		v := fieldView{Label: f.Label, Param: f.Param, Value: visible(f.Value), Raw: f.Value, Hidden: hasHidden(f.Value)}
		if f.Param != "" && editable[f.Param] {
			v.Edit, v.Raw = true, args[f.Param]
			v.Hidden = v.Hidden || hasHidden(v.Raw)
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
	s.owner(w, r, sess, "connections", "Connections", "connections", s.connections(r.Context()))
}

// hidden reports runes that render as nothing or reorder text around them:
// controls, format characters such as zero-width spaces and bidi overrides,
// line and paragraph separators, and other default-ignorable code points.
func hidden(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) ||
		unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Other_Default_Ignorable_Code_Point)
}

func hasHidden(s string) bool { return strings.IndexFunc(s, hidden) >= 0 }

// visible escapes s and shows each hidden rune as a marked code point, so the
// owner sees every character that will be sent.
func visible(s string) template.HTML {
	var b strings.Builder
	start := 0
	for i, r := range s {
		if !hidden(r) {
			continue
		}
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
	case "pending", "executing", "setup":
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
