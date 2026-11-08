package owner

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
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
	s.mu.Lock()
	by, _ := s.callerKey(r)
	s.note(activity{Kind: "connector.unlinked", Connector: r.PathValue("name")}, by)
	s.mu.Unlock()
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
	v := r.URL.Query()
	q := connector.ContainerQuery{Q: v.Get("q"), Limit: 100}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 500 {
			fail(w, http.StatusBadRequest, "limit is 1 to 500")
			return
		}
		q.Limit = n
	}
	if v.Has("parent") {
		p := v.Get("parent")
		q.Parent = &p
	}
	if ids := v.Get("ids"); ids != "" {
		q.IDs = strings.Split(ids, ",")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	chats, err := c.Containers(ctx, q)
	if err != nil {
		failErr(w, connectorError(err))
		return
	}
	if chats == nil {
		chats = []connector.Container{}
	}
	s.markHidden(r.PathValue("name"), chats)
	reply(w, http.StatusOK, chats)
}

// markHidden marks the chats never-share hides: one named, one in a chat
// named, and a DM whose person is named. An entry from before Discord
// stored a guild as guild:ID names it by its bare ID, as the gateway reads
// it.
func (s *Server) markHidden(conn string, chats []connector.Container) {
	hidden := map[string]bool{}
	people := map[string]bool{}
	s.mu.Lock()
	for _, n := range s.st.NeverShare {
		switch {
		case n.Connector != conn:
		case n.Kind == "chat":
			hidden[n.ID], hidden["guild:"+n.ID] = true, true
		case n.Kind == "person":
			people[n.ID] = true
		}
	}
	s.mu.Unlock()
	for i := range chats {
		c := &chats[i]
		c.Hidden = hidden[c.ID] ||
			slices.ContainsFunc(c.Parents, func(p connector.Parent) bool { return hidden[p.ID] }) ||
			c.Kind == "dm" && slices.ContainsFunc(c.Participants, func(p store.Participant) bool { return people[p.ID] })
	}
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
	// A chat picked inside its parent may come named by its own name only;
	// it is listed by its path.
	if r.Method == http.MethodPut && n.Kind == "chat" {
		if pl := s.place(r.Context(), chatRefs{n.Connector: {n.ID}})[[2]string{n.Connector, n.ID}]; pl.path != "" {
			n.Name = pl.path
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	had := false
	s.st.NeverShare = slices.DeleteFunc(s.st.NeverShare, func(o neverShare) bool {
		match := o.Connector == n.Connector && o.Kind == n.Kind && o.ID == n.ID
		if match && n.Name == "" {
			n.Name = o.Name
		}
		had = had || match
		return match
	})
	if r.Method == http.MethodPut {
		s.st.NeverShare = append(s.st.NeverShare, n)
	}
	if err := s.commit(); err != nil {
		failErr(w, err)
		return
	}
	// What is hidden: a chat, or a person by name.
	a := activity{Kind: "never_share.added", Connector: n.Connector, Detail: n.Kind + " " + cmp.Or(n.Name, n.ID)}
	if n.Kind == "chat" {
		a.Chats = []chat{{ID: n.ID, Name: n.Name}}
	}
	if r.Method == http.MethodDelete {
		a.Kind = "never_share.removed"
	}
	if r.Method == http.MethodPut || had {
		by, _ := s.callerKey(r)
		s.note(a, by)
	}
	reply(w, http.StatusOK, struct{}{})
}
