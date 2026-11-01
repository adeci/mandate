package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
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
	"golang.org/x/crypto/bcrypt"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

const (
	sessionCookie = "__Host-mandate"
	sessionTTL    = 12 * time.Hour
	loginTTL      = 5 * time.Minute
)

// session is a signed-in browser; key is zero for a password session.
type session struct {
	hash []byte
	key  approval.Key
	// version is the console's state as the page began loading.
	version string
}

func (s session) who() string {
	if s.key.Name == "" {
		return "password"
	}
	return s.key.Name
}

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// A passkey session ends once its key is no longer trusted.
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
	if id == "" {
		return session{hash: hash}, true
	}
	k, ok := s.keys.Find(id)
	if !ok {
		return session{}, false
	}
	return session{hash: hash, key: k}, true
}

func (s *Server) page(h func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.session(r)
		if !ok {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		sess.version = s.version(r.Context())
		h(w, r, sess)
	}
}

func (s *Server) api(h func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.session(r)
		if !ok {
			connector.WriteError(w, &connector.Error{Status: http.StatusUnauthorized, Message: "sign in first"})
			return
		}
		h(w, r, sess)
	}
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
	if len(s.keys.Trusted()) == 0 {
		connector.WriteError(w, &connector.Error{Status: http.StatusServiceUnavailable, Message: "no passkeys yet; sign in with the password"})
		return
	}
	connector.WriteJSON(w, http.StatusOK, s.requestOptions(s.logins.issue(s.now())))
}

type loginRequest struct {
	Assertion json.RawMessage `json:"assertion,omitempty"`
	// Unlock is the window the assertion also opens, covering the login
	// challenge.
	Unlock   *delegation `json:"unlock,omitempty"`
	Password string      `json:"password,omitempty"`
	Next     string      `json:"next"`
}

type loginResponse struct {
	Next string `json:"next"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	var (
		k   approval.Key
		err error
	)
	switch {
	case req.Unlock != nil:
		k, err = s.unlockLogin(req.Assertion, *req.Unlock)
	case req.Assertion != nil:
		k, err = s.verifyLogin(req.Assertion)
	default:
		err = s.checkPassword(req.Password)
	}
	if err != nil {
		s.cfg.Log.Warn("login refused", "err", err)
		connector.WriteError(w, err)
		return
	}
	token := rand.Text() + rand.Text()
	now := s.now()
	if err := s.db.createSession(r.Context(), tokenHash(token), k.CredentialID, now, now.Add(sessionTTL)); err != nil {
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
	s.cfg.Log.Info("logged in", "with", session{key: k}.who())
	connector.WriteJSON(w, http.StatusOK, loginResponse{Next: localPath(req.Next)})
}

func (s *Server) verifyLogin(assertion json.RawMessage) (approval.Key, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertion)
	if err != nil {
		return approval.Key{}, connector.Invalid("parse assertion: %v", webauthnErr(err))
	}
	k, ok := s.keys.Find(b64(parsed.RawID))
	if !ok {
		return approval.Key{}, forbidden(approval.ErrUnknown)
	}
	challenge := parsed.Response.CollectedClientData.Challenge
	c, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil {
		return approval.Key{}, forbidden(errors.New("not a login challenge"))
	}
	now := s.now()
	if err := s.logins.check(c, now); err != nil {
		return approval.Key{}, forbidden(err)
	}
	key, err := base64.RawURLEncoding.DecodeString(k.PublicKey)
	if err != nil {
		return approval.Key{}, err
	}
	err = parsed.Verify(challenge, s.cfg.RPID, "", []string{s.cfg.Origin}, nil, nil,
		protocol.TopOriginExplicitVerificationMode, false, true, true, key, protocol.SignaturePolicy{})
	if err != nil {
		return approval.Key{}, forbidden(fmt.Errorf("%w (%s): %w", approval.ErrSignature, k.Name, webauthnErr(err)))
	}
	if err := s.logins.spend(c, now); err != nil {
		return approval.Key{}, forbidden(err)
	}
	return k, nil
}

func (s *Server) unlockLogin(assertion json.RawMessage, unlock delegation) (approval.Key, error) {
	d, err := unlock.decode()
	if err != nil {
		return approval.Key{}, err
	}
	d.Assertion = assertion
	now := s.now()
	if err := s.logins.check(d.Covers, now); err != nil {
		return approval.Key{}, forbidden(err)
	}
	k, err := s.verifier.VerifyDelegation(d)
	if err != nil {
		return approval.Key{}, forbidden(webauthnErr(err))
	}
	if err := s.logins.spend(d.Covers, now); err != nil {
		return approval.Key{}, forbidden(err)
	}
	return k, nil
}

// Password attempts after freeTries failures in a row wait 1s, 2s, 4s...
// up to about 17 minutes. Attempts run one at a time.
const freeTries = 5

type throttle struct {
	mu       sync.Mutex
	failures int
	until    time.Time
}

func (s *Server) checkPassword(password string) error {
	t := &s.password
	t.mu.Lock()
	defer t.mu.Unlock()
	now := s.now()
	if now.Before(t.until) {
		wait := t.until.Sub(now).Round(time.Second)
		return &connector.Error{Status: http.StatusTooManyRequests, Message: fmt.Sprintf("too many attempts; try again in %s", max(wait, time.Second))}
	}
	if bcrypt.CompareHashAndPassword(s.cfg.PasswordHash, []byte(password)) != nil {
		t.failures++
		if t.failures >= freeTries {
			t.until = now.Add(time.Second << min(t.failures-freeTries, 10))
		}
		return forbidden(errors.New("wrong password"))
	}
	t.failures = 0
	return nil
}

func forbidden(err error) *connector.Error {
	return &connector.Error{Status: http.StatusForbidden, Message: err.Error()}
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
