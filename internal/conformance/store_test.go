package conformance

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/adeci/mandate/store"
	"github.com/adeci/mandate/store/storetest"
)

func TestStoreRules(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, sql string
		check     func(context.Context, *sql.DB) error
	}{
		{"times", `UPDATE messages SET edited_at = '2026-11-02T14:05:09Z'`, checkTimes},
		{"nesting", `UPDATE containers SET parent_id = 'c3' WHERE id = 'c0'`, checkNesting},
		{"self parent", `UPDATE containers SET parent_id = id WHERE id = 'c0'`, checkNesting},
		{"depth", `INSERT INTO containers (id, kind, parent_id, updated_at) VALUES ('c4', 'thread', 'c3', '')`, checkNesting},
		{"names", `UPDATE containers SET name = '~ Sam' WHERE id = 'c1'`, checkNames},
		{"participants", `UPDATE containers SET participants_json = '[{"id":"p","name":"~~Sam"}]' WHERE id = 'c1'`, checkNames},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data.db")
			st, err := store.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			for i, parent := range []string{"", "c0", "c1", "c2"} {
				c := store.Container{ID: "c" + string(rune('0'+i)), Kind: "group", ParentID: parent, Name: "~Sam"}
				if err := st.UpsertContainer(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			m := store.Message{ID: "m", ContainerID: "c3", SenderName: "Sam", SentAt: time.Now(), Body: "hi"}
			if err := st.UpsertMessage(ctx, m); err != nil {
				t.Fatal(err)
			}
			k := &kit{ctx: ctx, out: io.Discard, prefix: "test_"}
			if err := k.checkSchema(path); err != nil {
				t.Fatalf("schema: %v", err)
			}
			if err := tc.check(ctx, st.DB()); err != nil {
				t.Fatalf("before %s: %v", tc.sql, err)
			}
			if _, err := st.DB().ExecContext(ctx, tc.sql); err != nil {
				t.Fatal(err)
			}
			if err := tc.check(ctx, st.DB()); err == nil {
				t.Fatalf("passes after %s", tc.sql)
			}
		})
	}
}

// The chats each connector's own code stores, as its fixture keeps them,
// pass the store rules.
func TestConnectorFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "connectors", "*", "testdata", "chats.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, p := range paths {
		st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "data.db"))
		if err != nil {
			t.Fatal(err)
		}
		storetest.Load(t, st, p)
		for _, check := range []func(context.Context, *sql.DB) error{checkNesting, checkNames} {
			if err := check(t.Context(), st.DB()); err != nil {
				t.Errorf("%s: %v", p, err)
			}
		}
		st.Close()
	}
}
