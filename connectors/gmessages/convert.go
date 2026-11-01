package main

import (
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/store"
)

type opKind int

const (
	opUpsert opKind = iota + 1
	opDelete
)

type op struct {
	kind   opKind
	msg    store.Message // opUpsert
	target string        // opDelete: store message ID
}

func kindFor(conv *gmproto.Conversation) string {
	if conv.GetIsGroupChat() {
		return "group"
	}
	return "dm"
}

// others are the visible participants other than the owner.
func others(conv *gmproto.Conversation) []*gmproto.Participant {
	var out []*gmproto.Participant
	for _, p := range conv.GetParticipants() {
		if !p.GetIsMe() && p.GetIsVisible() {
			out = append(out, p)
		}
	}
	return out
}

// participantID is the phone number where there is one: participant IDs are
// only meaningful within the pairing.
func participantID(p *gmproto.Participant) string {
	if n := p.GetID().GetNumber(); n != "" {
		return n
	}
	return p.GetID().GetParticipantID()
}

// participantName prefers the owner's contact name, then the number. A name
// from outside the contacts (an RCS profile) is marked with "~", since the
// sender picks it.
func participantName(p *gmproto.Participant) string {
	switch {
	case p.GetContactID() != "" && p.GetFullName() != "":
		return p.GetFullName()
	case p.GetContactID() != "" && p.GetFirstName() != "":
		return p.GetFirstName()
	case p.GetFormattedNumber() != "":
		return p.GetFormattedNumber()
	case p.GetID().GetNumber() != "":
		return p.GetID().GetNumber()
	case p.GetFullName() != "":
		return "~" + p.GetFullName()
	}
	return ""
}

func container(conv *gmproto.Conversation) store.Container {
	c := store.Container{
		ID:      conv.GetConversationID(),
		Kind:    kindFor(conv),
		IsGroup: conv.GetIsGroupChat(),
		Name:    conv.GetName(),
	}
	if ts := conv.GetLastMessageTimestamp(); ts > 0 {
		c.LastMessageAt = time.UnixMicro(ts).UTC()
	}
	ps := others(conv)
	if !c.IsGroup {
		if len(ps) == 1 {
			if n := participantName(ps[0]); n != "" {
				c.Name = n
			}
		}
		return c
	}
	c.Participants = make([]store.Participant, 0, len(ps))
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		part := store.Participant{ID: participantID(p), Name: participantName(p)}
		c.Participants = append(c.Participants, part)
		names = append(names, part.Name)
	}
	if c.Name == "" {
		c.Name = strings.Join(names, ", ")
	}
	return c
}

// sender finds the participant behind id. Google Messages sometimes sends
// from a hidden duplicate of a member, which is matched to the visible one
// by number.
func sender(conv *gmproto.Conversation, id string) *gmproto.Participant {
	var match *gmproto.Participant
	for _, p := range conv.GetParticipants() {
		if p.GetID().GetParticipantID() == id {
			match = p
			if p.GetIsVisible() {
				return p
			}
		}
	}
	if match == nil || match.GetID().GetNumber() == "" {
		return match
	}
	for _, p := range conv.GetParticipants() {
		if p.GetIsVisible() && p.GetID().GetNumber() == match.GetID().GetNumber() {
			return p
		}
	}
	return match
}

func me(conv *gmproto.Conversation) *gmproto.Participant {
	for _, p := range conv.GetParticipants() {
		if p.GetIsMe() {
			return p
		}
	}
	return nil
}

func isTombstone(s gmproto.MessageStatusType) bool { return s >= 200 && s < 300 }

// Outgoing statuses are below 100, incoming ones from 100.
func isOutgoing(s gmproto.MessageStatusType) bool { return s >= 1 && s < 100 }

// awaitingDownload covers MMS whose content the phone has not fetched (yet).
func awaitingDownload(s gmproto.MessageStatusType) bool {
	switch s {
	case gmproto.MessageStatusType_INCOMING_YET_TO_MANUAL_DOWNLOAD,
		gmproto.MessageStatusType_INCOMING_RETRYING_MANUAL_DOWNLOAD,
		gmproto.MessageStatusType_INCOMING_MANUAL_DOWNLOADING,
		gmproto.MessageStatusType_INCOMING_RETRYING_AUTO_DOWNLOAD,
		gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_FAILED,
		gmproto.MessageStatusType_INCOMING_EXPIRED_OR_NOT_AVAILABLE,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_CANCELED,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_FAILED_TOO_LARGE,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_FAILED_SIM_HAS_NO_DATA,
		gmproto.MessageStatusType_INCOMING_AWAITING_AUTO_DOWNLOAD,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_RESTRICTED:
		return true
	}
	return false
}

// convert maps a message of conv to a store operation. Tombstones (joins,
// protocol switches and the like) and messages without content are skipped.
// The same message ID arrives again on every status change; storing it again
// updates it in place.
func convert(m *gmproto.Message, conv *gmproto.Conversation) (op, bool) {
	status := m.GetMessageStatus().GetStatus()
	switch {
	case m.GetMessageID() == "" || isTombstone(status):
		return op{}, false
	case status == gmproto.MessageStatusType_MESSAGE_DELETED:
		return op{kind: opDelete, target: m.GetMessageID()}, true
	case m.GetTimestamp() <= 0:
		return op{}, false
	}
	body, files := content(m)
	msg := store.Message{
		ID:          m.GetMessageID(),
		ContainerID: m.GetConversationID(),
		SentAt:      time.UnixMicro(m.GetTimestamp()).UTC(),
		IsFromMe:    isOutgoing(status),
		Body:        body,
		ReplyToID:   m.GetReplyMessage().GetMessageID(),
	}
	switch {
	case len(files) > 0:
		msg.Media = files
	case body != "":
	case awaitingDownload(status):
		msg.MediaType = "attachment"
	default:
		return op{}, false
	}
	if msg.IsFromMe {
		if p := me(conv); p != nil {
			msg.SenderID = p.GetID().GetNumber()
		}
		return op{kind: opUpsert, msg: msg}, true
	}
	p := sender(conv, m.GetParticipantID())
	if p == nil {
		p = m.GetSenderParticipant()
	}
	if p != nil {
		msg.SenderID, msg.SenderName = participantID(p), participantName(p)
	}
	if msg.SenderID == "" {
		msg.SenderID = m.GetParticipantID()
	}
	return op{kind: opUpsert, msg: msg}, true
}

// content is the subject and text parts, and the files.
func content(m *gmproto.Message) (body string, files []store.Media) {
	var parts []string
	if s := m.GetSubject(); s != "" {
		parts = append(parts, s)
	}
	for _, info := range m.GetMessageInfo() {
		if t := info.GetMessageContent().GetContent(); t != "" {
			parts = append(parts, t)
		}
		if mc := info.GetMediaContent(); mc != nil {
			files = append(files, attachment(mc))
		}
	}
	return strings.Join(parts, "\n"), files
}
