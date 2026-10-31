package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

const (
	keyChangeTTL  = 24 * time.Hour
	maxKeyChanges = 10
	registerTTL   = 5 * time.Minute
)

// Passkey algorithms offered at registration, in order of preference.
var credParams = []protocol.CredentialParameter{
	{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
	{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgEdDSA},
	{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgRS256},
}

// registration is a passkey creation in progress, keyed by its challenge.
type registration struct {
	name    string
	session []byte
	expires time.Time
}

type rpEntity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type userEntity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type authenticatorSelection struct {
	ResidentKey      string `json:"residentKey"`
	UserVerification string `json:"userVerification"`
}

// creationOptions is PublicKeyCredentialCreationOptions in its JSON form.
type creationOptions struct {
	RP                     rpEntity                       `json:"rp"`
	User                   userEntity                     `json:"user"`
	Challenge              string                         `json:"challenge"`
	PubKeyCredParams       []protocol.CredentialParameter `json:"pubKeyCredParams"`
	ExcludeCredentials     []credentialDescriptor         `json:"excludeCredentials"`
	AuthenticatorSelection authenticatorSelection         `json:"authenticatorSelection"`
	Attestation            string                         `json:"attestation"`
	Timeout                int                            `json:"timeout"`
}

func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_. ", r) {
			return false
		}
	}
	return true
}

type registerRequest struct {
	Name string `json:"name"`
}

func (s *Server) registerOptions(w http.ResponseWriter, r *http.Request, sess session) {
	var req registerRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	if !validName(req.Name) {
		connector.WriteError(w, connector.Invalid("name must be 1-64 letters, digits, spaces, or -_."))
		return
	}
	pending, err := s.db.keyChanges(r.Context(), s.now())
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	challenge, userID := make([]byte, 32), make([]byte, 16)
	rand.Read(challenge)
	rand.Read(userID)
	now := s.now()
	s.regMu.Lock()
	for c, reg := range s.regs {
		if !now.Before(reg.expires) {
			delete(s.regs, c)
		}
	}
	s.regs[b64(challenge)] = registration{name: req.Name, session: sess.hash, expires: now.Add(registerTTL)}
	s.regMu.Unlock()

	opts := creationOptions{
		RP:                     rpEntity{ID: s.cfg.RPID, Name: "mandate"},
		User:                   userEntity{ID: b64(userID), Name: req.Name, DisplayName: "mandate " + req.Name},
		Challenge:              b64(challenge),
		PubKeyCredParams:       credParams,
		ExcludeCredentials:     []credentialDescriptor{},
		AuthenticatorSelection: authenticatorSelection{ResidentKey: "preferred", UserVerification: "required"},
		Attestation:            "none",
		Timeout:                int(registerTTL.Milliseconds()),
	}
	for _, k := range s.keys.Trusted() {
		opts.ExcludeCredentials = append(opts.ExcludeCredentials, credentialDescriptor{Type: "public-key", ID: k.CredentialID})
	}
	for _, c := range pending {
		opts.ExcludeCredentials = append(opts.ExcludeCredentials, credentialDescriptor{Type: "public-key", ID: c.Key.CredentialID})
	}
	connector.WriteJSON(w, http.StatusOK, opts)
}

type addKeyResponse struct {
	Trusted bool `json:"trusted"`
}

// addKey finishes a registration. The first key is trusted at once; any
// later one waits for a trusted key to approve it.
func (s *Server) addKey(w http.ResponseWriter, r *http.Request, sess session) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		connector.WriteError(w, connector.Invalid("read request: %v", err))
		return
	}
	pcc, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		connector.WriteError(w, connector.Invalid("parse registration: %v", webauthnErr(err)))
		return
	}
	challenge := pcc.Response.CollectedClientData.Challenge
	s.regMu.Lock()
	reg, ok := s.regs[challenge]
	delete(s.regs, challenge)
	s.regMu.Unlock()
	if !ok || !s.now().Before(reg.expires) || !bytes.Equal(reg.session, sess.hash) {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: "this key creation expired; start again"})
		return
	}
	_, err = pcc.Verify(challenge, s.cfg.RPID, []string{s.cfg.Origin}, nil, nil,
		protocol.TopOriginExplicitVerificationMode, false, true, true, nil, credParams,
		protocol.AttestationPolicy{}, protocol.SignaturePolicy{})
	if err != nil {
		connector.WriteError(w, connector.Invalid("verify registration: %v", webauthnErr(err)))
		return
	}
	auth := pcc.Response.AttestationObject.AuthData
	k := approval.Key{
		Name:         reg.name,
		CredentialID: b64(auth.AttData.CredentialID),
		PublicKey:    b64(auth.AttData.CredentialPublicKey),
		Synced:       auth.Flags.HasBackupEligible(),
	}
	if len(s.keys.Trusted()) == 0 {
		change, err := s.keys.Next(approval.OpAdd, k, s.now())
		if err == nil {
			err = s.keys.Apply(approval.SignedChange{Change: change})
		}
		if err != nil {
			connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: err.Error()})
			return
		}
		s.cfg.Log.Info("first key trusted", "key", k.Name, "by", sess.who())
		s.syncKeys(r.Context())
		connector.WriteJSON(w, http.StatusOK, addKeyResponse{Trusted: true})
		return
	}
	if err := s.requestChange(r.Context(), approval.OpAdd, k); err != nil {
		connector.WriteError(w, err)
		return
	}
	s.cfg.Log.Info("key waiting for approval", "op", approval.OpAdd, "key", k.Name, "by", sess.who())
	connector.WriteJSON(w, http.StatusOK, addKeyResponse{})
}

func (s *Server) removeKey(w http.ResponseWriter, r *http.Request, sess session) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	k, ok := s.keys.Find(r.FormValue("credential_id"))
	if !ok {
		s.errorPage(w, http.StatusNotFound, "No such key.")
		return
	}
	if err := s.requestChange(r.Context(), approval.OpRemove, k); err != nil {
		e := asError(err)
		s.errorPage(w, e.Status, e.Message)
		return
	}
	s.cfg.Log.Info("key waiting for approval", "op", approval.OpRemove, "key", k.Name, "by", sess.who())
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

// requestChange holds op on k until a trusted key approves it.
func (s *Server) requestChange(ctx context.Context, op string, k approval.Key) error {
	now := s.now()
	if _, err := s.keys.Next(op, k, now); err != nil {
		return &connector.Error{Status: http.StatusConflict, Message: err.Error()}
	}
	err := s.db.addKeyChange(ctx, keyChange{ID: rand.Text(), Op: op, Key: k, CreatedAt: now, ExpiresAt: now.Add(keyChangeTTL)}, maxKeyChanges)
	switch {
	case errors.Is(err, errKeyPending):
		return &connector.Error{Status: http.StatusConflict, Message: err.Error()}
	case errors.Is(err, errTooMany):
		return &connector.Error{Status: http.StatusTooManyRequests, Message: "too many key changes are waiting"}
	}
	return err
}

func (s *Server) discardKeyChange(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	err := s.db.deleteKeyChange(r.Context(), id)
	if err != nil && !errors.Is(err, errNoKeyChange) {
		s.cfg.Log.Error("discard key change", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't discard the change.")
		return
	}
	s.cfg.Log.Info("key change discarded", "id", id, "by", sess.who())
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

type keyChallengeResponse struct {
	Change  string         `json:"change"`
	Options requestOptions `json:"options"`
}

// keyChallenge encodes a waiting change as the next log entry for a trusted
// key to sign.
func (s *Server) keyChallenge(w http.ResponseWriter, r *http.Request, _ session) {
	c, err := s.db.keyChange(r.Context(), r.PathValue("id"), s.now())
	if err != nil {
		connector.WriteError(w, notFound(err))
		return
	}
	change, err := s.keys.Next(c.Op, c.Key, s.now())
	if err != nil {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: err.Error()})
		return
	}
	connector.WriteJSON(w, http.StatusOK, keyChallengeResponse{
		Change:  b64(change),
		Options: s.requestOptions(approval.KeyChallenge(change)),
	})
}

type keyApproveRequest struct {
	Change    string          `json:"change"`
	Assertion json.RawMessage `json:"assertion"`
}

func (s *Server) approveKeyChange(w http.ResponseWriter, r *http.Request, sess session) {
	var req keyApproveRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	pending, err := s.db.keyChange(ctx, id, s.now())
	if err != nil {
		connector.WriteError(w, notFound(err))
		return
	}
	change, err := base64.RawURLEncoding.DecodeString(req.Change)
	if err != nil {
		connector.WriteError(w, connector.Invalid("invalid change"))
		return
	}
	c, err := approval.DecodeChange(change)
	if err != nil {
		connector.WriteError(w, connector.Invalid("%v", err))
		return
	}
	if c.Op != pending.Op || c.Key != pending.Key {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: "this is not the change that is waiting"})
		return
	}
	err = s.keys.Apply(approval.SignedChange{Change: change, Signer: b64(assertionRawID(req.Assertion)), Assertion: req.Assertion})
	if errors.Is(err, approval.ErrOutOfOrder) {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: "the keys changed meanwhile; start again"})
		return
	}
	if err != nil {
		s.cfg.Log.Warn("key change refused", "id", id, "err", err)
		connector.WriteError(w, forbidden(err))
		return
	}
	if err := s.db.deleteKeyChange(ctx, id); err != nil {
		s.cfg.Log.Error("delete applied key change", "id", id, "err", err)
	}
	s.cfg.Log.Info("key change applied", "op", c.Op, "key", c.Key.Name, "seq", c.Seq, "session", sess.who())
	s.syncKeys(ctx)
	connector.WriteJSON(w, http.StatusOK, struct{}{})
}

// syncKeys pushes the log to every connector whose head differs from ours.
func (s *Server) syncKeys(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range s.conns {
		wg.Go(func() { s.syncConn(ctx, c) })
	}
	wg.Wait()
}

// syncConn returns why c's key log differs from ours, or "".
func (s *Server) syncConn(ctx context.Context, c *conn) string {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	n, head := s.keys.Head()
	h, err := c.client.Keys(ctx)
	if err == nil && (h.Length != n || h.Head != head) {
		_, err = c.client.PutKeys(ctx, s.keys.Log())
	}
	problem := ""
	if err != nil {
		problem = err.Error()
	}
	c.mu.Lock()
	changed := problem != c.keysProblem
	c.keysProblem = problem
	c.mu.Unlock()
	if changed && problem != "" {
		s.cfg.Log.Warn("connector's keys differ", "connector", c.name, "err", problem)
	}
	return problem
}

func (c *conn) keysProblemNow() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keysProblem
}

func kind(synced bool) string {
	if synced {
		return "synced"
	}
	return "hardware"
}

type keyView struct {
	approval.Key
	Kind    string
	Added   time.Time
	Current bool
}

type keyChangeView struct {
	ID, Op, Name, Kind, Left string
}

type keysView struct {
	Keys     []keyView
	Changes  []keyChangeView
	Problems []connProblem
}

type connProblem struct {
	Name, Problem string
}

func (s *Server) keysPage(w http.ResponseWriter, r *http.Request, sess session) {
	ctx := r.Context()
	now := s.now()
	changes, err := s.db.keyChanges(ctx, now)
	if err != nil {
		s.cfg.Log.Error("list key changes", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load key changes.")
		return
	}
	s.syncKeys(ctx)

	added := map[string]time.Time{}
	for _, sc := range s.keys.Log() {
		if c, err := approval.DecodeChange(sc.Change); err == nil && c.Op == approval.OpAdd {
			added[c.Key.CredentialID] = c.At
		}
	}
	var v keysView
	for _, k := range s.keys.Trusted() {
		v.Keys = append(v.Keys, keyView{Key: k, Kind: kind(k.Synced), Added: added[k.CredentialID], Current: k.CredentialID == sess.key.CredentialID})
	}
	for _, c := range changes {
		v.Changes = append(v.Changes, keyChangeView{ID: c.ID, Op: c.Op, Name: c.Key.Name, Kind: kind(c.Key.Synced), Left: left(c.ExpiresAt.Sub(now))})
	}
	v.Problems = s.keysProblems()
	s.owner(w, r, sess, "keys", "Keys", "keys", v)
}

func (s *Server) keysProblems() []connProblem {
	var out []connProblem
	for _, c := range s.conns {
		if p := c.keysProblemNow(); p != "" {
			out = append(out, connProblem{Name: c.name, Problem: p})
		}
	}
	slices.SortFunc(out, func(a, b connProblem) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func notFound(err error) error {
	if errors.Is(err, errNoKeyChange) {
		return &connector.Error{Status: http.StatusNotFound, Message: err.Error()}
	}
	return err
}

func asError(err error) *connector.Error {
	if e, ok := errors.AsType[*connector.Error](err); ok {
		return e
	}
	return &connector.Error{Status: http.StatusInternalServerError, Message: err.Error()}
}
