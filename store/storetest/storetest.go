// Package storetest keeps fixtures of the chats each connector stores, made
// by the connector's own code, so the gateway's tests match agents' terms
// against the names and paths a real store holds. Lock makes a store busy,
// as the gateway's slow queries can.
package storetest

import (
	"database/sql"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/adeci/mandate/store"
)

type chat struct {
	ID           string              `json:"id"`
	Kind         string              `json:"kind"`
	ParentID     string              `json:"parent_id,omitempty"`
	Name         string              `json:"name,omitempty"`
	Participants []store.Participant `json:"participants,omitempty"`
}

// Check compares the chats in db with the fixture at path. With
// UPDATE_FIXTURES=1 it writes the fixture instead.
func Check(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT id, kind, COALESCE(parent_id, ''), COALESCE(name, ''), participants_json FROM containers ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cs []chat
	for rows.Next() {
		var c chat
		var parts string
		if err := rows.Scan(&c.ID, &c.Kind, &c.ParentID, &c.Name, &parts); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(parts), &c.Participants); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; write it with UPDATE_FIXTURES=1", err)
	}
	if string(got) != string(want) {
		t.Errorf("the chats stored differ from %s; if the change is meant, update it with UPDATE_FIXTURES=1 and check the gateway's tests:\n%s", path, got)
	}
}

// Load stores the chats of the fixture at path in st.
func Load(t *testing.T, st *store.Store, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cs []chat
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if err := st.UpsertContainer(t.Context(), store.Container{
			ID: c.ID, Kind: c.Kind, ParentID: c.ParentID, Name: c.Name,
			IsGroup: c.Kind == "group", Participants: c.Participants,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// Lock holds a read transaction on st's file, as a slow query of the
// gateway's does, until unlock is called. Meanwhile a write to st fails with
// SQLITE_BUSY once its busy timeout is out, which Lock cuts to 10 ms: Open
// keeps one connection, so the pragma holds for all of st's writes.
func Lock(t *testing.T, st *store.Store) (unlock func()) {
	t.Helper()
	ctx := t.Context()
	var path string
	if err := st.DB().QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `PRAGMA busy_timeout = 10`); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenReadOnly(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	unlock = sync.OnceFunc(func() { _ = tx.Rollback() })
	t.Cleanup(unlock)
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return unlock
}
