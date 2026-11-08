package owner

import (
	"context"
	"strings"
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

// nameChats fills in the grants' missing chat names from their connectors,
// one lookup per connector. A connector that fails leaves the IDs unnamed.
// The caller must not hold s.mu.
func (s *Server) nameChats(ctx context.Context, views ...[]grantView) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	want := map[string][]string{}
	for _, vs := range views {
		for _, v := range vs {
			for _, id := range v.Chats {
				if v.ChatNames[id] == "" {
					want[v.Connector] = append(want[v.Connector], id)
				}
			}
		}
	}
	names := map[[2]string]string{}
	for conn, ids := range want {
		for id, name := range s.lookUpNames(ctx, conn, ids) {
			names[[2]string{conn, id}] = name
		}
	}
	for _, vs := range views {
		for i := range vs {
			v := &vs[i]
			for _, id := range v.Chats {
				name := names[[2]string{v.Connector, id}]
				if v.ChatNames[id] != "" || name == "" {
					continue
				}
				if v.ChatNames == nil {
					v.ChatNames = map[string]string{}
				}
				v.ChatNames[id] = name
			}
		}
	}
}

// lookUpNames names ids by their path, as request cards show them, from the
// cache or else the connector.
func (s *Server) lookUpNames(ctx context.Context, conn string, ids []string) map[string]string {
	now := time.Now()
	out := make(map[string]string, len(ids))
	var missing []string
	s.names.mu.Lock()
	for _, id := range ids {
		if c, ok := s.names.m[[2]string{conn, id}]; ok && now.Sub(c.at) < chatNameTTL {
			out[id] = c.name
		} else {
			missing = append(missing, id)
		}
	}
	s.names.mu.Unlock()
	client := s.conns[conn]
	if len(missing) == 0 || client == nil || ctx.Err() != nil {
		return out
	}
	chats, err := client.Containers(ctx, connector.ContainerQuery{IDs: missing})
	if err != nil {
		s.log.Warn("look up chat names", "connector", conn, "err", err)
		return out
	}
	found := map[string]string{}
	for _, c := range chats {
		path := make([]string, 0, len(c.Parents)+1)
		for _, p := range c.Parents {
			path = append(path, p.Name)
		}
		found[c.ID] = strings.Join(append(path, c.Name), " / ")
	}
	s.names.mu.Lock()
	if s.names.m == nil {
		s.names.m = map[[2]string]cachedName{}
	}
	for _, id := range missing {
		out[id] = found[id]
		s.names.m[[2]string{conn, id}] = cachedName{found[id], now}
	}
	s.names.mu.Unlock()
	return out
}
