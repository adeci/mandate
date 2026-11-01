package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

var spec = connector.Spec{
	Name: "matrix",
	Doc: "Rooms and messages arrive by sync; fetch reads a room's older history from the homeserver.\n" +
		"A message starting with [unverified] came with a key that does not trace back to a cross-signed device of its sender, so someone else may have written it.\n" +
		"send proposes a message; it goes out from the owner's account once they approve it.",
	Setup: setupParams,
	Ops: []connector.Op{
		{
			Name: "fetch", Kind: connector.Read,
			Summary: "Fetch older messages of a room from the homeserver, newest first. Page with before.",
			Params: []connector.Param{
				{Name: "chat", Doc: "room ID from mandate matrix chats", Required: true, Container: true},
				{Name: "before", Doc: "seq; messages before it"},
				{Name: "limit", Doc: "1 to 100, default 50"},
			},
		},
		{
			Name: "send", Kind: connector.Write,
			Summary: "Send a text message to a room.",
			Params: []connector.Param{
				{Name: "chat", Doc: "room ID from mandate matrix chats", Required: true, Container: true},
				{Name: "text", Doc: "message text", Required: true, Editable: true},
			},
		},
	},
}

func (c *mxConnector) Spec() connector.Spec { return spec }

var errNotLinked = &connector.Error{Status: http.StatusServiceUnavailable, Message: "matrix is not linked"}

func (c *mxConnector) Prepare(ctx context.Context, op string, args map[string]string) (connector.Prepared, error) {
	if op != "send" {
		return connector.Prepared{}, connector.ErrUnknownOp
	}
	for k := range args {
		if k != "chat" && k != "text" {
			return connector.Prepared{}, connector.Invalid("unknown arg %q", k)
		}
	}
	text := args["text"]
	if strings.TrimSpace(text) == "" {
		return connector.Prepared{}, connector.Invalid("text is required")
	}
	a := c.use()
	if a == nil {
		return connector.Prepared{}, errNotLinked
	}
	a.release()
	chat := args["chat"]
	to, err := c.recipient(ctx, chat)
	if err != nil {
		return connector.Prepared{}, err
	}
	return connector.Prepared{
		Args:      map[string]string{"chat": chat, "text": text},
		Display:   []approval.Field{to, {Label: "Text", Value: text, Param: "text"}},
		Container: chat,
	}, nil
}

// recipient checks that chat is a stored room and renders it for the owner:
// a DM by the other person's user ID, a room by its ID, each next to the name
// they chose.
func (c *mxConnector) recipient(ctx context.Context, chat string) (approval.Field, error) {
	var kind, name, participants string
	err := c.st.DB().QueryRowContext(ctx, `SELECT kind, COALESCE(name, ''), participants_json FROM containers WHERE id = ?1`, chat).
		Scan(&kind, &name, &participants)
	if errors.Is(err, sql.ErrNoRows) {
		return approval.Field{}, connector.Invalid("unknown chat %q: run mandate matrix chats", chat)
	} else if err != nil {
		return approval.Field{}, fmt.Errorf("look up chat: %w", err)
	}
	if kind != kindDM {
		return approval.Field{Label: "Room", Value: named(name, chat)}, nil
	}
	var ps []store.Participant
	if err := json.Unmarshal([]byte(participants), &ps); err != nil || len(ps) != 1 {
		return approval.Field{}, fmt.Errorf("chat %s has no other participant", chat)
	}
	return approval.Field{Label: "To", Value: named(ps[0].Name, ps[0].ID)}, nil
}

// named shows a name someone else chose, marked with "~", next to the ID it
// stands for.
func named(name, ident string) string {
	name = strings.TrimPrefix(name, "~")
	if name == "" || name == ident {
		return ident
	}
	return "~" + name + " (" + ident + ")"
}

// Execute makes one PUT. Only a 4xx answer means the homeserver refused it;
// any other failure may have sent the message, so it is Unknown.
func (c *mxConnector) Execute(ctx context.Context, rec approval.Record) connector.Result {
	a := c.use()
	if a == nil {
		return connector.Result{Status: connector.Failed, Detail: errNotLinked.Message}
	}
	defer a.release()
	room := id.RoomID(rec.Args["chat"])
	text := rec.Args["text"]
	var payload any = &event.MessageEventContent{MsgType: event.MsgText, Body: text}
	evtType := event.EventMessage
	encrypted, err := a.state.IsEncrypted(ctx, room)
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: "look up room encryption: " + err.Error()}
	}
	if encrypted {
		if payload, err = a.encrypt(ctx, room, event.EventMessage, payload); err != nil {
			return connector.Result{Status: connector.Failed, Detail: "encrypt: " + err.Error()}
		}
		evtType = event.EventEncrypted
	}
	resp, err := a.cli.SendMessageEvent(ctx, room, evtType, payload, mautrix.ReqSendEvent{TransactionID: "mandate-" + rec.ID})
	if err != nil {
		var he mautrix.HTTPError
		if errors.As(err, &he) && he.Response != nil && he.Response.StatusCode >= 400 && he.Response.StatusCode < 500 {
			return connector.Result{Status: connector.Failed, Detail: err.Error()}
		}
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	res := connector.Result{Status: connector.Done, Ref: resp.EventID.String()}
	err = c.st.UpsertMessage(ctx, store.Message{
		ID:          resp.EventID.String(),
		ContainerID: room.String(),
		SenderID:    a.sess.UserID.String(),
		SenderName:  a.name(ctx, room, a.sess.UserID, nil),
		SentAt:      time.Now(),
		IsFromMe:    true,
		Body:        text,
	})
	if err != nil {
		c.log.Error("store sent message", "id", resp.EventID, "room", room, "err", err)
		res.Detail = "sent, but not stored: " + err.Error()
	}
	return res
}

// encrypt shares a new group session with the room's joined members first if
// the current one is missing or used up. Invitees get it once they join.
func (a *account) encrypt(ctx context.Context, room id.RoomID, t event.Type, content any) (*event.EncryptedEventContent, error) {
	enc, err := a.mach.EncryptMegolmEvent(ctx, room, t, content)
	if !errors.Is(err, crypto.ErrNoGroupSession) && !errors.Is(err, crypto.ErrSessionExpired) && !errors.Is(err, crypto.ErrSessionNotShared) {
		return enc, err
	}
	members, err := a.state.GetRoomMembers(ctx, room, event.MembershipJoin)
	if err != nil {
		return nil, fmt.Errorf("room members: %w", err)
	}
	if err := a.mach.ShareGroupSession(ctx, room, slices.Collect(maps.Keys(members))); err != nil {
		return nil, fmt.Errorf("share room key: %w", err)
	}
	return a.mach.EncryptMegolmEvent(ctx, room, t, content)
}
