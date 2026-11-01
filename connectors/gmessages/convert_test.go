package main

import (
	"reflect"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/store"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func ts(minutes int) int64 { return t0.Add(time.Duration(minutes) * time.Minute).UnixMicro() }

func participant(pid, number, formatted string) *gmproto.Participant {
	return &gmproto.Participant{
		ID:              &gmproto.SmallInfo{Type: gmproto.IdentifierType_PHONE, Number: number, ParticipantID: pid},
		FormattedNumber: formatted,
		IsVisible:       true,
	}
}

func contact(p *gmproto.Participant, name string) *gmproto.Participant {
	p.FullName, p.ContactID = name, "c"+p.GetID().GetParticipantID()
	return p
}

func self(pid string) *gmproto.Participant {
	p := participant(pid, "+15550000001", "(555) 000-0001")
	p.IsMe = true
	return p
}

// faridDM is a contact; strangerDM is not; family is a group whose member
// Mina also appears as a hidden duplicate.
func faridDM() *gmproto.Conversation {
	return &gmproto.Conversation{
		ConversationID:       "1",
		Name:                 "Farid Haddad",
		LastMessageTimestamp: ts(2),
		DefaultOutgoingID:    "1",
		Participants:         []*gmproto.Participant{self("1"), contact(participant("11", "+15551230001", "(555) 123-0001"), "Farid Haddad")},
	}
}

func strangerDM() *gmproto.Conversation {
	p := participant("12", "+15559990002", "(555) 999-0002")
	p.FullName = "Prize Desk"
	return &gmproto.Conversation{
		ConversationID:       "2",
		Name:                 "Prize Desk",
		LastMessageTimestamp: ts(1),
		DefaultOutgoingID:    "1",
		Participants:         []*gmproto.Participant{self("1"), p},
	}
}

func family() *gmproto.Conversation {
	hidden := participant("22", "+15551230003", "")
	hidden.IsVisible = false
	return &gmproto.Conversation{
		ConversationID:       "3",
		Name:                 "Family",
		IsGroupChat:          true,
		LastMessageTimestamp: ts(3),
		DefaultOutgoingID:    "4",
		Participants: []*gmproto.Participant{
			self("4"),
			contact(participant("21", "+15551230001", "(555) 123-0001"), "Farid Haddad"),
			hidden,
			contact(participant("23", "+15551230003", "(555) 123-0003"), "Mina"),
		},
	}
}

func message(id, conv, pid string, status gmproto.MessageStatusType, at int64, text string) *gmproto.Message {
	m := &gmproto.Message{
		MessageID:      id,
		ConversationID: conv,
		ParticipantID:  pid,
		Timestamp:      at,
		MessageStatus:  &gmproto.MessageStatus{Status: status},
	}
	if text != "" {
		m.MessageInfo = []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: text}}}}
	}
	return m
}

// withMedia attaches a file; an empty id is one the phone has not
// downloaded. The fake phone decrypts a file with key-ID.
func withMedia(m *gmproto.Message, f gmproto.MediaFormats, id, name string) *gmproto.Message {
	mc := &gmproto.MediaContent{Format: f, MediaName: name}
	if id != "" {
		mc.MediaID, mc.DecryptionKey, mc.Size = id, []byte("key-"+id), 1234
	}
	m.MessageInfo = append(m.MessageInfo, &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MediaContent{MediaContent: mc}})
	return m
}

const (
	incoming = gmproto.MessageStatusType_INCOMING_COMPLETE
	outgoing = gmproto.MessageStatusType_OUTGOING_DELIVERED
)

func TestConvert(t *testing.T) {
	t.Parallel()
	reply := message("m5", "1", "11", incoming, ts(5), "yes")
	reply.ReplyMessage = &gmproto.ReplyMessage{MessageID: "m1"}
	subject := message("m6", "1", "11", incoming, ts(6), "see you there")
	subject.Subject = new("Dinner")
	unknownSender := message("m7", "3", "99", incoming, ts(7), "who am i")
	unknownSender.SenderParticipant = participant("99", "+15557770099", "(555) 777-0099")

	for _, tc := range []struct {
		name string
		conv *gmproto.Conversation
		m    *gmproto.Message
		want op
		ok   bool
	}{
		{"from a contact", faridDM(), message("m1", "1", "11", incoming, ts(1), "dinner?"), op{kind: opUpsert, msg: store.Message{
			ID: "m1", ContainerID: "1", SenderID: "+15551230001", SenderName: "Farid Haddad", SentAt: t0.Add(time.Minute), Body: "dinner?",
		}}, true},
		{"from a stranger", strangerDM(), message("m2", "2", "12", incoming, ts(1), "you won"), op{kind: opUpsert, msg: store.Message{
			ID: "m2", ContainerID: "2", SenderID: "+15559990002", SenderName: "(555) 999-0002", SentAt: t0.Add(time.Minute), Body: "you won",
		}}, true},
		{"from me", faridDM(), message("m3", "1", "1", outgoing, ts(2), "sure"), op{kind: opUpsert, msg: store.Message{
			ID: "m3", ContainerID: "1", SenderID: "+15550000001", SentAt: t0.Add(2 * time.Minute), IsFromMe: true, Body: "sure",
		}}, true},
		{"hidden duplicate", family(), message("m4", "3", "22", incoming, ts(4), "hi all"), op{kind: opUpsert, msg: store.Message{
			ID: "m4", ContainerID: "3", SenderID: "+15551230003", SenderName: "Mina", SentAt: t0.Add(4 * time.Minute), Body: "hi all",
		}}, true},
		{"reply", faridDM(), reply, op{kind: opUpsert, msg: store.Message{
			ID: "m5", ContainerID: "1", SenderID: "+15551230001", SenderName: "Farid Haddad", SentAt: t0.Add(5 * time.Minute), Body: "yes", ReplyToID: "m1",
		}}, true},
		{"subject", faridDM(), subject, op{kind: opUpsert, msg: store.Message{
			ID: "m6", ContainerID: "1", SenderID: "+15551230001", SenderName: "Farid Haddad", SentAt: t0.Add(6 * time.Minute), Body: "Dinner\nsee you there",
		}}, true},
		{"sender not in the conversation", family(), unknownSender, op{kind: opUpsert, msg: store.Message{
			ID: "m7", ContainerID: "3", SenderID: "+15557770099", SenderName: "(555) 777-0099", SentAt: t0.Add(7 * time.Minute), Body: "who am i",
		}}, true},
		{"photo with caption", faridDM(), withMedia(message("m8", "1", "11", incoming, ts(8), "look"), gmproto.MediaFormats_IMAGE_JPEG, "md1", "IMG_1.jpg"), op{kind: opUpsert, msg: store.Message{
			ID: "m8", ContainerID: "1", SenderID: "+15551230001", SenderName: "Farid Haddad", SentAt: t0.Add(8 * time.Minute), Body: "look",
			Media: []store.Media{{Type: "image/jpeg", Name: "IMG_1.jpg", Size: 1234, Ref: []byte(`{"id":"md1","key":"a2V5LW1kMQ=="}`)}},
		}}, true},
		{"voice note not downloaded", faridDM(), withMedia(message("m9", "1", "11", gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING, ts(9), ""), gmproto.MediaFormats_AUDIO_AMR, "", ""), op{kind: opUpsert, msg: store.Message{
			ID: "m9", ContainerID: "1", SenderID: "+15551230001", SenderName: "Farid Haddad", SentAt: t0.Add(9 * time.Minute),
			Media: []store.Media{{Type: "audio/amr", Ref: []byte(`{}`)}},
		}}, true},
		{"MMS not downloaded", faridDM(), message("m10", "1", "11", gmproto.MessageStatusType_INCOMING_YET_TO_MANUAL_DOWNLOAD, ts(10), ""), op{kind: opUpsert, msg: store.Message{
			ID: "m10", ContainerID: "1", SenderID: "+15551230001", SenderName: "Farid Haddad", SentAt: t0.Add(10 * time.Minute), MediaType: "attachment",
		}}, true},
		{"deleted", faridDM(), message("m1", "1", "", gmproto.MessageStatusType_MESSAGE_DELETED, ts(11), ""), op{kind: opDelete, target: "m1"}, true},
		{"tombstone", family(), message("m12", "3", "21", gmproto.MessageStatusType_TOMBSTONE_PARTICIPANT_JOINED, ts(12), "Farid joined"), op{}, false},
		{"no content", faridDM(), message("m13", "1", "11", gmproto.MessageStatusType_INCOMING_FAILED_TO_DECRYPT, ts(13), ""), op{}, false},
	} {
		got, ok := convert(tc.m, tc.conv)
		if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, %v\nwant %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestContainer(t *testing.T) {
	t.Parallel()
	unnamed := family()
	unnamed.Name = ""
	for _, tc := range []struct {
		conv *gmproto.Conversation
		want store.Container
	}{
		{faridDM(), store.Container{ID: "1", Kind: "dm", Name: "Farid Haddad", LastMessageAt: t0.Add(2 * time.Minute)}},
		// The name a stranger's RCS profile gives is not the chat's name.
		{strangerDM(), store.Container{ID: "2", Kind: "dm", Name: "(555) 999-0002", LastMessageAt: t0.Add(time.Minute)}},
		{family(), store.Container{
			ID: "3", Kind: "group", IsGroup: true, Name: "Family", LastMessageAt: t0.Add(3 * time.Minute),
			Participants: []store.Participant{{ID: "+15551230001", Name: "Farid Haddad"}, {ID: "+15551230003", Name: "Mina"}},
		}},
		{unnamed, store.Container{
			ID: "3", Kind: "group", IsGroup: true, Name: "Farid Haddad, Mina", LastMessageAt: t0.Add(3 * time.Minute),
			Participants: []store.Participant{{ID: "+15551230001", Name: "Farid Haddad"}, {ID: "+15551230003", Name: "Mina"}},
		}},
	} {
		if got := container(tc.conv); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("container(%s):\n got %+v\nwant %+v", tc.conv.GetConversationID(), got, tc.want)
		}
	}
}
