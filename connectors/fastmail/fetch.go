package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/adeci/mandate/connector"
)

// Read serves fetch: it reads an email's whole text from Fastmail into the
// stored body.
func (c *fmConnector) Read(ctx context.Context, op string, args map[string]string) (json.RawMessage, error) {
	if op != "fetch" {
		return nil, connector.ErrUnknownOp
	}
	if err := only(args, "chat", "seq"); err != nil {
		return nil, err
	}
	seq, err := strconv.ParseInt(args["seq"], 10, 64)
	if err != nil || seq < 1 {
		return nil, connector.Invalid("seq must be a seq from mandate's output")
	}
	a, cn, err := c.linked()
	if err != nil {
		return nil, err
	}
	var id string
	err = c.st.DB().QueryRowContext(ctx, `SELECT id FROM messages WHERE seq = ?1 AND container_id = ?2`, seq, args["chat"]).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connector.Invalid("no email with seq %d in chat %s", seq, args["chat"])
	} else if err != nil {
		return nil, err
	}
	list, err := c.getEmails(ctx, a, cn, []string{id}, emailProperties, 4*maxFetch)
	if err != nil {
		return nil, &connector.Error{Status: http.StatusBadGateway, Message: "read from Fastmail: " + err.Error()}
	}
	if len(list) == 0 {
		return nil, &connector.Error{Status: http.StatusNotFound, Message: "the email is no longer on Fastmail"}
	}
	// Not an edit: the email is the same, only less of it was stored.
	if _, err := c.st.DB().ExecContext(ctx, `UPDATE messages SET body = ?2 WHERE id = ?1`, id, render(list[0], maxFetch)); err != nil {
		return nil, err
	}
	return json.Marshal(connector.ReadResult{MessageIDs: []string{id}})
}
