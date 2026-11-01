package console

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
	"sync"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
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

type Config struct {
	// RPID is the WebAuthn relying party ID, the console's hostname.
	RPID string
	// Origin is the console's origin as browsers report it, e.g.
	// https://box.tailnet.ts.net.
	Origin string
	// PasswordHash is the bcrypt hash of the admin password.
	PasswordHash []byte
	Connectors   map[string]string
	StateDir     string
	Log          *slog.Logger
	Now          func() time.Time
}

type Server struct {
	cfg      Config
	db       *db
	keys     *approval.Keys
	verifier *approval.Verifier
	conns    map[string]*conn
	tmpl     *template.Template
	logins   *loginChallenges
	password throttle

	regMu sync.Mutex
	regs  map[string]registration
}

type conn struct {
	name   string
	client *connector.Client

	mu      sync.Mutex
	spec    connector.Spec
	fetched time.Time
	// keysProblem is why the connector's key log differs from ours, if it
	// does.
	keysProblem string
}

// New opens the database and key log in cfg.StateDir and settles proposals
// left executing by a previous run.
func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	keys, err := approval.OpenKeys(filepath.Join(cfg.StateDir, "keys.json"), cfg.RPID, cfg.Origin)
	if err != nil {
		return nil, fmt.Errorf("open key log: %w", err)
	}
	d, err := openDB(ctx, filepath.Join(cfg.StateDir, "console.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Server{
		cfg:      cfg,
		db:       d,
		keys:     keys,
		verifier: &approval.Verifier{Keys: keys, Now: cfg.Now},
		conns:    map[string]*conn{},
		tmpl:     templates,
		logins:   newLoginChallenges(),
		regs:     map[string]registration{},
	}
	for name, sock := range cfg.Connectors {
		s.conns[name] = &conn{name: name, client: connector.Dial(sock, executeTimeout)}
	}
	ids, err := d.recoverInterrupted(ctx)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("recover interrupted proposals: %w", err)
	}
	for _, id := range ids {
		cfg.Log.Warn("proposal was interrupted while executing; it may have run", "id", id)
	}
	return s, nil
}

func (s *Server) Close() error { return s.db.Close() }

// now is truncated to what the database stores, so a record built from a
// stored proposal matches one built from the original.
func (s *Server) now() time.Time { return s.cfg.Now().UTC().Truncate(time.Millisecond) }

// Sweep pushes the key log to connectors that lack it, now and every
// interval, and cleans up expired state.
func (s *Server) Sweep(ctx context.Context, interval time.Duration) {
	s.syncKeys(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.db.sweep(ctx, s.now()); err != nil && ctx.Err() == nil {
				s.cfg.Log.Error("sweep", "err", err)
			}
			s.syncKeys(ctx)
		}
	}
}

func (s *Server) url(id string) string { return s.cfg.Origin + "/r/" + id }

// getSpec caches the spec for specTTL. A connector answering under another
// name or protocol is refused and not cached.
func (c *conn) getSpec(ctx context.Context, now time.Time) (connector.Spec, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetched.IsZero() || now.Sub(c.fetched) > specTTL {
		spec, err := c.client.Spec(ctx)
		if err != nil {
			return connector.Spec{}, fmt.Errorf("fetch %s spec: %w", c.name, err)
		}
		// Connectors check the record's connector against their own name.
		if spec.Name != c.name {
			return connector.Spec{}, fmt.Errorf("connector configured as %q calls itself %q", c.name, spec.Name)
		}
		c.spec, c.fetched = spec, now
	}
	return c.spec, nil
}

func (c *conn) op(ctx context.Context, name string, now time.Time) (connector.Op, error) {
	spec, err := c.getSpec(ctx, now)
	if err != nil {
		return connector.Op{}, err
	}
	op, ok := spec.Op(name)
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
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	ctx := r.Context()
	c, ok := s.conns[req.Connector]
	if !ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusNotFound, Message: fmt.Sprintf("no connector %q", req.Connector)})
		return
	}
	now := s.now()
	if _, err := c.op(ctx, req.Op, now); err != nil {
		connector.WriteError(w, upstream(err))
		return
	}
	// Args and display come from the connector, never from the gateway.
	p, err := c.client.Prepare(ctx, req.Op, req.Args)
	if err != nil {
		connector.WriteError(w, upstream(err))
		return
	}
	pr := row{
		Proposal: proposal.Proposal{
			ID: rand.Text(), Connector: req.Connector, Op: req.Op, Display: p.Display, Note: req.Note,
			Status: proposal.Pending, CreatedAt: now, ExpiresAt: now.Add(proposalTTL),
		},
		Args: p.Args,
	}
	if err := s.db.insert(ctx, pr, maxPending); err != nil {
		if errors.Is(err, errTooMany) {
			err = &connector.Error{Status: http.StatusTooManyRequests, Message: fmt.Sprintf("%d proposals are already waiting for the owner", maxPending)}
		}
		connector.WriteError(w, err)
		return
	}
	s.cfg.Log.Info("proposed", "id", pr.ID, "connector", req.Connector, "op", req.Op)
	pr.URL = s.url(pr.ID)
	connector.WriteJSON(w, http.StatusCreated, pr.Proposal)
}

func (s *Server) getProposal(w http.ResponseWriter, r *http.Request) {
	pr, err := s.load(r.Context(), r.PathValue("id"))
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	connector.WriteJSON(w, http.StatusOK, pr.Proposal)
}

// load expires due proposals and returns one with its URL filled in.
func (s *Server) load(ctx context.Context, id string) (row, error) {
	if err := s.db.expireDue(ctx, s.now()); err != nil {
		return row{}, err
	}
	pr, err := s.db.get(ctx, id)
	if errors.Is(err, errNotFound) {
		return row{}, &connector.Error{Status: http.StatusNotFound, Message: err.Error()}
	}
	if err != nil {
		return row{}, err
	}
	pr.URL = s.url(pr.ID)
	return pr, nil
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

func (s *Server) requestOptions(challenge []byte) requestOptions {
	opts := requestOptions{
		Challenge:        b64(challenge),
		RPID:             s.cfg.RPID,
		AllowCredentials: []credentialDescriptor{},
		UserVerification: "required",
		Timeout:          int((2 * time.Minute).Milliseconds()),
	}
	for _, k := range s.keys.Trusted() {
		opts.AllowCredentials = append(opts.AllowCredentials, credentialDescriptor{Type: "public-key", ID: k.CredentialID})
	}
	return opts
}

// challengeResponse carries what the passkey will sign: the page shows
// Preview, rendered from the record's display, before asking for the key.
type challengeResponse struct {
	Nonce   string           `json:"nonce"`
	Options requestOptions   `json:"options"`
	Display []approval.Field `json:"display"`
	Preview string           `json:"preview"`
}

// challenge applies the owner's edits, has the connector prepare the result,
// and stores the exact record bytes the passkey will sign. Each call adds a
// challenge; earlier ones stay valid until one is used.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request, sess session) {
	var req challengeRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	ctx := r.Context()
	pr, err := s.load(ctx, r.PathValue("id"))
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	if pr.Status != proposal.Pending {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: "request is " + string(pr.Status)})
		return
	}
	c, ok := s.conns[pr.Connector]
	if !ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusBadGateway, Message: fmt.Sprintf("connector %q is no longer configured", pr.Connector)})
		return
	}
	op, err := c.op(ctx, pr.Op, s.now())
	if err != nil {
		connector.WriteError(w, upstream(err))
		return
	}
	editable := map[string]bool{}
	for _, p := range op.Params {
		editable[p.Name] = p.Editable
	}
	args := maps.Clone(pr.Args)
	for k, v := range req.Edits {
		if !editable[k] {
			connector.WriteError(w, connector.Invalid("%s is not editable", k))
			return
		}
		args[k] = v
	}
	p, err := c.client.Prepare(ctx, pr.Op, args)
	if err != nil {
		connector.WriteError(w, upstream(err))
		return
	}
	record, err := approval.Encode(approval.Record{
		ID: pr.ID, Connector: pr.Connector, Op: pr.Op, Args: p.Args, Display: p.Display,
		Note: pr.Note, CreatedAt: pr.CreatedAt, ExpiresAt: pr.ExpiresAt,
	})
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	ch := challenge{Nonce: make([]byte, 16), Proposal: pr.ID, Record: record, IssuedAt: s.cfg.Now().UTC(), Session: sess.hash}
	rand.Read(ch.Nonce)
	if err := s.db.addChallenge(ctx, ch, s.now()); err != nil {
		connector.WriteError(w, conflict(err))
		return
	}
	preview, err := s.renderString("fields", fieldViews(p.Display, nil, nil))
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	connector.WriteJSON(w, http.StatusOK, challengeResponse{
		Nonce:   b64(ch.Nonce),
		Options: s.requestOptions(approval.Challenge(record, ch.Nonce, ch.IssuedAt)),
		Display: p.Display,
		Preview: preview,
	})
}

// approveRequest carries a passkey assertion over the challenge, or, in an
// unlock window, the delegation and the session key's signature over it.
type approveRequest struct {
	Nonce      string          `json:"nonce"`
	Assertion  json.RawMessage `json:"assertion,omitempty"`
	Delegation *delegation     `json:"delegation,omitempty"`
	Signature  string          `json:"signature,omitempty"`
}

// delegation is an approval.Delegation as pages send it, in base64url.
type delegation struct {
	PublicKey string          `json:"public_key"`
	ExpiresAt time.Time       `json:"expires_at"`
	Covers    string          `json:"covers"`
	Assertion json.RawMessage `json:"assertion,omitempty"`
}

func (d delegation) decode() (approval.Delegation, error) {
	pub, err := base64.RawURLEncoding.DecodeString(d.PublicKey)
	if err != nil {
		return approval.Delegation{}, connector.Invalid("invalid unlock public key")
	}
	covers, err := base64.RawURLEncoding.DecodeString(d.Covers)
	if err != nil {
		return approval.Delegation{}, connector.Invalid("invalid unlock covers")
	}
	return approval.Delegation{PublicKey: pub, ExpiresAt: d.ExpiresAt, Covers: covers, Assertion: d.Assertion}, nil
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request, sess session) {
	var req approveRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	nonce, err := base64.RawURLEncoding.DecodeString(req.Nonce)
	if err != nil {
		connector.WriteError(w, connector.Invalid("invalid nonce"))
		return
	}
	a := approval.Approval{Assertion: req.Assertion}
	// proof is what claim stores; signer is the passkey's assertion.
	proof, signer := req.Assertion, req.Assertion
	if req.Delegation != nil {
		if sess.key.CredentialID == "" {
			connector.WriteError(w, forbidden(errors.New("a password session can't unlock; sign in with a passkey")))
			return
		}
		d, err := req.Delegation.decode()
		if err != nil {
			connector.WriteError(w, err)
			return
		}
		if a.Signature, err = base64.RawURLEncoding.DecodeString(req.Signature); err != nil {
			connector.WriteError(w, connector.Invalid("invalid signature"))
			return
		}
		a.Delegation = &d
		if proof, err = json.Marshal(windowProof{a.Delegation, a.Signature}); err != nil {
			connector.WriteError(w, err)
			return
		}
		signer = d.Assertion
	}
	id := r.PathValue("id")
	ch, err := s.db.challenge(r.Context(), nonce)
	if errors.Is(err, errNotFound) || (err == nil && ch.Proposal != id) {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: "this approval was already used or has expired; start again"})
		return
	}
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	// The page that asked for the challenge is the one that showed it.
	if string(ch.Session) != string(sess.hash) {
		connector.WriteError(w, &connector.Error{Status: http.StatusForbidden, Message: "this approval was started in another session"})
		return
	}
	pr, err := s.load(r.Context(), id)
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	c, ok := s.conns[pr.Connector]
	if !ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusBadGateway, Message: fmt.Sprintf("connector %q is no longer configured", pr.Connector)})
		return
	}
	a.Record, a.Nonce, a.IssuedAt = ch.Record, ch.Nonce, ch.IssuedAt
	// The connector verifies again; checking here refuses bad approvals
	// before anything is marked executing.
	rec, err := s.verifier.Verify(a)
	if err != nil {
		s.cfg.Log.Warn("approval refused", "id", id, "err", err)
		connector.WriteError(w, &connector.Error{Status: http.StatusForbidden, Message: err.Error()})
		return
	}
	if rec.ID != id {
		connector.WriteError(w, &connector.Error{Status: http.StatusForbidden, Message: "approval is for a different request"})
		return
	}
	approver, _ := s.keys.Find(b64(assertionRawID(signer)))
	// From here on the request runs to completion even if the page goes
	// away: once claimed, the outcome must be recorded.
	ctx := context.WithoutCancel(r.Context())
	if err := s.db.claim(ctx, ch, rec, proof, approver.Name, s.now()); err != nil {
		connector.WriteError(w, conflict(err))
		return
	}
	res := s.execute(ctx, c, a)
	s.cfg.Log.Info("executed", "id", id, "connector", pr.Connector, "op", pr.Op, "status", res.Status, "ref", res.Ref)
	if err := s.db.finish(ctx, id, res); err != nil {
		// The proposal stays executing and becomes unknown on restart.
		s.cfg.Log.Error("store result", "id", id, "status", res.Status, "err", err)
		connector.WriteError(w, err)
		return
	}
	out, err := s.load(ctx, id)
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	connector.WriteJSON(w, http.StatusOK, out.Proposal)
}

// windowProof stands in for the assertion of an approval made in an unlock
// window; it unmarshals into an approval.Approval.
type windowProof struct {
	Delegation *approval.Delegation `json:"delegation"`
	Signature  []byte               `json:"signature"`
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

func (s *Server) deny(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	err := s.db.reject(r.Context(), id, sess.who(), s.now())
	switch {
	case errors.Is(err, errNotFound):
		s.errorPage(w, http.StatusNotFound, "No such request.")
		return
	case errors.Is(err, errNotPending):
		s.errorPage(w, http.StatusConflict, "This request was already decided.")
		return
	case err != nil:
		s.cfg.Log.Error("deny", "id", id, "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't deny the request.")
		return
	}
	s.cfg.Log.Info("denied", "id", id, "by", sess.who())
	http.Redirect(w, r, "/r/"+id, http.StatusSeeOther)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	err := s.db.remove(r.Context(), id)
	switch {
	case errors.Is(err, errNotFound):
		s.errorPage(w, http.StatusNotFound, "No such request.")
		return
	case errors.Is(err, errUnsettled):
		s.errorPage(w, http.StatusConflict, "Deny the request before deleting it.")
		return
	case err != nil:
		s.cfg.Log.Error("delete", "id", id, "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't delete the request.")
		return
	}
	s.cfg.Log.Info("deleted", "id", id, "by", sess.who())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func assertionRawID(assertion json.RawMessage) []byte {
	var cred struct {
		RawID string `json:"rawId"`
	}
	if json.Unmarshal(assertion, &cred) != nil {
		return nil
	}
	id, _ := base64.RawURLEncoding.DecodeString(cred.RawID)
	return id
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
