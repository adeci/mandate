package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
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

func TestOpenUsesRollbackJournal(t *testing.T) {
	t.Parallel()
	s := open(t)
	var mode string
	must(t, s.DB().QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&mode))
	if mode != "delete" {
		t.Fatalf("journal_mode = %q, want delete", mode)
	}
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

func TestMediaRoundTrip(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	must(t, s.UpsertContainer(ctx, Container{ID: "c1", Kind: "dm"}))
	photo := Media{Type: "image/jpeg", Size: 1234, Ref: []byte{0, 1, 2}}
	doc := Media{Type: "application/pdf", Name: "plan.pdf", Ref: []byte("key")}
	msg := Message{ID: "m1", ContainerID: "c1", SentAt: at, Body: "look", Media: []Media{photo, doc}}
	must(t, s.UpsertMessage(ctx, msg))

	// Redelivery without files keeps them.
	msg.Media = nil
	must(t, s.UpsertMessage(ctx, msg))
	for i, want := range []Media{photo, doc} {
		got, err := s.Media(ctx, "m1", i)
		must(t, err)
		if got.Type != want.Type || got.Name != want.Name || got.Size != want.Size || string(got.Ref) != string(want.Ref) {
			t.Errorf("item %d = %+v, want %+v", i, got, want)
		}
	}
	if _, err := s.Media(ctx, "m1", 2); !errors.Is(err, ErrNoMedia) {
		t.Errorf("past the last item: %v, want ErrNoMedia", err)
	}

	// A deleted message keeps no references, and redelivery adds none.
	must(t, s.DeleteMessage(ctx, "m1"))
	msg.Media = []Media{photo}
	must(t, s.UpsertMessage(ctx, msg))
	if _, err := s.Media(ctx, "m1", 0); !errors.Is(err, ErrNoMedia) {
		t.Errorf("deleted message: %v, want ErrNoMedia", err)
	}
}

func TestSeqIsMonotonic(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	at := time.Now()
	must(t, s.UpsertContainer(ctx, Container{ID: "c", Kind: "dm"}))
	must(t, s.UpsertMessage(ctx, Message{ID: "a", ContainerID: "c", SentAt: at}))
	must(t, s.UpsertMessage(ctx, Message{ID: "b", ContainerID: "c", SentAt: at}))
	a := get(t, s, "a").seq
	_, err := s.DB().ExecContext(ctx, `DELETE FROM messages WHERE id = 'b'`)
	must(t, err)
	must(t, s.UpsertMessage(ctx, Message{ID: "c", ContainerID: "c", SentAt: at}))
	if c := get(t, s, "c").seq; c <= a+1 {
		t.Fatalf("seq reused after delete: a=%d c=%d", a, c)
	}
}

func TestUpsertContainerKeepsKnownFields(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	must(t, s.UpsertContainer(ctx, Container{
		ID: "ch", Kind: "channel", ParentID: "g", Name: "general",
		Participants: []Participant{{ID: "u1", Name: "A"}},
	}))
	// A sparse update (e.g. a guess from a message event) must not erase data.
	must(t, s.UpsertContainer(ctx, Container{ID: "ch", Kind: "channel"}))

	var parent, name, participants string
	must(t, s.DB().QueryRowContext(ctx,
		`SELECT parent_id, name, participants_json FROM containers WHERE id = 'ch'`).
		Scan(&parent, &name, &participants))
	if parent != "g" || name != "general" || participants != `[{"id":"u1","name":"A"}]` {
		t.Fatalf("sparse update erased fields: %q %q %q", parent, name, participants)
	}
}

func TestUpsertContainersIsAtomic(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	must(t, s.UpsertContainers(ctx, []Container{{ID: "g", Kind: "guild", Name: "Club"}, {ID: "ch", Kind: "channel", ParentID: "g"}}))
	if err := s.UpsertContainers(ctx, []Container{{ID: "ch2", Kind: "channel"}, {ID: "bad"}}); err == nil {
		t.Fatal("accepted a container without kind")
	}
	var n int
	must(t, s.DB().QueryRowContext(ctx, `SELECT count(*) FROM containers`).Scan(&n))
	if n != 2 {
		t.Fatalf("%d containers, want the first batch only", n)
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	for name, err := range map[string]error{
		"container without kind": s.UpsertContainer(ctx, Container{ID: "x"}),
		"message without id":     s.UpsertMessage(ctx, Message{ContainerID: "c", SentAt: time.Now()}),
		"message without time":   s.UpsertMessage(ctx, Message{ID: "m", ContainerID: "c"}),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func lastMessageAt(t *testing.T, s *Store, id string) string {
	t.Helper()
	var at sql.NullString
	must(t, s.DB().QueryRowContext(t.Context(), `SELECT last_message_at FROM containers WHERE id = ?`, id).Scan(&at))
	return at.String
}

func TestLastMessageAtOnlyMovesForward(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 30, 18, 40, 0, 0, time.UTC)
	must(t, s.UpsertContainer(ctx, Container{ID: "c", Kind: "dm", LastMessageAt: at}))
	must(t, s.UpsertContainer(ctx, Container{ID: "c", Kind: "dm", LastMessageAt: at.Add(-time.Hour)}))
	must(t, s.UpsertContainer(ctx, Container{ID: "c", Kind: "dm"}))
	if got := lastMessageAt(t, s, "c"); got != formatTime(at) {
		t.Fatalf("last_message_at = %q, want %q", got, formatTime(at))
	}
	must(t, s.UpsertContainer(ctx, Container{ID: "c", Kind: "dm", LastMessageAt: at.Add(time.Minute)}))
	if got := lastMessageAt(t, s, "c"); got != formatTime(at.Add(time.Minute)) {
		t.Fatalf("last_message_at = %q after a newer time", got)
	}
}

// The schema before last_message_at, as on databases in use.
const schemaV0 = `CREATE TABLE containers (
    id                TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,
    parent_id         TEXT,
    name              TEXT,
    is_group          INTEGER NOT NULL DEFAULT 0,
    participants_json TEXT NOT NULL DEFAULT '[]',
    updated_at        TEXT NOT NULL
);
INSERT INTO containers (id, kind, name, updated_at) VALUES ('old', 'dm', 'Farid', '2026-10-01T00:00:00.000Z');`

func TestOpenMigratesOldDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "data.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	must(t, err)
	_, err = db.ExecContext(t.Context(), schemaV0)
	must(t, err)
	must(t, db.Close())

	for range 2 {
		s, err := Open(t.Context(), path)
		must(t, err)
		at := time.Date(2026, 10, 30, 18, 40, 0, 0, time.UTC)
		must(t, s.UpsertContainer(t.Context(), Container{ID: "old", Kind: "dm", LastMessageAt: at}))
		var name string
		must(t, s.DB().QueryRowContext(t.Context(), `SELECT name FROM containers WHERE id = 'old'`).Scan(&name))
		if got := lastMessageAt(t, s, "old"); name != "Farid" || got != formatTime(at) {
			t.Fatalf("after migration: name %q, last_message_at %q", name, got)
		}
		must(t, s.Close())
	}
}
