package main

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/store"
)

var errLoggedOut = errors.New("logged out by the homeserver")

const (
	syncTimeout = 30 * time.Second
	maxBackoff  = time.Minute
	// Presence and typing are never stored.
	syncFilter = `{"presence":{"not_types":["*"]},"room":{"ephemeral":{"not_types":["*"]}}}`
)

// sync stores what /sync brings until ctx is done or the homeserver ends
// the session. A batch is stored before its token is saved, so a crash
// redelivers it.
func (c *mxConnector) sync(ctx context.Context, a *account) error {
	var wait time.Duration
	for {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		err := c.syncOnce(ctx, a)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, mautrix.MUnknownToken):
			return errLoggedOut
		case err != nil:
			c.log.Warn("sync failed", "err", err)
			a.setSyncErr(syncDetail(err))
			wait = min(max(2*wait, time.Second), maxBackoff)
		default:
			a.setSyncErr("")
			wait = 0
		}
	}
}

func (a *account) setSyncErr(s string) {
	a.mu.Lock()
	a.syncErr = s
	a.mu.Unlock()
}

func syncDetail(err error) string {
	var he mautrix.HTTPError
	switch {
	case errors.As(err, &he) && he.RespError != nil:
		return "sync failed: " + he.RespError.ErrCode
	case errors.As(err, &he) && he.Response != nil:
		return "sync failed: " + he.Response.Status
	case errors.As(err, &he):
		return "cannot reach the homeserver"
	}
	return "storing synced messages failed"
}

func (c *mxConnector) syncOnce(ctx context.Context, a *account) error {
	if a.direct == nil {
		var direct event.DirectChatsEventContent
		if err := a.cli.GetAccountData(ctx, event.AccountDataDirectChats.Type, &direct); err != nil && !errors.Is(err, mautrix.MNotFound) {
			return err
		}
		a.direct = invertDirect(direct)
	}
	if a.since == nil {
		// SQLCryptoStore.GetNextBatch returns "" for a token it finds.
		var since string
		if err := a.db.QueryRow(ctx, `SELECT COALESCE(sync_token, '') FROM crypto_account WHERE account_id = $1`,
			a.crypto.AccountID).Scan(&since); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		a.since = &since
	}
	resp, err := a.cli.FullSyncRequest(ctx, mautrix.ReqSync{
		Since:       *a.since,
		Timeout:     int(syncTimeout / time.Millisecond),
		FilterID:    syncFilter,
		SetPresence: event.PresenceOffline,
	})
	if err != nil {
		return err
	}
	if err := c.ingest(ctx, a, resp, *a.since); err != nil {
		return err
	}
	if err := a.crypto.PutNextBatch(ctx, resp.NextBatch); err != nil {
		return err
	}
	a.since = &resp.NextBatch
	return nil
}

func invertDirect(d event.DirectChatsEventContent) map[id.RoomID]id.UserID {
	m := map[id.RoomID]id.UserID{}
	for user, rooms := range d {
		for _, r := range rooms {
			m[r] = user
		}
	}
	return m
}

func (c *mxConnector) ingest(ctx context.Context, a *account, resp *mautrix.RespSync, since string) error {
	// Room keys arrive as to-device messages, so they go first.
	a.mach.ProcessSyncResponse(ctx, resp, since)
	for _, evt := range resp.AccountData.Events {
		if evt.Type.Type != event.AccountDataDirectChats.Type {
			continue
		}
		var direct event.DirectChatsEventContent
		if err := json.Unmarshal(evt.Content.VeryRaw, &direct); err == nil {
			a.direct = invertDirect(direct)
		}
	}
	late := a.retry()
	slices.SortFunc(late, func(x, y *event.Event) int { return cmp.Compare(x.Timestamp, y.Timestamp) })
	for _, evt := range late {
		if _, err := c.apply(ctx, a, evt, nil); err != nil {
			return err
		}
	}
	for room, data := range resp.Rooms.Join {
		if err := c.ingestRoom(ctx, a, room, data); err != nil {
			return err
		}
	}
	return nil
}

func (c *mxConnector) ingestRoom(ctx context.Context, a *account, room id.RoomID, data *mautrix.SyncJoinedRoom) error {
	var name, alias *string
	state := func(evt *event.Event) {
		mautrix.UpdateStateStore(ctx, a.state, evt)
		switch evt.Type {
		case event.StateMember:
			a.mach.HandleMemberEvent(ctx, evt)
		case event.StateRoomName:
			n := evt.Content.AsRoomName().Name
			name = &n
		case event.StateCanonicalAlias:
			s := evt.Content.AsCanonicalAlias().Alias.String()
			alias = &s
		}
	}
	for _, evt := range data.State.Events {
		prepare(evt, room)
		state(evt)
	}
	var last int64 // ms
	for _, evt := range data.Timeline.Events {
		prepare(evt, room)
		if evt.StateKey != nil {
			state(evt)
			continue
		}
		if isMessage(evt.Type) {
			last = max(last, evt.Timestamp)
		}
		if _, err := c.apply(ctx, a, evt, nil); err != nil {
			return err
		}
	}
	ct, err := c.container(ctx, a, room, name, alias, data.Summary.Heroes)
	if err != nil {
		return err
	}
	if last > 0 {
		ct.LastMessageAt = time.UnixMilli(last)
	}
	return c.st.UpsertContainer(ctx, ct)
}

// container describes room: a DM by the other person's name, a group by its
// name or alias. name and alias are set when this batch changed them.
func (c *mxConnector) container(ctx context.Context, a *account, room id.RoomID, name, alias *string, heroes []id.UserID) (store.Container, error) {
	if other, ok := a.direct[room]; ok {
		n := a.name(ctx, room, other, nil)
		if n == "" {
			n = other.String()
		}
		return store.Container{
			ID: room.String(), Kind: kindDM, Name: n,
			Participants: []store.Participant{{ID: other.String(), Name: n}},
		}, nil
	}
	ct := store.Container{ID: room.String(), Kind: kindGroup, IsGroup: true}
	switch {
	case name != nil && *name != "":
		ct.Name = *name
	case alias != nil && *alias != "":
		ct.Name = *alias
	case len(heroes) > 0:
		// An unnamed room is named after its members when first stored.
		var one int
		err := c.st.DB().QueryRowContext(ctx, `SELECT 1 FROM containers WHERE id = ?1`, room.String()).Scan(&one)
		if err == nil {
			break
		} else if !errors.Is(err, sql.ErrNoRows) {
			return ct, err
		}
		names := make([]string, 0, len(heroes))
		for _, h := range heroes {
			n := a.name(ctx, room, h, nil)
			if n == "" {
				n = h.String()
			}
			names = append(names, n)
		}
		ct.Name = strings.Join(names, ", ")
	}
	return ct, nil
}
