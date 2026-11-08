package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/adeci/mandate/store"
)

// filter prefixes every query with the chats and msgs an agent may see. ?1
// and ?2 are the never-shared chat and person IDs, ?3 the scope (see rule),
// all JSON arrays.
//
// A rule naming a chat covers that chat. A rule naming a holder (a server,
// space, forum) also covers every chat under it, so the owner can grant a
// whole server, new channels included; one naming a mailbox covers the
// chats filed directly in it, not its folders, as mail is filed into one
// mailbox and folders are picked apart. Both as filed when read. A rule
// with an until, an ended grant, shows only what it showed before its end;
// bounded spares reads, which carry none, the per-rule check.
//
// A DM's ID is not always its other person's (Discord channels, Matrix
// rooms), so hiding a person also matches DMs by participants and by who
// wrote in them. The unary + keeps SQLite driving both lookups from the
// hidden people.
//
// A chat's name is its path, its parents' names and its own ("main /
// #general"), whatever the grant covers: names are not content, and the
// path tells twins apart. Three ancestors are joined by primary key, far
// cheaper than a recursive walk per query; a deeper chain is left out, so
// every ancestor is checked.
//
// Discord guilds were stored under their bare snowflake before they became
// guild:ID, so a never-share entry made then names the guild that way.
//
// The login-code check reads file names too, as senders name a file after
// the code; deleted bodies stay in the store for restore and never show.
const filter = `WITH
hidden_person(id) AS (SELECT value FROM json_each(?2)),
hidden(id) AS (
  SELECT value FROM json_each(?1)
  UNION SELECT 'guild:' || value FROM json_each(?1)
  UNION SELECT id FROM hidden_person
  UNION SELECT c.id FROM hidden_person AS s CROSS JOIN containers AS c CROSS JOIN json_each(c.participants_json) AS p
    WHERE +c.kind = 'dm' AND p.value ->> 'id' = s.id
  UNION SELECT container_id FROM messages
    WHERE sender_id IN (SELECT id FROM hidden_person) AND NOT is_from_me
      AND +container_id IN (SELECT id FROM containers WHERE kind = 'dm')),
rule(cutoff, until, chats) AS (SELECT value ->> 'cutoff', COALESCE(value ->> 'until', ''), value ->> 'chats' FROM json_each(?3)),
named AS (
  SELECT c.id, c.kind, c.parent_id, c.last_message_at, c.name AS own,
         iif(c.kind = 'dm', c.participants_json ->> '$[0].handle') AS handle,
         iif(p1.kind = 'mailbox' AND c.kind != 'mailbox' OR p1.kind IN ` + store.HolderKinds + `, p1.id, NULL) AS via1,
         iif(p2.kind IN ` + store.HolderKinds + `, p2.id, NULL) AS via2,
         iif(p3.kind IN ` + store.HolderKinds + `, p3.id, NULL) AS via3,
         concat_ws(' / ', p3.name, p2.name, p1.name) AS prefix
  FROM containers AS c
  LEFT JOIN containers AS p1 ON p1.id = c.parent_id
  LEFT JOIN containers AS p2 ON p2.id = p1.parent_id
  LEFT JOIN containers AS p3 ON p3.id = p2.parent_id
  WHERE p3.parent_id IS NULL
    AND c.id NOT IN (SELECT id FROM hidden)
    AND COALESCE(c.parent_id, '') NOT IN (SELECT id FROM hidden)
    AND COALESCE(p1.parent_id, '') NOT IN (SELECT id FROM hidden)
    AND COALESCE(p2.parent_id, '') NOT IN (SELECT id FROM hidden)),
covered AS (
  SELECT n.id, n.kind, n.parent_id, n.last_message_at, n.via1, n.via2, n.via3, n.own, n.handle,
         iif(n.prefix = '', n.own, n.prefix || ' / ' || COALESCE(n.own, n.id)) AS name,
         (SELECT min(g.cutoff) FROM rule AS g
          WHERE g.chats IS NULL OR EXISTS (SELECT 1 FROM json_each(g.chats) AS k WHERE k.value IN (n.id, n.via1, n.via2, n.via3))) AS cutoff,
         EXISTS (SELECT 1 FROM rule AS g
          WHERE g.until != '' AND (g.chats IS NULL OR EXISTS (SELECT 1 FROM json_each(g.chats) AS k WHERE k.value IN (n.id, n.via1, n.via2, n.via3)))) AS bounded
  FROM named AS n),
chats AS (
  SELECT id, kind, parent_id, last_message_at, via1, via2, via3, cutoff, bounded, iif(name_code, '[login code hidden]', name) AS name,
         iif(name_code, NULL, own) AS own, iif(name_code, NULL, handle) AS handle, name_code
  FROM (SELECT *, name IS NOT NULL AND login_code(name) AS name_code FROM covered WHERE cutoff IS NOT NULL)),
stored AS (
  SELECT m.*, COALESCE(
    (SELECT iif(count(*) = 1, '[' || type || COALESCE(' ' || name, '') || ']',
                group_concat('[#' || idx || ' ' || type || COALESCE(' ' || name, '') || ']', ' ' ORDER BY idx))
     FROM media WHERE message_id = m.id),
    '[' || m.media_type || ']') AS files
  FROM messages AS m JOIN chats AS c ON c.id = m.container_id
  WHERE COALESCE(m.sender_id, '') NOT IN (SELECT id FROM hidden_person) AND m.sent_at >= c.cutoff
    AND (NOT c.bounded OR EXISTS (SELECT 1 FROM rule AS g
      WHERE (g.chats IS NULL OR EXISTS (SELECT 1 FROM json_each(g.chats) AS k WHERE k.value IN (c.id, c.via1, c.via2, c.via3)))
        AND m.sent_at >= g.cutoff AND (g.until = '' OR m.sent_at < g.until)))),
visible AS (
  SELECT *, login_code(concat_ws(' ', files, body)) AS code FROM stored),
msgs AS (
  SELECT seq, id, container_id, sent_at, strftime('%Y-%m-%d %H:%M', sent_at) AS at, reply_to_id,
         sender_id, CASE WHEN is_from_me THEN 'me' ELSE sender_name END AS sender,
         code, deleted,
         CASE WHEN code OR deleted THEN NULL ELSE body END AS body,
         CASE WHEN code THEN '[login code hidden]'
              WHEN deleted THEN '[deleted]'
              ELSE concat_ws(' ', files, NULLIF(body, '')) END AS text
  FROM visible)
`

// rule is one grant in the filter's scope. Chats nil covers every chat;
// Cutoff is the earliest sent_at shown, "" for all history, and Until the
// first one no longer shown, "" for no bound.
type rule struct {
	Chats  []string `json:"chats,omitempty"`
	Cutoff string   `json:"cutoff"`
	Until  string   `json:"until,omitempty"`
}

// everything is the scope of every chat never-share leaves visible, for
// finding the chats an agent asks for.
var everything = []rule{{}}

// table is a read's answer.
type table struct {
	Columns    []string `json:"columns"`
	Rows       [][]any  `json:"rows"`
	More       bool     `json:"more,omitempty"`
	NextBefore int64    `json:"next_before,omitempty"`
	// Containers counts the visible chats a read op stored.
	Containers int `json:"containers,omitempty"`
}

var (
	chatColumns    = []string{"id", "kind", "name", "last_message"}
	messageColumns = []string{"seq", "sent_at", "sender_id", "sender", "reply_to", "body"}
	searchColumns  = []string{"seq", "sent_at", "chat_id", "chat", "sender_id", "sender", "reply_to", "body"}
)

// view is a connector's store as one scope shows it at one time. A nil db
// is a store the connector has not made yet, which holds nothing.
type view struct {
	db                        *sql.DB
	hideChats, hidePeople, sc string
	// now dates chats for store.Matching, in RFC 3339.
	now string
}

func newView(db *sql.DB, ns neverShared, scope []rule, now time.Time) (*view, error) {
	v := &view{db: db, now: now.UTC().Format(time.RFC3339Nano)}
	// A JSON null would be one NULL row in json_each, and NOT IN a list
	// holding NULL is never true.
	for _, p := range []struct {
		dst *string
		val []string
	}{{&v.hideChats, ns.chats}, {&v.hidePeople, ns.people}} {
		b, err := json.Marshal(append([]string{}, p.val...))
		if err != nil {
			return nil, err
		}
		*p.dst = string(b)
	}
	b, err := json.Marshal(append([]rule{}, scope...))
	if err != nil {
		return nil, err
	}
	v.sc = string(b)
	return v, nil
}

// query runs filter plus q, which reads chats and msgs; args bind from ?4.
func (v *view) query(ctx context.Context, q string, args ...any) ([][]any, error) {
	out := [][]any{}
	if v.db == nil {
		return out, nil
	}
	rows, err := v.db.QueryContext(ctx, filter+q, append([]any{v.hideChats, v.hidePeople, v.sc}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (v *view) one(ctx context.Context, q string, args ...any) (bool, error) {
	rows, err := v.query(ctx, q, args...)
	return len(rows) > 0, err
}

func (v *view) chatVisible(ctx context.Context, id string) (bool, error) {
	return v.one(ctx, `SELECT 1 FROM chats WHERE id = ?4`, id)
}

// messageVisible says whether the message with this stored ID shows its body.
func (v *view) messageVisible(ctx context.Context, id string) (bool, error) {
	return v.one(ctx, `SELECT 1 FROM msgs WHERE id = ?4 AND NOT code AND NOT deleted`, id)
}

// seqVisible says whether message seq in chat shows its body.
func (v *view) seqVisible(ctx context.Context, chat string, seq int64) (bool, error) {
	return v.one(ctx, `SELECT 1 FROM msgs WHERE container_id = ?4 AND seq = ?5 AND NOT code AND NOT deleted`, chat, seq)
}

// visibleCount counts the visible chats among ids.
func (v *view) visibleCount(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return 0, err
	}
	rows, err := v.query(ctx, `SELECT count(*) FROM chats WHERE id IN (SELECT value FROM json_each(?4))`, string(b))
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	n, _ := rows[0][0].(int64)
	return int(n), nil
}

// names maps the visible chats among ids to their names.
func (v *view) names(ctx context.Context, ids []string) (map[string]string, error) {
	b, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := v.query(ctx, `SELECT id, COALESCE(name, '') FROM chats WHERE id IN (SELECT value FROM json_each(?4))`, string(b))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		id, _ := r[0].(string)
		out[id], _ = r[1].(string)
	}
	return out, nil
}

// page trims the extra row queried past limit; seqCol is the seq column or -1.
func page(t *table, rows [][]any, limit int64, seqCol int) *table {
	if int64(len(rows)) > limit {
		rows = rows[:limit]
		t.More = true
		if seqCol >= 0 {
			t.NextBefore, _ = rows[len(rows)-1][seqCol].(int64)
		}
	}
	t.Rows = rows
	return t
}

// replyTo is the seq of the visible message m answers.
const replyTo = `(SELECT r.seq FROM msgs AS r WHERE r.id = m.reply_to_id)`

// listed is the chats as listings show them: with activity at or after
// their cutoff, the later of the newest visible message and what the
// connector reported. A chat whose stored messages are all hidden, login
// codes or deleted is left out, as connectors name some chats after a
// message (mail threads).
const listed = `, listed AS MATERIALIZED (
  SELECT * FROM (
    SELECT c.id, c.kind, c.parent_id, c.name, c.own, c.handle, c.name_code, c.cutoff, NULLIF(max(
      COALESCE((SELECT max(sent_at) FROM msgs WHERE container_id = c.id), ''),
      COALESCE(c.last_message_at, '')), '') AS at
    FROM chats AS c
    WHERE EXISTS (SELECT 1 FROM msgs WHERE container_id = c.id AND NOT code AND NOT deleted)
       OR NOT EXISTS (SELECT 1 FROM messages WHERE container_id = c.id))
  WHERE cutoff = '' OR at >= cutoff)
`

// chats lists the chats an agent may read, newest first, or those search
// finds as a request's term would, in that order. Holders have nothing to
// read, so they are left out.
func (v *view) chats(ctx context.Context, search string, limit int64) (*table, error) {
	if search == "" {
		rows, err := v.query(ctx, listed+`SELECT id, kind, name, strftime('%Y-%m-%d %H:%M', at) FROM listed
WHERE kind NOT IN `+store.HolderKinds+`
ORDER BY at DESC NULLS LAST, id LIMIT ?4`, limit+1)
		if err != nil {
			return nil, err
		}
		return page(&table{Columns: chatColumns}, rows, limit, -1), nil
	}
	ts, err := store.Terms([]string{search})
	if err != nil {
		return nil, err
	}
	// Without holders, a server's name finds the chats in it by their path.
	rows, err := v.query(ctx, listed+`, candidates AS (
  SELECT id, kind, parent_id, own, iif(name_code, '', name) AS path, handle, at FROM listed WHERE kind NOT IN `+store.HolderKinds+`),
`+store.Matching("?4", "?6")+`
SELECT l.id, l.kind, l.name, strftime('%Y-%m-%d %H:%M', l.at) FROM matched AS m JOIN listed AS l ON l.id = m.id
ORDER BY m.n LIMIT ?5`, ts, limit+1, v.now)
	if err != nil {
		return nil, err
	}
	return page(&table{Columns: chatColumns}, rows, limit, -1), nil
}

// matches finds up to per chats for each term, as store.Matching does, in
// one query: rows of term index, id and name. Resolving every term at once
// keeps a request's cost from growing with what each term matches.
func (v *view) matches(ctx context.Context, terms []string, per int64) ([][]any, error) {
	ts, err := store.Terms(terms)
	if err != nil {
		return nil, err
	}
	return v.query(ctx, listed+`, candidates AS (
  SELECT id, kind, parent_id, own, iif(name_code, '', name) AS path, handle, at FROM listed),
`+store.Matching("?4", "?6")+`
SELECT m.k, m.id, l.name, l.kind FROM matched AS m JOIN listed AS l ON l.id = m.id
WHERE m.n <= ?5 ORDER BY m.k, m.n`, ts, per, v.now)
}

// before is the seq of the last message seen; the page continues after it in
// (sent_at, seq) order, since seq follows ingestion, not sending.
func (v *view) messages(ctx context.Context, chat string, before, limit int64) (*table, error) {
	rows, err := v.query(ctx, `SELECT m.seq, m.at, m.sender_id, m.sender, `+replyTo+`, m.text FROM msgs AS m
WHERE m.container_id = ?4 AND (?5 = 0 OR (m.sent_at, m.seq) < (SELECT sent_at, seq FROM msgs WHERE seq = ?5))
ORDER BY m.sent_at DESC, m.seq DESC LIMIT ?6`, chat, before, limit+1)
	if err != nil {
		return nil, err
	}
	return page(&table{Columns: messageColumns}, rows, limit, 0), nil
}

const searchSelect = `SELECT m.seq, m.at, m.container_id, c.name, m.sender_id, m.sender, ` + replyTo + `, m.text`

// search matches the shown body, where login codes are blank; before as in
// messages.
func (v *view) search(ctx context.Context, text, chat, since string, before, limit int64) (*table, error) {
	rows, err := v.query(ctx, searchSelect+` FROM msgs AS m JOIN chats AS c ON c.id = m.container_id
WHERE instr(lower(m.body), lower(?4)) AND (?5 = '' OR m.container_id = ?5) AND m.sent_at >= ?6
  AND (?7 = 0 OR (m.sent_at, m.seq) < (SELECT sent_at, seq FROM msgs WHERE seq = ?7))
ORDER BY m.sent_at DESC, m.seq DESC LIMIT ?8`, text, chat, since, before, limit+1)
	if err != nil {
		return nil, err
	}
	return page(&table{Columns: searchColumns}, rows, limit, 0), nil
}

// byID reads a read op's message IDs back in the connector's order.
func (v *view) byID(ctx context.Context, ids []string) (*table, error) {
	b, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := v.query(ctx, searchSelect+` FROM json_each(?4) AS j JOIN msgs AS m ON m.id = j.value
JOIN chats AS c ON c.id = m.container_id ORDER BY j.key`, string(b))
	if err != nil {
		return nil, err
	}
	return &table{Columns: searchColumns, Rows: rows}, nil
}

// file is what the gateway knows of a visible message whose file an agent
// asks for.
type file struct {
	message string
	hidden  bool  // a login code or deleted
	marked  bool  // media_type is set
	count   int64 // stored files
	size    int64 // of the file asked for; 0 if unknown or not stored
}

// file looks up message seq in chat; nil if the scope does not show it.
func (v *view) file(ctx context.Context, chat string, seq, index int64) (*file, error) {
	rows, err := v.query(ctx, `SELECT id, code OR deleted, media_type IS NOT NULL,
  (SELECT count(*) FROM media WHERE message_id = m.id),
  COALESCE((SELECT size FROM media WHERE message_id = m.id AND idx = ?6), 0)
FROM visible AS m WHERE container_id = ?4 AND seq = ?5`, chat, seq, index)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	f := &file{}
	f.message, _ = r[0].(string)
	hidden, _ := r[1].(int64)
	marked, _ := r[2].(int64)
	f.hidden, f.marked = hidden != 0, marked != 0
	f.count, _ = r[3].(int64)
	f.size, _ = r[4].(int64)
	return f, nil
}
