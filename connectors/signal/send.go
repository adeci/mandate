package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	sgstore "go.mau.fi/mautrix-signal/pkg/signalmeow/store"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func (c *sgConnector) Spec() connector.Spec {
	return connector.Spec{Name: "signal", Ops: []connector.Op{{
		Name:    "send",
		Kind:    connector.Write,
		Summary: "Send a text message to a chat.",
		Params: []connector.Param{
			{Name: "chat", Doc: "chat ID from mandate signal chats", Required: true, Container: true},
			{Name: "text", Doc: "message text", Required: true, Editable: true},
		},
	}}}
}

func (c *sgConnector) Prepare(ctx context.Context, op string, args map[string]string) (connector.Prepared, error) {
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
	_, dev, _ := c.current()
	if dev == nil {
		return connector.Prepared{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "signal is not linked"}
	}
	chat := args["chat"]
	to, err := c.recipient(ctx, dev, chat)
	if err != nil {
		return connector.Prepared{}, err
	}
	return connector.Prepared{
		Args:      map[string]string{"chat": chat, "text": text},
		Display:   []approval.Field{to, {Label: "Text", Value: text, Param: "text"}},
		Container: chat,
	}, nil
}

// recipient checks that chat is stored and renders it for the owner from
// the local contact store.
func (c *sgConnector) recipient(ctx context.Context, dev *sgstore.Device, chat string) (approval.Field, error) {
	var group string
	err := c.st.DB().QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM containers WHERE id = ?1`, chat).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return approval.Field{}, connector.Invalid("unknown chat %q: run mandate signal chats --search %s", chat, chat)
	}
	if err != nil {
		return approval.Field{}, fmt.Errorf("look up chat: %w", err)
	}
	if isGroup(chat) {
		if group == "" {
			group = chat
		}
		return approval.Field{Label: "Group", Value: group}, nil
	}
	sid, err := libsignalgo.ServiceIDFromString(chat)
	if err != nil {
		return approval.Field{}, connector.Invalid("invalid chat ID %q", chat)
	}
	aci, pni := sid.ToACIAndPNI()
	r, err := dev.RecipientStore.LoadAndUpdateRecipient(ctx, aci, pni, nil)
	if err != nil {
		return approval.Field{}, fmt.Errorf("look up contact: %w", err)
	}
	name, number := displayName(r, dev.ACI), r.E164
	switch {
	case name != "" && number != "":
		return approval.Field{Label: "To", Value: name + " (" + number + ")"}, nil
	case name != "":
		return approval.Field{Label: "To", Value: name}, nil
	case number != "":
		return approval.Field{Label: "To", Value: number + " (not in your contacts)"}, nil
	}
	return approval.Field{Label: "To", Value: chat + " (not in your contacts)"}, nil
}

// Execute never retries: once a send starts the message may have been
// delivered, so any error after that point is Unknown.
func (c *sgConnector) Execute(ctx context.Context, rec approval.Record) connector.Result {
	chat, text := rec.Args["chat"], rec.Args["text"]
	cli, dev, connected := c.current()
	if cli == nil || !connected {
		return connector.Result{Status: connector.Failed, Detail: "signal is not connected"}
	}
	ts := uint64(time.Now().UnixMilli())
	dm := &signalpb.DataMessage{Body: &text, Timestamp: &ts}
	var res connector.Result
	if isGroup(chat) {
		gid := types.GroupIdentifier(chat)
		g, _, err := cli.RetrieveGroupByID(ctx, gid, 0)
		if err != nil {
			return connector.Result{Status: connector.Failed, Detail: "look up group: " + err.Error()}
		}
		if d := g.DisappearingMessagesDuration; d > 0 {
			dm.ExpireTimer = &d
		}
		res = groupResult(cli.SendGroupMessage(ctx, gid, signalmeow.WrapDataMessage(dm)))
	} else {
		to, err := libsignalgo.ServiceIDFromString(chat)
		if err != nil {
			return connector.Result{Status: connector.Failed, Detail: "invalid chat: " + err.Error()}
		}
		seconds, version, err := c.timers.get(ctx, chat)
		if err != nil {
			return connector.Result{Status: connector.Failed, Detail: "look up disappearing timer: " + err.Error()}
		}
		if seconds > 0 {
			dm.ExpireTimer = &seconds
			if version > 0 {
				dm.ExpireTimerVersion = &version
			}
		}
		r := cli.SendMessage(ctx, to, signalmeow.WrapDataMessage(dm))
		if !r.WasSuccessful {
			return connector.Result{Status: connector.Unknown, Detail: fmt.Sprint(r.Error)}
		}
		res = connector.Result{Status: connector.Done}
	}
	if res.Status != connector.Done {
		return res
	}
	res.Ref = strconv.FormatUint(ts, 10)
	if err := c.record(ctx, dev.ACI, chat, text, ts); err != nil {
		c.log.Error("store sent message", "chat", chat, "err", err)
		if res.Detail != "" {
			res.Detail += "; "
		}
		res.Detail += "sent, but not stored: " + err.Error()
	}
	return res
}

// A group message reached the members it was sent to; it is Unknown only
// if it may have reached none.
func groupResult(r *signalmeow.GroupMessageSendResult, err error) connector.Result {
	switch {
	case err != nil:
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	case len(r.SuccessfullySentTo) == 0 && len(r.FailedToSendTo) > 0:
		return connector.Result{Status: connector.Unknown, Detail: fmt.Sprint(r.FailedToSendTo[0].Error)}
	case len(r.FailedToSendTo) > 0:
		return connector.Result{Status: connector.Done, Detail: fmt.Sprintf("not delivered to %d of %d members",
			len(r.FailedToSendTo), len(r.FailedToSendTo)+len(r.SuccessfullySentTo))}
	}
	return connector.Result{Status: connector.Done}
}

func (c *sgConnector) record(ctx context.Context, own uuid.UUID, chat, text string, ts uint64) error {
	at := time.UnixMilli(int64(ts)).UTC()
	ct := store.Container{ID: chat, Kind: kindFor(chat), IsGroup: isGroup(chat), LastMessageAt: at}
	if err := c.st.UpsertContainer(ctx, ct); err != nil {
		return err
	}
	return c.st.UpsertMessage(ctx, store.Message{
		ID:          messageID(chat, own, ts),
		ContainerID: chat,
		SenderID:    own.String(),
		SentAt:      at,
		IsFromMe:    true,
		Body:        text,
	})
}
