package console

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/adeci/mandate/connector"
)

const (
	maxChatResults = 20
	// chatDepth covers guild, channel and thread, as deep as any connector
	// nests.
	chatDepth   = 3
	maxChildren = 500
	// treeNodes caps the chats one access request or picked chat shows,
	// itself and those under it, so a request naming large servers stays
	// cheap to show.
	treeNodes = 50
)

// chatNode is a chat as the owner picks it: labelled with its stored name
// and ID, and with its current descendants, each picked on its own.
type chatNode struct {
	ID string `json:"id"`
	// Label is the stored name, "~" included, and the ID, escaped with
	// hidden characters marked.
	Label    template.HTML `json:"label"`
	Kind     string        `json:"kind,omitempty"`
	Active   string        `json:"active,omitempty"`
	Children []chatNode    `json:"children,omitempty"`
	// More counts the chats under it left out, past treeNodes.
	More int `json:"more,omitempty"`
}

func chatLabel(name, id string) template.HTML {
	return visible(cmpOr(name, id)) + " (" + visible(id) + ")"
}

func chatNodeOf(c connector.Container) chatNode {
	n := chatNode{ID: c.ID, Label: chatLabel(c.Name, c.ID), Kind: c.Kind}
	if !c.LastActivity.IsZero() {
		n.Active = c.LastActivity.Local().Format(stamp)
	}
	return n
}

func (s *Server) conn(name string) (*conn, error) {
	c, ok := s.conns[name]
	if !ok {
		return nil, &connector.Error{Status: http.StatusNotFound, Message: fmt.Sprintf("no connector %q", name)}
	}
	return c, nil
}

// listChats asks a connector for its stored chats.
func (s *Server) listChats(ctx context.Context, name, q, parent string, limit int) ([]connector.Container, error) {
	c, err := s.conn(name)
	if err != nil {
		return nil, err
	}
	cs, err := c.client.Containers(ctx, q, parent, limit)
	if err != nil {
		return nil, upstream(err)
	}
	return cs, nil
}

// forwardOnly reports whether connector name forwards raw reads, so it
// keeps no chats and its grants can't name chats or a horizon.
func (s *Server) forwardOnly(ctx context.Context, name string) (bool, error) {
	c, err := s.conn(name)
	if err != nil {
		return false, err
	}
	spec, err := c.getSpec(ctx, s.now())
	if err != nil {
		return false, upstream(err)
	}
	return spec.Forward, nil
}

// chat looks up one chat by ID.
func (s *Server) chat(ctx context.Context, name, id string) (connector.Container, bool) {
	cs, err := s.listChats(ctx, name, id, "", maxChatResults)
	if err != nil {
		s.cfg.Log.Warn("look up chat", "connector", name, "err", err)
	}
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return connector.Container{}, false
}

// children are the chats under parent, depth levels down, at most budget
// of them; more counts those under parent left out.
func (s *Server) children(ctx context.Context, name, parent string, depth int, budget *int) (nodes []chatNode, more int, err error) {
	if depth == 0 {
		return nil, 0, nil
	}
	cs, err := s.listChats(ctx, name, "", parent, maxChildren)
	if err != nil {
		return nil, 0, err
	}
	for _, c := range cs {
		if *budget == 0 {
			more++
			continue
		}
		*budget--
		n := chatNodeOf(c)
		if n.Children, n.More, err = s.children(ctx, name, c.ID, depth-1, budget); err != nil {
			return nil, 0, err
		}
		nodes = append(nodes, n)
	}
	return nodes, more, nil
}

// chatTree is chat id with the chats under it, at most budget of them;
// known is its name if the connector no longer has it.
func (s *Server) chatTree(ctx context.Context, name, id, known string, budget *int) (chatNode, error) {
	n := chatNode{ID: id, Label: chatLabel(known, id)}
	if c, ok := s.chat(ctx, name, id); ok {
		n = chatNodeOf(c)
	}
	var err error
	n.Children, n.More, err = s.children(ctx, name, id, chatDepth-1, budget)
	return n, err
}

// requestTree is what the owner may grant for a request that names chats:
// those chats and, within treeNodes, the chats under them now.
func (s *Server) requestTree(ctx context.Context, ar accessRequest) ([]chatNode, error) {
	budget := max(treeNodes-len(ar.Chats), 0)
	out := make([]chatNode, len(ar.Chats))
	for i, c := range ar.Chats {
		var err error
		if out[i], err = s.chatTree(ctx, ar.Connector, c.ID, c.Name, &budget); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// chatLabels labels ids, preferring known names and asking the connector
// for the rest.
func (s *Server) chatLabels(ctx context.Context, name string, ids []string, known map[string]string) template.HTML {
	labels := make([]string, len(ids))
	for i, id := range ids {
		n, ok := known[id]
		if !ok {
			c, _ := s.chat(ctx, name, id)
			n = c.Name
		}
		labels[i] = string(chatLabel(n, id))
	}
	return template.HTML(strings.Join(labels, ", "))
}

type chatsResponse struct {
	Chats []chatNode `json:"chats"`
}

// searchChats serves the chat picker's search.
func (s *Server) searchChats(w http.ResponseWriter, r *http.Request, _ session) {
	q := r.URL.Query()
	name := q.Get("connector")
	fo, err := s.forwardOnly(r.Context(), name)
	if err == nil && fo {
		err = connector.Invalid("%s keeps no chats", name)
	}
	var cs []connector.Container
	if err == nil {
		cs, err = s.listChats(r.Context(), name, strings.TrimSpace(q.Get("q")), "", maxChatResults)
	}
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	out := chatsResponse{Chats: make([]chatNode, len(cs))}
	for i, c := range cs {
		out.Chats[i] = chatNodeOf(c)
	}
	connector.WriteJSON(w, http.StatusOK, out)
}

// chatTreeHandler serves a picked chat with the chats under it.
func (s *Server) chatTreeHandler(w http.ResponseWriter, r *http.Request, _ session) {
	q := r.URL.Query()
	budget := treeNodes - 1
	n, err := s.chatTree(r.Context(), q.Get("connector"), q.Get("id"), "", &budget)
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	connector.WriteJSON(w, http.StatusOK, n)
}
