package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	if s.nameTaken(pending, req.Name) {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: errNameTaken.Error()})
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

// nameTaken reports whether a trusted key or a waiting addition is called
// name.
func (s *Server) nameTaken(pending []keyChange, name string) bool {
	for _, k := range s.keys.Trusted() {
		if strings.EqualFold(k.Name, name) {
			return true
		}
	}
	return slices.ContainsFunc(pending, func(c keyChange) bool {
		return c.Op == approval.OpAdd && strings.EqualFold(c.Key.Name, name)
	})
}

// addKey finishes a registration. The first key is trusted at once; any
// later one waits for a trusted key to approve it and then for the owner to
// confirm it on the server.
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
		// Deleted rather than refused, so they stay ended after a key reset.
		if err := s.db.deletePasswordSessions(r.Context()); err != nil {
			s.cfg.Log.Error("end password sessions", "err", err)
		}
		s.syncKeys(r.Context())
		connector.WriteJSON(w, http.StatusOK, addKeyResponse{Trusted: true})
		return
	}
	if s.nameTaken(nil, k.Name) {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: errNameTaken.Error()})
		return
	}
	if err := s.requestChange(r.Context(), approval.OpAdd, k, auth.AttData.AAGUID); err != nil {
		connector.WriteError(w, err)
		return
	}
	s.cfg.Log.Info("key waiting for approval", "op", approval.OpAdd, "key", k.Name, "fingerprint", fingerprint(k), "by", sess.who())
	connector.WriteJSON(w, http.StatusOK, addKeyResponse{})
}

func (s *Server) removeKey(w http.ResponseWriter, r *http.Request, sess session) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	k, ok := s.keys.Find(r.FormValue("credential_id"))
	if !ok {
		s.errorPage(w, http.StatusNotFound, "No such key.")
		return
	}
	if err := s.requestChange(r.Context(), approval.OpRemove, k, nil); err != nil {
		e := asError(err)
		s.errorPage(w, e.Status, e.Message)
		return
	}
	s.cfg.Log.Info("key waiting for approval", "op", approval.OpRemove, "key", k.Name, "by", sess.who())
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

// requestChange holds op on k until a trusted key approves it.
func (s *Server) requestChange(ctx context.Context, op string, k approval.Key, aaguid []byte) error {
	now := s.now()
	if _, err := s.keys.Next(op, k, now); err != nil {
		return &connector.Error{Status: http.StatusConflict, Message: err.Error()}
	}
	err := s.db.addKeyChange(ctx, keyChange{
		ID: rand.Text(), Op: op, Key: k, AAGUID: aaguid, CreatedAt: now, ExpiresAt: now.Add(keyChangeTTL),
	}, maxKeyChanges)
	switch {
	case errors.Is(err, errKeyPending), errors.Is(err, errNameTaken):
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
	if c.Approved != nil {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: "already approved; confirm it on the server"})
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
	sc := approval.SignedChange{Change: change, Signer: b64(assertionRawID(req.Assertion)), Assertion: req.Assertion}
	// An addition also waits for the owner on the server: script on this
	// origin could get one touch to sign the addition of its own key.
	if c.Op == approval.OpAdd {
		err = s.keys.Check(sc)
	} else {
		err = s.keys.Apply(sc)
	}
	if errors.Is(err, approval.ErrOutOfOrder) {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: "the keys changed meanwhile; start again"})
		return
	}
	if err != nil {
		s.cfg.Log.Warn("key change refused", "id", id, "err", err)
		connector.WriteError(w, forbidden(err))
		return
	}
	if c.Op == approval.OpAdd {
		if err := s.db.approveKeyChange(ctx, id, &sc, s.now()); err != nil {
			connector.WriteError(w, notFound(err))
			return
		}
		s.cfg.Log.Info("key addition waiting for the server", "key", c.Key.Name, "fingerprint", fingerprint(c.Key), "session", sess.who())
		connector.WriteJSON(w, http.StatusOK, struct{}{})
		return
	}
	if err := s.db.deleteKeyChange(ctx, id); err != nil {
		s.cfg.Log.Error("delete applied key change", "id", id, "err", err)
	}
	s.cfg.Log.Info("key change applied", "op", c.Op, "key", c.Key.Name, "seq", c.Seq, "session", sess.who())
	s.syncKeys(ctx)
	connector.WriteJSON(w, http.StatusOK, struct{}{})
}

type confirmRequest struct {
	Fingerprint string `json:"fingerprint"`
}

type confirmResponse struct {
	Name string `json:"name"`
}

// confirmKey applies the approved addition of the key with the given
// fingerprint. It is served only on the admin socket.
func (s *Server) confirmKey(w http.ResponseWriter, r *http.Request) {
	var req confirmRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	ctx := r.Context()
	changes, err := s.db.keyChanges(ctx, s.now())
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	i := slices.IndexFunc(changes, func(c keyChange) bool {
		return c.Op == approval.OpAdd && fingerprint(c.Key) == req.Fingerprint
	})
	if i < 0 {
		connector.WriteError(w, &connector.Error{Status: http.StatusNotFound, Message: fmt.Sprintf("no key with fingerprint %q is waiting to be added", req.Fingerprint)})
		return
	}
	c := changes[i]
	if c.Approved == nil {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: fmt.Sprintf("approve adding %q with a trusted passkey in the console first", c.Key.Name)})
		return
	}
	err = s.keys.Apply(*c.Approved)
	if errors.Is(err, approval.ErrOutOfOrder) {
		if err := s.db.approveKeyChange(ctx, c.ID, nil, s.now()); err != nil {
			s.cfg.Log.Error("clear stale key approval", "id", c.ID, "err", err)
		}
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: fmt.Sprintf("the keys changed since adding %q was approved; approve it again in the console", c.Key.Name)})
		return
	}
	if err != nil {
		connector.WriteError(w, &connector.Error{Status: http.StatusConflict, Message: err.Error()})
		return
	}
	if err := s.db.deleteKeyChange(ctx, c.ID); err != nil {
		s.cfg.Log.Error("delete applied key change", "id", c.ID, "err", err)
	}
	s.cfg.Log.Info("key change applied", "op", approval.OpAdd, "key", c.Key.Name, "by", "the server")
	s.syncKeys(ctx)
	connector.WriteJSON(w, http.StatusOK, confirmResponse{Name: c.Key.Name})
}

// fingerprint is a short hash of a credential for the owner to compare and
// type on the server.
func fingerprint(k approval.Key) string {
	h := sha256.Sum256([]byte(k.CredentialID + "." + k.PublicKey))
	x := hex.EncodeToString(h[:8])
	return x[:4] + "-" + x[4:8] + "-" + x[8:12] + "-" + x[12:]
}

// authenticators names common passkey providers by AAGUID, from
// github.com/passkeydeveloper/passkey-authenticator-aaguids.
var authenticators = map[string]string{
	"ea9b8d66-4d01-1d21-3ce4-b6b48cb575d4": "Google Password Manager",
	"fbfc3007-154e-4ecc-8c0b-6e020557d7bd": "Apple Passwords",
	"dd4ec289-e01d-41c9-bb89-70fa845d4bf2": "iCloud Keychain (Managed)",
	"adce0002-35bc-c60a-648b-0b25f1f05503": "Chrome on Mac",
	"08987058-cadc-4b81-b6e1-30de50dcbe96": "Windows Hello",
	"9ddd1817-af5a-4672-a2b9-3e3dd95000a9": "Windows Hello",
	"6028b017-b1d4-4c02-b4b3-afcdafc96bb2": "Windows Hello",
	"bada5566-a7aa-401f-bd96-45619a55120d": "1Password",
	"d548826e-79b4-db40-a3d8-11116f7e8349": "Bitwarden",
	"531126d6-e717-415c-9320-3d9aa6981239": "Dashlane",
	"53414d53-554e-4700-0000-000000000000": "Samsung Pass",
	"50726f74-6f6e-5061-7373-50726f746f6e": "Proton Pass",
	"fdb141b2-5d84-443e-8a35-4698c205a502": "KeePassXC",
}

func formatAAGUID(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
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
	if err != nil && !unreachable(err) {
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

type keyView struct {
	approval.Key
	Added   time.Time
	Current bool
}

type keyChangeView struct {
	ID, Op, Name, Left                 string
	Fingerprint, AAGUID, Authenticator string
	Synced, Approved                   bool
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
		v.Keys = append(v.Keys, keyView{Key: k, Added: added[k.CredentialID], Current: k.CredentialID == sess.key.CredentialID})
	}
	for _, c := range changes {
		aaguid := formatAAGUID(c.AAGUID)
		v.Changes = append(v.Changes, keyChangeView{
			ID: c.ID, Op: c.Op, Name: c.Key.Name, Left: left(c.ExpiresAt.Sub(now)),
			Fingerprint: fingerprint(c.Key), AAGUID: aaguid, Authenticator: authenticators[aaguid],
			Synced: c.Key.Synced, Approved: c.Approved != nil,
		})
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
