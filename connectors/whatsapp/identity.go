package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types/events"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/store"
)

// whatsmeow trusts a contact's new identity key and keeps sending, as the
// apps do. The change is kept here so approvals can show it.
const identitySchema = `
CREATE TABLE IF NOT EXISTS whatsapp_identity_changes (
    jid        TEXT PRIMARY KEY,  -- non-AD, phone number preferred
    changed_at INTEGER NOT NULL   -- unix seconds
)`

// identityWarning is how long an approval shows a changed security code.
const identityWarning = 7 * 24 * time.Hour

func openStore(ctx context.Context, path string) (*store.Store, error) {
	st, err := store.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if _, err := st.DB().ExecContext(ctx, identitySchema); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// identityChanged records a changed primary identity. An implicit change
// on a companion device is a relinked device, which the apps do not report.
func (in *ingester) identityChanged(ctx context.Context, e *events.IdentityChange) {
	if e.Implicit && e.JID.Device != 0 {
		return
	}
	at := e.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	j := in.pn(e.JID)
	_, err := in.st.DB().ExecContext(ctx, `
INSERT INTO whatsapp_identity_changes (jid, changed_at) VALUES (?1, ?2)
ON CONFLICT(jid) DO UPDATE SET changed_at = max(changed_at, excluded.changed_at)`,
		j.String(), at.Unix())
	if err != nil {
		in.log.Error("store identity change", "jid", j.String(), "err", err)
		return
	}
	in.log.Warn("security code changed", "jid", j.String())
}

// identityChanges returns when each of ids, non-AD JIDs, last changed its
// security code, for changes in the last week.
func identityChanges(ctx context.Context, db *sql.DB, ids []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time)
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{time.Now().Add(-identityWarning).Unix()}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `SELECT jid, changed_at FROM whatsapp_identity_changes
WHERE changed_at >= ?1 AND jid IN (`+strings.Repeat("?,", len(ids)-1)+`?)`, args...)
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
	return approval.Field{Label: "Security", Value: "! security code changed " + strings.Join(changes, ", ")}
}

func changedAt(t time.Time) string { return t.Format("2006-01-02 15:04 UTC") }
