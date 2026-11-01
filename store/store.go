// Package store owns the per-source message database written by each ingester.
//
// Rollback-journal mode, not WAL: a WAL reader needs write access to the -shm
// file, which the gateway's user lacks.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // registers "sqlite3"
)

//go:embed schema.sql
var schema string

type Participant struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type Container struct {
	ID           string
	Kind         string
	ParentID     string
	Name         string
	IsGroup      bool
	Participants []Participant
	// LastMessageAt is the newest activity the service reports, which may be
	// newer than any stored message. Zero or an older time keeps the stored value.
	LastMessageAt time.Time
}

// Body holds raw text; the gateway, not the store, decides what clients may see.
type Message struct {
	ID          string
	ContainerID string
	SenderID    string
	SenderName  string
	SentAt      time.Time
	IsFromMe    bool
	Body        string
	MediaType   string
	ReplyToID   string
}

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(DELETE)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// One writer connection: serializes writes and avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate adds what schema.sql's CREATE TABLE IF NOT EXISTS cannot add to a
// database created by an older version.
func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('containers') WHERE name = 'last_message_at'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE containers ADD COLUMN last_message_at TEXT`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

// Empty Name/ParentID and nil Participants do not overwrite existing values.
func (s *Store) UpsertContainer(ctx context.Context, c Container) error {
	if c.ID == "" || c.Kind == "" {
		return errors.New("store: container id and kind are required")
	}
	var participants any
	if c.Participants != nil {
		b, err := json.Marshal(c.Participants)
		if err != nil {
			return err
		}
		participants = string(b)
	}
	var lastMessageAt any
	if !c.LastMessageAt.IsZero() {
		lastMessageAt = formatTime(c.LastMessageAt)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO containers (id, kind, parent_id, name, is_group, participants_json, updated_at, last_message_at)
VALUES (?1, ?2, NULLIF(?3, ''), NULLIF(?4, ''), ?5, COALESCE(?6, '[]'), ?7, ?8)
ON CONFLICT(id) DO UPDATE SET
    kind              = excluded.kind,
    parent_id         = COALESCE(excluded.parent_id, containers.parent_id),
    name              = COALESCE(excluded.name, containers.name),
    is_group          = excluded.is_group,
    participants_json = COALESCE(?6, containers.participants_json),
    updated_at        = excluded.updated_at,
    last_message_at   = NULLIF(max(COALESCE(?8, ''), COALESCE(containers.last_message_at, '')), '')`,
		c.ID, c.Kind, c.ParentID, c.Name, c.IsGroup, participants, now(), lastMessageAt)
	return err
}

// Identical redelivery is a no-op; changed content sets edited_at. The seq of
// an existing message never changes.
func (s *Store) UpsertMessage(ctx context.Context, m Message) error {
	if m.ID == "" || m.ContainerID == "" {
		return errors.New("store: message id and container id are required")
	}
	if m.SentAt.IsZero() {
		return errors.New("store: message sent_at is required")
	}
	ts := now()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO messages (id, container_id, sender_id, sender_name, sent_at, is_from_me,
                      body, media_type, reply_to_id, ingested_at)
VALUES (?1, ?2, NULLIF(?3, ''), NULLIF(?4, ''), ?5, ?6, ?7, NULLIF(?8, ''), NULLIF(?9, ''), ?10)
ON CONFLICT(id) DO UPDATE SET
    sender_name = COALESCE(excluded.sender_name, messages.sender_name),
    media_type  = COALESCE(excluded.media_type, messages.media_type),
    edited_at   = CASE WHEN excluded.body IS NOT messages.body THEN ?10 ELSE messages.edited_at END,
    body        = excluded.body`,
		m.ID, m.ContainerID, m.SenderID, m.SenderName, formatTime(m.SentAt), m.IsFromMe,
		m.Body, m.MediaType, m.ReplyToID, ts)
	return err
}

// Unknown IDs are ignored.
func (s *Store) EditMessage(ctx context.Context, id, body string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET body = ?2, edited_at = ?3 WHERE id = ?1`,
		id, body, formatTime(at))
	return err
}

// The body is kept on disk; the gateway exposes the flag.
func (s *Store) DeleteMessage(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET deleted = 1 WHERE id = ?1`, id)
	return err
}

func formatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func now() string { return formatTime(time.Now()) }
