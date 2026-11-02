package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"time"
)

// Listed is a container as ListContainers reports it.
type Listed struct {
	ID, Kind, ParentID, Name string
	// LastActivity is the newer of the service's report and the newest
	// stored message; zero if neither is known.
	LastActivity time.Time
}

// ListContainers lists up to limit containers of the database at path,
// most recently active first: those whose name contains q (any case) or
// whose ID is q, and, unless parent is empty, whose parent is parent. A
// missing database has none. It opens path read-only, so it may run beside
// the Store that writes it.
func ListContainers(ctx context.Context, path, q, parent string, limit int) ([]Listed, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rs, err := db.QueryContext(ctx, `
SELECT id, kind, COALESCE(parent_id, ''), COALESCE(name, ''),
       max(COALESCE(last_message_at, ''), COALESCE((SELECT max(sent_at) FROM messages WHERE container_id = c.id), '')) AS active
FROM containers AS c
WHERE (?1 = '' OR id = ?1 OR instr(lower(COALESCE(name, '')), lower(?1)))
  AND (?2 = '' OR parent_id = ?2)
ORDER BY active DESC, id
LIMIT ?3`, q, parent, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Listed
	for rs.Next() {
		var (
			c      Listed
			active string
		)
		if err := rs.Scan(&c.ID, &c.Kind, &c.ParentID, &c.Name, &active); err != nil {
			return nil, err
		}
		if active != "" {
			if c.LastActivity, err = time.Parse(time.RFC3339Nano, active); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	return out, rs.Err()
}
