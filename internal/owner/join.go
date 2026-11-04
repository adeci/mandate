package owner

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/adeci/mandate/internal/exchange"
	"github.com/adeci/mandate/record"
)

const (
	joinTTL      = 10 * time.Minute
	joinAttempts = 3
	joinsPerHour = 5
	// attemptTTL frees an exchange from an attempt nobody answers.
	attemptTTL = time.Minute
)

// nothingOpen answers a join that no exchange waits for.
var nothingOpen = map[string]string{
	exchange.Agent:  "nothing is waiting for a login; open Add an agent in the mandate app",
	exchange.Device: "nothing is waiting for a device; open Add a device in the mandate app",
}

// joining is a code exchange the owner opened: open until a new party
// confirms the code, then confirmed until the owner admits it (approved),
// or ended. The code lives only here, in memory.
type joining struct {
	id        string
	kind      string
	code      string
	startedAt time.Time

	attempt *attempt // the one running, if any
	wrong   int
	ended   bool // cancelled, replaced or out of attempts

	// Set once a new party confirmed the code.
	name, address string
	payload       *exchange.Payload
	approved      bool
}

// attempt is one try at the code by a new party.
type attempt struct {
	*exchange.Server
	name, address string
	startedAt     time.Time
}

func (j *joining) expiresAt() time.Time { return j.startedAt.Add(joinTTL) }

func (j *joining) state(now time.Time) string {
	switch {
	case j.approved:
		return "approved"
	case j.ended || !now.Before(j.expiresAt()):
		return "ended"
	case j.payload != nil:
		return "confirmed"
	}
	return "open"
}

// joinEvent tells the app that j changed state. The caller holds s.mu.
func (s *Server) joinEvent(j *joining) {
	s.publish("exchange", map[string]string{"id": j.id, "kind": j.kind, "state": j.state(s.now())})
}

// endJoin ends j and wakes the new party's poll. The caller holds s.mu.
func (s *Server) endJoin(j *joining) {
	j.ended, j.attempt = true, nil
	s.joinEvent(j)
	s.changed()
}

// liveJoin is the exchange of kind still open or confirmed, of which there
// is at most one. The caller holds s.mu.
func (s *Server) liveJoin(kind string, now time.Time) *joining {
	for _, j := range s.joins {
		if st := j.state(now); j.kind == kind && (st == "open" || st == "confirmed") {
			return j
		}
	}
	return nil
}

// expireJoins ends exchanges out of time and forgets those long over. The
// caller holds s.mu.
func (s *Server) expireJoins(now time.Time) {
	for id, j := range s.joins {
		switch {
		case now.Sub(j.startedAt) > 2*joinTTL:
			delete(s.joins, id)
		case !j.ended && !j.approved && !now.Before(j.expiresAt()):
			s.endJoin(j)
		}
	}
	for addr, starts := range s.joinStarts {
		starts = slices.DeleteFunc(starts, func(t time.Time) bool { return now.Sub(t) >= time.Hour })
		if len(starts) == 0 {
			delete(s.joinStarts, addr)
		} else {
			s.joinStarts[addr] = starts
		}
	}
}

// clientAddress is the address front saw, passed on by the gateway for
// agents.
func clientAddress(r *http.Request) string {
	addr, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
	return strings.TrimSpace(addr)
}

// openJoin opens an exchange for the owner, ending the earlier one of its
// kind.
func (s *Server) openJoin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Kind != exchange.Agent && body.Kind != exchange.Device {
		fail(w, http.StatusBadRequest, "kind must be agent or device")
		return
	}
	id := make([]byte, 16)
	rand.Read(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expireJoins(now)
	if old := s.liveJoin(body.Kind, now); old != nil {
		s.endJoin(old)
	}
	j := &joining{id: hex.EncodeToString(id), kind: body.Kind, code: exchange.NewCode(), startedAt: now}
	s.joins[j.id] = j
	s.joinEvent(j)
	reply(w, http.StatusCreated, map[string]any{"id": j.id, "code": j.code, "expires_at": j.expiresAt()})
}

// joinStart starts a new party's attempt at the code of the open exchange.
func (s *Server) joinStart(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		name := strings.TrimSpace(body.Name)
		if body.Kind != kind || name == "" || len(name) > 100 {
			fail(w, http.StatusBadRequest, "kind must be "+kind+" and name 1 to 100 bytes")
			return
		}
		addr := clientAddress(r)
		s.mu.Lock()
		defer s.mu.Unlock()
		now := s.now()
		s.expireJoins(now)
		j := s.liveJoin(kind, now)
		switch {
		case j == nil || j.payload != nil:
			fail(w, http.StatusNotFound, nothingOpen[kind])
			return
		case len(s.joinStarts[addr]) >= joinsPerHour:
			fail(w, http.StatusTooManyRequests, "too many tries from this address; try again later")
			return
		case j.attempt != nil && now.Sub(j.attempt.startedAt) < attemptTTL:
			fail(w, http.StatusConflict, "another try at the code is running; try again in a minute")
			return
		}
		id, _ := hex.DecodeString(j.id)
		a, err := exchange.Start(kind, id, j.code)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		j.attempt = &attempt{a, name, addr, now}
		s.joinStarts[addr] = append(s.joinStarts[addr], now)
		reply(w, http.StatusCreated, map[string]string{"id": j.id, "msg": hex.EncodeToString(a.Msg())})
	}
}

// An empty kind matches either. The caller holds s.mu.
func (s *Server) join(r *http.Request, kind string) *joining {
	j := s.joins[r.PathValue("id")]
	if j == nil || kind != "" && j.kind != kind {
		return nil
	}
	return j
}

// joinPoll waits until the exchange is approved or ended.
func (s *Server) joinPoll(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wait, ok := waitParam(r)
		if !ok {
			fail(w, http.StatusBadRequest, "wait is a number of seconds")
			return
		}
		var state string
		s.await(r.Context(), wait, func() bool {
			state = ""
			if j := s.join(r, kind); j != nil {
				state = j.state(s.now())
			}
			return state != "open" && state != "confirmed"
		})
		if state == "" {
			fail(w, http.StatusNotFound, "no such exchange")
			return
		}
		reply(w, http.StatusOK, map[string]string{"status": state})
	}
}

func (s *Server) joinAnswer(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Msg     string `json:"msg"`
			Confirm string `json:"confirm"`
			Sealed  string `json:"sealed"`
		}
		if !decode(w, r, &body) {
			return
		}
		msg, err1 := hex.DecodeString(body.Msg)
		confirm, err2 := hex.DecodeString(body.Confirm)
		sealed, err3 := hex.DecodeString(body.Sealed)
		spki, err4 := hex.DecodeString(r.Header.Get("Mandate-Spki-Sha256"))
		if err := errors.Join(err1, err2, err3); err != nil {
			fail(w, http.StatusBadRequest, "msg, confirm and sealed are hex")
			return
		}
		if err4 != nil || len(spki) != 32 {
			fail(w, http.StatusBadRequest, "no TLS key fingerprint from front")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		j := s.join(r, kind)
		if j == nil {
			fail(w, http.StatusNotFound, "no such exchange")
			return
		}
		switch j.state(s.now()) {
		case "ended":
			fail(w, http.StatusGone, "the exchange ended")
			return
		case "confirmed", "approved":
			fail(w, http.StatusConflict, "the code was confirmed already")
			return
		}
		a := j.attempt
		if a == nil {
			fail(w, http.StatusConflict, "no try at the code is running; start one with POST join")
			return
		}
		j.attempt = nil
		p, serverConfirm, err := a.Finish(msg, confirm, sealed, spki)
		if err != nil {
			j.wrong++
			left := joinAttempts - j.wrong
			if left == 0 {
				s.endJoin(j)
			}
			if errors.Is(err, exchange.ErrConfirm) {
				reply(w, http.StatusForbidden, apiError{Error: "wrong code", Code: codes[http.StatusForbidden], AttemptsLeft: &left})
			} else {
				fail(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		j.name, j.address, j.payload = a.name, a.address, &p
		s.joinEvent(j)
		reply(w, http.StatusOK, map[string]string{"confirm": hex.EncodeToString(serverConfirm)})
	}
}

func (s *Server) listJoins(w http.ResponseWriter, _ *http.Request) {
	type view struct {
		ID        string            `json:"id"`
		Kind      string            `json:"kind"`
		State     string            `json:"state"`
		Name      string            `json:"name,omitempty"`
		Address   string            `json:"address,omitempty"`
		Payload   *exchange.Payload `json:"payload,omitempty"`
		StartedAt time.Time         `json:"started_at"`
		ExpiresAt time.Time         `json:"expires_at"`
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []view{}
	for _, j := range s.joins {
		if st := j.state(s.now()); st == "open" || st == "confirmed" {
			out = append(out, view{j.id, j.kind, st, j.name, j.address, j.payload, j.startedAt, j.expiresAt()})
		}
	}
	slices.SortFunc(out, func(a, b view) int { return a.StartedAt.Compare(b.StartedAt) })
	reply(w, http.StatusOK, out)
}

func (s *Server) cancelJoin(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.join(r, "")
	switch {
	case j == nil:
		fail(w, http.StatusNotFound, "no such exchange")
		return
	case j.approved:
		fail(w, http.StatusConflict, "the exchange was approved already")
		return
	case !j.ended:
		s.endJoin(j)
	}
	reply(w, http.StatusOK, struct{}{})
}

func (s *Server) joinChallenge(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.join(r, "")
	if j == nil {
		fail(w, http.StatusNotFound, "no such exchange")
		return
	}
	switch j.state(s.now()) {
	case "approved":
		fail(w, http.StatusConflict, "the exchange was approved already")
		return
	case "ended":
		fail(w, http.StatusGone, "the exchange ended")
		return
	case "open":
		fail(w, http.StatusConflict, "nobody has confirmed the code yet")
		return
	}
	token := j.payload.TokenSHA256
	if s.tokenTaken(token) {
		fail(w, http.StatusConflict, "the token is in use")
		return
	}
	holds := func() bool { return j.state(s.now()) == "confirmed" && !s.tokenTaken(token) }
	if j.kind == exchange.Agent {
		ident := record.Identity{ID: record.NewID(), TokenSHA256: token, CreatedAt: rtime(s.now())}
		s.issue(w, ident, &challenge{holds: holds, apply: func(env record.Envelope, _ record.Record) error {
			s.st.Identities[ident.ID] = &identity{Envelope: env, Record: ident, Name: j.name}
			j.approved = true
			s.joinEvent(j)
			if err := s.commit("identities"); err != nil {
				return err
			}
			s.note(activity{Kind: "agent.admitted", Identity: ident.ID, Detail: j.name})
			return nil
		}})
		return
	}
	k := *j.payload.Key
	s.keyChallenge(w, "add", k, holds, func() error {
		s.st.Devices[k.ID] = token
		j.approved = true
		s.joinEvent(j)
		if err := s.commit("keys"); err != nil {
			return err
		}
		s.note(activity{Kind: "device.joined", Detail: k.Name})
		return nil
	})
}

// The caller holds s.mu.
func (s *Server) tokenTaken(sum string) bool {
	for _, t := range s.st.Devices {
		if t == sum {
			return true
		}
	}
	for _, i := range s.st.Identities {
		if i.Record.TokenSHA256 == sum {
			return true
		}
	}
	return false
}
