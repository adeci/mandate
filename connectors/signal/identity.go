package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/adeci/mandate/approval"
)

// signalmeow trusts a contact's new identity key and keeps sending, as the
// apps do. The change is kept here so approvals can show it.
const identitiesSchema = `
CREATE TABLE IF NOT EXISTS mandate_identity_changes (
    service_id TEXT PRIMARY KEY,
    changed_at INTEGER NOT NULL -- unix seconds
)`

// identityWarning is how long an approval shows a changed safety number.
const identityWarning = 7 * 24 * time.Hour

type identities struct{ db *sql.DB }

func (s *identities) changed(ctx context.Context, serviceID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO mandate_identity_changes (service_id, changed_at) VALUES (?1, ?2)
ON CONFLICT(service_id) DO UPDATE SET changed_at = max(changed_at, excluded.changed_at)`,
		serviceID, at.Unix())
	return err
}

// recent returns when each of serviceIDs last changed its identity key, for
// changes in the last week.
func (s *identities) recent(ctx context.Context, serviceIDs []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time)
	if len(serviceIDs) == 0 {
		return out, nil
	}
	args := []any{time.Now().Add(-identityWarning).Unix()}
	for _, id := range serviceIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT service_id, changed_at FROM mandate_identity_changes
WHERE changed_at >= ?1 AND service_id IN (`+strings.Repeat("?,", len(serviceIDs)-1)+`?)`, args...)
	if err != nil {
		return nil, fmt.Errorf("look up identity changes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = time.Unix(at, 0).UTC()
	}
	return out, rows.Err()
}

// security renders changes, precise to the minute so that a second change
// on the same day changes the approval too.
func security(changes ...string) approval.Field {
	return approval.Field{Label: "Security", Value: "! safety number changed " + strings.Join(changes, ", ")}
}

func changedAt(t time.Time) string { return t.Format("2006-01-02 15:04 UTC") }
