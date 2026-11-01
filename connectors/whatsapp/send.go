package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func (c *waConnector) Spec() connector.Spec {
	return connector.Spec{Name: "whatsapp", Ops: []connector.Op{{
		Name:    "send",
		Kind:    connector.Write,
		Summary: "Send a text message to a chat.",
		Params: []connector.Param{
			{Name: "chat", Doc: "chat ID from mandate whatsapp chats", Required: true, Container: true},
			{Name: "text", Doc: "message text", Required: true, Editable: true},
		},
	}}, Setup: []connector.Param{
		{Name: "phone", Doc: "Get a pairing code for this number instead of a QR code, e.g. +15551234567"},
	}}
}

func (c *waConnector) Prepare(ctx context.Context, op string, args map[string]string) (connector.Prepared, error) {
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
	_, dev, linked := c.current()
	if !linked {
		return connector.Prepared{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "whatsapp is not linked"}
	}
	chat := args["chat"]
	to, err := c.recipient(ctx, dev, chat)
	if err != nil {
		return connector.Prepared{}, err
	}
	return connector.Prepared{
		Args:      map[string]string{"chat": chat, "text": text},
		Display:   append(to, approval.Field{Label: "Text", Value: text, Param: "text"}),
		Container: chat,
	}, nil
}

// recipient checks that chat is stored and renders it for the owner, with
// a warning when a security code behind it changed in the last week.
func (c *waConnector) recipient(ctx context.Context, dev *wastore.Device, chat string) ([]approval.Field, error) {
	var subject, members string
	err := c.st.DB().QueryRowContext(ctx, `SELECT COALESCE(name, ''), participants_json FROM containers WHERE id = ?1`, chat).
		Scan(&subject, &members)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connector.Invalid("unknown chat %q: run mandate whatsapp chats --search %s", chat, chat)
	}
	if err != nil {
		return nil, fmt.Errorf("look up chat: %w", err)
	}
	j, err := types.ParseJID(chat)
	if err != nil {
		return nil, connector.Invalid("invalid chat ID %q", chat)
	}
	switch j.Server {
	case types.GroupServer:
		return c.group(ctx, chat, subject, members)
	case types.DefaultUserServer, types.HiddenUserServer:
	default:
		return nil, connector.Invalid("cannot send to %s", chat)
	}
	var pn, lid types.JID
	if j.Server == types.HiddenUserServer {
		lid = j
		if pn, err = dev.LIDs.GetPNForLID(ctx, j); err != nil {
			return nil, fmt.Errorf("look up phone number: %w", err)
		}
	} else {
		pn = j
		if lid, err = dev.LIDs.GetLIDForPN(ctx, j); err != nil {
			return nil, fmt.Errorf("look up lid: %w", err)
		}
	}
	name, err := addressBookName(ctx, dev, pn, lid)
	if err != nil {
		return nil, err
	}
	number := chat
	if !pn.IsEmpty() {
		number = "+" + pn.User
	}
	to := approval.Field{Label: "To", Value: number + " (not in your contacts)"}
	if name != "" {
		to.Value = name + " (" + number + ")"
	}
	var ids []string
	for _, u := range []types.JID{pn, lid} {
		if !u.IsEmpty() {
			ids = append(ids, u.String())
		}
	}
	changes, err := identityChanges(ctx, c.st.DB(), ids)
	if err != nil {
		return nil, err
	}
	var latest time.Time
	for _, t := range changes {
		if t.After(latest) {
			latest = t
		}
	}
	if latest.IsZero() {
		return []approval.Field{to}, nil
	}
	return []approval.Field{to, security(changedAt(latest))}, nil
}

// group renders a group by its subject, which any admin may set, and its
// ID, with the members whose security code changed.
func (c *waConnector) group(ctx context.Context, chat, subject, members string) ([]approval.Field, error) {
	g := approval.Field{Label: "Group", Value: chat}
	if subject != "" {
		g.Value = "~" + subject + " (" + chat + ")"
	}
	var ps []store.Participant
	if err := json.Unmarshal([]byte(members), &ps); err != nil {
		return nil, fmt.Errorf("read group members: %w", err)
	}
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	changes, err := identityChanges(ctx, c.st.DB(), ids)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, p := range ps {
		t, ok := changes[p.ID]
		if !ok {
			continue
		}
		member := p.ID
		if j, err := types.ParseJID(p.ID); err == nil && j.Server == types.DefaultUserServer {
			member = "+" + j.User
		}
		changed = append(changed, "for "+member+" "+changedAt(t))
	}
	if len(changed) == 0 {
		return []approval.Field{g}, nil
	}
	return []approval.Field{g, security(changed...)}, nil
}

// Only address-book names are shown; push names are set by the sender.
// Entries may be keyed by phone number or by LID.
func addressBookName(ctx context.Context, dev *wastore.Device, ids ...types.JID) (string, error) {
	for _, j := range ids {
		if j.IsEmpty() {
			continue
		}
		ci, err := dev.Contacts.GetContact(ctx, j)
		if err != nil {
			return "", fmt.Errorf("look up contact: %w", err)
		}
		if ci.FullName != "" {
			return ci.FullName, nil
		}
		if ci.FirstName != "" {
			return ci.FirstName, nil
		}
	}
	return "", nil
}

// Execute never retries: once SendMessage is called the message may have
// been delivered, so any error after that point is Unknown.
func (c *waConnector) Execute(ctx context.Context, rec approval.Record) connector.Result {
	to, err := types.ParseJID(rec.Args["chat"])
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: "invalid chat: " + err.Error()}
	}
	text := rec.Args["text"]
	cli, dev, _ := c.current()
	if !cli.IsConnected() || !cli.IsLoggedIn() {
		return connector.Result{Status: connector.Failed, Detail: "whatsapp is not connected"}
	}
	resp, err := cli.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	res := connector.Result{Status: connector.Done, Ref: resp.ID}
	if err := c.record(ctx, dev, to, text, resp); err != nil {
		c.log.Error("store sent message", "id", resp.ID, "chat", to.String(), "err", err)
		res.Detail = "sent, but not stored: " + err.Error()
	}
	return res
}

func (c *waConnector) record(ctx context.Context, dev *wastore.Device, to types.JID, text string, resp whatsmeow.SendResponse) error {
	ct := store.Container{ID: to.String(), Kind: kindFor(to), IsGroup: to.Server == types.GroupServer}
	if err := c.st.UpsertContainer(ctx, ct); err != nil {
		return err
	}
	at := resp.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	msg := store.Message{
		ID:          messageID(to, resp.ID),
		ContainerID: to.String(),
		SenderName:  dev.PushName,
		SentAt:      at,
		IsFromMe:    true,
		Body:        text,
	}
	if own := dev.GetJID(); !own.IsEmpty() {
		msg.SenderID = own.ToNonAD().String()
	}
	return c.st.UpsertMessage(ctx, msg)
}
