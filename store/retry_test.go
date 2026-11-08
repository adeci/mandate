package store_test

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/store"
	"github.com/adeci/mandate/store/storetest"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func body(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var b string
	if err := st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(max(body), '') FROM messages WHERE id = ?1`, id).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// A reader holding the store past the busy timeout fails a write, which the
// queue keeps until the reader lets go. A write that comes meanwhile waits
// behind it, so an edit lands after its message. A shutdown logs what is
// left.
func TestQueue(t *testing.T) {
	t.Parallel()
	st := open(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	q := store.NewQueue(log, "a fetch stores it again")
	at := time.Date(2026, 11, 7, 21, 37, 0, 0, time.UTC)
	message := func(id string) func(context.Context) error {
		return func(ctx context.Context) error {
			if err := st.UpsertContainer(ctx, store.Container{ID: "c1", Kind: "channel"}); err != nil {
				return err
			}
			return st.UpsertMessage(ctx, store.Message{ID: id, ContainerID: "c1", SentAt: at, Body: "hi"})
		}
	}

	unlock := storetest.Lock(t, st)
	q.Do("message m1", message("m1"))
	if b := body(t, st, "m1"); b != "" {
		t.Fatalf("stored %q while locked", b)
	}
	unlock()
	q.Do("edit of m1", func(ctx context.Context) error { return st.EditMessage(ctx, "m1", "hi!", at.Add(time.Minute)) })
	for deadline := time.Now().Add(10 * time.Second); body(t, st, "m1") != "hi!"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("m1 is %q, want the edit applied after the message", body(t, st, "m1"))
		}
	}
	// Close waits for the goroutine that retries, so its log is complete.
	q.Close()

	q = store.NewQueue(log, "a fetch stores it again")
	storetest.Lock(t, st)
	q.Do("message m2", message("m2"))
	q.Close()
	got := logs.String()
	for _, want := range []string{
		`msg="store busy; keeping writes until it is free" write="message m1"`,
		`msg="store free again; stored the writes kept" count=2`,
		`msg="not stored before shutdown; a fetch stores it again" write="message m2"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %s:\n%s", want, got)
		}
	}
}

func TestRetry(t *testing.T) {
	t.Parallel()
	st := open(t)
	unlock := storetest.Lock(t, st)
	tries := 0
	err := store.Retry(t.Context(), func() error {
		if tries++; tries == 2 {
			unlock()
		}
		return st.UpsertContainer(t.Context(), store.Container{ID: "c1", Kind: "dm"})
	})
	if err != nil || tries != 2 {
		t.Fatalf("Retry = %v after %d tries, want success on the second", err, tries)
	}
}
