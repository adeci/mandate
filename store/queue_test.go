package store

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

// A full queue drops what comes next, and says so once per busy spell with
// what recovers it.
func TestQueueBound(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	q := NewQueue(slog.New(slog.NewTextHandler(&logs, nil)), "a fetch stores it again")
	q.max = 2
	busy := func(context.Context) error { return sqlite3.BUSY }
	for _, what := range []string{"w1", "w2", "w3", "w4"} {
		if q.Do(what, busy) {
			t.Fatalf("%s stored while the store is busy", what)
		}
	}
	q.Close()
	got := logs.String()
	for _, want := range []string{
		`level=ERROR msg="store busy too long; dropping writes past the 2 kept until it is free; a fetch stores it again" write=w3`,
		`msg="not stored before shutdown; a fetch stores it again" write=w1`,
		`msg="not stored before shutdown; a fetch stores it again" write=w2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %s:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "level=ERROR"); n != 1 || strings.Contains(got, "w4") {
		t.Errorf("want one error, for w3 only:\n%s", got)
	}
}
