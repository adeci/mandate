package main

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/adeci/mandate/store"
)

const (
	// The first sync stores the last 90 days, at most 2000 emails: what an
	// agent is asked about is mostly recent, and a busy inbox stays tens of
	// megabytes at worst. Updated mail older than the window stays out too.
	window     = 90 * 24 * time.Hour
	maxInitial = 2000
	getBatch   = 50
	maxChanges = 500

	// Bodies are stored up to maxText; fetch reads up to maxFetch. HTML is
	// fetched at four times that, as markup outweighs the text it holds.
	maxText  = 16 << 10
	maxFetch = 1 << 20

	kindThread = "thread"
)

const schema = `
CREATE TABLE IF NOT EXISTS fastmail_sync (
    account_id  TEXT PRIMARY KEY,
    email_state TEXT NOT NULL
);
-- What answering a stored email needs.
CREATE TABLE IF NOT EXISTS fastmail_emails (
    id         TEXT PRIMARY KEY,
    message_id TEXT NOT NULL,
    refs       TEXT NOT NULL, -- the References of a reply to it
    subject    TEXT NOT NULL,
    reply_to   TEXT NOT NULL, -- where a reply goes, comma-separated
    recipients TEXT NOT NULL  -- its To and Cc, lowercase, comma-separated
);
-- Accounts whose emails stored before attachments were recorded have them.
CREATE TABLE IF NOT EXISTS fastmail_backfill (
    account_id TEXT PRIMARY KEY
);
CREATE INDEX IF NOT EXISTS fastmail_emails_message_id ON fastmail_emails(message_id);
`

// run syncs a every poll interval, or when kicked, until ctx is done or
// Fastmail refuses the token.
func (c *fmConnector) run(ctx context.Context, a *account) {
	for {
		err := c.cycle(ctx, a)
		if ctx.Err() != nil {
			return
		}
		a.mu.Lock()
		a.err = err
		a.mu.Unlock()
		switch {
		case errors.Is(err, errUnauthorized):
			c.log.Warn("Fastmail refused the API token; unlink and link a new one")
			return
		case err != nil:
			c.log.Warn("sync", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-a.kick:
		case <-time.After(c.poll):
		}
	}
}

func (c *fmConnector) cycle(ctx context.Context, a *account) error {
	cn, _, _ := a.state()
	var err error
	if cn == nil {
		cn, err = c.connect(ctx, a)
	} else {
		cn, err = c.refresh(ctx, a, cn)
	}
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.conn = cn
	a.mu.Unlock()
	if err := c.backfill(ctx, a, cn); err != nil {
		return err
	}
	var state string
	err = c.st.DB().QueryRowContext(ctx, `SELECT email_state FROM fastmail_sync WHERE account_id = ?1`, cn.accountID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return c.initial(ctx, a, cn)
	} else if err != nil {
		return err
	}
	return c.changes(ctx, a, cn, state)
}

func (c *fmConnector) saveState(ctx context.Context, account, state string) error {
	_, err := c.st.DB().ExecContext(ctx, `
INSERT INTO fastmail_sync (account_id, email_state) VALUES (?1, ?2)
ON CONFLICT(account_id) DO UPDATE SET email_state = excluded.email_state`, account, state)
	return err
}

// initial stores the recent window, oldest first so replies find what they
// answer, and remembers the state from before the query.
func (c *fmConnector) initial(ctx context.Context, a *account, cn *conn) error {
	a.mu.Lock()
	a.syncing = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.syncing = false
		a.mu.Unlock()
	}()
	filter := map[string]any{"after": utcDate(time.Now().Add(-window))}
	var skip []string
	for _, role := range []string{"trash", "junk", "drafts"} {
		if id := cn.roles[role]; id != "" {
			skip = append(skip, id)
		}
	}
	if len(skip) > 0 {
		filter["inMailboxOtherThan"] = skip
	}
	var state string
	var ids []string
	for len(ids) < maxInitial {
		calls := []invocation{{"Email/query", map[string]any{
			"accountId": cn.accountID,
			"filter":    filter,
			"sort":      []map[string]any{{"property": "receivedAt", "isAscending": false}},
			"position":  len(ids),
			"limit":     maxInitial - len(ids),
		}, "q"}}
		if state == "" {
			calls = slices.Insert(calls, 0, invocation{"Email/get", map[string]any{"accountId": cn.accountID, "ids": []string{}}, "s"})
		}
		resps, err := a.call(ctx, cn.api, calls...)
		if err != nil {
			return err
		}
		if state == "" {
			g, err := result[getResponse[email]](resps, "Email/get", "s")
			if err != nil {
				return err
			}
			state = g.State
		}
		q, err := result[queryResponse](resps, "Email/query", "q")
		if err != nil {
			return err
		}
		if len(q.IDs) == 0 {
			break
		}
		ids = append(ids, q.IDs...)
	}
	slices.Reverse(ids)
	if err := c.store(ctx, a, cn, ids); err != nil {
		return err
	}
	c.log.Info("first sync done", "emails", len(ids))
	return c.saveState(ctx, cn.accountID, state)
}

func (c *fmConnector) changes(ctx context.Context, a *account, cn *conn, since string) error {
	for {
		resps, err := a.call(ctx, cn.api, invocation{"Email/changes", map[string]any{
			"accountId": cn.accountID, "sinceState": since, "maxChanges": maxChanges,
		}, "c"})
		if err != nil {
			return err
		}
		ch, err := result[changesResponse](resps, "Email/changes", "c")
		var me *methodError
		if errors.As(err, &me) && me.Type == "cannotCalculateChanges" {
			c.log.Info("Fastmail no longer knows the last sync; syncing the recent window again")
			return c.initial(ctx, a, cn)
		} else if err != nil {
			return err
		}
		for _, id := range ch.Destroyed {
			if err := c.st.DeleteMessage(ctx, id); err != nil {
				return err
			}
		}
		if err := c.apply(ctx, a, cn, ch.Created, ch.Updated); err != nil {
			return err
		}
		if err := c.saveState(ctx, cn.accountID, ch.NewState); err != nil {
			return err
		}
		if !ch.HasMoreChanges {
			return nil
		}
		since = ch.NewState
	}
}

// apply stores created emails and follows updated ones in and out of Trash
// and Spam. An email's content never changes, so a stored one is not read
// again.
func (c *fmConnector) apply(ctx context.Context, a *account, cn *conn, created, updated []string) error {
	need := slices.Clone(created)
	for batch := range slices.Chunk(updated, getBatch) {
		list, err := c.getEmails(ctx, a, cn, batch, []string{"id", "mailboxIds", "keywords", "receivedAt"}, 0)
		if err != nil {
			return err
		}
		for _, e := range list {
			known, err := c.mark(ctx, cn, e)
			if err != nil {
				return err
			}
			if !known && !cn.skipped(e) && e.ReceivedAt.After(time.Now().Add(-window)) {
				need = append(need, e.ID)
			}
		}
	}
	return c.store(ctx, a, cn, need)
}

// store reads ids in full and stores them.
func (c *fmConnector) store(ctx context.Context, a *account, cn *conn, ids []string) error {
	for batch := range slices.Chunk(ids, getBatch) {
		list, err := c.getEmails(ctx, a, cn, batch, emailProperties, 4*maxText)
		if err != nil {
			return err
		}
		for _, e := range list {
			if err := c.put(ctx, cn, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// getEmails reads ids with props; bodyBytes > 0 fetches text bodies up to
// that size each.
func (c *fmConnector) getEmails(ctx context.Context, a *account, cn *conn, ids, props []string, bodyBytes int) ([]email, error) {
	args := map[string]any{"accountId": cn.accountID, "ids": ids, "properties": props}
	if bodyBytes > 0 {
		args["fetchTextBodyValues"] = true
		args["maxBodyValueBytes"] = bodyBytes
	}
	resps, err := a.call(ctx, cn.api, invocation{"Email/get", args, "g"})
	if err != nil {
		return nil, err
	}
	g, err := result[getResponse[email]](resps, "Email/get", "g")
	return g.List, err
}

// mark marks a stored email deleted while it is skipped, keeping its body,
// and restores it when it comes back. It reports whether e is stored.
func (c *fmConnector) mark(ctx context.Context, cn *conn, e email) (bool, error) {
	var deleted bool
	err := c.st.DB().QueryRowContext(ctx, `SELECT deleted FROM messages WHERE id = ?1`, e.ID).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	switch skip := cn.skipped(e); {
	case skip && !deleted:
		_, err = c.st.DB().ExecContext(ctx, `UPDATE messages SET deleted = 1 WHERE id = ?1`, e.ID)
	case !skip && deleted:
		_, err = c.st.DB().ExecContext(ctx, `UPDATE messages SET deleted = 0 WHERE id = ?1`, e.ID)
	}
	return true, err
}

// put stores e, its thread, and what replying to it needs.
func (c *fmConnector) put(ctx context.Context, cn *conn, e email) error {
	known, err := c.mark(ctx, cn, e)
	if known || err != nil || cn.skipped(e) {
		return err
	}
	msg := store.Message{
		ID:          e.ID,
		ContainerID: e.ThreadID,
		SentAt:      e.ReceivedAt,
		Body:        render(e, maxText),
		Media:       media(e.Attachments),
	}
	if len(e.From) > 0 {
		msg.SenderID, msg.SenderName = strings.ToLower(e.From[0].Email), e.From[0].Name
		_, msg.IsFromMe = cn.identity(e.From[0].Email)
	}
	if len(e.InReplyTo) > 0 {
		err := c.st.DB().QueryRowContext(ctx, `SELECT id FROM fastmail_emails WHERE message_id = ?1 AND id != ?2 LIMIT 1`,
			e.InReplyTo[0], e.ID).Scan(&msg.ReplyToID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}

	var messageID string
	if len(e.MessageID) > 0 {
		messageID = e.MessageID[0]
	}
	refs := e.References
	if len(refs) == 0 {
		refs = e.InReplyTo
	}
	if messageID != "" {
		refs = append(slices.Clone(refs), messageID)
	}
	replyTo := e.From
	switch {
	case msg.IsFromMe:
		replyTo = e.To
	case len(e.ReplyTo) > 0:
		replyTo = e.ReplyTo
	}
	var recipients []string
	for _, r := range slices.Concat(e.To, e.Cc) {
		recipients = append(recipients, strings.ToLower(r.Email))
	}
	if _, err := c.st.DB().ExecContext(ctx, `
INSERT OR REPLACE INTO fastmail_emails (id, message_id, refs, subject, reply_to, recipients)
VALUES (?1, ?2, ?3, ?4, ?5, ?6)`,
		e.ID, messageID, strings.Join(refs, " "), e.Subject, addresses(replyTo), strings.Join(recipients, ",")); err != nil {
		return err
	}
	if err := c.st.UpsertContainer(ctx, store.Container{
		ID: e.ThreadID, Kind: kindThread, Name: threadName(e.Subject), LastMessageAt: e.ReceivedAt,
	}); err != nil {
		return err
	}
	return c.st.UpsertMessage(ctx, msg)
}

// addresses joins the bare addresses of as.
func addresses(as []address) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.Email
	}
	return strings.Join(s, ", ")
}

// backfill records the attachments of emails stored before they were, once,
// and renders their bodies without the attachment lines those had.
func (c *fmConnector) backfill(ctx context.Context, a *account, cn *conn) error {
	var done bool
	err := c.st.DB().QueryRowContext(ctx, `SELECT 1 FROM fastmail_backfill WHERE account_id = ?1`, cn.accountID).Scan(&done)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := c.st.DB().QueryContext(ctx, `
SELECT id FROM messages WHERE NOT EXISTS (SELECT 1 FROM media WHERE message_id = messages.id)`)
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
		list, err := c.getEmails(ctx, a, cn, batch, emailProperties, 4*maxText)
		if err != nil {
			return err
		}
		for _, e := range list {
			if len(e.Attachments) == 0 {
				continue
			}
			if err := c.addMedia(ctx, e); err != nil {
				return err
			}
		}
	}
	_, err = c.st.DB().ExecContext(ctx, `INSERT INTO fastmail_backfill (account_id) VALUES (?1)`, cn.accountID)
	return err
}

// addMedia stores the attachments and body of a stored e in place: the email
// is the same, so this is not an edit.
func (c *fmConnector) addMedia(ctx context.Context, e email) error {
	tx, err := c.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET body = ?2 WHERE id = ?1`, e.ID, render(e, maxText)); err != nil {
		return err
	}
	for i, m := range media(e.Attachments) {
		if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO media (message_id, idx, type, name, size, ref) VALUES (?1, ?2, ?3, NULLIF(?4, ''), NULLIF(?5, 0), ?6)`,
			e.ID, i, m.Type, m.Name, m.Size, m.Ref); err != nil {
			return err
		}
	}
	return tx.Commit()
}
