package owner

import (
	"context"
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
	joinTTL       = 10 * time.Minute
	joinAttempts  = 3
	joinsWaiting  = 3
	joinsPerHour  = 5
	codeAnswerTTL = 2 * time.Minute
)

// joining is a code exchange: a new party waiting to be let in.
type joining struct {
	id        string
	kind      string
	name      string
	address   string
	startedAt time.Time

	attempt *exchange.Server // the owner's try at the code, until answered
	tries   int              // attempts started
	answers int              // attempts answered
	wrong   int
	payload *exchange.Payload // set once the new party confirmed
	// approved is set once the owner signed the record for the payload.
	approved bool
}

func (j *joining) ended(now time.Time) bool {
	return !j.approved && (j.wrong >= joinAttempts || !now.Before(j.startedAt.Add(joinTTL)))
}

func (j *joining) waiting(now time.Time) bool { return !j.approved && !j.ended(now) }

// expireJoins forgets exchanges long over and wakes polls on ones that just
// ended. The caller holds s.mu.
func (s *Server) expireJoins(now time.Time) {
	woke := false
	for id, j := range s.joins {
		if now.Sub(j.startedAt) > 2*joinTTL {
			delete(s.joins, id)
		} else if !woke && !j.approved && now.Sub(j.startedAt) >= joinTTL {
			s.changed()
			woke = true
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
		waiting := 0
		for _, j := range s.joins {
			if j.kind == kind && j.waiting(now) {
				waiting++
			}
		}
		if waiting >= joinsWaiting || len(s.joinStarts[addr]) >= joinsPerHour {
			fail(w, http.StatusTooManyRequests, "too many code exchanges; try again later")
			return
		}
		id := make([]byte, 16)
		rand.Read(id)
		j := &joining{id: hex.EncodeToString(id), kind: kind, name: name, address: addr, startedAt: now}
		s.joins[j.id] = j
		s.joinStarts[addr] = append(s.joinStarts[addr], now)
		s.publish("exchange", map[string]string{"id": j.id, "kind": kind, "name": name})
		reply(w, http.StatusCreated, map[string]string{"id": j.id})
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

func (s *Server) joinPoll(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wait, ok := waitParam(r)
		if !ok {
			fail(w, http.StatusBadRequest, "wait is a number of seconds")
			return
		}
		var out map[string]string
		s.await(r.Context(), wait, func() bool {
			j := s.join(r, kind)
			switch {
			case j == nil:
				out = nil
				return true
			case j.approved:
				out = map[string]string{"status": "approved"}
			case j.ended(s.now()):
				out = map[string]string{"status": "ended"}
			case j.attempt != nil:
				out = map[string]string{"msg": hex.EncodeToString(j.attempt.Msg())}
			default:
				out = map[string]string{"status": "pending"}
				return false
			}
			return true
		})
		if out == nil {
			fail(w, http.StatusNotFound, "no such exchange")
			return
		}
		reply(w, http.StatusOK, out)
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
		switch {
		case j == nil:
			fail(w, http.StatusNotFound, "no such exchange")
			return
		case j.ended(s.now()):
			fail(w, http.StatusGone, "the exchange ended")
			return
		case j.attempt == nil:
			fail(w, http.StatusConflict, "the owner has not typed the code")
			return
		}
		attempt := j.attempt
		j.attempt = nil
		j.answers = j.tries
		p, serverConfirm, err := attempt.Finish(msg, confirm, sealed, spki)
		defer s.changed()
		if err != nil {
			j.wrong++
			if errors.Is(err, exchange.ErrConfirm) {
				fail(w, http.StatusForbidden, "wrong code")
			} else {
				fail(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		j.payload = &p
		reply(w, http.StatusOK, map[string]string{"confirm": hex.EncodeToString(serverConfirm)})
	}
}

func (s *Server) listJoins(w http.ResponseWriter, _ *http.Request) {
	type view struct {
		ID        string    `json:"id"`
		Kind      string    `json:"kind"`
		Name      string    `json:"name"`
		Address   string    `json:"address"`
		StartedAt time.Time `json:"started_at"`
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []view{}
	for _, j := range s.joins {
		if j.waiting(s.now()) {
			out = append(out, view{j.id, j.kind, j.name, j.address, j.startedAt})
		}
	}
	slices.SortFunc(out, func(a, b view) int { return a.StartedAt.Compare(b.StartedAt) })
	reply(w, http.StatusOK, out)
}

// joinCode starts the owner's attempt at the code and waits for the new
// party's answer.
func (s *Server) joinCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.mu.Lock()
	j := s.join(r, "")
	var err error
	switch {
	case j == nil:
		err = errorf(http.StatusNotFound, "no such exchange")
	case j.ended(s.now()):
		err = errorf(http.StatusGone, "the exchange ended")
	case j.payload != nil:
		err = errorf(http.StatusConflict, "the code was confirmed already")
	default:
		id, _ := hex.DecodeString(j.id)
		var a *exchange.Server
		if a, err = exchange.Start(j.kind, id, body.Code); err != nil {
			err = errorf(http.StatusBadRequest, "the code is 6 digits")
		} else {
			j.attempt = a
			j.tries++
			s.changed()
		}
	}
	try := 0
	if j != nil {
		try = j.tries
	}
	s.mu.Unlock()
	if err != nil {
		failErr(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), codeAnswerTTL)
	defer cancel()
	s.await(ctx, codeAnswerTTL, func() bool { return j.answers >= try || j.ended(s.now()) })
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case j.answers < try && !j.ended(s.now()):
		fail(w, http.StatusConflict, "the new party did not answer")
	case j.payload != nil:
		reply(w, http.StatusOK, map[string]any{"payload": j.payload})
	default:
		left := max(joinAttempts-j.wrong, 0)
		reply(w, http.StatusForbidden, apiError{Error: "wrong code", Code: codes[http.StatusForbidden], AttemptsLeft: &left})
	}
}

func (s *Server) joinChallenge(w http.ResponseWriter, r *http.Request) {
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
	case j.ended(s.now()):
		fail(w, http.StatusGone, "the exchange ended")
		return
	case j.payload == nil:
		fail(w, http.StatusConflict, "the code was not confirmed")
		return
	}
	token := j.payload.TokenSHA256
	if s.tokenTaken(token) {
		fail(w, http.StatusConflict, "the token is in use")
		return
	}
	holds := func() bool { return j.waiting(s.now()) && !s.tokenTaken(token) }
	if j.kind == exchange.Agent {
		ident := record.Identity{ID: record.NewID(), TokenSHA256: token, CreatedAt: rtime(s.now())}
		s.issue(w, ident, &challenge{holds: holds, apply: func(env record.Envelope, _ record.Record) error {
			s.st.Identities[ident.ID] = &identity{Envelope: env, Record: ident, Name: j.name}
			j.approved = true
			return s.commit("identities")
		}})
		return
	}
	k := *j.payload.Key
	s.keyChallenge(w, "add", k, holds, func() error {
		s.st.Devices[k.ID] = token
		j.approved = true
		return s.commit("keys")
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
