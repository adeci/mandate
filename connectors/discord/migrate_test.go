package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/adeci/mandate/store"
)

// A store from before guilds had their own IDs: in guild 1 the #general
// channel shares the guild's ID and replaced its row, parented to itself;
// guild 2 kept its row; a channel the account may not view is stored too.
func TestMigrate(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const (
		g1 = "1100000000000000001" // also its #general
		g2 = "1200000000000000001"
	)
	if _, err := st.DB().ExecContext(t.Context(), `
INSERT INTO containers (id, kind, parent_id, name, updated_at) VALUES
  (?1, 'channel', ?1, '#general', ''),
  ('1100000000000000012', 'channel', ?1, '#releases', ''),
  ('1100000000000000013', 'thread', '1100000000000000012', 'v2 rollout', ''),
  ('1100000000000000015', 'thread', ?1, 'in #general', ''),
  (?2, 'guild', NULL, 'other', ''),
  ('1200000000000000011', 'channel', ?2, '#general', ''),
  ('1200000000000000016', 'thread', ?2, 'channel unknown', ''),
  ('1200000000000000014', 'channel', ?2, '#___hidden___', ''),
  ('1200000000000000017', 'channel', ?2, '#___hidden___', '')`, g1, g2); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMessage(t.Context(), store.Message{ID: "m", ContainerID: "1200000000000000017", SentAt: noon}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrate(t.Context(), st.DB()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.DB().QueryContext(t.Context(), `SELECT id, kind, COALESCE(parent_id, ''), COALESCE(name, '') FROM containers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][3]string{}
	for rows.Next() {
		var id string
		var c [3]string
		if err := rows.Scan(&id, &c[0], &c[1], &c[2]); err != nil {
			t.Fatal(err)
		}
		got[id] = c
	}
	want := map[string][3]string{
		"guild:" + g1:         {"guild", "", ""}, // named again by the next READY
		g1:                    {"channel", "guild:" + g1, "#general"},
		"1100000000000000012": {"channel", "guild:" + g1, "#releases"},
		"1100000000000000013": {"thread", "1100000000000000012", "v2 rollout"},
		"1100000000000000015": {"thread", g1, "in #general"},
		"guild:" + g2:         {"guild", "", "other"},
		"1200000000000000011": {"channel", "guild:" + g2, "#general"},
		"1200000000000000016": {"thread", "guild:" + g2, "channel unknown"},
		"1200000000000000017": {"channel", "guild:" + g2, "#___hidden___"}, // has messages
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after migrating\n got %v\nwant %v", got, want)
	}
}
