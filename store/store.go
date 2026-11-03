// Package store is a connector's message store, data.db, as
// docs/spec/connector.md describes it. The connector writes it through Store;
// the gateway reads it through OpenReadOnly.
//
// Rollback-journal mode, not WAL: a WAL reader needs write access to the -shm
// file, which the gateway's group lacks.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
)

// Version is the schema version in PRAGMA user_version.
const Version = 1

//go:embed schema.sql
var schema string

type Participant struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Handle is the unique name the service shows for the account, where
	// that is not the ID, such as a Discord username.
	Handle string `json:"handle,omitempty"`
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
	// MediaType marks content without files to fetch, such as "poll" or
	// "location"; agents see it as [poll]. A message with Media is marked by
	// its files instead.
	MediaType string
	ReplyToID string
	// Media are the message's files in the service's order; nil keeps the
	// stored ones, an empty slice removes them.
	Media []Media
}

// Media is a file attached to a message. Its content stays with the service
// until an agent asks for it.
type Media struct {
	Type string // MIME type, application/octet-stream if unknown
	Name string // file name, if the sender gave one
	Size int64  // bytes, 0 if unknown
	// Ref is what the connector needs to fetch the file, such as a URL, an
	// ID or a decryption key. Agents never see it.
	Ref []byte
}

var ErrNoMedia = errors.New("store: no such media item")

type Store struct {
	db *sql.DB
}

// Open opens the store at path for writing, creating it or migrating it to
// Version.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(DELETE)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	// One writer connection: serializes writes and avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate store %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// migrate brings the store to Version. Stores from before versioning have
// user_version 0 and already hold the version 1 tables.
func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	var v int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	switch {
	case v == Version:
		return nil
	case v > Version:
		return fmt.Errorf("version %d is newer than this connector's %d", v, Version)
	}
	var tables int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'containers'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("create tables: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, Version)); err != nil {
		return err
	}
	return tx.Commit()
}

// OpenReadOnly opens the store at path for reading only and refuses any
// version but Version. init, if not nil, runs on each new connection, such
// as to register functions.
func OpenReadOnly(ctx context.Context, path string, init func(*sqlite3.Conn) error) (*sql.DB, error) {
	db, err := driver.Open("file:"+path+"?mode=ro&_pragma=busy_timeout(10000)", init)
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		db.Close()
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	if v != Version {
		db.Close()
		return nil, fmt.Errorf("store %s is version %d, not %d", path, v, Version)
	}
	return db, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

// Empty Name/ParentID and nil Participants do not overwrite existing values.
func (s *Store) UpsertContainer(ctx context.Context, c Container) error {
	return upsertContainer(ctx, s.db, c)
}

// UpsertContainers stores cs in one transaction, as UpsertContainer would.
func (s *Store) UpsertContainers(ctx context.Context, cs []Container) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	for _, c := range cs {
		if err := upsertContainer(ctx, tx, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func upsertContainer(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, c Container,
) error {
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
	_, err := db.ExecContext(ctx, `
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
// an existing message never changes, and a deleted message stays deleted.
func (s *Store) UpsertMessage(ctx context.Context, m Message) error {
	if m.ID == "" || m.ContainerID == "" {
		return errors.New("store: message id and container id are required")
	}
	if m.SentAt.IsZero() {
		return errors.New("store: message sent_at is required")
	}
	for _, md := range m.Media {
		if md.Type == "" {
			return errors.New("store: media type is required")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	ts := now()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO messages (id, container_id, sender_id, sender_name, sent_at, is_from_me,
                      body, media_type, reply_to_id, ingested_at)
VALUES (?1, ?2, NULLIF(?3, ''), NULLIF(?4, ''), ?5, ?6, ?7, NULLIF(?8, ''), NULLIF(?9, ''), ?10)
ON CONFLICT(id) DO UPDATE SET
    sender_name = COALESCE(excluded.sender_name, messages.sender_name),
    media_type  = COALESCE(excluded.media_type, messages.media_type),
    edited_at   = CASE WHEN excluded.body IS NOT messages.body THEN ?10 ELSE messages.edited_at END,
    body        = excluded.body
WHERE NOT messages.deleted`,
		m.ID, m.ContainerID, m.SenderID, m.SenderName, formatTime(m.SentAt), m.IsFromMe,
		m.Body, m.MediaType, m.ReplyToID, ts); err != nil {
		return err
	}
	if m.Media != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM media WHERE message_id = ?1`, m.ID); err != nil {
			return err
		}
		for i, md := range m.Media {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO media (message_id, idx, type, name, size, ref)
SELECT ?1, ?2, ?3, NULLIF(?4, ''), NULLIF(?5, 0), ?6
WHERE EXISTS (SELECT 1 FROM messages WHERE id = ?1 AND NOT deleted)`,
				m.ID, i, md.Type, md.Name, md.Size, md.Ref); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Media returns item index of a message's files, or ErrNoMedia.
func (s *Store) Media(ctx context.Context, messageID string, index int) (Media, error) {
	var m Media
	var name sql.NullString
	var size sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT type, name, size, ref FROM media WHERE message_id = ?1 AND idx = ?2`,
		messageID, index).Scan(&m.Type, &name, &size, &m.Ref)
	if errors.Is(err, sql.ErrNoRows) {
		return Media{}, ErrNoMedia
	}
	m.Name, m.Size = name.String, size.Int64
	return m, err
}

// Unknown and deleted IDs are ignored.
func (s *Store) EditMessage(ctx context.Context, id, body string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET body = ?2, edited_at = ?3 WHERE id = ?1 AND NOT deleted`,
		id, body, formatTime(at))
	return err
}

// DeleteMessage flags the message and clears its body and files.
func (s *Store) DeleteMessage(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET deleted = 1, body = NULL WHERE id = ?1`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM media WHERE message_id = ?1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func formatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func now() string { return formatTime(time.Now()) }
