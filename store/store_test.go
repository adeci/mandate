package store

import (
	"context"
	"database/sql"
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
	if r := get(t, s, "m1"); r.deleted != 1 || r.body.String != "final" {
		t.Fatalf("DeleteMessage must flag and keep the body: %+v", r)
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
