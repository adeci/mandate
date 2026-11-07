package owner

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
)

// chatNameTTL is how long a name looked up from a connector is reused.
const chatNameTTL = time.Minute

// chatNames caches the names of chats the owner picked beyond a request's
// matched terms, looked up from the connector by ID.
type chatNames struct {
	mu sync.Mutex
	m  map[[2]string]cachedName // by connector, chat ID
}

type cachedName struct {
	name string // "" if the connector has no such chat
	at   time.Time
}

// nameChats fills in the grants' missing chat names from their connectors.
// A connector that fails leaves the IDs unnamed. The caller must not hold s.mu.
func (s *Server) nameChats(ctx context.Context, views ...[]grantView) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, vs := range views {
		for i := range vs {
			v := &vs[i]
			for _, id := range v.Chats {
				if v.ChatNames[id] != "" {
					continue
				}
				if name := s.chatName(ctx, v.Connector, id); name != "" {
					if v.ChatNames == nil {
						v.ChatNames = map[string]string{}
					}
					v.ChatNames[id] = name
				}
			}
		}
	}
}

func (s *Server) chatName(ctx context.Context, conn, id string) string {
	key := [2]string{conn, id}
	now := time.Now()
	s.names.mu.Lock()
	c, ok := s.names.m[key]
	s.names.mu.Unlock()
	if ok && now.Sub(c.at) < chatNameTTL {
		return c.name
	}
	client := s.conns[conn]
	if client == nil || ctx.Err() != nil {
		return ""
	}
	// q also matches names, so look for the exact ID among the matches; it
	// comes first. The name is the chat's path, as request cards show it.
	chats, err := client.Containers(ctx, id, "", 50)
	if err != nil {
		s.log.Warn("look up chat name", "connector", conn, "chat", id, "err", err)
		return c.name
	}
	name := ""
	if i := slices.IndexFunc(chats, func(c connector.Container) bool { return c.ID == id }); i >= 0 {
		name = chats[i].Name
		if p := chats[i].ParentName; p != "" {
			name = p + " / " + name
		}
	}
	s.names.mu.Lock()
	if s.names.m == nil {
		s.names.m = map[[2]string]cachedName{}
	}
	s.names.m[key] = cachedName{name, now}
	s.names.mu.Unlock()
	return name
}
