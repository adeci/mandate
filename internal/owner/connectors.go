package owner

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
)

func (s *Server) listConnectors(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Name   string           `json:"name"`
		Health connector.Health `json:"health"`
		Setup  bool             `json:"setup"`
	}
	out := make([]view, 0, len(s.conns))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, c := range s.conns {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			h, err := c.Health(ctx)
			cancel()
			if err != nil {
				h = connector.Health{State: connector.Broken, Detail: "the connector does not answer"}
			}
			mu.Lock()
			out = append(out, view{name, h, h.State == connector.NeedsSetup})
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b view) int { return strings.Compare(a.Name, b.Name) })
	reply(w, http.StatusOK, out)
}

func (s *Server) conn(w http.ResponseWriter, r *http.Request) *connector.Client {
	c := s.conns[r.PathValue("name")]
	if c == nil {
		fail(w, http.StatusNotFound, "no such connector")
	}
	return c
}

// connectorSetup relays the linking dialogue; GET starts it.
func (s *Server) connectorSetup(w http.ResponseWriter, r *http.Request) {
	c := s.conn(w, r)
	if c == nil {
		return
	}
	var body struct {
		Step   string            `json:"step"`
		Values map[string]string `json:"values"`
	}
	if r.Method == http.MethodPost && !decode(w, r, &body) {
		return
	}
	if body.Values == nil {
		body.Values = map[string]string{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	step, err := c.Setup(ctx, body.Step, body.Values)
	if err != nil {
		failErr(w, connectorError(err))
		return
	}
	if step.Done {
		s.connectorsChanged()
	}
	reply(w, http.StatusOK, step)
}

func (s *Server) connectorUnlink(w http.ResponseWriter, r *http.Request) {
	c := s.conn(w, r)
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := c.Unlink(ctx); err != nil {
		failErr(w, connectorError(err))
		return
	}
	s.connectorsChanged()
	reply(w, http.StatusOK, struct{}{})
}

func (s *Server) connectorsChanged() {
	s.mu.Lock()
	s.publish("changed", map[string]string{"what": "connectors"})
	s.mu.Unlock()
}

func (s *Server) connectorChats(w http.ResponseWriter, r *http.Request) {
	c := s.conn(w, r)
	if c == nil {
		return
	}
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			fail(w, http.StatusBadRequest, "limit is 1 to 500")
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	chats, err := c.Containers(ctx, q.Get("q"), q.Get("parent"), limit)
	if err != nil {
		failErr(w, connectorError(err))
		return
	}
	if chats == nil {
		chats = []connector.Container{}
	}
	reply(w, http.StatusOK, chats)
}

func (s *Server) listNeverShare(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply(w, http.StatusOK, append([]neverShare{}, s.st.NeverShare...))
}

// PUT adds the entry named in the path, DELETE removes it.
func (s *Server) editNeverShare(w http.ResponseWriter, r *http.Request) {
	n := neverShare{Connector: r.PathValue("connector"), Kind: r.PathValue("kind"), ID: r.PathValue("id")}
	switch {
	case s.conns[n.Connector] == nil:
		fail(w, http.StatusNotFound, "no such connector")
		return
	case n.Kind != "chat" && n.Kind != "person":
		fail(w, http.StatusBadRequest, "kind is chat or person")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if r.Method == http.MethodPut && !decode(w, r, &body) {
		return
	}
	n.Name = body.Name
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.NeverShare = slices.DeleteFunc(s.st.NeverShare, func(o neverShare) bool {
		return o.Connector == n.Connector && o.Kind == n.Kind && o.ID == n.ID
	})
	if r.Method == http.MethodPut {
		s.st.NeverShare = append(s.st.NeverShare, n)
	}
	if err := s.commit(); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct{}{})
}
