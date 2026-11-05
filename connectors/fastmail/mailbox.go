package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/adeci/mandate/store"
)

const kindMailbox = "mailbox"

// mailboxChat is a mailbox's chat ID. JMAP IDs are unique per type only,
// and ":" is not in their alphabet, so it never is a thread's.
func mailboxChat(id string) string { return "mailbox:" + id }

// storeMailboxes stores every mailbox as a chat under its parent. A thread
// adds a level under its mailbox and the store allows three ancestors, so
// a folder deeper than two levels goes under the second.
func (c *fmConnector) storeMailboxes(ctx context.Context, boxes []mailbox) error {
	parent := make(map[string]string, len(boxes))
	for _, m := range boxes {
		parent[m.ID] = m.ParentID
	}
	cs := make([]store.Container, 0, len(boxes))
	var roots []string
	for _, m := range boxes {
		var up []string // its ancestors, the nearest first
		for p := m.ParentID; p != "" && len(up) < len(boxes); p = parent[p] {
			if _, ok := parent[p]; !ok {
				break
			}
			up = append(up, p)
		}
		ct := store.Container{ID: mailboxChat(m.ID), Kind: kindMailbox, Name: m.Name}
		switch {
		case len(up) == 0:
			roots = append(roots, ct.ID)
		case len(up) <= 2:
			ct.ParentID = mailboxChat(up[0])
		default:
			ct.ParentID = mailboxChat(up[len(up)-2])
		}
		cs = append(cs, ct)
	}
	if err := c.st.UpsertContainers(ctx, cs); err != nil {
		return err
	}
	// An upsert keeps a parent; a folder moved to the top has none.
	b, err := json.Marshal(roots)
	if err != nil {
		return err
	}
	_, err = c.st.DB().ExecContext(ctx, `UPDATE containers SET parent_id = NULL
WHERE kind = 'mailbox' AND parent_id IS NOT NULL AND id IN (SELECT value FROM json_each(?1))`, string(b))
	return err
}

// filing is the mailbox e is filed in, and its rank: a folder before Trash
// or Spam, before Sent or Drafts, which an email shares with its folder
// when sent to oneself.
func (cn *conn) filing(e email) (string, int) {
	best, rank := "", 3
	for id, in := range e.MailboxIDs {
		r := 0
		switch id {
		case cn.roles["trash"], cn.roles["junk"]:
			r = 1
		case cn.roles["sent"], cn.roles["drafts"]:
			r = 2
		}
		if in && (r < rank || r == rank && id < best) {
			best, rank = id, r
		}
	}
	return best, rank
}

// file records the mailbox each of list is filed in and files their threads
// anew, in one transaction: each commit syncs the disk.
func (c *fmConnector) file(ctx context.Context, cn *conn, list []email) error {
	tx, err := c.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var threads []string
	for _, e := range list {
		box, rank := cn.filing(e)
		if box == "" || e.ThreadID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO fastmail_filed (id, thread_id, mailbox, rank, received_at) VALUES (?1, ?2, ?3, ?4, ?5)`,
			e.ID, e.ThreadID, mailboxChat(box), rank, utcDate(e.ReceivedAt)); err != nil {
			return err
		}
		threads = append(threads, e.ThreadID)
	}
	slices.Sort(threads)
	for _, t := range slices.Compact(threads) {
		if err := refile(ctx, tx, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// unfile forgets a destroyed email and files its thread anew.
func (c *fmConnector) unfile(ctx context.Context, id string) error {
	var thread string
	err := c.st.DB().QueryRowContext(ctx, `DELETE FROM fastmail_filed WHERE id = ?1 RETURNING thread_id`, id).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	return refile(ctx, c.st.DB(), thread)
}

// refile puts a stored thread under the mailbox of its newest email of the
// best rank: where its latest mail not sent by the owner is, else Sent.
func refile(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, thread string,
) error {
	_, err := db.ExecContext(ctx, `
UPDATE containers SET parent_id = (
  SELECT mailbox FROM fastmail_filed WHERE thread_id = ?1 ORDER BY rank, received_at DESC, id DESC LIMIT 1)
WHERE id = ?1 AND kind = 'thread'`, thread)
	return err
}

// fileStored files, once, the emails stored before filing was recorded.
func (c *fmConnector) fileStored(ctx context.Context, a *account, cn *conn) error {
	var done bool
	err := c.st.DB().QueryRowContext(ctx, `SELECT 1 FROM fastmail_filed_backfill WHERE account_id = ?1`, cn.accountID).Scan(&done)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := c.st.DB().QueryContext(ctx, `SELECT id FROM messages WHERE id NOT IN (SELECT id FROM fastmail_filed)`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for batch := range slices.Chunk(ids, getBatch) {
		list, err := c.getEmails(ctx, a, cn, batch, []string{"id", "threadId", "mailboxIds", "receivedAt"}, 0)
		if err != nil {
			return err
		}
		if err := c.file(ctx, cn, list); err != nil {
			return err
		}
	}
	_, err = c.st.DB().ExecContext(ctx, `INSERT INTO fastmail_filed_backfill (account_id) VALUES (?1)`, cn.accountID)
	return err
}
