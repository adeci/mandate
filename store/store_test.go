package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type row struct {
	seq     int64
	body    sql.NullString
	edited  sql.NullString
	deleted int
	sender  sql.NullString
}

func get(t *testing.T, s *Store, id string) row {
	t.Helper()
	var r row
	must(t, s.DB().QueryRowContext(t.Context(),
		`SELECT seq, body, edited_at, deleted, sender_name FROM messages WHERE id = ?`, id).
		Scan(&r.seq, &r.body, &r.edited, &r.deleted, &r.sender))
	return r
}

func TestUpsertMessageLifecycle(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	must(t, s.UpsertContainer(ctx, Container{ID: "c1", Kind: "dm", Name: "Farid"}))

	msg := Message{ID: "m1", ContainerID: "c1", SenderID: "s1", SenderName: "Farid", SentAt: at, Body: "hi"}
	must(t, s.UpsertMessage(ctx, msg))
	must(t, s.UpsertMessage(ctx, Message{ID: "m2", ContainerID: "c1", SenderID: "s1", SentAt: at, Body: "second"}))
	first := get(t, s, "m1")

	must(t, s.UpsertMessage(ctx, msg))
	if r := get(t, s, "m1"); r.seq != first.seq || r.edited.Valid {
		t.Fatalf("redelivery changed row: %+v", r)
	}

	msg.SenderName = ""
	must(t, s.UpsertMessage(ctx, msg))
	if r := get(t, s, "m1"); r.sender.String != "Farid" {
		t.Fatalf("sender name lost: %+v", r)
	}

	// Changed content is an edit: body updated, edited_at set, seq kept so
	// cursor-based polling does not re-deliver it as new.
	msg.Body = "hi!"
	must(t, s.UpsertMessage(ctx, msg))
	r := get(t, s, "m1")
	if r.seq != first.seq || r.body.String != "hi!" || !r.edited.Valid {
		t.Fatalf("edit via upsert: %+v", r)
	}

	editAt := at.Add(time.Hour)
	must(t, s.EditMessage(ctx, "m1", "final", editAt))
	if r := get(t, s, "m1"); r.body.String != "final" || r.edited.String != formatTime(editAt) {
		t.Fatalf("EditMessage: %+v", r)
	}

	must(t, s.DeleteMessage(ctx, "m1"))
	if r := get(t, s, "m1"); r.deleted != 1 || r.body.Valid {
		t.Fatalf("DeleteMessage must flag and clear the body: %+v", r)
	}
	// Neither a late edit nor a redelivery brings the text back.
	must(t, s.EditMessage(ctx, "m1", "again", editAt.Add(time.Minute)))
	must(t, s.UpsertMessage(ctx, msg))
	if r := get(t, s, "m1"); r.deleted != 1 || r.body.Valid {
		t.Fatalf("deleted message restored: %+v", r)
	}
}

// A chat is never its own parent: the gateway would follow the cycle.
func TestSelfParentRefused(t *testing.T) {
	t.Parallel()
	s := open(t)
	if err := s.UpsertContainer(t.Context(), Container{ID: "c", Kind: "channel", ParentID: "c"}); err == nil {
		t.Fatal("stored a chat as its own parent")
	}
}

// schemaV1 is the tables of version 1, before containers.icon.
var schemaV1 = func() string {
	icon := strings.Index(schema, "\n    icon ")
	comma := strings.LastIndex(schema[:icon], ",")
	end := icon + strings.Index(schema[icon:], "\n);")
	return schema[:comma] + schema[comma+1:icon] + schema[end:]
}()

// A store written before versioning has user_version 0 and the version 1
// tables. The gateway refuses it until the connector has migrated it, and
// the connector keeps its messages.
func TestMigrateUnversionedStore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "data.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	must(t, err)
	_, err = old.ExecContext(ctx, schemaV1+`
INSERT INTO containers (id, kind, updated_at) VALUES ('c1', 'dm', '2026-10-01T12:00:00.000Z');
INSERT INTO messages (id, container_id, sent_at, body, ingested_at)
VALUES ('m1', 'c1', '2026-10-01T12:00:00.000Z', 'hi', '2026-10-01T12:00:01.000Z');`)
	must(t, err)
	must(t, old.Close())

	if db, err := OpenReadOnly(ctx, path, nil); err == nil {
		db.Close()
		t.Fatal("OpenReadOnly accepted an unversioned store")
	}

	s, err := Open(ctx, path)
	must(t, err)
	must(t, s.UpsertMessage(ctx, Message{ID: "m2", ContainerID: "c1", SentAt: time.Now(), Body: "after"}))
	must(t, s.Close())
	// Opening a current store again changes nothing.
	s, err = Open(ctx, path)
	must(t, err)
	must(t, s.Close())

	db, err := OpenReadOnly(ctx, path, nil)
	must(t, err)
	defer db.Close()
	var v int
	must(t, db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v))
	if v != Version {
		t.Fatalf("user_version = %d, want %d", v, Version)
	}
	var bodies string
	must(t, db.QueryRowContext(ctx, `SELECT group_concat(body, ',') FROM (SELECT body FROM messages ORDER BY seq)`).Scan(&bodies))
	if bodies != "hi,after" {
		t.Fatalf("messages after migration = %q, want hi,after", bodies)
	}
}

// An unversioned store made from the current tables migrates too.
func TestMigrateUnversionedCurrent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "data.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	must(t, err)
	_, err = old.ExecContext(ctx, schema)
	must(t, err)
	must(t, old.Close())
	s, err := Open(ctx, path)
	must(t, err)
	must(t, s.Close())
}

// Version 1 gains containers.icon at the end, as a new store has it, and an
// icon is kept until another replaces it.
func TestMigrateIcon(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	must(t, err)
	_, err = old.ExecContext(ctx, schemaV1+`PRAGMA user_version = 1;
INSERT INTO containers (id, kind, updated_at) VALUES ('c1', 'group', '2026-10-01T12:00:00.000Z');`)
	must(t, err)
	must(t, old.Close())
	s, err := Open(ctx, path)
	must(t, err)
	defer s.Close()
	fresh, err := Open(ctx, filepath.Join(dir, "fresh.db"))
	must(t, err)
	defer fresh.Close()
	columns := func(db *sql.DB) string {
		var c string
		must(t, db.QueryRowContext(ctx, `SELECT group_concat(name || ' ' || type, ', ') FROM pragma_table_info('containers')`).Scan(&c))
		return c
	}
	if got, want := columns(s.DB()), columns(fresh.DB()); got != want {
		t.Fatalf("migrated columns %s, new %s", got, want)
	}
	icon := func() string {
		var v sql.NullString
		must(t, s.DB().QueryRowContext(ctx, `SELECT icon FROM containers WHERE id = 'c1'`).Scan(&v))
		return v.String
	}
	must(t, s.UpsertContainer(ctx, Container{ID: "c1", Kind: "group", Icon: "a1b2"}))
	must(t, s.UpsertContainer(ctx, Container{ID: "c1", Kind: "group", Name: "Club"}))
	if got := icon(); got != "a1b2" {
		t.Fatalf("icon after an upsert without one = %q", got)
	}
	must(t, s.UpsertContainer(ctx, Container{ID: "c1", Kind: "group", Icon: "c3d4"}))
	if got := icon(); got != "c3d4" {
		t.Fatalf("icon = %q", got)
	}
}

// A reader whose context ends lets go of the store mid-query, so the
// connector's commit waiting for it goes through instead of failing after
// busy_timeout.
func TestSlowReaderGivesWay(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := Open(ctx, path)
	must(t, err)
	defer s.Close()
	must(t, s.UpsertContainer(ctx, Container{ID: "c1", Kind: "dm"}))
	reading := make(chan struct{})
	var once sync.Once
	db, err := OpenReadOnly(ctx, path, func(c *sqlite3.Conn) error {
		return c.CreateFunction("reading", 0, 0, func(sqlite3.Context, ...sqlite3.Value) {
			once.Do(func() { close(reading) })
		})
	})
	must(t, err)
	defer db.Close()

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// Counts without end, holding the store's shared lock.
		var n int
		done <- db.QueryRowContext(rctx, `WITH RECURSIVE i(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM i)
SELECT count(*) FROM i WHERE n > (SELECT count(*) FROM containers) AND reading() IS NULL`).Scan(&n)
	}()
	select {
	case <-reading:
	case err := <-done:
		t.Fatalf("the query ended before the write: %v", err)
	}
	time.AfterFunc(100*time.Millisecond, cancel)
	must(t, s.UpsertContainer(ctx, Container{ID: "c2", Kind: "dm"}))
	if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, sqlite3.INTERRUPT) {
		t.Fatalf("the query ended with %v, want it interrupted", err)
	}
}
