package approvals

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/connector"
	"github.com/adeci/mandate/internal/proposal"
)

const (
	maxPending  = 20
	proposalTTL = 24 * time.Hour
	// specTTL bounds how long a connector restart with new ops goes unnoticed.
	specTTL        = 5 * time.Minute
	executeTimeout = 2 * time.Minute
	maxBody        = 1 << 20
)

// Config is everything the approvals unit needs; Command builds it from flags.
type Config struct {
	// RPID is the WebAuthn relying party ID, the page's hostname.
	RPID string
	// Origin is the page's origin as browsers report it, e.g.
	// https://box.tailnet.ts.net:8781.
	Origin string
	// Approvers may be empty: enrollment still works, approving doesn't.
	Approvers []approval.Approver
	// Connectors maps connector names to their sockets.
	Connectors map[string]string
	StateDir   string
	// Ntfy is optional.
	Ntfy *Ntfy
	Log  *slog.Logger
	Now  func() time.Time
}

// Server is the approvals unit: proposal store, approval pages, enrollment.
type Server struct {
	cfg      Config
	db       *db
	verifier *approval.Verifier
	conns    map[string]*conn
	tmpl     *template.Template

	enrollMu sync.Mutex
	enrolls  map[string]*enrollment
}

// conn is a connector and its cached spec.
type conn struct {
	name   string
	client *connector.Client

	mu      sync.Mutex
	spec    connector.Spec
	fetched time.Time
}

// New opens the proposal database in cfg.StateDir and settles proposals left
// executing by a previous run.
func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	d, err := openDB(ctx, filepath.Join(cfg.StateDir, "approvals.db"), cfg.Log)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Server{
		cfg: cfg,
		db:  d,
		verifier: &approval.Verifier{
			RPID: cfg.RPID, Origin: cfg.Origin, Approvers: cfg.Approvers, Now: cfg.Now,
		},
		conns:   map[string]*conn{},
		tmpl:    templates,
		enrolls: map[string]*enrollment{},
	}
	for name, sock := range cfg.Connectors {
		s.conns[name] = &conn{name: name, client: connector.Dial(sock, executeTimeout)}
	}
	if err := d.verifyAudit(ctx); err != nil {
		cfg.Log.Error("audit log failed verification", "err", err)
	}
	if err := d.recoverInterrupted(ctx, s.now()); err != nil {
		d.Close()
		return nil, fmt.Errorf("recover interrupted proposals: %w", err)
	}
	return s, nil
}

func (s *Server) Close() error { return s.db.Close() }

// now is truncated to what the database stores, so a record built from a
// stored proposal matches one built from the original.
func (s *Server) now() time.Time { return s.cfg.Now().UTC().Truncate(time.Millisecond) }

// Sweep marks expired proposals every interval until ctx is done.
func (s *Server) Sweep(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.db.expireDue(ctx, s.now()); err != nil && ctx.Err() == nil {
				s.cfg.Log.Error("expire proposals", "err", err)
			}
		}
	}
}

func (c *conn) op(ctx context.Context, name string, now time.Time) (connector.Op, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetched.IsZero() || now.Sub(c.fetched) > specTTL {
		spec, err := c.client.Spec(ctx)
		if err != nil {
			return connector.Op{}, fmt.Errorf("fetch %s spec: %w", c.name, err)
		}
		// Connectors check the record's connector against their own name.
		if spec.Name != c.name {
			return connector.Op{}, fmt.Errorf("connector configured as %q calls itself %q", c.name, spec.Name)
		}
		c.spec, c.fetched = spec, now
	}
	op, ok := c.spec.Op(name)
	if !ok || op.Kind != connector.Write {
		return connector.Op{}, &connector.Error{Status: http.StatusNotFound, Message: fmt.Sprintf("%s has no write op %q", c.name, name)}
	}
	return op, nil
}

// ProposalHandler serves the gateway-facing proposal API.
func (s *Server) ProposalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /proposals", s.submit)
	mux.HandleFunc("GET /proposals/{id}", s.getProposal)
	return mux
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var req proposal.Request
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	c, ok := s.conns[req.Connector]
	if !ok {
		writeErr(w, &connector.Error{Status: http.StatusNotFound, Message: fmt.Sprintf("no connector %q", req.Connector)})
		return
	}
	now := s.now()
	if _, err := c.op(ctx, req.Op, now); err != nil {
		writeErr(w, upstream(err))
		return
	}
	// Args and display come from the connector, never from the gateway.
	p, err := c.client.Prepare(ctx, req.Op, req.Args)
	if err != nil {
		writeErr(w, upstream(err))
		return
	}
	if err := s.db.expireDue(ctx, now); err != nil {
		writeErr(w, err)
		return
	}
	id := rand.Text()
	pr := row{
		Proposal: proposal.Proposal{
			ID: id, Connector: req.Connector, Op: req.Op, Display: p.Display, Note: req.Note,
			Status: proposal.Pending, CreatedAt: now, ExpiresAt: now.Add(proposalTTL),
		},
		Args: p.Args,
	}
	if err := s.db.insert(ctx, pr, maxPending); err != nil {
		if errors.Is(err, errTooMany) {
			err = &connector.Error{Status: http.StatusTooManyRequests, Message: fmt.Sprintf("%d proposals are already waiting for the owner", maxPending)}
		}
		writeErr(w, err)
		return
	}
	s.cfg.Log.Info("proposed", "id", id, "connector", req.Connector, "op", req.Op)
	msg := "waiting for approval"
	if sum := summary(p.Display); sum != "" {
		msg = sum + ": " + msg
	}
	s.notify(ctx, req.Connector+" "+req.Op, msg, s.cfg.Origin+"/p/"+id)
	writeJSON(w, http.StatusCreated, pr.Proposal)
}

func (s *Server) getProposal(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.load(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, pr.Proposal)
}

// load expires due proposals and returns the one named in the path.
func (s *Server) load(w http.ResponseWriter, r *http.Request) (row, bool) {
	if err := s.db.expireDue(r.Context(), s.now()); err != nil {
		writeErr(w, err)
		return row{}, false
	}
	pr, err := s.db.get(r.Context(), r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		err = &connector.Error{Status: http.StatusNotFound, Message: err.Error()}
	}
	if err != nil {
		writeErr(w, err)
		return row{}, false
	}
	return pr, true
}

func (s *Server) notify(ctx context.Context, title, message, click string) {
	if s.cfg.Ntfy == nil {
		return
	}
	if err := s.cfg.Ntfy.publish(context.WithoutCancel(ctx), title, message, click); err != nil {
		s.cfg.Log.Warn("ntfy publish failed", "err", err)
	}
}

type challengeRequest struct {
	Edits map[string]string `json:"edits"`
}

type credentialDescriptor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// requestOptions is PublicKeyCredentialRequestOptions in its JSON form:
// binary fields are base64url.
type requestOptions struct {
	Challenge        string                 `json:"challenge"`
	RPID             string                 `json:"rpId"`
	AllowCredentials []credentialDescriptor `json:"allowCredentials"`
	UserVerification string                 `json:"userVerification"`
	Timeout          int                    `json:"timeout"`
}

// challenge applies the owner's edits, has the connector prepare the result,
// and stores the exact record bytes the passkey will sign.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	var req challengeRequest
	if !decode(w, r, &req) {
		return
	}
	pr, ok := s.load(w, r)
	if !ok {
		return
	}
	if pr.Status != proposal.Pending {
		writeErr(w, &connector.Error{Status: http.StatusConflict, Message: "proposal is " + string(pr.Status)})
		return
	}
	if len(s.cfg.Approvers) == 0 {
		writeErr(w, &connector.Error{Status: http.StatusServiceUnavailable, Message: "no passkeys are enrolled; approving is disabled"})
		return
	}
	ctx := r.Context()
	c, ok := s.conns[pr.Connector]
	if !ok {
		writeErr(w, &connector.Error{Status: http.StatusBadGateway, Message: fmt.Sprintf("connector %q is no longer configured", pr.Connector)})
		return
	}
	op, err := c.op(ctx, pr.Op, s.now())
	if err != nil {
		writeErr(w, upstream(err))
		return
	}
	editable := map[string]bool{}
	for _, p := range op.Params {
		editable[p.Name] = p.Editable
	}
	args := maps.Clone(pr.Args)
	var edited []string
	for k, v := range req.Edits {
		if !editable[k] {
			writeErr(w, connector.Invalid("%s is not editable", k))
			return
		}
		if args[k] != v {
			args[k] = v
			edited = append(edited, k)
		}
	}
	slices.Sort(edited)
	p, err := c.client.Prepare(ctx, pr.Op, args)
	if err != nil {
		writeErr(w, upstream(err))
		return
	}
	record, err := approval.Encode(approval.Record{
		ID: pr.ID, Connector: pr.Connector, Op: pr.Op, Args: p.Args, Display: p.Display,
		Note: pr.Note, CreatedAt: pr.CreatedAt, ExpiresAt: pr.ExpiresAt,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	issuedAt := s.cfg.Now().UTC()
	if err := s.db.setChallenge(ctx, pr.ID, record, nonce, issuedAt, edited); err != nil {
		writeErr(w, conflict(err))
		return
	}
	opts := requestOptions{
		Challenge:        b64(approval.Challenge(record, nonce, issuedAt)),
		RPID:             s.cfg.RPID,
		UserVerification: "required",
		Timeout:          int((2 * time.Minute).Milliseconds()),
	}
	for _, a := range s.cfg.Approvers {
		opts.AllowCredentials = append(opts.AllowCredentials, credentialDescriptor{Type: "public-key", ID: a.CredentialID})
	}
	writeJSON(w, http.StatusOK, opts)
}

type approveRequest struct {
	Assertion json.RawMessage `json:"assertion"`
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if !decode(w, r, &req) {
		return
	}
	pr, ok := s.load(w, r)
	if !ok {
		return
	}
	if pr.Status != proposal.Pending {
		writeErr(w, &connector.Error{Status: http.StatusConflict, Message: "proposal is " + string(pr.Status)})
		return
	}
	if pr.Record == nil {
		writeErr(w, &connector.Error{Status: http.StatusConflict, Message: "no challenge was issued"})
		return
	}
	c, ok := s.conns[pr.Connector]
	if !ok {
		writeErr(w, &connector.Error{Status: http.StatusBadGateway, Message: fmt.Sprintf("connector %q is no longer configured", pr.Connector)})
		return
	}
	a := approval.Approval{Record: pr.Record, Nonce: pr.Nonce, IssuedAt: pr.IssuedAt, Assertion: req.Assertion}
	// The connector verifies again; checking here refuses bad approvals
	// before anything is marked executing.
	rec, err := s.verifier.Verify(a)
	if err != nil {
		s.cfg.Log.Warn("approval refused", "id", pr.ID, "err", err)
		writeErr(w, &connector.Error{Status: http.StatusForbidden, Message: err.Error()})
		return
	}
	if rec.ID != pr.ID {
		writeErr(w, &connector.Error{Status: http.StatusForbidden, Message: "approval is for a different proposal"})
		return
	}
	// From here on the request runs to completion even if the page goes
	// away: once claimed, the outcome must be recorded.
	ctx := context.WithoutCancel(r.Context())
	if err := s.db.claim(ctx, pr.ID, pr.Nonce, rec, s.approverName(req.Assertion), s.now()); err != nil {
		writeErr(w, conflict(err))
		return
	}
	res := s.execute(ctx, c, a)
	s.cfg.Log.Info("executed", "id", pr.ID, "connector", pr.Connector, "op", pr.Op, "status", res.Status, "ref", res.Ref)
	if err := s.db.finish(ctx, pr.ID, res, s.now()); err != nil {
		// The proposal stays executing and becomes unknown on restart.
		s.cfg.Log.Error("store result", "id", pr.ID, "status", res.Status, "err", err)
		writeErr(w, err)
		return
	}
	out, err := s.db.get(ctx, pr.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out.Proposal)
}

// execute hands the approval to the connector exactly once. Only failures
// that prove nothing ran become failed; anything else may have reached the
// service and is unknown.
func (s *Server) execute(ctx context.Context, c *conn, a approval.Approval) connector.Result {
	ctx, cancel := context.WithTimeout(ctx, executeTimeout)
	defer cancel()
	res, err := c.client.Execute(ctx, a)
	var refused *connector.Error
	var opErr *net.OpError
	switch {
	case err == nil:
		switch res.Status {
		case connector.Done, connector.Failed, connector.Unknown:
			return res
		}
		return connector.Result{Status: connector.Unknown, Detail: fmt.Sprintf("connector returned status %q", res.Status)}
	case errors.As(err, &refused):
		// Connectors answer non-2xx only before executing.
		return connector.Result{Status: connector.Failed, Detail: "connector refused: " + refused.Message}
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return connector.Result{Status: connector.Failed, Detail: "connector unreachable: " + err.Error()}
	default:
		return connector.Result{Status: connector.Unknown, Detail: "lost contact with connector: " + err.Error()}
	}
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.load(w, r)
	if !ok {
		return
	}
	if err := s.db.reject(r.Context(), pr.ID, s.now()); err != nil {
		writeErr(w, conflict(err))
		return
	}
	s.cfg.Log.Info("rejected", "id", pr.ID)
	http.Redirect(w, r, "/p/"+pr.ID, http.StatusSeeOther)
}

// approverName names the passkey behind an assertion for the audit log.
func (s *Server) approverName(assertion json.RawMessage) string {
	var cred struct {
		RawID string `json:"rawId"`
	}
	if json.Unmarshal(assertion, &cred) != nil {
		return ""
	}
	for _, a := range s.cfg.Approvers {
		if a.CredentialID == cred.RawID {
			return a.Name
		}
	}
	return ""
}

func upstream(err error) error {
	var e *connector.Error
	if errors.As(err, &e) {
		return e
	}
	return &connector.Error{Status: http.StatusBadGateway, Message: err.Error()}
}

func conflict(err error) error {
	if errors.Is(err, errNotPending) {
		return &connector.Error{Status: http.StatusConflict, Message: err.Error()}
	}
	return err
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, connector.Invalid("invalid request: %v", err))
		return false
	}
	return true
}

// writeErr answers in connector.Error's shape, which proposal.Client decodes.
func writeErr(w http.ResponseWriter, err error) {
	var e *connector.Error
	if !errors.As(err, &e) {
		e = &connector.Error{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	if e.Status == 0 {
		e.Status = http.StatusBadRequest
	}
	writeJSON(w, e.Status, e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
