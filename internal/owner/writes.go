package owner

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/record"
)

type newProposal struct {
	Identity  string             `json:"identity"`
	Connector string             `json:"connector"`
	Op        string             `json:"op"`
	Prepared  connector.Prepared `json:"prepared"`
	Note      string             `json:"note"`
}

func (s *Server) postProposal(w http.ResponseWriter, r *http.Request) {
	var np newProposal
	if !decode(w, r, &np) {
		return
	}
	if np.Op == "" || np.Prepared.Args == nil {
		fail(w, http.StatusBadRequest, "op and prepared args are required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Identities[np.Identity] == nil || s.conns[np.Connector] == nil {
		fail(w, http.StatusNotFound, "unknown identity or connector")
		return
	}
	now := s.now().UTC().Truncate(time.Second)
	p := &proposal{
		ID: record.NewID(), Identity: np.Identity, Connector: np.Connector, Op: np.Op,
		Prepared: np.Prepared, Note: np.Note, CreatedAt: now, ExpiresAt: now.Add(proposalTTL),
		Status: "pending",
	}
	s.st.Proposals[p.ID] = p
	s.seen(np.Identity, now)
	if err := s.commit("now"); err != nil {
		failErr(w, err)
		return
	}
	s.publish("waiting", map[string]string{"kind": "proposal", "id": p.ID, "identity": p.Identity, "connector": p.Connector})
	reply(w, http.StatusCreated, map[string]string{"id": p.ID})
}

func (s *Server) proposalStatus(w http.ResponseWriter, r *http.Request) {
	wait, ok := waitParam(r)
	if !ok {
		fail(w, http.StatusBadRequest, "wait is a number of seconds")
		return
	}
	id, ident := r.PathValue("id"), r.URL.Query().Get("identity")
	var view map[string]any
	s.await(r.Context(), wait, func() bool {
		p := s.st.Proposals[id]
		if p == nil || p.Identity != ident {
			view = nil
			return true
		}
		view = map[string]any{"id": p.ID, "status": p.Status}
		if p.Result != nil {
			view["result"] = struct {
				Ref    string `json:"ref,omitempty"`
				Detail string `json:"detail,omitempty"`
				Output string `json:"output,omitempty"`
			}{p.Result.Ref, p.Result.Detail, p.Result.Output}
		}
		return p.Status != "pending" && p.Status != "executing"
	})
	if view == nil {
		fail(w, http.StatusNotFound, "no such proposal")
		return
	}
	reply(w, http.StatusOK, view)
}

// proposalView is a proposal for the app: what the owner signs, which
// fields can be edited, and the agent's note.
type proposalView struct {
	ID           string         `json:"id"`
	Identity     string         `json:"identity"`
	IdentityName string         `json:"identity_name"`
	Connector    string         `json:"connector"`
	Op           string         `json:"op"`
	Display      []record.Field `json:"display"`
	Editable     []string       `json:"editable"`
	Note         string         `json:"note,omitempty"`
	Status       string         `json:"status"`
	CreatedAt    time.Time      `json:"created_at"`
	ExpiresAt    time.Time      `json:"expires_at"`
}

func (s *Server) proposalView(p *proposal) proposalView {
	v := proposalView{
		ID: p.ID, Identity: p.Identity, Connector: p.Connector, Op: p.Op,
		Display: p.Prepared.Display, Editable: editable(p.Prepared), Note: p.Note,
		Status: p.Status, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
	}
	if i := s.st.Identities[p.Identity]; i != nil {
		v.IdentityName = i.Name
	}
	return v
}

// editable lists the params the display marks as the owner's to change.
func editable(p connector.Prepared) []string {
	out := []string{}
	for _, f := range p.Display {
		if f.Param != "" {
			out = append(out, f.Param)
		}
	}
	return out
}

func (s *Server) getProposal(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Proposals[r.PathValue("id")]
	if p == nil {
		fail(w, http.StatusNotFound, "no such proposal")
		return
	}
	reply(w, http.StatusOK, s.proposalView(p))
}

// pendingProposal is the proposal the owner can still decide, or an error.
// The caller holds s.mu.
func (s *Server) pendingProposal(id string) (*proposal, error) {
	p := s.st.Proposals[id]
	switch {
	case p == nil:
		return nil, errorf(http.StatusNotFound, "no such proposal")
	case p.Status == "expired" || p.Status == "pending" && !s.now().Before(p.ExpiresAt):
		return nil, errorf(http.StatusGone, "the proposal expired")
	case p.Status != "pending":
		return nil, errorf(http.StatusConflict, "the proposal was decided meanwhile")
	}
	return p, nil
}

func (s *Server) proposalChallenge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Edits map[string]string `json:"edits"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	p, err := s.pendingProposal(id)
	if err != nil {
		s.mu.Unlock()
		failErr(w, err)
		return
	}
	prepared, conn, op := p.Prepared, s.conns[p.Connector], p.Op
	s.mu.Unlock()

	if len(body.Edits) > 0 {
		args := maps.Clone(prepared.Args)
		ok := editable(prepared)
		for k, v := range body.Edits {
			if !slices.Contains(ok, k) {
				fail(w, http.StatusBadRequest, k+" can't be edited")
				return
			}
			args[k] = v
		}
		// The connector renders the edited write again, as the owner caller.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		again, err := conn.Prepare(ctx, op, args)
		cancel()
		if err != nil {
			failErr(w, connectorError(err))
			return
		}
		if again.Container != prepared.Container || again.Message != prepared.Message {
			fail(w, http.StatusBadRequest, "the edit changes the chat or the message answered")
			return
		}
		prepared = again
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p, err = s.pendingProposal(id); err != nil {
		failErr(w, err)
		return
	}
	wr := record.Write{
		ID: p.ID, Connector: p.Connector, Op: p.Op, Args: prepared.Args, Display: prepared.Display,
		Note: p.Note, CreatedAt: rtime(p.CreatedAt), ExpiresAt: rtime(p.ExpiresAt),
	}
	s.issue(w, wr, &challenge{
		identity: p.Identity,
		holds:    func() bool { return s.st.Proposals[id] == p && p.Status == "pending" },
		apply: func(env record.Envelope, _ record.Record) error {
			p.Status, p.DecidedAt, p.Prepared, p.Approval = "executing", s.now(), prepared, &env
			// Saved before running, so a crash leaves it unknown rather than
			// running it twice.
			if err := s.commit("now"); err != nil {
				return err
			}
			s.executing.Add(1)
			go s.execute(p.ID, p.Connector, env)
			return nil
		},
	})
}

// execute runs an approved write once and keeps the result.
func (s *Server) execute(id, conn string, env record.Envelope) {
	defer s.executing.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	// The connector must trust a key added moments ago.
	err := s.pushKeys(ctx, conn)
	var res connector.Result
	if err == nil {
		res, err = s.conns[conn].Execute(ctx, env)
	}
	cancel()
	if err != nil {
		res = connector.Result{Status: connector.Unknown, Detail: err.Error()}
		// A 4xx refusal comes before the connector runs anything.
		if e, ok := errors.AsType[*connector.Error](err); ok && e.Status/100 == 4 {
			res = connector.Result{Status: connector.Failed, Detail: e.Message}
		}
		s.log.Warn("execute", "proposal", id, "connector", conn, "err", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Proposals[id]
	if p == nil {
		return
	}
	p.Status, p.Result, p.DecidedAt = string(res.Status), &res, s.now()
	if err := s.commit("now"); err != nil {
		s.log.Error("keep write result", "proposal", id, "result", res.Status)
	}
}

func (s *Server) denyProposal(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.pendingProposal(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	p.Status, p.DecidedAt = "rejected", s.now()
	if err := s.commit("now"); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct{}{})
}
