package main

import (
	"context"
	"database/sql"
)

// migrate repairs a store from before guilds had their own chat IDs. A
// guild's first channel may share the guild's snowflake, and the channel's
// row overwrote the guild's with itself as parent. A channel's parent is
// always a guild, so the guilds are the guild rows and the channels'
// parents; a thread's parent is its channel, if there is one by that ID.
// It also drops the channels the account may not view, which hold nothing.
// Every step finds nothing to do once done.
func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`CREATE TEMP TABLE old_guilds AS
SELECT id FROM containers WHERE kind = 'guild' AND id NOT LIKE 'guild:%'
UNION SELECT parent_id FROM containers WHERE kind = 'channel' AND parent_id NOT LIKE 'guild:%'`,
		`INSERT OR IGNORE INTO containers (id, kind, name, updated_at)
SELECT 'guild:' || g.id, 'guild', (SELECT name FROM containers WHERE id = g.id AND kind = 'guild'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
FROM old_guilds AS g`,
		`UPDATE containers SET parent_id = 'guild:' || parent_id
WHERE parent_id IN (SELECT id FROM old_guilds)
  AND (kind = 'channel' OR NOT EXISTS (SELECT 1 FROM containers AS c WHERE c.id = containers.parent_id AND c.kind = 'channel'))`,
		`DELETE FROM containers WHERE kind = 'guild' AND id IN (SELECT id FROM old_guilds)`,
		`DROP TABLE old_guilds`,
		`DELETE FROM containers WHERE kind = 'channel' AND name = '#` + hiddenName + `'
  AND NOT EXISTS (SELECT 1 FROM messages WHERE container_id = containers.id)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}
