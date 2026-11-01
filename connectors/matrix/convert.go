package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/store"
)

const (
	kindDM    = "dm"
	kindGroup = "group"

	// mediaEncrypted marks a message stored before it could be decrypted;
	// the gateway shows it as [encrypted].
	mediaEncrypted = "encrypted"
)

// prepare parses a raw event from room. Content of unknown types stays
// unparsed, which the As* accessors read as empty.
func prepare(evt *event.Event, room id.RoomID) {
	evt.RoomID = room
	if evt.StateKey != nil {
		evt.Type.Class = event.StateEventType
	} else {
		evt.Type.Class = event.MessageEventType
	}
	_ = evt.Content.ParseRaw(evt.Type)
}

func isMessage(t event.Type) bool {
	return t == event.EventMessage || t == event.EventEncrypted || t == event.EventSticker
}

// apply stores what a timeline event does to the room's messages: a new
// message, an edit, or a redaction. It reports whether a message with the
// event's ID is stored. names maps senders to display names, falling back to
// the room's current members.
func (c *mxConnector) apply(ctx context.Context, a *account, evt *event.Event, names map[id.UserID]string) (bool, error) {
	if evt.Unsigned.RedactedBecause != nil {
		return false, c.st.DeleteMessage(ctx, evt.ID.String())
	}
	switch evt.Type {
	case event.EventRedaction:
		return false, c.redact(ctx, a, evt)
	case event.EventEncrypted:
		dec, err := a.mach.DecryptMegolmEvent(ctx, evt)
		if err != nil {
			if errors.Is(err, crypto.ErrNoSessionFound) {
				a.keep(evt)
			}
			if evt.Content.AsEncrypted().RelatesTo.GetReplaceID() != "" {
				return false, nil // an edit we cannot read yet
			}
			return c.placeholder(ctx, a, evt, names)
		}
		evt = dec
	}
	if evt.Type != event.EventMessage && evt.Type != event.EventSticker {
		return false, nil
	}
	content := evt.Content.AsMessage()
	if target := content.RelatesTo.GetReplaceID(); target != "" {
		if content.NewContent == nil {
			return false, nil
		}
		body, _ := messageBody(evt.Type, content.NewContent)
		return false, c.edit(ctx, evt.RoomID, target, evt.Sender, unverified(evt, body), time.UnixMilli(evt.Timestamp))
	}
	msg := store.Message{
		ID:          evt.ID.String(),
		ContainerID: evt.RoomID.String(),
		SenderID:    evt.Sender.String(),
		SenderName:  a.name(ctx, evt.RoomID, evt.Sender, names),
		SentAt:      time.UnixMilli(evt.Timestamp),
		IsFromMe:    evt.Sender == a.sess.UserID,
		ReplyToID:   content.RelatesTo.GetReplyTo().String(),
	}
	msg.Body, msg.MediaType = messageBody(evt.Type, content)
	msg.Body = unverified(evt, msg.Body)
	// The server bundles the latest edit with the original, which matters
	// when history is read backwards and the edit comes first.
	var editedAt time.Time
	if e := a.bundledEdit(ctx, evt); e != nil {
		body, _ := messageBody(e.Type, e.Content.AsMessage().NewContent)
		msg.Body = unverified(e, body)
		editedAt = time.UnixMilli(e.Timestamp)
	}
	if err := c.put(ctx, msg); err != nil {
		return false, err
	}
	if !editedAt.IsZero() {
		if err := c.edit(ctx, evt.RoomID, evt.ID, evt.Sender, msg.Body, editedAt); err != nil {
			return false, err
		}
	}
	return true, nil
}

// bundledEdit is the latest edit the server bundled with evt, if its
// sender may make it and it can be read.
func (a *account) bundledEdit(ctx context.Context, evt *event.Event) *event.Event {
	e := evt.Unsigned.Relations.GetLatestEdit()
	if e == nil || e.Sender != evt.Sender {
		return nil
	}
	prepare(e, evt.RoomID)
	if e.Type == event.EventEncrypted {
		dec, err := a.mach.DecryptMegolmEvent(ctx, e)
		if err != nil {
			return nil
		}
		e = dec
	}
	content := e.Content.AsMessage()
	if e.Type != evt.Type || content.RelatesTo.GetReplaceID() != evt.ID || content.NewContent == nil {
		return nil
	}
	return e
}

func messageBody(t event.Type, content *event.MessageEventContent) (body, media string) {
	if t == event.EventSticker {
		return content.Body, "sticker"
	}
	content.RemoveReplyFallback()
	switch content.MsgType {
	case event.MsgEmote:
		return "/me " + content.Body, ""
	case event.MsgImage:
		return content.GetCaption(), "image"
	case event.MsgVideo:
		return content.GetCaption(), "video"
	case event.MsgAudio:
		return content.GetCaption(), "audio"
	case event.MsgFile:
		return content.GetCaption(), "file"
	case event.MsgLocation:
		return content.Body, "location"
	}
	return content.Body, ""
}

// unverified marks the body of a message whose room key does not trace back
// to a cross-signed device of its sender: a device the homeserver may have
// added, or a key from the backup, which the homeserver can write to.
func unverified(evt *event.Event, body string) string {
	if !evt.Mautrix.WasEncrypted || evt.Mautrix.TrustState >= id.TrustStateCrossSignedTOFU {
		return body
	}
	return "[unverified] " + body
}

// name is user's display name in room, marked with "~" unless it is the
// owner's, since anyone can pick one.
func (a *account) name(ctx context.Context, room id.RoomID, user id.UserID, names map[id.UserID]string) string {
	n := names[user]
	if n == "" {
		if m, _ := a.state.TryGetMember(ctx, room, user); m != nil {
			n = m.Displayname
		}
	}
	if n == "" || user == a.sess.UserID {
		return n
	}
	return "~" + n
}

// put stores m. A message stored before its key arrived is filled in without
// marking it edited.
func (c *mxConnector) put(ctx context.Context, m store.Message) error {
	if _, err := c.st.DB().ExecContext(ctx,
		`UPDATE messages SET body = ?2, media_type = NULLIF(?3, '') WHERE id = ?1 AND media_type = ?4`,
		m.ID, m.Body, m.MediaType, mediaEncrypted); err != nil {
		return err
	}
	return c.st.UpsertMessage(ctx, m)
}

// placeholder stores an event that cannot be decrypted, unless it is
// stored already.
func (c *mxConnector) placeholder(ctx context.Context, a *account, evt *event.Event, names map[id.UserID]string) (bool, error) {
	var one int
	err := c.st.DB().QueryRowContext(ctx, `SELECT 1 FROM messages WHERE id = ?1`, evt.ID.String()).Scan(&one)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return err == nil, err
	}
	return true, c.st.UpsertMessage(ctx, store.Message{
		ID:          evt.ID.String(),
		ContainerID: evt.RoomID.String(),
		SenderID:    evt.Sender.String(),
		SenderName:  a.name(ctx, evt.RoomID, evt.Sender, names),
		SentAt:      time.UnixMilli(evt.Timestamp),
		IsFromMe:    evt.Sender == a.sess.UserID,
		MediaType:   mediaEncrypted,
	})
}

// edit replaces the body of target with an edit made at at. Only the sender
// may edit a message, and an older edit never replaces a newer one.
func (c *mxConnector) edit(ctx context.Context, room id.RoomID, target id.EventID, sender id.UserID, body string, at time.Time) error {
	var author string
	var editedAt sql.NullString
	err := c.st.DB().QueryRowContext(ctx,
		`SELECT COALESCE(sender_id, ''), edited_at FROM messages WHERE id = ?1 AND container_id = ?2`,
		target.String(), room.String()).Scan(&author, &editedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if author != sender.String() {
		return nil
	}
	if editedAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, editedAt.String); err == nil && t.After(at) {
			return nil
		}
	}
	return c.st.EditMessage(ctx, target.String(), body, at)
}

// redact marks the target deleted if the redaction's sender wrote it or may
// redact others' messages.
func (c *mxConnector) redact(ctx context.Context, a *account, evt *event.Event) error {
	target := evt.Redacts
	if target == "" {
		target = evt.Content.AsRedaction().Redacts
	}
	var author string
	err := c.st.DB().QueryRowContext(ctx,
		`SELECT COALESCE(sender_id, '') FROM messages WHERE id = ?1 AND container_id = ?2`,
		target.String(), evt.RoomID.String()).Scan(&author)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if author != evt.Sender.String() {
		pl, err := a.state.GetPowerLevels(ctx, evt.RoomID)
		if err != nil {
			return err
		}
		if pl == nil || pl.GetUserLevel(evt.Sender) < pl.Redact() {
			return nil
		}
	}
	return c.st.DeleteMessage(ctx, target.String())
}
