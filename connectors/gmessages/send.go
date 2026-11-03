package main

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/record"
)

func (c *gmConnector) Spec() connector.Spec {
	return connector.Spec{
		Doc: "SMS and RCS through Google Messages for Web, paired with an Android phone. Chats are the phone's conversation IDs.",
		Ops: []connector.Op{{
			Name:    "send",
			Kind:    connector.Write,
			Summary: "Send a text message to a chat",
			Params: []connector.Param{
				{Name: "chat", Doc: "chat ID", Required: true, Container: true},
				{Name: "text", Doc: "message text", Required: true, Editable: true},
			},
		}},
	}
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
		Display:   []record.Field{to, {Label: "Text", Value: text, Param: "text"}},
		Container: chat,
	}, nil
}

// recipient checks that chat is stored and renders it for the owner: a
// direct chat by the phone's contact entry, otherwise by the stored name,
// which members may set, and the members' numbers.
func (c *gmConnector) recipient(ctx context.Context, cli gmClient, chat string) (record.Field, error) {
	var kind, name string
	err := c.st.DB().QueryRowContext(ctx, `SELECT kind, COALESCE(name, '') FROM containers WHERE id = ?1`, chat).Scan(&kind, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return record.Field{}, connector.Invalid("unknown chat %q", chat)
	}
	if err != nil {
		return record.Field{}, fmt.Errorf("look up chat: %w", err)
	}
	conv, err := c.in.lookup(ctx, cli, chat)
	if err != nil {
		return record.Field{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "look up the chat on the phone: " + err.Error()}
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
		f := record.Field{Label: "Chat", Value: id}
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
		return record.Field{Label: "To", Value: contact + " (" + id + ")"}, nil
	}
	return record.Field{Label: "To", Value: id + " (not in your contacts)"}, nil
}

func number(p *gmproto.Participant) string {
	return cmp.Or(p.GetFormattedNumber(), p.GetID().GetNumber())
}

// Execute does not retry: once SendMessage is called the phone may send the
// message, so any error after that point is Unknown. The message is stored
// when the phone reports it back.
func (c *gmConnector) Execute(ctx context.Context, w record.Write) connector.Result {
	chat, text := w.Args["chat"], w.Args["text"]
	cli, h := c.state()
	if h.State != connector.Healthy {
		return connector.Result{Status: connector.Failed, Detail: "google messages is not connected: " + h.Detail}
	}
	conv, err := c.in.lookup(ctx, cli, chat)
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: "look up the chat on the phone: " + err.Error()}
	}
	req := sendRequest(conv, c.in.sim(conv), text, util.GenerateTmpID())
	return sendResult(cli.SendMessage(singleAttempt(ctx), req))
}

type singleAttemptKey struct{}

// singleAttempt lets the first HTTP request under ctx through and refuses
// any after it. libgm resends a request answered with a server error, which
// the relay may already have passed to the phone.
func singleAttempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, singleAttemptKey{}, new(atomic.Bool))
}

var errNotResent = errors.New("google answered with a server error and the message was not resent; it may or may not have gone out")

// onceTransport enforces singleAttempt for libgm's HTTP clients.
type onceTransport struct{ http.RoundTripper }

func (t onceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if sent, ok := req.Context().Value(singleAttemptKey{}).(*atomic.Bool); ok && sent.Swap(true) {
		return nil, errNotResent
	}
	return t.RoundTripper.RoundTrip(req)
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
