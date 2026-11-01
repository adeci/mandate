package main

import (
	"cmp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/adeci/mandate/store"
)

type pnResolver func(types.JID) types.JID

type opKind int

const (
	opUpsert opKind = iota + 1
	opEdit
	opDelete
)

type op struct {
	kind   opKind
	chat   types.JID     // container JID (non-AD, phone number preferred)
	sender types.JID     // author, editor or deleter (non-AD, phone number preferred)
	fromMe bool          // opEdit, opDelete: sent by the owner
	msg    store.Message // opUpsert
	target string        // opEdit, opDelete: store message ID
	body   string        // opEdit
	at     time.Time     // opEdit: server time of the edit
}

func messageID(chat types.JID, id types.MessageID) string {
	return chat.String() + "/" + id
}

func kindFor(chat types.JID) string {
	if chat.Server == types.GroupServer {
		return "group"
	}
	return "dm"
}

func preferPN(j, alt types.JID, pn pnResolver) types.JID {
	j = j.ToNonAD()
	if j.Server != types.HiddenUserServer {
		return j
	}
	if alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	return pn(j)
}

// Status updates, outgoing broadcast lists, newsletters, and bots are not ingested.
func chatFor(src types.MessageSource, pn pnResolver) (types.JID, bool) {
	chat := src.Chat.ToNonAD()
	switch chat.Server {
	case types.GroupServer, types.DefaultUserServer:
		return chat, true
	case types.HiddenUserServer:
		var alt types.JID
		if src.IsFromMe {
			alt = src.RecipientAlt
		} else if src.Sender.User == chat.User {
			alt = src.SenderAlt
		}
		return preferPN(chat, alt, pn), true
	case types.BroadcastServer:
		if src.IsIncomingBroadcast() {
			// Broadcast-list messages show up in the DM with the sender.
			return preferPN(src.Sender, src.SenderAlt, pn), true
		}
	}
	return types.EmptyJID, false
}

func historyChat(raw types.JID, pn pnResolver) (types.JID, bool) {
	switch raw.Server {
	case types.GroupServer, types.DefaultUserServer:
		return raw.ToNonAD(), true
	case types.HiddenUserServer:
		return preferPN(raw, types.EmptyJID, pn), true
	}
	return types.EmptyJID, false
}

func historyContainer(chat types.JID, conv *waHistorySync.Conversation, pn pnResolver) store.Container {
	c := store.Container{
		ID:      chat.String(),
		Kind:    kindFor(chat),
		IsGroup: chat.Server == types.GroupServer,
		Name:    conv.GetName(),
	}
	if c.Name == "" {
		c.Name = conv.GetDisplayName()
	}
	if ts := cmp.Or(conv.GetLastMsgTimestamp(), conv.GetConversationTimestamp()); ts > 0 {
		c.LastMessageAt = time.Unix(int64(ts), 0).UTC()
	}
	if c.IsGroup && len(conv.GetParticipant()) > 0 {
		c.Participants = make([]store.Participant, 0, len(conv.GetParticipant()))
		for _, p := range conv.GetParticipant() {
			j, err := types.ParseJID(p.GetUserJID())
			if err != nil {
				continue
			}
			c.Participants = append(c.Participants, store.Participant{ID: preferPN(j, types.EmptyJID, pn).String()})
		}
	}
	return c
}

func groupContainer(info *types.GroupInfo, pn pnResolver, name func(types.JID) string) store.Container {
	c := store.Container{
		ID:           info.JID.ToNonAD().String(),
		Kind:         "group",
		IsGroup:      true,
		Name:         info.Name,
		Participants: make([]store.Participant, 0, len(info.Participants)),
	}
	for _, p := range info.Participants {
		j := preferPN(p.JID, p.PhoneNumber, pn)
		part := store.Participant{ID: j.String(), Name: name(j)}
		if part.Name == "" {
			part.Name = p.DisplayName
		}
		c.Participants = append(c.Participants, part)
	}
	return c
}

// Reactions, poll votes, and protocol messages are not stored.
func convert(evt *events.Message, pn pnResolver) (op, bool) {
	chat, ok := chatFor(evt.Info.MessageSource, pn)
	if !ok || evt.Info.ID == "" {
		return op{}, false
	}
	sender := preferPN(evt.Info.Sender, evt.Info.SenderAlt, pn)
	change := op{chat: chat, sender: sender, fromMe: evt.Info.IsFromMe, at: evt.Info.Timestamp}
	m := evt.Message
	if m == nil {
		// History sync keeps revoked messages as stubs without content.
		if evt.SourceWebMsg.GetMessageStubType() == waWeb.WebMessageInfo_REVOKE {
			change.kind, change.target = opDelete, messageID(chat, evt.Info.ID)
			return change, true
		}
		return op{}, false
	}
	if p := m.GetProtocolMessage(); p != nil {
		target := p.GetKey().GetID()
		if target == "" {
			return op{}, false
		}
		change.target = messageID(chat, target)
		switch p.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			change.kind = opDelete
			return change, true
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			change.kind = opEdit
			change.body, _, _ = content(p.GetEditedMessage())
			return change, true
		}
		return op{}, false
	}
	body, media, ok := content(m)
	if !ok {
		return op{}, false
	}
	if evt.IsEdit {
		change.kind, change.target, change.body = opEdit, messageID(chat, evt.Info.ID), body
		return change, true
	}
	if evt.Info.Timestamp.IsZero() {
		return op{}, false
	}
	msg := store.Message{
		ID:          messageID(chat, evt.Info.ID),
		ContainerID: chat.String(),
		SentAt:      evt.Info.Timestamp,
		IsFromMe:    evt.Info.IsFromMe,
		Body:        body,
	}
	if file, ok := attachment(m); ok {
		msg.Media = []store.Media{file}
	} else {
		msg.MediaType = media
	}
	if !sender.IsEmpty() {
		msg.SenderID = sender.String()
	}
	if name := evt.Info.PushName; name != "-" {
		msg.SenderName = name
	}
	if reply := contextInfo(m).GetStanzaID(); reply != "" {
		msg.ReplyToID = messageID(chat, reply)
	}
	return op{kind: opUpsert, chat: chat, sender: sender, msg: msg}, true
}

func content(m *waE2E.Message) (body, media string, ok bool) {
	switch {
	case m == nil:
		return "", "", false
	case m.Conversation != nil:
		return m.GetConversation(), "", true
	case m.ExtendedTextMessage != nil:
		return m.GetExtendedTextMessage().GetText(), "", true
	case m.ImageMessage != nil:
		return m.GetImageMessage().GetCaption(), "image", true
	case m.VideoMessage != nil:
		return m.GetVideoMessage().GetCaption(), "video", true
	case m.PtvMessage != nil:
		return m.GetPtvMessage().GetCaption(), "video", true
	case m.AudioMessage != nil:
		return "", "audio", true
	case m.DocumentMessage != nil:
		return m.GetDocumentMessage().GetCaption(), "document", true
	case m.StickerMessage != nil:
		return "", "sticker", true
	case m.LocationMessage != nil:
		l := m.GetLocationMessage()
		return joinNonEmpty(l.GetName(), l.GetAddress()), "location", true
	case m.LiveLocationMessage != nil:
		return m.GetLiveLocationMessage().GetCaption(), "location", true
	case m.ContactMessage != nil:
		return m.GetContactMessage().GetDisplayName(), "contact", true
	case m.ContactsArrayMessage != nil:
		return m.GetContactsArrayMessage().GetDisplayName(), "contact", true
	}
	for _, p := range []*waE2E.PollCreationMessage{
		m.GetPollCreationMessage(), m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(),
		m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6(),
	} {
		if p != nil {
			return p.GetName(), "poll", true
		}
	}
	return "", "", false
}

func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	for _, c := range []interface{ GetContextInfo() *waE2E.ContextInfo }{
		m.GetExtendedTextMessage(), m.GetImageMessage(), m.GetVideoMessage(), m.GetPtvMessage(),
		m.GetAudioMessage(), m.GetDocumentMessage(), m.GetStickerMessage(), m.GetLocationMessage(),
		m.GetLiveLocationMessage(), m.GetContactMessage(), m.GetContactsArrayMessage(),
		m.GetPollCreationMessage(), m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(),
		m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6(),
	} {
		if ci := c.GetContextInfo(); ci != nil {
			return ci
		}
	}
	return nil
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}
