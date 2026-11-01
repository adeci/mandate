package main

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"

	"github.com/adeci/mandate/store"
)

type opKind int

const (
	opUpsert opKind = iota + 1
	opEdit
	opDelete
)

type op struct {
	kind   opKind
	sender uuid.UUID     // opUpsert
	msg    store.Message // opUpsert
	target string        // opEdit, opDelete: store message ID
	body   string        // opEdit
	at     time.Time     // opEdit
}

// A Signal message is identified by its author and sent timestamp.
func messageID(chat string, author uuid.UUID, ts uint64) string {
	return chat + "/" + author.String() + "/" + strconv.FormatUint(ts, 10)
}

// Group chats are base64 group identifiers, DMs service IDs.
func isGroup(chat string) bool {
	_, err := types.GroupIdentifier(chat).Bytes()
	return err == nil
}

func kindFor(chat string) string {
	if isGroup(chat) {
		return "group"
	}
	return "dm"
}

// Reactions, receipts, typing, calls, and group changes are not stored.
// Admin deletes are ignored: they name another member's message, and group
// roles are not tracked here. name renders a mentioned user.
func convert(e *events.ChatEvent, chat string, own uuid.UUID, name func(uuid.UUID) string) (op, bool) {
	sender := e.Info.Sender
	switch m := e.Event.(type) {
	case *signalpb.DataMessage:
		switch {
		case m.Delete != nil:
			// The target ID includes its author, so only the author's own
			// delete finds it; edits work the same way.
			return op{kind: opDelete, target: messageID(chat, sender, m.Delete.GetTargetSentTimestamp())}, true
		case m.AdminDelete != nil:
			return op{}, false
		}
		body, media, ok := content(m, name)
		ts := m.GetTimestamp()
		if !ok || ts == 0 {
			return op{}, false
		}
		msg := store.Message{
			ID:          messageID(chat, sender, ts),
			ContainerID: chat,
			SenderID:    sender.String(),
			SentAt:      time.UnixMilli(int64(ts)).UTC(),
			IsFromMe:    sender == own,
			Body:        body,
			MediaType:   media,
		}
		if q := m.GetQuote(); q.GetId() != 0 {
			author, err := signalmeow.ParseStringOrBinaryUUID(q.GetAuthorAci(), q.GetAuthorAciBinary())
			if err == nil {
				msg.ReplyToID = messageID(chat, author, q.GetId())
			}
		}
		return op{kind: opUpsert, sender: sender, msg: msg}, true
	case *signalpb.EditMessage:
		body, _, ok := content(m.GetDataMessage(), name)
		if !ok || m.GetTargetSentTimestamp() == 0 {
			return op{}, false
		}
		return op{
			kind:   opEdit,
			target: messageID(chat, sender, m.GetTargetSentTimestamp()),
			body:   body,
			at:     time.UnixMilli(int64(m.GetDataMessage().GetTimestamp())).UTC(),
		}, true
	}
	return op{}, false
}

func content(m *signalpb.DataMessage, name func(uuid.UUID) string) (body, media string, ok bool) {
	body = mentions(m.GetBody(), m.GetBodyRanges(), name)
	switch {
	case m == nil:
		return "", "", false
	case len(m.Attachments) > 0:
		return body, attachmentType(m.Attachments[0]), true
	case m.Sticker != nil:
		return m.Sticker.GetEmoji(), "sticker", true
	case len(m.Contact) > 0:
		n := m.Contact[0].GetName()
		return strings.TrimSpace(n.GetGivenName() + " " + n.GetFamilyName()), "contact", true
	case m.PollCreate != nil:
		return m.PollCreate.GetQuestion(), "poll", true
	case m.Body != nil:
		return body, "", true
	}
	return "", "", false
}

func attachmentType(a *signalpb.AttachmentPointer) string {
	ct := a.GetContentType()
	switch {
	case a.GetFlags()&uint32(signalpb.AttachmentPointer_VOICE_MESSAGE) != 0, strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	}
	return "document"
}

// mentions replaces the placeholder Signal puts in the body for each
// mention with "@" and the user's name. Ranges count UTF-16 code units.
func mentions(body string, ranges []*signalpb.BodyRange, name func(uuid.UUID) string) string {
	type mention struct {
		start, stop int
		aci         uuid.UUID
	}
	var ms []mention
	for _, r := range ranges {
		aci, err := signalmeow.ParseStringOrBinaryUUID(r.GetMentionAci(), r.GetMentionAciBinary())
		if err == nil {
			ms = append(ms, mention{int(r.GetStart()), int(r.GetStart()) + int(r.GetLength()), aci})
		}
	}
	if len(ms) == 0 {
		return body
	}
	slices.SortFunc(ms, func(a, b mention) int { return a.start - b.start })
	u := utf16.Encode([]rune(body))
	var b strings.Builder
	end := 0
	for _, m := range ms {
		if m.start < end || m.stop > len(u) {
			continue
		}
		b.WriteString(string(utf16.Decode(u[end:m.start])))
		b.WriteString("@" + name(m.aci))
		end = m.stop
	}
	b.WriteString(string(utf16.Decode(u[end:])))
	return b.String()
}
