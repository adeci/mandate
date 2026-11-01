package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"go.mau.fi/whatsmeow"
)

// A full contact sync fills the contact store without per-contact events, so
// DMs stored before it are named afterwards; only the address book gives an
// unmarked name.
func TestNameDMs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	dev, err := f.container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newIngester(whatsmeow.NewClient(dev, nil), f.st, slog.New(slog.NewTextHandler(io.Discard, nil))).nameDMs(ctx)
	for id, want := range map[string]string{
		farid.String():    "Farid Haddad", // replaces the push name stored earlier
		samParkL.String(): "Sam Park",     // listed in the address book under its LID
		stranger.String(): "~Mom",         // not in the address book
	} {
		var got string
		if err := f.st.DB().QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM containers WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s named %q, want %q", id, got, want)
		}
	}
}
