package store

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestMatchingNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := Open(ctx, path)
	must(t, err)
	defer s.Close()
	must(t, s.UpsertContainers(ctx, []Container{
		{ID: "g1", Kind: "group", Name: "Über uns", IsGroup: true},
		{ID: "d1", Kind: "dm", Name: "Анна"},
		{ID: "guild:1", Kind: "guild", Name: "r/NixOS"},
		{ID: "11", Kind: "channel", ParentID: "guild:1", Name: "#help"},
	}))
	db, err := OpenReadOnly(ctx, path, nil)
	must(t, err)
	defer db.Close()
	for term, want := range map[string][]string{
		"über uns":       {"g1"},
		"ÜBER":           {"g1"},
		"анна":           {"d1"},
		"r/NixOS":        {"guild:1"},
		"r/nixos / help": {"11"},
	} {
		terms, err := Terms([]string{term})
		must(t, err)
		rows, err := db.QueryContext(ctx, `WITH candidates AS (
  SELECT c.id, c.kind, c.parent_id, c.name AS own, concat_ws(' / ', p.name, c.name) AS path, NULL AS handle, c.last_message_at AS at
  FROM containers AS c LEFT JOIN containers AS p ON p.id = c.parent_id),
`+Matching("?1", "?2")+`
SELECT id FROM matched ORDER BY n`, terms, "2026-11-19T12:00:00Z")
		must(t, err)
		var got []string
		for rows.Next() {
			var id string
			must(t, rows.Scan(&id))
			got = append(got, id)
		}
		must(t, rows.Err())
		if !slices.Equal(got, want) {
			t.Errorf("%q matches %v, want %v", term, got, want)
		}
	}
}
