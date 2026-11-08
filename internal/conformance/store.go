package conformance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/adeci/mandate/store"
)

// specTables are the store's tables as connector.md declares them, one
// column per string: name, type, NOT NULL, key (part of the primary key).
var specTables = map[string][]string{
	"containers": {
		"id TEXT key", "kind TEXT NOT NULL", "parent_id TEXT", "name TEXT",
		"is_group INTEGER NOT NULL", "participants_json TEXT NOT NULL",
		"updated_at TEXT NOT NULL", "last_message_at TEXT", "icon TEXT",
	},
	"messages": {
		"seq INTEGER key", "id TEXT NOT NULL", "container_id TEXT NOT NULL",
		"sender_id TEXT", "sender_name TEXT", "sent_at TEXT NOT NULL",
		"is_from_me INTEGER NOT NULL", "body TEXT", "media_type TEXT",
		"reply_to_id TEXT", "edited_at TEXT", "deleted INTEGER NOT NULL",
		"ingested_at TEXT NOT NULL",
	},
	"media": {
		"message_id TEXT NOT NULL key", "idx INTEGER NOT NULL key", "type TEXT NOT NULL",
		"name TEXT", "size INTEGER", "ref BLOB",
	},
}

func openStore(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite3", "file:"+path+"?mode=ro")
}

func (k *kit) checkSchema(path string) error {
	db, err := openStore(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var v int
	if err := db.QueryRowContext(k.ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v != store.Version {
		return fmt.Errorf("user_version %d, not %d", v, store.Version)
	}
	tables, err := query(k.ctx, db, `SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return err
	}
	for _, t := range tables {
		_, spec := specTables[t[0]]
		if !spec && !strings.HasPrefix(t[0], "sqlite_") && !strings.HasPrefix(t[0], k.prefix) {
			return fmt.Errorf("table %s is neither the spec's nor named %s…", t[0], k.prefix)
		}
	}
	for table, want := range specTables {
		rows, err := query(k.ctx, db, `SELECT name, type, "notnull", pk FROM pragma_table_info(?1)`, table)
		if err != nil {
			return err
		}
		var got []string
		for _, r := range rows {
			c := r[0] + " " + strings.ToUpper(r[1])
			if r[2] == "1" {
				c += " NOT NULL"
			}
			if r[3] != "0" {
				c += " key"
			}
			got = append(got, c)
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("table %s has columns %q, not %q", table, got, want)
		}
	}
	return nil
}

// query returns every row as strings; NULL is "".
func query(ctx context.Context, db *sql.DB, q string, args ...any) ([][]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		row := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		strs := make([]string, len(cols))
		for i, s := range row {
			strs[i] = s.String
		}
		out = append(out, strs)
	}
	return out, rows.Err()
}

// storeRules checks an existing store against the rules of connector.md
// that its contents show.
func (k *kit) storeRules(path string) {
	k.check("store "+path+": schema and user_version", k.checkSchema(path))
	db, err := openStore(path)
	if err != nil {
		k.check("store "+path+": open", err)
		return
	}
	defer db.Close()
	k.check("store "+path+": times", checkTimes(k.ctx, db))
	k.check("store "+path+": chat nesting", checkNesting(k.ctx, db))
	k.check("store "+path+": ~ names", checkNames(k.ctx, db))
}

const storeTime = "2006-01-02T15:04:05.000Z"

func checkTimes(ctx context.Context, db *sql.DB) error {
	for _, c := range [][2]string{
		{"containers", "updated_at"},
		{"containers", "last_message_at"},
		{"messages", "sent_at"},
		{"messages", "edited_at"},
		{"messages", "ingested_at"},
	} {
		rows, err := query(ctx, db, fmt.Sprintf(`SELECT id, %s FROM %s WHERE %[1]s IS NOT NULL`, c[1], c[0]))
		if err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := time.Parse(storeTime, r[1]); err != nil {
				return fmt.Errorf("%s.%s of %s is %q, not like %s", c[0], c[1], r[0], r[1], storeTime)
			}
		}
	}
	return nil
}

// checkNesting wants parent_id chains acyclic and at most three ancestors
// long, as far as the gateway follows them.
func checkNesting(ctx context.Context, db *sql.DB) error {
	rows, err := query(ctx, db, `SELECT id, parent_id FROM containers WHERE parent_id <> '' ORDER BY id`)
	if err != nil {
		return err
	}
	parent := make(map[string]string, len(rows))
	for _, r := range rows {
		parent[r[0]] = r[1]
	}
	for _, r := range rows {
		seen := map[string]bool{r[0]: true}
		for n, id := 0, r[0]; parent[id] != ""; n++ {
			id = parent[id]
			switch {
			case seen[id]:
				return fmt.Errorf("chat %s is its own ancestor", id)
			case n == 3:
				return fmt.Errorf("chat %s has more than three ancestors", r[0])
			}
			seen[id] = true
		}
	}
	return nil
}

// checkNames wants a name after every ~ marker: not none, a space or a
// second marker. Whether a name was chosen by its person is the service's
// to know. A handle is the service's own, so it is never marked.
func checkNames(ctx context.Context, db *sql.DB) error {
	rows, err := query(ctx, db, `SELECT 'chat ' || id, name FROM containers
		UNION ALL SELECT 'sender of ' || id, sender_name FROM messages`)
	if err != nil {
		return err
	}
	parts, err := query(ctx, db, `SELECT id, participants_json FROM containers`)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range parts {
		var ps []store.Participant
		if err := json.Unmarshal([]byte(r[1]), &ps); err != nil {
			return fmt.Errorf("participants_json of chat %s: %w", r[0], err)
		}
		for _, p := range ps {
			rows = append(rows, []string{"participant " + p.ID + " of chat " + r[0], p.Name})
			if strings.HasPrefix(p.Handle, "~") {
				errs = append(errs, fmt.Errorf("participant %s of chat %s has the handle %q", p.ID, r[0], p.Handle))
			}
		}
	}
	for _, r := range rows {
		rest, marked := strings.CutPrefix(r[1], "~")
		if marked && (rest == "" || strings.HasPrefix(rest, "~") || unicode.IsSpace([]rune(rest)[0])) {
			errs = append(errs, fmt.Errorf("%s is named %q", r[0], r[1]))
		}
	}
	if len(errs) > 3 {
		errs = append(errs[:3], fmt.Errorf("and %d more", len(errs)-3))
	}
	return errors.Join(errs...)
}
