package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

const (
	sessionCookie = "__Host-mandate"
	sessionTTL    = 12 * time.Hour
	loginTTL      = 5 * time.Minute
)

type session struct {
	hash     []byte
	approver approval.Approver
}

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// A session ends early once its passkey is no longer pinned.
func (s *Server) session(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return session{}, false
	}
	hash := tokenHash(c.Value)
	id, err := s.db.session(r.Context(), hash, s.now())
	if err != nil {
		if !errors.Is(err, errNotFound) {
			s.cfg.Log.Error("load session", "err", err)
		}
		return session{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return session{}, false
	}
	a, _, ok := s.findApprover(raw)
	if !ok {
		return session{}, false
	}
	return session{hash: hash, approver: a}, true
}

func (s *Server) page(h func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.session(r)
		if !ok {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		h(w, r, sess)
	}
}

func (s *Server) api(h func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.session(r)
		if !ok {
			connector.WriteError(w, &connector.Error{Status: http.StatusUnauthorized, Message: "log in with your passkey first"})
			return
		}
		h(w, r, sess)
	}
}

func (s *Server) findApprover(rawID []byte) (approval.Approver, []byte, bool) {
	for _, a := range s.cfg.Approvers {
		id, err := base64.RawURLEncoding.DecodeString(a.CredentialID)
		if err != nil || len(rawID) == 0 || subtle.ConstantTimeCompare(id, rawID) != 1 {
			continue
		}
		key, err := base64.RawURLEncoding.DecodeString(a.PublicKey)
		if err != nil {
			return approval.Approver{}, nil, false
		}
		return a, key, true
	}
	return approval.Approver{}, nil, false
}

// loginChallenges issues login challenges without storing them: each is a
// timestamp and random bytes under an HMAC. Only challenges that came back
// with a valid signature are remembered, so unauthenticated callers can't
// grow the state or crowd out the owner's login.
type loginChallenges struct {
	key []byte

	mu    sync.Mutex
	spent map[string]time.Time
}

const loginChallengeSize = 8 + 16 + sha256.Size

func newLoginChallenges() *loginChallenges {
	key := make([]byte, 32)
	rand.Read(key)
	return &loginChallenges{key: key, spent: map[string]time.Time{}}
}

func (l *loginChallenges) issue(now time.Time) []byte {
	c := make([]byte, 24, loginChallengeSize)
	binary.BigEndian.PutUint64(c, uint64(now.UnixMilli()))
	rand.Read(c[8:24])
	m := hmac.New(sha256.New, l.key)
	m.Write(c)
	return m.Sum(c)
}

func (l *loginChallenges) check(c []byte, now time.Time) error {
	if len(c) != loginChallengeSize {
		return errors.New("not a login challenge")
	}
	m := hmac.New(sha256.New, l.key)
	m.Write(c[:24])
	if !hmac.Equal(m.Sum(nil), c[24:]) {
		return errors.New("not a login challenge")
	}
	issued := time.UnixMilli(int64(binary.BigEndian.Uint64(c)))
	if issued.After(now.Add(time.Minute)) || now.Sub(issued) > loginTTL {
		return errors.New("login challenge expired")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.spent[string(c)]; ok {
		return errors.New("login challenge already used")
	}
	return nil
}

func (l *loginChallenges) spend(c []byte, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, at := range l.spent {
		if now.Sub(at) > loginTTL+time.Minute {
			delete(l.spent, k)
		}
	}
	if _, ok := l.spent[string(c)]; ok {
		return errors.New("login challenge already used")
	}
	l.spent[string(c)] = now
	return nil
}

func (s *Server) loginOptions(w http.ResponseWriter, _ *http.Request) {
	if len(s.cfg.Approvers) == 0 {
		connector.WriteError(w, &connector.Error{Status: http.StatusServiceUnavailable, Message: "no passkeys are pinned yet"})
		return
	}
	connector.WriteJSON(w, http.StatusOK, s.requestOptions(s.logins.issue(s.now())))
}

type loginRequest struct {
	Assertion json.RawMessage `json:"assertion"`
	Next      string          `json:"next"`
}

type loginResponse struct {
	Next string `json:"next"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	a, err := s.verifyLogin(req.Assertion)
	if err != nil {
		s.cfg.Log.Warn("login refused", "err", err)
		connector.WriteError(w, &connector.Error{Status: http.StatusForbidden, Message: err.Error()})
		return
	}
	token := rand.Text() + rand.Text()
	now := s.now()
	if err := s.db.createSession(r.Context(), tokenHash(token), a.CredentialID, now, now.Add(sessionTTL)); err != nil {
		connector.WriteError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	s.cfg.Log.Info("logged in", "passkey", a.Name)
	connector.WriteJSON(w, http.StatusOK, loginResponse{Next: localPath(req.Next)})
}

func (s *Server) verifyLogin(assertion json.RawMessage) (approval.Approver, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertion)
	if err != nil {
		return approval.Approver{}, fmt.Errorf("parse assertion: %w", webauthnErr(err))
	}
	a, key, ok := s.findApprover(parsed.RawID)
	if !ok {
		return approval.Approver{}, approval.ErrUnknown
	}
	challenge := parsed.Response.CollectedClientData.Challenge
	c, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil {
		return approval.Approver{}, errors.New("not a login challenge")
	}
	now := s.now()
	if err := s.logins.check(c, now); err != nil {
		return approval.Approver{}, err
	}
	err = parsed.Verify(challenge, s.cfg.RPID, "", []string{s.cfg.Origin}, nil, nil,
		protocol.TopOriginExplicitVerificationMode, false, true, true, key, protocol.SignaturePolicy{})
	if err != nil {
		return approval.Approver{}, fmt.Errorf("%w (%s): %w", approval.ErrSignature, a.Name, webauthnErr(err))
	}
	return a, s.logins.spend(c, now)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sess session) {
	if err := s.db.deleteSession(r.Context(), sess.hash); err != nil {
		s.cfg.Log.Error("delete session", "err", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// localPath returns next if it is a path on this origin, else "/". It guards
// the post-login redirect against pointing elsewhere.
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, `\`) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || strings.HasPrefix(u.Path, "/login") {
		return "/"
	}
	return next
}

// webauthnErr adds the detail go-webauthn keeps out of Error().
func webauthnErr(err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.DevInfo != "" {
		return fmt.Errorf("%w: %s", err, pe.DevInfo)
	}
	return err
}
