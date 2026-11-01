package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// filter prefixes every query: chats leaves out hidden containers, their
// children and DMs with hidden senders; named gives each chat its parent's
// name as a prefix; msgs leaves out hidden senders and blanks login-code
// bodies. ?1 and ?2 are the hidden container and sender IDs as JSON arrays.
//
// msgs.text is the body as agents see it: login codes replaced, deletions and
// media marked in brackets. Search matches body, not the markers.
const filter = `WITH
hidden_chat(id) AS (SELECT value FROM json_each(?1)),
hidden_sender(id) AS (SELECT value FROM json_each(?2)),
chats AS (
  SELECT id, kind, parent_id, name, last_message_at FROM containers
  WHERE id NOT IN (SELECT id FROM hidden_chat)
    AND id NOT IN (SELECT id FROM hidden_sender)
    AND COALESCE(parent_id, '') NOT IN (SELECT id FROM hidden_chat)),
named AS (
  SELECT c.id, c.kind, c.last_message_at,
         CASE WHEN p.name IS NULL THEN c.name ELSE p.name || ' / ' || COALESCE(c.name, c.id) END AS name
  FROM chats AS c LEFT JOIN chats AS p ON p.id = c.parent_id),
visible AS (
  SELECT m.*, is_auth(m.sender_id, m.body) AS auth
  FROM messages AS m JOIN chats AS c ON c.id = m.container_id
  WHERE COALESCE(m.sender_id, '') NOT IN (SELECT id FROM hidden_sender)),
msgs AS (
  SELECT seq, id, container_id, sent_at, strftime('%Y-%m-%d %H:%M', sent_at) AS at, reply_to_id,
         sender_id, CASE WHEN is_from_me THEN 'me' ELSE sender_name END AS sender,
         CASE WHEN auth THEN NULL ELSE body END AS body,
         CASE WHEN auth THEN '[login code hidden]'
              ELSE rtrim(iif(deleted, '[deleted] ', '') || COALESCE('[' || media_type || '] ', '') || COALESCE(body, ''), ' ')
         END AS text
  FROM visible)
`

type table struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	// NextBefore is the --before value for the next page of messages.
	More       bool  `json:"more,omitempty"`
	NextBefore int64 `json:"next_before,omitempty"`
	// Set by connector read ops.
	TotalResults int `json:"total_results,omitempty"`
	Containers   int `json:"containers,omitempty"`
}

// source is a source database opened for one request. db is nil until the
// connector has stored something.
type source struct {
	db                  *sqlite3.Conn
	hideChats, hideFrom string
}

func (h *handler) openSource(ctx context.Context, name string) (_ *source, err error) {
	pol, err := h.policy.get()
	if err != nil {
		return nil, err
	}
	exp := pol.sources[name]
	s := &source{}
	if s.hideChats, err = jsonList(exp.HideContainers); err != nil {
		return nil, err
	}
	if s.hideFrom, err = jsonList(exp.HideSenders); err != nil {
		return nil, err
	}
	path := h.cfg.Sources[name]
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	db, err := sqlite3.OpenFlags(path, sqlite3.OPEN_READONLY)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer func() {
		if err != nil {
			db.Close()
		}
	}()
	if err := db.BusyTimeout(time.Second); err != nil {
		return nil, err
	}
	if err := db.CreateFunction("is_auth", 2, sqlite3.DETERMINISTIC|sqlite3.INNOCUOUS,
		func(ctx sqlite3.Context, arg ...sqlite3.Value) {
			ctx.ResultBool(pol.classifier.IsAuth(arg[0].Text(), arg[1].Text()))
		}); err != nil {
		return nil, err
	}
	db.SetInterrupt(ctx)
	s.db = db
	return s, nil
}

func jsonList(v []string) (string, error) {
	if v == nil {
		return "[]", nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func (s *source) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// query runs filter plus sql, which reads chats and msgs; args bind from ?3.
func (s *source) query(sql string, args ...any) ([][]any, error) {
	rows := [][]any{}
	if s.db == nil {
		return rows, nil
	}
	stmt, _, err := s.db.Prepare(filter + sql)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for i, a := range append([]any{s.hideChats, s.hideFrom}, args...) {
		switch a := a.(type) {
		case string:
			err = stmt.BindText(i+1, a)
		case int64:
			err = stmt.BindInt64(i+1, a)
		default:
			err = fmt.Errorf("param %d: unsupported type %T", i+1, a)
		}
		if err != nil {
			return nil, err
		}
	}
	n := stmt.ColumnCount()
	for stmt.Step() {
		row := make([]any, n)
		for i := range n {
			switch stmt.ColumnType(i) {
			case sqlite3.INTEGER:
				row[i] = stmt.ColumnInt64(i)
			case sqlite3.FLOAT:
				row[i] = stmt.ColumnFloat(i)
			case sqlite3.TEXT:
				row[i] = stmt.ColumnText(i)
			case sqlite3.BLOB:
				row[i] = base64.StdEncoding.EncodeToString(stmt.ColumnRawBlob(i))
			}
		}
		rows = append(rows, row)
	}
	return rows, stmt.Err()
}

func (s *source) visible(id string) (bool, error) {
	rows, err := s.query(`SELECT 1 FROM chats WHERE id = ?3`, id)
	return len(rows) == 1, err
}

// page trims the extra row queried past limit; seqCol is the seq column or -1.
func page(t *table, rows [][]any, limit int64, seqCol int) *table {
	if int64(len(rows)) > limit {
		rows = rows[:limit]
		t.More = true
		if seqCol >= 0 && len(rows) > 0 {
			t.NextBefore, _ = rows[len(rows)-1][seqCol].(int64)
		}
	}
	t.Rows = rows
	return t
}

var (
	chatColumns    = []string{"id", "kind", "name", "last_message"}
	messageColumns = []string{"seq", "sent_at", "sender_id", "sender", "reply_to", "body"}
	searchColumns  = []string{"seq", "sent_at", "chat_id", "chat", "sender_id", "sender", "reply_to", "body"}
)

// replyTo is the seq of the visible message m replies to.
const replyTo = `(SELECT r.seq FROM msgs AS r WHERE r.id = m.reply_to_id)`

// last_message is the later of the newest visible message and the activity
// the connector reported for the chat.
func (s *source) chats(search string, limit int64) (*table, error) {
	rows, err := s.query(`SELECT id, kind, name, strftime('%Y-%m-%d %H:%M', at) FROM (
  SELECT c.id, c.kind, c.name, NULLIF(max(
    COALESCE((SELECT max(sent_at) FROM msgs WHERE container_id = c.id), ''),
    COALESCE(c.last_message_at, '')), '') AS at
  FROM named AS c
  WHERE ?3 = '' OR instr(lower(COALESCE(c.name, '')), lower(?3)) OR instr(lower(c.id), lower(?3)))
ORDER BY at DESC NULLS LAST, id LIMIT ?4`, search, limit+1)
	if err != nil {
		return nil, err
	}
	return page(&table{Columns: chatColumns}, rows, limit, -1), nil
}

// before is the seq of the last message seen; the page continues after it in
// (sent_at, seq) order, since seq follows ingestion, not sending.
func (s *source) messages(chat string, before, limit int64) (*table, error) {
	rows, err := s.query(`SELECT m.seq, m.at, m.sender_id, m.sender, `+replyTo+`, m.text FROM msgs AS m
WHERE m.container_id = ?3 AND (?4 = 0 OR (m.sent_at, m.seq) < (SELECT sent_at, seq FROM msgs WHERE seq = ?4))
ORDER BY m.sent_at DESC, m.seq DESC LIMIT ?5`, chat, before, limit+1)
	if err != nil {
		return nil, err
	}
	return page(&table{Columns: messageColumns}, rows, limit, 0), nil
}

const searchSelect = `SELECT m.seq, m.at, m.container_id, c.name, m.sender_id, m.sender, ` + replyTo + `, m.text`

// Matches the filtered body, where login codes are blank. before as in messages.
func (s *source) search(text, chat, since string, before, limit int64) (*table, error) {
	rows, err := s.query(searchSelect+` FROM msgs AS m JOIN named AS c ON c.id = m.container_id
WHERE instr(lower(m.body), lower(?3)) AND (?4 = '' OR m.container_id = ?4) AND m.sent_at >= ?5
  AND (?6 = 0 OR (m.sent_at, m.seq) < (SELECT sent_at, seq FROM msgs WHERE seq = ?6))
ORDER BY m.sent_at DESC, m.seq DESC LIMIT ?7`, text, chat, since, before, limit+1)
	if err != nil {
		return nil, err
	}
	return page(&table{Columns: searchColumns}, rows, limit, 0), nil
}

// byID reads a connector's message IDs back in the connector's order.
func (s *source) byID(ids []string) (*table, error) {
	b, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.query(searchSelect+` FROM json_each(?3) AS j JOIN msgs AS m ON m.id = j.value
JOIN named AS c ON c.id = m.container_id ORDER BY j.key`, string(b))
	if err != nil {
		return nil, err
	}
	return &table{Columns: searchColumns, Rows: rows}, nil
}
