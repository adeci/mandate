package main

import (
	"path/filepath"
	"testing"

	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/store/storetest"
)

// The chats a sync stores, kept in testdata/chats.json for the gateway's
// tests of agents' terms: a space with its rooms, a room named like one
// of them outside it, and a DM.
func TestChatsFixture(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.link()
	const (
		space   = id.RoomID("!space:example.org")
		general = id.RoomID("!general:example.org")
		lobby   = id.RoomID("!lobby:example.org")
	)
	batch := joined(map[id.RoomID]map[string]any{
		space: room([]map[string]any{
			stateEvent("m.room.create", "", bob, map[string]any{"type": "m.space"}),
			stateEvent("m.room.name", "", bob, map[string]any{"name": "Club"}),
			stateEvent("m.space.child", teamRoom.String(), bob, map[string]any{"via": []string{"example.org"}}),
			stateEvent("m.space.child", general.String(), bob, map[string]any{"via": []string{"example.org"}}),
		}, nil),
		teamRoom: room([]map[string]any{stateEvent("m.room.name", "", bob, map[string]any{"name": "Team"})}, nil),
		general:  room([]map[string]any{stateEvent("m.room.name", "", bob, map[string]any{"name": "general"})}, nil),
		lobby:    room([]map[string]any{stateEvent("m.room.name", "", bob, map[string]any{"name": "general"})}, nil),
		dmRoom:   room([]map[string]any{member(owner, "Owner"), member(bob, "Bob")}, nil),
	})
	batch["account_data"] = map[string]any{"events": []map[string]any{
		{"type": "m.direct", "content": map[string]any{bob.String(): []id.RoomID{dmRoom}}},
	}}
	n := f.hs.push(batch)
	f.hs.waitSynced(n)
	storetest.Check(t, f.st.DB(), filepath.Join("testdata", "chats.json"))
}
