package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/connector"
)

const (
	defaultLimit = 50
	maxLimit     = 100
	// maxPages bounds the requests one fetch makes when a room's history is
	// mostly state changes.
	maxPages = 5
)

type fetchRequest struct {
	room   id.RoomID
	before int64 // seq, or 0 for the newest messages
	limit  int
}

func parseFetch(args map[string]string) (fetchRequest, error) {
	r := fetchRequest{room: id.RoomID(args["chat"]), limit: defaultLimit}
	for k, v := range args {
		switch k {
		case "chat":
		case "before":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 {
				return r, connector.Invalid("before must be a seq from mandate's output")
			}
			r.before = n
		case "limit":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxLimit {
				return r, connector.Invalid("limit must be between 1 and %d", maxLimit)
			}
			r.limit = n
		default:
			return r, connector.Invalid("unknown arg %q", k)
		}
	}
	if r.room == "" {
		return r, connector.Invalid("chat is required")
	}
	return r, nil
}

func (c *mxConnector) Read(ctx context.Context, op string, args map[string]string) (json.RawMessage, error) {
	if op != "fetch" {
		return nil, connector.ErrUnknownOp
	}
	req, err := parseFetch(args)
	if err != nil {
		return nil, err
	}
	a := c.use()
	if a == nil {
		return nil, errNotLinked
	}
	defer a.release()
	ids, err := c.fetch(ctx, a, req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(connector.ReadResult{MessageIDs: ids})
}

// fetch pages back through the room's history from the newest message, or
// from the one before seq, and stores what it finds. It returns the stored
// message IDs, newest first.
func (c *mxConnector) fetch(ctx context.Context, a *account, req fetchRequest) ([]string, error) {
	var one int
	err := c.st.DB().QueryRowContext(ctx, `SELECT 1 FROM containers WHERE id = ?1`, req.room.String()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connector.Invalid("unknown chat %q: run mandate matrix chats", req.room)
	} else if err != nil {
		return nil, err
	}
	var from string
	if req.before > 0 {
		var anchor string
		err := c.st.DB().QueryRowContext(ctx, `SELECT id FROM messages WHERE seq = ?1 AND container_id = ?2`,
			req.before, req.room.String()).Scan(&anchor)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connector.Invalid("no message with seq %d in chat %s", req.before, req.room)
		} else if err != nil {
			return nil, err
		}
		around, err := a.cli.Context(ctx, req.room, id.EventID(anchor), nil, 1)
		if err != nil {
			return nil, upstream("find the message", err)
		}
		from = around.Start
	}

	names := map[id.UserID]string{}
	var evts []*event.Event // newest first
	found := 0
	for range maxPages {
		resp, err := a.cli.Messages(ctx, req.room, from, "", mautrix.DirectionBackward, &mautrix.FilterPart{LazyLoadMembers: true}, req.limit)
		if err != nil {
			return nil, upstream("read history", err)
		}
		// Members as of these messages, for sender names; the current room
		// state, which encryption relies on, is left to sync.
		for _, evt := range resp.State {
			if evt.Type == event.StateMember && evt.StateKey != nil {
				prepare(evt, req.room)
				if n := evt.Content.AsMember().Displayname; n != "" {
					names[id.UserID(*evt.StateKey)] = n
				}
			}
		}
		for _, evt := range resp.Chunk {
			prepare(evt, req.room)
			if isMessage(evt.Type) {
				found++
			}
		}
		evts = append(evts, resp.Chunk...)
		if found >= req.limit || resp.End == "" || len(resp.Chunk) == 0 {
			break
		}
		from = resp.End
	}

	// Oldest first, so an edit or redaction finds its message stored.
	var ids []string
	for _, evt := range slices.Backward(evts) {
		if evt.StateKey != nil {
			continue
		}
		stored, err := c.apply(ctx, a, evt, names)
		if err != nil {
			return nil, err
		}
		if stored {
			ids = append(ids, evt.ID.String())
		}
	}
	slices.Reverse(ids)
	return ids[:min(len(ids), req.limit)], nil
}
