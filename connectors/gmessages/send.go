package main

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

func (c *gmConnector) Spec() connector.Spec {
	return connector.Spec{Name: "gmessages", Ops: []connector.Op{{
		Name:    "send",
		Kind:    connector.Write,
		Summary: "Send a text message to a chat.",
		Params: []connector.Param{
			{Name: "chat", Doc: "chat ID from mandate gmessages chats", Required: true, Container: true},
			{Name: "text", Doc: "message text", Required: true, Editable: true},
		},
	}}, Setup: []connector.Param{{
		Name: "cookies", Secret: true,
		Doc: "For pairing with your Google account, when the phone's Device pairing offers no QR code scanner. " +
			"In a desktop browser, preferably a private window, sign in to messages.google.com/web with the Google account your phone's Messages uses. " +
			"Open devtools → Network, pick any request to messages.google.com and copy its Cookie request header (Copy as cURL works too), then close the window. " +
			"These cookies are a signed-in Google session: mandate-gmessages can act as that Google account. " +
			"After unlinking, you can end that session under Google Account → Security → Your devices. " +
			"Leave it empty to pair by QR code where Google still offers it.",
	}}}
}

func (c *gmConnector) Prepare(ctx context.Context, op string, args map[string]string) (connector.Prepared, error) {
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
	cli, _ := c.state()
	if cli == nil {
		return connector.Prepared{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "google messages is not linked"}
	}
	chat := args["chat"]
	to, err := c.recipient(ctx, cli, chat)
	if err != nil {
		return connector.Prepared{}, err
	}
	return connector.Prepared{
		Args:      map[string]string{"chat": chat, "text": text},
		Display:   []approval.Field{to, {Label: "Text", Value: text, Param: "text"}},
		Container: chat,
	}, nil
}

// recipient checks that chat is stored and renders it for the owner: a
// direct chat by the phone's contact entry, otherwise by the stored name,
// which members may set, and the members' numbers.
func (c *gmConnector) recipient(ctx context.Context, cli gmClient, chat string) (approval.Field, error) {
	var kind, name string
	err := c.st.DB().QueryRowContext(ctx, `SELECT kind, COALESCE(name, '') FROM containers WHERE id = ?1`, chat).Scan(&kind, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return approval.Field{}, connector.Invalid("unknown chat %q: run mandate gmessages chats --search %s", chat, chat)
	}
	if err != nil {
		return approval.Field{}, fmt.Errorf("look up chat: %w", err)
	}
	conv, err := c.in.lookup(ctx, cli, chat)
	if err != nil {
		return approval.Field{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "look up the chat on the phone: " + err.Error()}
	}
	ps := others(conv)
	if kind == "group" || len(ps) != 1 {
		var numbers []string
		for _, p := range ps {
			if n := number(p); n != "" {
				numbers = append(numbers, n)
			}
		}
		id := strings.Join(numbers, ", ")
		if id == "" {
			id = "conversation " + chat
		}
		f := approval.Field{Label: "Chat", Value: id}
		if kind == "group" {
			f.Label = "Group"
		}
		if name != "" {
			f.Value = "~" + name + " (" + id + ")"
		}
		return f, nil
	}
	p := ps[0]
	id := number(p)
	if id == "" {
		id = "conversation " + chat
	}
	if contact := cmp.Or(p.GetFullName(), p.GetFirstName()); p.GetContactID() != "" && contact != "" {
		return approval.Field{Label: "To", Value: contact + " (" + id + ")"}, nil
	}
	return approval.Field{Label: "To", Value: id + " (not in your contacts)"}, nil
}

func number(p *gmproto.Participant) string {
	return cmp.Or(p.GetFormattedNumber(), p.GetID().GetNumber())
}

// Execute does not retry: once SendMessage is called the phone may send the
// message, so any error after that point is Unknown. The message is stored
// when the phone reports it back.
func (c *gmConnector) Execute(ctx context.Context, rec approval.Record) connector.Result {
	chat, text := rec.Args["chat"], rec.Args["text"]
	cli, h := c.state()
	if h.State != connector.Healthy {
		return connector.Result{Status: connector.Failed, Detail: "google messages is not connected: " + h.Detail}
	}
	conv, err := c.in.lookup(ctx, cli, chat)
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: "look up the chat on the phone: " + err.Error()}
	}
	req := sendRequest(conv, c.in.sim(conv), text, util.GenerateTmpID())
	return sendResult(cli.SendMessage(ctx, req))
}

func sendRequest(conv *gmproto.Conversation, sim *gmproto.SIMPayload, text, tmpID string) *gmproto.SendMessageRequest {
	id := conv.GetConversationID()
	return &gmproto.SendMessageRequest{
		ConversationID: id,
		MessagePayload: &gmproto.MessagePayload{
			TmpID:          tmpID,
			TmpID2:         tmpID,
			ConversationID: id,
			ParticipantID:  conv.GetDefaultOutgoingID(),
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: text}},
			}},
		},
		SIMPayload: sim,
		TmpID:      tmpID,
	}
}

func sendResult(resp *gmproto.SendMessageResponse, err error) connector.Result {
	if err != nil {
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	switch s := resp.GetStatus(); s {
	case gmproto.SendMessageResponse_SUCCESS:
		return connector.Result{Status: connector.Done}
	case gmproto.SendMessageResponse_FAILURE_2, gmproto.SendMessageResponse_FAILURE_3:
		// Temporary failures, after which the phone may still send it.
		return connector.Result{Status: connector.Unknown, Detail: "the phone answered " + s.String()}
	default:
		return connector.Result{Status: connector.Failed, Detail: "the phone refused the message: " + s.String()}
	}
}
