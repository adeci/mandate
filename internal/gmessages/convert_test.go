package gmessages

import (
	"reflect"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/sieve/internal/store"
)

func participant(pid, number, full, first, formatted string, visible, me bool) *gmproto.Participant {
	return &gmproto.Participant{
		ID:              &gmproto.SmallInfo{ParticipantID: pid, Number: number},
		FullName:        full,
		FirstName:       first,
		FormattedNumber: formatted,
		IsVisible:       visible,
		IsMe:            me,
	}
}

func status(s gmproto.MessageStatusType) *gmproto.MessageStatus {
	return &gmproto.MessageStatus{Status: s}
}

func text(s string) *gmproto.MessageInfo {
	return &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MessageContent{
		MessageContent: &gmproto.MessageContent{Content: s},
	}}
}

func media(mime string, format gmproto.MediaFormats) *gmproto.MessageInfo {
	return &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MediaContent{
		MediaContent: &gmproto.MediaContent{MimeType: mime, Format: format, MediaName: "file"},
	}}
}

// dmConv is a one-to-one conversation: self, Alice, and a hidden duplicate
// entry for Alice's number under another participant ID.
var dmConv = &gmproto.Conversation{
	ConversationID: "c1",
	Name:           "Alice",
	Participants: []*gmproto.Participant{
		participant("1", "+15550000000", "Me Myself", "", "(555) 000-0000", true, true),
		participant("5", "+15551111111", "Alice Smith", "Alice", "(555) 111-1111", true, false),
		participant("9", "+15551111111", "", "", "", false, false),
	},
}

var groupConv = &gmproto.Conversation{
	ConversationID: "c2",
	Name:           "Family",
	IsGroupChat:    true,
	Participants: []*gmproto.Participant{
		participant("1", "+15550000000", "", "Me", "", false, true),
		participant("6", "+15552222222", "", "Bob", "", true, false),
		participant("7", "", "", "", "(555) 333-3333", true, false),
		participant("8", unknownSenderNumber, "Carol", "", "", true, false),
	},
}

func TestConversationToContainer(t *testing.T) {
	tests := []struct {
		name string
		conv *gmproto.Conversation
		want store.Container
	}{
		{
			name: "dm skips hidden duplicate",
			conv: dmConv,
			want: store.Container{
				ID: "c1", Kind: "dm", Name: "Alice",
				Participants: []store.Participant{
					{ID: "+15550000000", Name: "Me Myself"},
					{ID: "+15551111111", Name: "Alice Smith"},
				},
			},
		},
		{
			name: "group with name fallbacks and missing numbers",
			conv: groupConv,
			want: store.Container{
				ID: "c2", Kind: "group", Name: "Family", IsGroup: true,
				Participants: []store.Participant{
					{ID: "+15550000000", Name: "Me"},
					{ID: "+15552222222", Name: "Bob"},
					{ID: "7", Name: "(555) 333-3333"},
					{ID: "8", Name: "Carol"},
				},
			},
		},
		{
			name: "no participants leaves list unset",
			conv: &gmproto.Conversation{ConversationID: "c3", Name: "Empty"},
			want: store.Container{ID: "c3", Kind: "dm", Name: "Empty"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := conversationToContainer(tt.conv)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestMessageToStore(t *testing.T) {
	const ts = int64(1_700_000_000_123_456)
	sentAt := time.UnixMicro(ts)
	subject := "Weekend"
	tests := []struct {
		name   string
		msg    *gmproto.Message
		conv   *gmproto.Conversation
		want   store.Message
		wantOK bool
	}{
		{
			name: "incoming text resolves sender number and name",
			msg: &gmproto.Message{
				MessageID: "m1", ConversationID: "c1", ParticipantID: "5", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_COMPLETE),
				MessageInfo:   []*gmproto.MessageInfo{text("hello"), text("world")},
				ReplyMessage:  &gmproto.ReplyMessage{MessageID: "m0"},
			},
			conv: dmConv,
			want: store.Message{
				ID: "m1", ContainerID: "c1", SenderID: "+15551111111", SenderName: "Alice Smith",
				SentAt: sentAt, Body: "hello\nworld", ReplyToID: "m0",
			},
			wantOK: true,
		},
		{
			name: "hidden participant maps to visible entry with same number",
			msg: &gmproto.Message{
				MessageID: "m2", ConversationID: "c1", ParticipantID: "9", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_DISPLAYED),
				MessageInfo:   []*gmproto.MessageInfo{text("hi")},
			},
			conv: dmConv,
			want: store.Message{
				ID: "m2", ContainerID: "c1", SenderID: "+15551111111", SenderName: "Alice Smith",
				SentAt: sentAt, Body: "hi",
			},
			wantOK: true,
		},
		{
			name: "media with caption",
			msg: &gmproto.Message{
				MessageID: "m3", ConversationID: "c2", ParticipantID: "6", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_COMPLETE),
				MessageInfo: []*gmproto.MessageInfo{
					media("image/jpeg", gmproto.MediaFormats_IMAGE_JPEG),
					text("look at this"),
				},
			},
			conv: groupConv,
			want: store.Message{
				ID: "m3", ContainerID: "c2", SenderID: "+15552222222", SenderName: "Bob",
				SentAt: sentAt, Body: "look at this", MediaType: "image/jpeg",
			},
			wantOK: true,
		},
		{
			name: "media without caption or mime uses format",
			msg: &gmproto.Message{
				MessageID: "m4", ConversationID: "c2", ParticipantID: "7", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_COMPLETE),
				MessageInfo:   []*gmproto.MessageInfo{media("", gmproto.MediaFormats_VIDEO_MP4)},
			},
			conv: groupConv,
			want: store.Message{
				ID: "m4", ContainerID: "c2", SenderID: "7", SenderName: "(555) 333-3333",
				SentAt: sentAt, MediaType: "video/mp4",
			},
			wantOK: true,
		},
		{
			name: "unspecified image format gives class only",
			msg: &gmproto.Message{
				MessageID: "m5", ConversationID: "c2", ParticipantID: "6", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_COMPLETE),
				MessageInfo:   []*gmproto.MessageInfo{media("", gmproto.MediaFormats_IMAGE_UNSPECIFIED)},
			},
			conv: groupConv,
			want: store.Message{
				ID: "m5", ContainerID: "c2", SenderID: "+15552222222", SenderName: "Bob",
				SentAt: sentAt, MediaType: "image",
			},
			wantOK: true,
		},
		{
			name: "outgoing status is from me with own number",
			msg: &gmproto.Message{
				MessageID: "m6", ConversationID: "c1", ParticipantID: "1", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_OUTGOING_DELIVERED),
				MessageInfo:   []*gmproto.MessageInfo{text("on my way")},
			},
			conv: dmConv,
			want: store.Message{
				ID: "m6", ContainerID: "c1", SenderID: "+15550000000", SenderName: "Me Myself",
				SentAt: sentAt, IsFromMe: true, Body: "on my way",
			},
			wantOK: true,
		},
		{
			name: "outgoing with unknown participant falls back to self entry",
			msg: &gmproto.Message{
				MessageID: "m7", ConversationID: "c2", ParticipantID: "42", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_OUTGOING_SENDING),
				MessageInfo:   []*gmproto.MessageInfo{text("sending")},
			},
			conv: groupConv,
			want: store.Message{
				ID: "m7", ContainerID: "c2", SenderID: "+15550000000", SenderName: "Me",
				SentAt: sentAt, IsFromMe: true, Body: "sending",
			},
			wantOK: true,
		},
		{
			name: "unknown status uses IsMe flag",
			msg: &gmproto.Message{
				MessageID: "m8", ConversationID: "c1", ParticipantID: "1", Timestamp: ts,
				MessageInfo: []*gmproto.MessageInfo{text("x")},
			},
			conv: dmConv,
			want: store.Message{
				ID: "m8", ContainerID: "c1", SenderID: "+15550000000", SenderName: "Me Myself",
				SentAt: sentAt, IsFromMe: true, Body: "x",
			},
			wantOK: true,
		},
		{
			name: "incoming status overrides participant ID 1",
			msg: &gmproto.Message{
				MessageID: "m9", ConversationID: "c9", ParticipantID: "1", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_COMPLETE),
				MessageInfo:   []*gmproto.MessageInfo{text("x")},
			},
			want: store.Message{
				ID: "m9", ContainerID: "c9", SenderID: "1", SentAt: sentAt, Body: "x",
			},
			wantOK: true,
		},
		{
			name: "unknown conversation uses sender participant on message",
			msg: &gmproto.Message{
				MessageID: "m10", ConversationID: "c9", ParticipantID: "3", Timestamp: ts,
				MessageStatus:     status(gmproto.MessageStatusType_INCOMING_COMPLETE),
				MessageInfo:       []*gmproto.MessageInfo{text("x")},
				SenderParticipant: participant("3", "+15554444444", "Dave", "", "", true, false),
			},
			want: store.Message{
				ID: "m10", ContainerID: "c9", SenderID: "+15554444444", SenderName: "Dave",
				SentAt: sentAt, Body: "x",
			},
			wantOK: true,
		},
		{
			name: "unknown conversation without sender info keeps participant ID",
			msg: &gmproto.Message{
				MessageID: "m11", ConversationID: "c9", ParticipantID: "3", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_COMPLETE),
				MessageInfo:   []*gmproto.MessageInfo{text("x")},
			},
			want: store.Message{
				ID: "m11", ContainerID: "c9", SenderID: "3", SentAt: sentAt, Body: "x",
			},
			wantOK: true,
		},
		{
			name: "subject precedes text",
			msg: &gmproto.Message{
				MessageID: "m12", ConversationID: "c1", ParticipantID: "5", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_COMPLETE),
				Subject:       &subject,
				MessageInfo:   []*gmproto.MessageInfo{text("plans?")},
			},
			conv: dmConv,
			want: store.Message{
				ID: "m12", ContainerID: "c1", SenderID: "+15551111111", SenderName: "Alice Smith",
				SentAt: sentAt, Body: "Weekend\nplans?",
			},
			wantOK: true,
		},
		{
			name: "no content yet",
			msg: &gmproto.Message{
				MessageID: "m13", ConversationID: "c1", ParticipantID: "5", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_YET_TO_MANUAL_DOWNLOAD),
			},
			conv: dmConv,
		},
		{
			name: "auto-downloading MMS skipped",
			msg: &gmproto.Message{
				MessageID: "m14", ConversationID: "c1", ParticipantID: "5", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING),
				MessageInfo:   []*gmproto.MessageInfo{text("x")},
			},
			conv: dmConv,
		},
		{
			name: "draft skipped",
			msg: &gmproto.Message{
				MessageID: "m15", ConversationID: "c1", ParticipantID: "1", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_OUTGOING_DRAFT),
				MessageInfo:   []*gmproto.MessageInfo{text("unsent")},
			},
			conv: dmConv,
		},
		{
			name: "tombstone skipped",
			msg: &gmproto.Message{
				MessageID: "m16", ConversationID: "c2", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_TOMBSTONE_PARTICIPANT_JOINED),
				MessageInfo:   []*gmproto.MessageInfo{text("Bob joined")},
			},
			conv: groupConv,
		},
		{
			name: "deletion is not an upsert",
			msg: &gmproto.Message{
				MessageID: "m17", ConversationID: "c1", Timestamp: ts,
				MessageStatus: status(gmproto.MessageStatusType_MESSAGE_DELETED),
				MessageInfo:   []*gmproto.MessageInfo{text("x")},
			},
			conv: dmConv,
		},
		{
			name: "missing timestamp",
			msg: &gmproto.Message{
				MessageID: "m18", ConversationID: "c1", ParticipantID: "5",
				MessageStatus: status(gmproto.MessageStatusType_INCOMING_COMPLETE),
				MessageInfo:   []*gmproto.MessageInfo{text("x")},
			},
			conv: dmConv,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := messageToStore(tt.msg, tt.conv)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestIsDeletion(t *testing.T) {
	tests := []struct {
		status gmproto.MessageStatusType
		want   bool
	}{
		{gmproto.MessageStatusType_MESSAGE_DELETED, true},
		{gmproto.MessageStatusType_INCOMING_COMPLETE, false},
		{gmproto.MessageStatusType_OUTGOING_COMPLETE, false},
	}
	for _, tt := range tests {
		t.Run(tt.status.String(), func(t *testing.T) {
			if got := isDeletion(&gmproto.Message{MessageStatus: status(tt.status)}); got != tt.want {
				t.Errorf("isDeletion = %v, want %v", got, tt.want)
			}
		})
	}
}
