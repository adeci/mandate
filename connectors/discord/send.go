package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

const maxMessageLength = 2000 // characters, Discord's limit without Nitro

func (w *worker) Prepare(ctx context.Context, op string, args map[string]string) (connector.Prepared, error) {
	if op != "send" {
		return connector.Prepared{}, connector.ErrUnknownOp
	}
	for k := range args {
		if k != "chat" && k != "text" {
			return connector.Prepared{}, connector.Invalid("unknown arg %q", k)
		}
	}
	text := args["text"]
	switch n := utf8.RuneCountInString(text); {
	case strings.TrimSpace(text) == "":
		return connector.Prepared{}, connector.Invalid("text is required")
	case !utf8.ValidString(text):
		return connector.Prepared{}, connector.Invalid("text must be valid UTF-8")
	case n > maxMessageLength:
		return connector.Prepared{}, connector.Invalid("text is %d characters; Discord allows %d", n, maxMessageLength)
	}
	if err := w.writable(); err != nil {
		return connector.Prepared{}, err
	}
	chat := args["chat"]
	to, err := recipient(ctx, w.svc.st.DB(), chat)
	if err != nil {
		return connector.Prepared{}, err
	}
	return connector.Prepared{
		Args:      map[string]string{"chat": chat, "text": text},
		Display:   []approval.Field{to, {Label: "Text", Value: text, Param: "text"}},
		Container: chat,
	}, nil
}

// writable refuses while reads would be refused for want of the owner.
func (w *worker) writable() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.owner != "":
		return &connector.Error{Status: http.StatusServiceUnavailable, Message: w.owner}
	case !w.linked:
		return toConnectorError(errNotLinked)
	}
	return nil
}

// recipient checks that chat is stored and can take messages, and names it
// as the gateway's chats verb does.
func recipient(ctx context.Context, db *sql.DB, chat string) (approval.Field, error) {
	var kind, name, parent string
	err := db.QueryRowContext(ctx, `SELECT c.kind, COALESCE(c.name, ''), COALESCE(p.name, '')
FROM containers AS c LEFT JOIN containers AS p ON p.id = c.parent_id WHERE c.id = ?1`, chat).Scan(&kind, &name, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return approval.Field{}, connector.Invalid("unknown chat %q: run mandate discord chats --search %s", chat, chat)
	}
	if err != nil {
		return approval.Field{}, fmt.Errorf("look up chat: %w", err)
	}
	if !isSnowflake(chat) {
		return approval.Field{}, connector.Invalid("invalid chat ID %q", chat)
	}
	if name == "" {
		name = chat
	}
	if parent != "" {
		name = parent + " / " + name
	}
	switch kind {
	case kindDM:
		return approval.Field{Label: "To", Value: name}, nil
	case kindGroup:
		return approval.Field{Label: "Group", Value: name}, nil
	case kindChannel:
		return approval.Field{Label: "Channel", Value: name}, nil
	case kindThread:
		return approval.Field{Label: "Thread", Value: name}, nil
	}
	return approval.Field{}, connector.Invalid("cannot send to a %s; pick a channel, thread or DM", kind)
}

// Execute runs on the worker like a read, so a stopped or unlinked connector
// refuses it the same way.
func (w *worker) Execute(ctx context.Context, rec approval.Record) connector.Result {
	var res connector.Result
	_, err := w.submit(ctx, "send", false, func(ctx context.Context, dl time.Time) (connector.ReadResult, error) {
		res = w.svc.send(ctx, dl, rec.Args["chat"], rec.Args["text"])
		return connector.ReadResult{}, nil
	})
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: err.Error()}
	}
	return res
}

// sendBody is the web client's message body, in its field order.
type sendBody struct {
	MobileNetworkType string `json:"mobile_network_type"`
	Content           string `json:"content"`
	Nonce             string `json:"nonce"`
	TTS               bool   `json:"tts"`
	Flags             int    `json:"flags"`
}

// send never retries: once the POST is out, a failure is Unknown.
func (s *service) send(ctx context.Context, dl time.Time, chat, text string) connector.Result {
	c, err := s.storedContainer(ctx, chat)
	if err != nil || c.ID == "" || !isSnowflake(chat) {
		return connector.Result{Status: connector.Failed, Detail: "unknown chat " + chat}
	}
	guild := ""
	if c.Kind == kindChannel || c.Kind == kindThread {
		guild = c.ParentID
	}
	r := route{
		template: "/channels/{channel_id}/messages",
		path:     "/channels/" + chat + "/messages",
		major:    chat,
		referer:  channelURL(guild, chat),
	}
	// The nonce is made as the request goes out, as the client does.
	body := func() []byte {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false) // as JSON.stringify
		if err := enc.Encode(sendBody{MobileNetworkType: "unknown", Content: text, Nonce: snowflakeAt(s.clock.Now())}); err != nil {
			panic(err) // strings, a bool and an int
		}
		return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	}
	var m apiMessage
	maybeSent, err := s.up.post(ctx, dl, r, body, &m)
	switch {
	case err != nil && maybeSent:
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	case err != nil:
		return connector.Result{Status: connector.Failed, Detail: err.Error()}
	}
	res := connector.Result{Status: connector.Done, Ref: m.ID}
	if m.ChannelID == "" {
		m.ChannelID = chat
	}
	msg, ok := toMessage(&m, "")
	if !ok || m.ChannelID != chat {
		res.Detail = "sent, but Discord's reply held no message to store"
		return res
	}
	msg.IsFromMe = true
	if err := s.st.UpsertMessage(ctx, msg); err != nil {
		s.up.log.Error("store sent message", "id", m.ID, "chat", chat, "err", err)
		res.Detail = "sent, but not stored: " + err.Error()
	}
	return res
}
