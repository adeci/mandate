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
// children and DMs with hidden senders; msgs leaves out hidden senders and
// blanks login-code bodies. ?1 and ?2 are the hidden container and sender
// IDs as JSON arrays.
const filter = `WITH
hidden_chat(id) AS (SELECT value FROM json_each(?1)),
hidden_sender(id) AS (SELECT value FROM json_each(?2)),
chats AS (
  SELECT id, kind, name, updated_at FROM containers
  WHERE id NOT IN (SELECT id FROM hidden_chat)
    AND id NOT IN (SELECT id FROM hidden_sender)
    AND COALESCE(parent_id, '') NOT IN (SELECT id FROM hidden_chat)),
msgs AS (
  SELECT m.seq, m.id, m.container_id, m.sent_at, COALESCE(m.sender_name, m.sender_id) AS sender,
         CASE WHEN is_auth(m.sender_id, m.body) THEN NULL ELSE m.body END AS body
  FROM messages AS m JOIN chats AS c ON c.id = m.container_id
  WHERE COALESCE(m.sender_id, '') NOT IN (SELECT id FROM hidden_sender))
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
	chatColumns    = []string{"id", "kind", "name", "updated_at"}
	messageColumns = []string{"seq", "sent_at", "sender", "body"}
	searchColumns  = []string{"seq", "sent_at", "chat", "sender", "body"}
)

func (s *source) chats(search string, limit int64) (*table, error) {
	rows, err := s.query(`SELECT id, kind, name, updated_at FROM chats
WHERE ?3 = '' OR instr(lower(COALESCE(name, '')), lower(?3)) OR instr(lower(id), lower(?3))
ORDER BY updated_at DESC, id LIMIT ?4`, search, limit+1)
	if err != nil {
		return nil, err
	}
	return page(&table{Columns: chatColumns}, rows, limit, -1), nil
}

func (s *source) messages(chat string, before, limit int64) (*table, error) {
	rows, err := s.query(`SELECT seq, sent_at, sender, body FROM msgs
WHERE container_id = ?3 AND (?4 = 0 OR seq < ?4)
ORDER BY seq DESC LIMIT ?5`, chat, before, limit+1)
	if err != nil {
		return nil, err
	}
	return page(&table{Columns: messageColumns}, rows, limit, 0), nil
}

// Matches the filtered body, where login codes are blank.
func (s *source) search(text, chat, since string, before, limit int64) (*table, error) {
	rows, err := s.query(`SELECT seq, sent_at, container_id, sender, body FROM msgs
WHERE instr(lower(body), lower(?3)) AND (?4 = '' OR container_id = ?4) AND sent_at >= ?5 AND (?6 = 0 OR seq < ?6)
ORDER BY seq DESC LIMIT ?7`, text, chat, since, before, limit+1)
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
	rows, err := s.query(`SELECT m.seq, m.sent_at, m.container_id, m.sender, m.body
FROM json_each(?3) AS j JOIN msgs AS m ON m.id = j.value ORDER BY j.key`, string(b))
	if err != nil {
		return nil, err
	}
	return &table{Columns: searchColumns, Rows: rows}, nil
}
