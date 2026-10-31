package gmessages

import (
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/internal/store"
)

// unknownSenderNumber is the placeholder Google Messages uses when the phone
// has no address for a participant.
const unknownSenderNumber = "UNKNOWN_SENDER!"

// Hidden duplicate entries the phone keeps for number normalization are
// skipped, as the bridge does.
func conversationToContainer(c *gmproto.Conversation) store.Container {
	kind := "dm"
	if c.GetIsGroupChat() {
		kind = "group"
	}
	var participants []store.Participant
	seen := make(map[string]bool)
	for _, p := range c.GetParticipants() {
		if !p.GetIsVisible() && !p.GetIsMe() {
			continue
		}
		id := participantKey(p)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		participants = append(participants, store.Participant{ID: id, Name: participantName(p)})
	}
	return store.Container{
		ID:           c.GetConversationID(),
		Kind:         kind,
		Name:         c.GetName(),
		IsGroup:      c.GetIsGroupChat(),
		Participants: participants,
	}
}

// Most conversations are one-to-one, so "dm" is the best guess; a later
// conversation event corrects it.
func fallbackContainer(conversationID string) store.Container {
	return store.Container{ID: conversationID, Kind: "dm"}
}

// Numbers are E.164 as sent by the phone, or an email for some RCS identities.
func participantKey(p *gmproto.Participant) string {
	if n := usableNumber(p.GetID().GetNumber()); n != "" {
		return n
	}
	return p.GetID().GetParticipantID()
}

func usableNumber(n string) string {
	if n == unknownSenderNumber {
		return ""
	}
	return n
}

func participantName(p *gmproto.Participant) string {
	switch {
	case p.GetFullName() != "":
		return p.GetFullName()
	case p.GetFirstName() != "":
		return p.GetFirstName()
	default:
		return p.GetFormattedNumber()
	}
}

func isDeletion(m *gmproto.Message) bool {
	return m.GetMessageStatus().GetStatus() == gmproto.MessageStatusType_MESSAGE_DELETED
}

// MMS still auto-downloading is skipped as in the bridge: the completed
// download arrives as a later update. Deletions are handled separately.
func skipStatus(s gmproto.MessageStatusType) bool {
	switch s {
	case gmproto.MessageStatusType_OUTGOING_DRAFT,
		gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING,
		gmproto.MessageStatusType_INCOMING_RETRYING_AUTO_DOWNLOAD:
		return true
	}
	return s >= 200
}

// conv supplies the participant list for the sender's number and may be nil.
func messageToStore(m *gmproto.Message, conv *gmproto.Conversation) (msg store.Message, ok bool) {
	status := m.GetMessageStatus().GetStatus()
	if skipStatus(status) || m.GetMessageID() == "" || m.GetConversationID() == "" || m.GetTimestamp() == 0 {
		return store.Message{}, false
	}
	body, mediaType := messageContent(m)
	if body == "" && mediaType == "" {
		return store.Message{}, false
	}
	senderID, senderName, fromMe := resolveSender(m, conv)
	return store.Message{
		ID:          m.GetMessageID(),
		ContainerID: m.GetConversationID(),
		SenderID:    senderID,
		SenderName:  senderName,
		SentAt:      time.UnixMicro(m.GetTimestamp()),
		IsFromMe:    fromMe,
		Body:        body,
		MediaType:   mediaType,
		ReplyToID:   m.GetReplyMessage().GetMessageID(),
	}, true
}

// Text parts alongside media are the caption. Media is never downloaded.
func messageContent(m *gmproto.Message) (body, mediaType string) {
	var parts []string
	if s := m.GetSubject(); s != "" {
		parts = append(parts, s)
	}
	for _, info := range m.GetMessageInfo() {
		switch data := info.GetData().(type) {
		case *gmproto.MessageInfo_MessageContent:
			if text := data.MessageContent.GetContent(); text != "" {
				parts = append(parts, text)
			}
		case *gmproto.MessageInfo_MediaContent:
			if mediaType == "" {
				mediaType = mediaMIME(data.MediaContent)
			}
		}
	}
	return strings.Join(parts, "\n"), mediaType
}

func mediaMIME(mc *gmproto.MediaContent) string {
	if mc.GetMimeType() != "" {
		return mc.GetMimeType()
	}
	if mt, ok := libgm.FormatToMediaType[mc.GetFormat()]; ok && mt.Format != "" {
		return mt.Format
	}
	switch mc.GetFormat() {
	case gmproto.MediaFormats_IMAGE_UNSPECIFIED:
		return "image"
	case gmproto.MediaFormats_VIDEO_UNSPECIFIED:
		return "video"
	case gmproto.MediaFormats_AUDIO_UNSPECIFIED:
		return "audio"
	}
	return "application/octet-stream"
}

// As in the bridge: statuses 1-99 are outgoing, 100-199 incoming; otherwise
// participant ID "1" or one flagged IsMe is the account holder.
func resolveSender(m *gmproto.Message, conv *gmproto.Conversation) (id, name string, fromMe bool) {
	pid := m.GetParticipantID()
	p := findParticipant(conv, pid)
	if p == nil && m.GetSenderParticipant().GetID() != nil {
		p = m.GetSenderParticipant()
	}
	status := m.GetMessageStatus().GetStatus()
	switch {
	case status >= 1 && status < 100:
		fromMe = true
	case status >= 100 && status < 200:
		fromMe = false
	default:
		fromMe = pid == "1" || p.GetIsMe()
	}
	if p == nil && fromMe {
		p = findSelf(conv)
	}
	if p == nil {
		return pid, "", fromMe
	}
	return participantKey(p), participantName(p), fromMe
}

func findParticipant(conv *gmproto.Conversation, participantID string) *gmproto.Participant {
	if participantID == "" {
		return nil
	}
	var hidden *gmproto.Participant
	for _, p := range conv.GetParticipants() {
		if p.GetID().GetParticipantID() != participantID {
			continue
		}
		if p.GetIsVisible() {
			return p
		}
		hidden = p
	}
	if hidden == nil {
		return nil
	}
	// The phone sometimes attributes messages to a hidden duplicate entry;
	// prefer the visible participant with the same number for its name.
	if n := usableNumber(hidden.GetID().GetNumber()); n != "" {
		for _, p := range conv.GetParticipants() {
			if p.GetIsVisible() && p.GetID().GetNumber() == n {
				return p
			}
		}
	}
	return hidden
}

func findSelf(conv *gmproto.Conversation) *gmproto.Participant {
	for _, p := range conv.GetParticipants() {
		if p.GetIsMe() {
			return p
		}
	}
	return nil
}
