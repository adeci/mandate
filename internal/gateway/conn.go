package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"
)

var viewDocs = map[string]string{
	"containers": "Chats, or guilds/channels/threads/DMs. kind: dm, group, guild, channel, thread. parent_id links channels and threads to their guild.",
	"messages":   "Messages. seq grows in the order messages are stored, not sent: poll with seq > :cursor for new ones, ORDER BY sent_at for chronology. Times are UTC ISO-8601 text. body is NULL and is_auth = 1 for messages that carry login codes or links.",
}

// Security model: client SQL may only read public views. Each public view
// passes through to a secret-named view that does the filtering; only secret
// views may read source tables or _hidden. The secret prefix is random per
// connection, so a CTE cannot impersonate a filtering view (the authorizer's
// "inner" argument is the CTE or view name).
type queryConn struct {
	conn    *sqlite3.Conn
	public  map[string]bool
	secret  map[string]bool
	sources []string
}

func openQueryConn(sources map[string]string, pol *policy) (_ *queryConn, err error) {
	conn, err := sqlite3.Open(":memory:")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	prefix := "_s" + hex.EncodeToString(nonce[:]) + "_"

	q := &queryConn{conn: conn, public: map[string]bool{}, secret: map[string]bool{}}

	if err := conn.CreateFunction("is_auth", 3, sqlite3.DETERMINISTIC|sqlite3.INNOCUOUS,
		func(ctx sqlite3.Context, arg ...sqlite3.Value) {
			ctx.ResultBool(pol.classifier.IsAuth(arg[0].Text(), arg[1].Text(), arg[2].Text()))
		}); err != nil {
		return nil, err
	}

	if err := conn.Exec(`CREATE TEMP TABLE _hidden (source TEXT NOT NULL, kind TEXT NOT NULL, value TEXT NOT NULL)`); err != nil {
		return nil, err
	}

	for _, name := range slices.Sorted(maps.Keys(sources)) {
		path := sources[name]
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue // ingester has not run yet
		}
		if err := attachReadOnly(conn, "src_"+name, path); err != nil {
			return nil, fmt.Errorf("attach %s: %w", name, err)
		}
		exp := pol.sources[name]
		if err := insertHidden(conn, name, "container", exp.HideContainers); err != nil {
			return nil, err
		}
		if err := insertHidden(conn, name, "sender", exp.HideSenders); err != nil {
			return nil, err
		}
		if err := q.createViews(name, prefix); err != nil {
			return nil, fmt.Errorf("views for %s: %w", name, err)
		}
		q.sources = append(q.sources, name)
	}

	if err := conn.Exec(`PRAGMA query_only = 1`); err != nil {
		return nil, err
	}
	conn.Limit(sqlite3.LIMIT_ATTACHED, len(q.sources))
	conn.Limit(sqlite3.LIMIT_SQL_LENGTH, maxSQLBytes)
	if err := conn.SetAuthorizer(q.authorize); err != nil {
		return nil, err
	}
	return q, nil
}

func attachReadOnly(conn *sqlite3.Conn, schema, path string) error {
	stmt, _, err := conn.Prepare(`ATTACH DATABASE ?1 AS ` + sqlite3.QuoteIdentifier(schema))
	if err != nil {
		return err
	}
	defer stmt.Close()
	if err := stmt.BindText(1, "file:"+path+"?mode=ro"); err != nil {
		return err
	}
	return stmt.Exec()
}

func insertHidden(conn *sqlite3.Conn, source, kind string, values []string) error {
	stmt, _, err := conn.Prepare(`INSERT INTO _hidden (source, kind, value) VALUES (?1, ?2, ?3)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, v := range values {
		if err := errors.Join(stmt.BindText(1, source), stmt.BindText(2, kind), stmt.BindText(3, v)); err != nil {
			return err
		}
		if err := stmt.Exec(); err != nil {
			return err
		}
		if err := stmt.Reset(); err != nil {
			return err
		}
	}
	return nil
}

func (q *queryConn) createViews(source, prefix string) error {
	src := sqlite3.QuoteIdentifier("src_" + source)
	lit := sqlite3.Quote(source)
	pubContainers, pubMessages := source+"_containers", source+"_messages"
	secContainers, secMessages := prefix+pubContainers, prefix+pubMessages

	hidden := func(kind string) string {
		return fmt.Sprintf(`(SELECT value FROM _hidden WHERE source = %s AND kind = %s)`, lit, sqlite3.Quote(kind))
	}
	stmts := []string{
		fmt.Sprintf(`CREATE TEMP VIEW %s AS
SELECT c.id, c.kind, c.parent_id, c.name, c.is_group, c.participants_json AS participants, c.updated_at
FROM %s.containers AS c
WHERE c.id NOT IN %s
  AND COALESCE(c.parent_id, '') NOT IN %s`,
			sqlite3.QuoteIdentifier(secContainers), src, hidden("container"), hidden("container")),

		fmt.Sprintf(`CREATE TEMP VIEW %s AS
SELECT m.seq, m.id, m.container_id, c.name AS container_name, c.kind AS container_kind,
       m.sender_id, m.sender_name, m.sent_at, m.is_from_me,
       CASE WHEN is_auth(%s, m.sender_id, m.body) THEN NULL ELSE m.body END AS body,
       is_auth(%s, m.sender_id, m.body) AS is_auth,
       m.media_type, m.reply_to_id, m.edited_at, m.deleted
FROM %s.messages AS m
JOIN %s AS c ON c.id = m.container_id
WHERE COALESCE(m.sender_id, '') NOT IN %s`,
			sqlite3.QuoteIdentifier(secMessages), lit, lit, src, sqlite3.QuoteIdentifier(secContainers), hidden("sender")),

		fmt.Sprintf(`CREATE TEMP VIEW %s AS SELECT * FROM %s`,
			sqlite3.QuoteIdentifier(pubContainers), sqlite3.QuoteIdentifier(secContainers)),
		fmt.Sprintf(`CREATE TEMP VIEW %s AS SELECT * FROM %s`,
			sqlite3.QuoteIdentifier(pubMessages), sqlite3.QuoteIdentifier(secMessages)),
	}
	for _, s := range stmts {
		if err := q.conn.Exec(s); err != nil {
			return err
		}
	}
	q.public[pubContainers], q.public[pubMessages] = true, true
	q.secret[secContainers], q.secret[secMessages] = true, true
	return nil
}

func (q *queryConn) authorize(action sqlite3.AuthorizerActionCode, name3rd, name4th, schema, inner string) sqlite3.AuthorizerReturnCode {
	switch action {
	case sqlite3.AUTH_SELECT, sqlite3.AUTH_RECURSIVE:
		return sqlite3.AUTH_OK
	case sqlite3.AUTH_FUNCTION:
		if deniedFunctions[strings.ToLower(name4th)] {
			return sqlite3.AUTH_DENY
		}
		return sqlite3.AUTH_OK
	case sqlite3.AUTH_READ:
		switch {
		case schema == "temp" && q.public[name3rd]:
			// The client reading a public view, directly or via its own CTE.
			return sqlite3.AUTH_OK
		case schema == "temp" && q.secret[name3rd] && (q.public[inner] || q.secret[inner]):
			// A public view expanding into its secret view, or a secret
			// view joining another. A CTE cannot be named after a secret
			// view without knowing the per-connection prefix.
			return sqlite3.AUTH_OK
		case q.secret[inner]:
			// Secret views read source tables and _hidden.
			return sqlite3.AUTH_OK
		}
	}
	return sqlite3.AUTH_DENY
}

var deniedFunctions = map[string]bool{
	"load_extension": true,
	"fts3_tokenizer": true,
	"readfile":       true,
	"writefile":      true,
}

type queryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
}

func (q *queryConn) run(ctx context.Context, sql string, params []any, maxRows int) (*queryResult, error) {
	old := q.conn.SetInterrupt(ctx)
	defer q.conn.SetInterrupt(old)

	stmt, tail, err := q.conn.Prepare(sql)
	if err != nil {
		return nil, err
	}
	if stmt == nil {
		return nil, errors.New("empty statement")
	}
	defer stmt.Close()
	if strings.TrimSpace(tail) != "" {
		return nil, errors.New("exactly one statement is allowed")
	}
	if !stmt.ReadOnly() {
		return nil, errors.New("only read-only statements are allowed")
	}
	if err := bindParams(stmt, params); err != nil {
		return nil, err
	}

	n := stmt.ColumnCount()
	res := &queryResult{Columns: make([]string, n), Rows: [][]any{}}
	for i := range n {
		res.Columns[i] = stmt.ColumnName(i)
	}
	for stmt.Step() {
		if len(res.Rows) == maxRows {
			res.Truncated = true
			break
		}
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
			default:
				row[i] = nil
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if err := stmt.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("query aborted: %w", ctxErr)
		}
		return nil, err
	}
	return res, nil
}

func bindParams(stmt *sqlite3.Stmt, params []any) error {
	for i, p := range params {
		idx := i + 1
		var err error
		switch v := p.(type) {
		case nil:
			err = stmt.BindNull(idx)
		case bool:
			err = stmt.BindBool(idx, v)
		case string:
			err = stmt.BindText(idx, v)
		case float64:
			if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
				err = stmt.BindInt64(idx, int64(v))
			} else {
				err = stmt.BindFloat(idx, v)
			}
		default:
			return fmt.Errorf("param %d: unsupported type %T", idx, p)
		}
		if err != nil {
			return fmt.Errorf("param %d: %w", idx, err)
		}
	}
	return nil
}

// describe lists the public views and their columns.
func (q *queryConn) describe() ([]viewInfo, error) {
	var out []viewInfo
	for _, source := range q.sources {
		for _, suffix := range []string{"containers", "messages"} {
			name := source + "_" + suffix
			stmt, _, err := q.conn.Prepare(`SELECT * FROM ` + sqlite3.QuoteIdentifier(name) + ` LIMIT 0`)
			if err != nil {
				return nil, err
			}
			cols := make([]string, stmt.ColumnCount())
			for i := range cols {
				cols[i] = stmt.ColumnName(i)
			}
			stmt.Close()
			out = append(out, viewInfo{Name: name, Source: source, Description: viewDocs[suffix], Columns: cols})
		}
	}
	return out, nil
}

type viewInfo struct {
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	Description string   `json:"description"`
	Columns     []string `json:"columns"`
}

func (q *queryConn) close() error { return q.conn.Close() }
