package console

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"net/http"
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
	"when":    func(t time.Time) string { return t.Local().Format("Jan 2 15:04") },
	"visible": visible,
	"tone":    tone,
}).Parse(pagesHTML))

// csp allows only the embedded script and stylesheet: no inline code, no
// framing, forms only to this origin.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Handler serves the console over HTTPS. Everything but login, enrollment
// links, and static files needs a session.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login/options", s.loginOptions)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /enroll/{token}", s.enrollPage)
	mux.HandleFunc("GET /enroll/{token}/options", s.enrollOptions)
	mux.HandleFunc("POST /enroll/{token}", s.finishEnroll)
	mux.Handle("GET /static/", http.StripPrefix("/static/", files))
	mux.Handle("GET /manifest.webmanifest", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		files.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /{$}", s.page(s.pendingPage))
	mux.HandleFunc("GET /history", s.page(s.historyPage))
	mux.HandleFunc("GET /r/{id}", s.page(s.requestPage))
	mux.HandleFunc("POST /r/{id}/challenge", s.api(s.challenge))
	mux.HandleFunc("POST /r/{id}/approve", s.api(s.approve))
	mux.HandleFunc("POST /r/{id}/deny", s.api(s.deny))
	mux.HandleFunc("GET /connections", s.page(s.connectionsPage))
	mux.HandleFunc("GET /connections/{name}/health", s.api(s.connectionHealth))
	mux.HandleFunc("POST /connections/{name}/setup", s.api(s.setup))
	mux.HandleFunc("GET /passkeys", s.page(s.passkeysPage))
	mux.HandleFunc("POST /passkeys", s.api(s.sessionEnroll))
	mux.HandleFunc("GET /session", s.api(func(w http.ResponseWriter, _ *http.Request, _ session) {
		w.WriteHeader(http.StatusNoContent)
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
	s.render(w, http.StatusOK, name, page{Title: title, Nav: nav, Pending: n, Who: sess.approver.Name, Data: data})
}

func (s *Server) errorPage(w http.ResponseWriter, status int, msg string) {
	s.render(w, status, "error", page{Title: "Error", Data: msg})
}

type loginView struct {
	Bootstrap bool
	Next      string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	next := localPath(r.URL.Query().Get("next"))
	if _, ok := s.session(r); ok {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", page{Title: "Log in", Data: loginView{Bootstrap: len(s.cfg.Approvers) == 0, Next: next}})
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
		it.When = pr.DecidedAt.Local().Format("Jan 2 15:04")
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

func (s *Server) pendingPage(w http.ResponseWriter, r *http.Request, sess session) {
	if err := s.db.expireDue(r.Context(), s.now()); err != nil {
		s.cfg.Log.Error("expire proposals", "err", err)
	}
	rows, err := s.db.pending(r.Context())
	if err != nil {
		s.cfg.Log.Error("list proposals", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load requests.")
		return
	}
	items := make([]listItem, 0, len(rows))
	for _, pr := range rows {
		items = append(items, s.item(pr))
	}
	s.owner(w, r, sess, "pending", "Pending", "pending", items)
}

func (s *Server) historyPage(w http.ResponseWriter, r *http.Request, sess session) {
	if err := s.db.expireDue(r.Context(), s.now()); err != nil {
		s.cfg.Log.Error("expire proposals", "err", err)
	}
	rows, err := s.db.decided(r.Context(), historySize)
	if err != nil {
		s.cfg.Log.Error("list history", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load history.")
		return
	}
	items := make([]listItem, 0, len(rows))
	for _, pr := range rows {
		items = append(items, s.item(pr))
	}
	s.owner(w, r, sess, "history", "History", "history", items)
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
		CanApprove: pr.Status == proposal.Pending && len(s.cfg.Approvers) > 0,
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
	s.owner(w, r, sess, "request", v.Title, "pending", v)
}

func (s *Server) connectionsPage(w http.ResponseWriter, r *http.Request, sess session) {
	s.owner(w, r, sess, "connections", "Connections", "connections", s.connections(r.Context()))
}

type passkeyView struct {
	Name, CredentialID string
	Kind               string
	EnrolledAt         time.Time
	Pinned, Current    bool
	Nix                string
}

func (s *Server) passkeysPage(w http.ResponseWriter, r *http.Request, sess session) {
	known, err := s.db.passkeys(r.Context())
	if err != nil {
		s.cfg.Log.Error("list passkeys", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load passkeys.")
		return
	}
	byID := map[string]passkey{}
	for _, p := range known {
		byID[p.CredentialID] = p
	}
	var out []passkeyView
	pinned := map[string]bool{}
	for _, a := range s.cfg.Approvers {
		pinned[a.CredentialID] = true
		v := passkeyView{Name: a.Name, CredentialID: a.CredentialID, Pinned: true, Current: a.CredentialID == sess.approver.CredentialID}
		if p, ok := byID[a.CredentialID]; ok {
			v.Kind, v.EnrolledAt = kind(p.Synced), p.EnrolledAt
		}
		out = append(out, v)
	}
	for _, p := range known {
		if !pinned[p.CredentialID] {
			out = append(out, passkeyView{
				Name: p.Name, CredentialID: p.CredentialID, Kind: kind(p.Synced),
				EnrolledAt: p.EnrolledAt, Nix: nixEntry(p.Approver),
			})
		}
	}
	s.owner(w, r, sess, "passkeys", "Passkeys", "passkeys", out)
}

func (s *Server) enrollPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	e, ok := s.liveEnrollment(token, false)
	if !ok {
		s.errorPage(w, http.StatusNotFound, errEnrollGone.Message)
		return
	}
	s.render(w, http.StatusOK, "enroll", page{Title: "Add a passkey", Data: map[string]string{"Token": token, "Name": e.name}})
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
	case "pending", "executing":
		return "wait"
	case "rejected", "expired", "resting":
		return "muted"
	case "failed", "unknown", "down", "setup":
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
