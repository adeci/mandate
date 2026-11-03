package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/adeci/mandate/connector"
)

// Read serves fetch: it reads an email's whole text from Fastmail into the
// stored body.
func (c *fmConnector) Read(ctx context.Context, op string, args map[string]string) (connector.ReadResult, error) {
	if op != "fetch" {
		return connector.ReadResult{}, connector.ErrUnknownOp
	}
	if err := only(args, "chat", "seq"); err != nil {
		return connector.ReadResult{}, err
	}
	seq, err := strconv.ParseInt(args["seq"], 10, 64)
	if err != nil || seq < 1 {
		return connector.ReadResult{}, connector.Invalid("seq must be a seq from mandate's output")
	}
	a, cn, err := c.linked()
	if err != nil {
		return connector.ReadResult{}, err
	}
	var id string
	var deleted bool
	err = c.st.DB().QueryRowContext(ctx, `SELECT id, deleted FROM messages WHERE seq = ?1 AND container_id = ?2`, seq, args["chat"]).Scan(&id, &deleted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return connector.ReadResult{}, connector.Invalid("no email with seq %d in chat %s", seq, args["chat"])
	case err != nil:
		return connector.ReadResult{}, err
	case deleted:
		return connector.ReadResult{}, errDeleted
	}
	list, err := c.getEmails(ctx, a, cn, []string{id}, emailProperties, 4*maxFetch)
	if err != nil {
		return connector.ReadResult{}, &connector.Error{Status: http.StatusBadGateway, Message: "read from Fastmail: " + err.Error()}
	}
	if len(list) == 0 {
		return connector.ReadResult{}, &connector.Error{Status: http.StatusGone, Message: "the email is no longer on Fastmail"}
	}
	// Not an edit: the email is the same, only less of it was stored.
	if _, err := c.st.DB().ExecContext(ctx, `UPDATE messages SET body = ?2 WHERE id = ?1`, id, render(list[0], maxFetch)); err != nil {
		return connector.ReadResult{}, err
	}
	return connector.ReadResult{MessageIDs: []string{id}}, nil
}
