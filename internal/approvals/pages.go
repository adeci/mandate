package approvals

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/adeci/mandate/internal/proposal"
)

//go:embed pages.html
var pagesHTML string

//go:embed static
var staticFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"when": func(t time.Time) string { return t.Local().Format("Jan 2 15:04") },
}).Parse(pagesHTML))

// csp allows only the embedded script and stylesheet: no inline code, no
// framing, forms only to this origin.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// PageHandler serves the owner-facing pages over HTTPS.
func (s *Server) PageHandler() http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.listPage)
	mux.HandleFunc("GET /p/{id}", s.proposalPage)
	mux.HandleFunc("POST /p/{id}/challenge", s.challenge)
	mux.HandleFunc("POST /p/{id}/approve", s.approve)
	mux.HandleFunc("POST /p/{id}/reject", s.reject)
	mux.HandleFunc("GET /enroll/{token}", s.enrollPage)
	mux.HandleFunc("GET /enroll/{token}/options", s.enrollOptions)
	mux.HandleFunc("POST /enroll/{token}", s.finishEnroll)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

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

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.cfg.Log.Error("render page", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) errorPage(w http.ResponseWriter, status int, msg string) {
	s.render(w, status, "error", msg)
}

type listItem struct {
	ID, Title, Summary string
	ExpiresAt          time.Time
}

func (s *Server) listPage(w http.ResponseWriter, r *http.Request) {
	if err := s.db.expireDue(r.Context(), s.now()); err != nil {
		s.cfg.Log.Error("expire proposals", "err", err)
	}
	rows, err := s.db.pending(r.Context())
	if err != nil {
		s.cfg.Log.Error("list proposals", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load proposals.")
		return
	}
	items := make([]listItem, 0, len(rows))
	for _, pr := range rows {
		items = append(items, listItem{
			ID: pr.ID, Title: pr.Connector + " " + pr.Op, Summary: summary(pr.Display), ExpiresAt: pr.ExpiresAt,
		})
	}
	s.render(w, http.StatusOK, "list", items)
}

type fieldView struct {
	Label, Value, Param string
	Edit                bool
}

type proposalView struct {
	proposal.Proposal
	Title      string
	Fields     []fieldView
	Pending    bool
	CanApprove bool
}

func (s *Server) proposalPage(w http.ResponseWriter, r *http.Request) {
	if err := s.db.expireDue(r.Context(), s.now()); err != nil {
		s.cfg.Log.Error("expire proposals", "err", err)
	}
	pr, err := s.db.get(r.Context(), r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		s.errorPage(w, http.StatusNotFound, "No such proposal.")
		return
	}
	if err != nil {
		s.cfg.Log.Error("load proposal", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load the proposal.")
		return
	}
	v := proposalView{
		Proposal:   pr.Proposal,
		Title:      pr.Connector + " " + pr.Op,
		Pending:    pr.Status == proposal.Pending,
		CanApprove: pr.Status == proposal.Pending && len(s.cfg.Approvers) > 0,
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
	for _, f := range pr.Display {
		fv := fieldView{Label: f.Label, Value: f.Value}
		if f.Param != "" && editable[f.Param] {
			// Edits apply to the arg, which the display may render differently.
			fv.Edit, fv.Param, fv.Value = true, f.Param, pr.Args[f.Param]
		}
		v.Fields = append(v.Fields, fv)
	}
	s.render(w, http.StatusOK, "proposal", v)
}

func (s *Server) enrollPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	e, ok := s.liveEnrollment(token, false)
	if !ok {
		s.errorPage(w, http.StatusNotFound, errEnrollGone.Message)
		return
	}
	s.render(w, http.StatusOK, "enroll", map[string]string{"Token": token, "Name": e.name})
}
