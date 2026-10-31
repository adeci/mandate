package approvals

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/connector"
)

const enrollTTL = 10 * time.Minute

// Passkey algorithms offered at enrollment, in order of preference.
var credParams = []protocol.CredentialParameter{
	{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
	{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgEdDSA},
	{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgRS256},
}

type enrollment struct {
	name      string
	expires   time.Time
	challenge []byte
	userID    []byte
}

type enrollRequest struct {
	Name string `json:"name"`
}

type enrollResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AdminHandler serves the root-only admin socket.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enroll", s.startEnroll)
	return mux
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

func (s *Server) startEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if !decode(w, r, &req) {
		return
	}
	if !validName(req.Name) {
		writeErr(w, connector.Invalid("name must be 1-64 letters, digits, spaces, or -_."))
		return
	}
	e := &enrollment{
		name:      req.Name,
		expires:   s.cfg.Now().Add(enrollTTL),
		challenge: make([]byte, 32),
		userID:    make([]byte, 16),
	}
	rand.Read(e.challenge)
	rand.Read(e.userID)
	token := rand.Text()
	s.enrollMu.Lock()
	for t, old := range s.enrolls {
		if !s.cfg.Now().Before(old.expires) {
			delete(s.enrolls, t)
		}
	}
	s.enrolls[token] = e
	s.enrollMu.Unlock()
	s.cfg.Log.Info("enrollment link issued", "name", req.Name, "expires", e.expires)
	writeJSON(w, http.StatusOK, enrollResponse{URL: s.cfg.Origin + "/enroll/" + token, ExpiresAt: e.expires})
}

// liveEnrollment returns the unexpired enrollment for token; take also
// spends it.
func (s *Server) liveEnrollment(token string, take bool) (*enrollment, bool) {
	s.enrollMu.Lock()
	defer s.enrollMu.Unlock()
	e, ok := s.enrolls[token]
	if !ok {
		return nil, false
	}
	if take || !s.cfg.Now().Before(e.expires) {
		delete(s.enrolls, token)
	}
	return e, s.cfg.Now().Before(e.expires)
}

var errEnrollGone = &connector.Error{Status: http.StatusNotFound, Message: "this enrollment link is invalid, used, or expired; run mandate approver enroll again"}

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

func (s *Server) enrollOptions(w http.ResponseWriter, r *http.Request) {
	e, ok := s.liveEnrollment(r.PathValue("token"), false)
	if !ok {
		writeErr(w, errEnrollGone)
		return
	}
	opts := creationOptions{
		RP:                     rpEntity{ID: s.cfg.RPID, Name: "mandate"},
		User:                   userEntity{ID: b64(e.userID), Name: e.name, DisplayName: "mandate " + e.name},
		Challenge:              b64(e.challenge),
		PubKeyCredParams:       credParams,
		ExcludeCredentials:     []credentialDescriptor{},
		AuthenticatorSelection: authenticatorSelection{ResidentKey: "preferred", UserVerification: "required"},
		Attestation:            "none",
		Timeout:                int((5 * time.Minute).Milliseconds()),
	}
	for _, a := range s.cfg.Approvers {
		opts.ExcludeCredentials = append(opts.ExcludeCredentials, credentialDescriptor{Type: "public-key", ID: a.CredentialID})
	}
	writeJSON(w, http.StatusOK, opts)
}

// finishEnroll verifies a registration and prints the approver entry. It
// trusts nothing: the key only approves once the owner adds it to the
// approvers file.
func (s *Server) finishEnroll(w http.ResponseWriter, r *http.Request) {
	e, ok := s.liveEnrollment(r.PathValue("token"), true)
	if !ok {
		writeErr(w, errEnrollGone)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, connector.Invalid("read request: %v", err))
		return
	}
	ap, err := s.verifyRegistration(e, body)
	if err != nil {
		s.cfg.Log.Warn("enrollment failed", "name", e.name, "err", err)
		writeErr(w, connector.Invalid("%v", err))
		return
	}
	if err := appendEnrolled(filepath.Join(s.cfg.StateDir, "enrolled.json"), ap); err != nil {
		s.cfg.Log.Error("save enrolled passkey", "err", err)
	}
	line, _ := json.Marshal(ap)
	s.cfg.Log.Info("passkey enrolled; add it to the approvers to trust it", "approver", string(line))
	if err := s.db.note(r.Context(), s.now(), evEnrolled, ap); err != nil {
		s.cfg.Log.Error("audit enrollment", "err", err)
	}
	s.notify(r.Context(), "mandate", "new passkey enrolled: "+ap.Name, "")
	writeJSON(w, http.StatusOK, ap)
}

func (s *Server) verifyRegistration(e *enrollment, body []byte) (approval.Approver, error) {
	pcc, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		return approval.Approver{}, fmt.Errorf("parse registration: %w", webauthnErr(err))
	}
	_, err = pcc.Verify(b64(e.challenge), s.cfg.RPID, []string{s.cfg.Origin}, nil, nil,
		protocol.TopOriginExplicitVerificationMode, false, true, true, nil, credParams,
		protocol.AttestationPolicy{}, protocol.SignaturePolicy{})
	if err != nil {
		return approval.Approver{}, fmt.Errorf("verify registration: %w", webauthnErr(err))
	}
	att := pcc.Response.AttestationObject.AuthData.AttData
	return approval.Approver{
		Name:         e.name,
		CredentialID: b64(att.CredentialID),
		PublicKey:    b64(att.CredentialPublicKey),
	}, nil
}

// webauthnErr adds the detail go-webauthn keeps out of Error().
func webauthnErr(err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.DevInfo != "" {
		return fmt.Errorf("%w: %s", err, pe.DevInfo)
	}
	return err
}

// appendEnrolled adds a to the JSON array at path, in the approvers file's
// format so entries can be copied over as they are.
func appendEnrolled(path string, a approval.Approver) error {
	var all []approval.Approver
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &all); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	all = append(all, a)
	out, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".enrolled-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Enroll is `mandate approver enroll`: it asks the running approvals unit
// for a one-time enrollment link.
func Enroll(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "enroll" {
		return errors.New("usage: mandate approver enroll --name NAME [--admin-socket PATH]")
	}
	flags := newFlagSet("approver enroll")
	name := flags.String("name", "", "a name for the passkey, e.g. phone (required)")
	sock := flags.String("admin-socket", defaultAdminSocket, "the approvals unit's admin socket")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *name == "" || flags.NArg() > 0 {
		return errors.New("usage: mandate approver enroll --name NAME [--admin-socket PATH]")
	}
	var resp enrollResponse
	if err := connector.Dial(*sock, 10*time.Second).Do(ctx, http.MethodPost, "/enroll", enrollRequest{Name: *name}, &resp); err != nil {
		return fmt.Errorf("request enrollment link: %w", err)
	}
	fmt.Printf("Open this link on the device that holds the passkey (valid until %s, usable once):\n\n  %s\n\n",
		resp.ExpiresAt.Local().Format("15:04"), resp.URL)
	fmt.Println("The page shows an approver entry; add it to the approvers in your NixOS config to trust it.")
	return nil
}
