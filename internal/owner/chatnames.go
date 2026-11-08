package owner

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// chatNameTTL is how long a chat looked up from a connector is reused.
const chatNameTTL = time.Minute

// chatNames caches chats looked up from their connectors by ID.
type chatNames struct {
	mu sync.Mutex
	m  map[[2]string]cachedName // by connector, chat ID
}

type cachedName struct {
	place
	at time.Time
}

// place is where a chat sits: its path, kind, picture version and parents.
// A chat the connector no longer has has none.
type place struct {
	path    string
	kind    string
	icon    string
	parents []connector.Parent
}

// chatRefs are the chats a response names, by connector, to place in one
// lookup per connector.
type chatRefs map[string][]string

func (r chatRefs) chats(conn string, cs []chat) {
	for _, c := range cs {
		r[conn] = append(r[conn], c.ID)
	}
}

func (r chatRefs) grants(views ...[]grantView) {
	for _, vs := range views {
		for _, v := range vs {
			r[v.Connector] = append(r[v.Connector], v.Chats...)
		}
	}
}

// places are chats as their connectors placed them, by connector and ID.
type places map[[2]string]place

// chats gives cs their kinds and parents, and a name if they had none.
func (p places) chats(conn string, cs []chat) {
	for i := range cs {
		if pl, ok := p[[2]string{conn, cs[i].ID}]; ok && pl.path != "" {
			cs[i].Kind, cs[i].Icon, cs[i].Parents = pl.kind, pl.icon, pl.parents
			cs[i].Name = cmp.Or(cs[i].Name, pl.path)
		}
	}
}

// grants lists each grant's chats with their kinds and parents, named as
// the request matched them, else by their path.
func (p places) grants(views ...[]grantView) {
	for _, vs := range views {
		for i := range vs {
			v := &vs[i]
			v.ChatRefs = make([]chat, 0, len(v.Chats))
			for _, id := range v.Chats {
				pl := p[[2]string{v.Connector, id}]
				name := v.ChatNames[id]
				if name == "" && pl.path != "" {
					name = pl.path
					if v.ChatNames == nil {
						v.ChatNames = map[string]string{}
					}
					v.ChatNames[id] = name
				}
				v.ChatRefs = append(v.ChatRefs, chat{ID: id, Name: name, Kind: pl.kind, Icon: pl.icon, Parents: pl.parents})
			}
		}
	}
}

// place looks up refs, from the cache or else their connectors, one call
// per connector. A connector that fails leaves its chats unplaced. The
// caller must not hold s.mu.
func (s *Server) place(ctx context.Context, refs chatRefs) places {
	ctx, cancel := context.WithTimeout(ctx, lookUpTimeout)
	defer cancel()
	out := places{}
	for conn, ids := range refs {
		found, err := s.lookUp(ctx, conn, ids)
		if err != nil {
			s.log.Warn("look up chats", "connector", conn, "err", err)
		}
		for id, pl := range found {
			out[[2]string{conn, id}] = pl
		}
	}
	return out
}

// lookUpTimeout bounds a connector's answer to a lookup.
const lookUpTimeout = 3 * time.Second

// covers says whether a grant naming chats covers the chat id at pl: one
// naming the chat, a holder it is in at any depth, or the mailbox it is
// filed directly in, as the gateway's filter has it.
func covers(chats []string, id string, pl place) bool {
	if slices.Contains(chats, id) {
		return true
	}
	for i, p := range pl.parents {
		direct := i == len(pl.parents)-1
		if slices.Contains(chats, p.ID) && (store.Holder(p.Kind) || direct && p.Kind == "mailbox" && pl.kind != "mailbox") {
			return true
		}
	}
	return false
}

// lookUp places ids, from the cache or else the connector. With an error,
// it has placed only what was cached.
func (s *Server) lookUp(ctx context.Context, conn string, ids []string) (map[string]place, error) {
	now := time.Now()
	out := make(map[string]place, len(ids))
	var missing []string
	s.names.mu.Lock()
	for _, id := range ids {
		if c, ok := s.names.m[[2]string{conn, id}]; ok && now.Sub(c.at) < chatNameTTL {
			out[id] = c.place
		} else if !slices.Contains(missing, id) {
			missing = append(missing, id)
		}
	}
	s.names.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	client := s.conns[conn]
	if client == nil {
		return out, fmt.Errorf("no connector %s", conn)
	}
	chats, err := client.Containers(ctx, connector.ContainerQuery{IDs: missing})
	if err != nil {
		return out, err
	}
	found := map[string]place{}
	for _, c := range chats {
		path := make([]string, 0, len(c.Parents)+1)
		for _, p := range c.Parents {
			path = append(path, p.Name)
		}
		found[c.ID] = place{strings.Join(append(path, c.Name), " / "), c.Kind, c.Icon, c.Parents}
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
	return out, nil
}
