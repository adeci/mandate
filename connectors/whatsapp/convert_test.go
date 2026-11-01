package main

import (
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/adeci/mandate/store"
)

var (
	ts      = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	alice   = types.NewJID("15550001111", types.DefaultUserServer)
	aliceAD = types.NewADJID("15550001111", 0, 3)
	aliceL  = types.NewJID("987654321", types.HiddenUserServer)
	me      = types.NewJID("15559990000", types.DefaultUserServer)
	group   = types.NewJID("120363000000000001", types.GroupServer)
)

func resolver(m map[types.JID]types.JID) pnResolver {
	return func(j types.JID) types.JID {
		if p, ok := m[j]; ok {
			return p
		}
		return j
	}
}

func dmInfo(id string) types.MessageInfo {
	return types.MessageInfo{
		MessageSource: types.MessageSource{Chat: alice, Sender: aliceAD},
		ID:            id,
		PushName:      "Alice",
		Timestamp:     ts,
	}
}

func groupInfo(id string) types.MessageInfo {
	return types.MessageInfo{
		MessageSource: types.MessageSource{Chat: group, Sender: aliceAD, IsGroup: true},
		ID:            id,
		PushName:      "Alice",
		Timestamp:     ts,
	}
}

func TestConvert(t *testing.T) {
	claimed := ts.Add(-time.Hour) // the editor's own clock, which is ignored
	tests := []struct {
		name  string
		evt   *events.Message
		pn    map[types.JID]types.JID
		want  op
		wantN bool // want skipped
	}{
		{
			name: "text dm",
			evt:  &events.Message{Info: dmInfo("A1"), Message: &waE2E.Message{Conversation: proto.String("hi")}},
			want: op{kind: opUpsert, chat: alice, sender: alice, msg: store.Message{
				ID: "15550001111@s.whatsapp.net/A1", ContainerID: "15550001111@s.whatsapp.net",
				SenderID: "15550001111@s.whatsapp.net", SenderName: "Alice", SentAt: ts, Body: "hi",
			}},
		},
		{
			name: "extended text reply in group",
			evt: &events.Message{Info: groupInfo("A2"), Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text:        proto.String("see https://example.com"),
				ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("A0")},
			}}},
			want: op{kind: opUpsert, chat: group, sender: alice, msg: store.Message{
				ID: "120363000000000001@g.us/A2", ContainerID: "120363000000000001@g.us",
				SenderID: "15550001111@s.whatsapp.net", SenderName: "Alice", SentAt: ts,
				Body: "see https://example.com", ReplyToID: "120363000000000001@g.us/A0",
			}},
		},
		{
			name: "captioned image in group",
			evt: &events.Message{Info: groupInfo("A3"), Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Caption: proto.String("look"), Mimetype: proto.String("image/jpeg"),
			}}},
			want: op{kind: opUpsert, chat: group, sender: alice, msg: store.Message{
				ID: "120363000000000001@g.us/A3", ContainerID: "120363000000000001@g.us",
				SenderID: "15550001111@s.whatsapp.net", SenderName: "Alice", SentAt: ts,
				Body: "look", MediaType: "image",
			}},
		},
		{
			name: "own message from phone",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: alice, Sender: types.NewADJID(me.User, 0, 0), IsFromMe: true},
					ID:            "A4", Timestamp: ts,
				},
				Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("x.pdf")}},
			},
			want: op{kind: opUpsert, chat: alice, sender: me, msg: store.Message{
				ID: "15550001111@s.whatsapp.net/A4", ContainerID: "15550001111@s.whatsapp.net",
				SenderID: "15559990000@s.whatsapp.net", SentAt: ts, IsFromMe: true, MediaType: "document",
			}},
		},
		{
			name: "lid group sender prefers phone number alt",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: group, Sender: aliceL, SenderAlt: alice, IsGroup: true},
					ID:            "A5", Timestamp: ts,
				},
				Message: &waE2E.Message{Conversation: proto.String("x")},
			},
			want: op{kind: opUpsert, chat: group, sender: alice, msg: store.Message{
				ID: "120363000000000001@g.us/A5", ContainerID: "120363000000000001@g.us",
				SenderID: "15550001111@s.whatsapp.net", SentAt: ts, Body: "x",
			}},
		},
		{
			name: "lid dm resolved through mapping",
			pn:   map[types.JID]types.JID{aliceL: alice},
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: aliceL, Sender: aliceL},
					ID:            "A6", Timestamp: ts,
				},
				Message: &waE2E.Message{Conversation: proto.String("x")},
			},
			want: op{kind: opUpsert, chat: alice, sender: alice, msg: store.Message{
				ID: "15550001111@s.whatsapp.net/A6", ContainerID: "15550001111@s.whatsapp.net",
				SenderID: "15550001111@s.whatsapp.net", SentAt: ts, Body: "x",
			}},
		},
		{
			name: "poll",
			evt: &events.Message{Info: groupInfo("A7"), Message: &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
				Name: proto.String("lunch?"),
			}}},
			want: op{kind: opUpsert, chat: group, sender: alice, msg: store.Message{
				ID: "120363000000000001@g.us/A7", ContainerID: "120363000000000001@g.us",
				SenderID: "15550001111@s.whatsapp.net", SenderName: "Alice", SentAt: ts, Body: "lunch?", MediaType: "poll",
			}},
		},
		{
			name: "location",
			evt: &events.Message{Info: dmInfo("A8"), Message: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
				Name: proto.String("Cafe"), Address: proto.String("1 Main St"),
			}}},
			want: op{kind: opUpsert, chat: alice, sender: alice, msg: store.Message{
				ID: "15550001111@s.whatsapp.net/A8", ContainerID: "15550001111@s.whatsapp.net",
				SenderID: "15550001111@s.whatsapp.net", SenderName: "Alice", SentAt: ts,
				Body: "Cafe\n1 Main St", MediaType: "location",
			}},
		},
		{
			name: "edit unwrapped from EditedMessage",
			evt: &events.Message{Info: dmInfo("E1"), IsEdit: true, Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
				Key:           &waCommon.MessageKey{ID: proto.String("A1")},
				EditedMessage: &waE2E.Message{Conversation: proto.String("hi!")},
				TimestampMS:   proto.Int64(claimed.UnixMilli()),
			}}},
			want: op{kind: opEdit, chat: alice, sender: alice, target: "15550001111@s.whatsapp.net/A1", body: "hi!", at: ts},
		},
		{
			name: "edit of caption",
			evt: &events.Message{Info: groupInfo("E2"), Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
				Key:  &waCommon.MessageKey{ID: proto.String("A3")},
				EditedMessage: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
					Caption: proto.String("look again"),
				}},
			}}},
			want: op{kind: opEdit, chat: group, sender: alice, target: "120363000000000001@g.us/A3", body: "look again", at: ts},
		},
		{
			name: "revoke",
			evt: &events.Message{Info: groupInfo("R1"), Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				Type: waE2E.ProtocolMessage_REVOKE.Enum(),
				Key:  &waCommon.MessageKey{ID: proto.String("A3")},
			}}},
			want: op{kind: opDelete, chat: group, sender: alice, target: "120363000000000001@g.us/A3", at: ts},
		},
		{
			name: "reaction skipped",
			evt: &events.Message{Info: dmInfo("X1"), Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
				Text: proto.String("+1"),
			}}},
			wantN: true,
		},
		{
			name: "status broadcast skipped",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: types.StatusBroadcastJID, Sender: aliceAD, IsGroup: true},
					ID:            "X2", Timestamp: ts,
				},
				Message: &waE2E.Message{Conversation: proto.String("status")},
			},
			wantN: true,
		},
		{
			name: "incoming broadcast list lands in sender dm",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: types.NewJID("1234", types.BroadcastServer), Sender: aliceAD, IsGroup: true},
					ID:            "B1", Timestamp: ts,
				},
				Message: &waE2E.Message{Conversation: proto.String("hello all")},
			},
			want: op{kind: opUpsert, chat: alice, sender: alice, msg: store.Message{
				ID: "15550001111@s.whatsapp.net/B1", ContainerID: "15550001111@s.whatsapp.net",
				SenderID: "15550001111@s.whatsapp.net", SentAt: ts, Body: "hello all",
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := convert(tt.evt, resolver(tt.pn))
			if tt.wantN {
				if ok {
					t.Fatalf("convert = %+v, want skipped", got)
				}
				return
			}
			if !ok {
				t.Fatal("convert skipped message")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("convert =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestConvertHistorySync(t *testing.T) {
	cli := whatsmeow.NewClient(&wastore.Device{ID: &me}, nil)
	sec := uint64(ts.Unix())
	tests := []struct {
		name  string
		chat  types.JID
		web   *waWeb.WebMessageInfo
		want  op
		wantN bool
	}{
		{
			name: "incoming dm",
			chat: alice,
			web: &waWeb.WebMessageInfo{
				Key:              &waCommon.MessageKey{RemoteJID: proto.String(alice.String()), ID: proto.String("H1")},
				MessageTimestamp: &sec,
				PushName:         proto.String("Alice"),
				Message:          &waE2E.Message{Conversation: proto.String("old")},
			},
			want: op{kind: opUpsert, chat: alice, sender: alice, msg: store.Message{
				ID: "15550001111@s.whatsapp.net/H1", ContainerID: "15550001111@s.whatsapp.net",
				SenderID: "15550001111@s.whatsapp.net", SenderName: "Alice", SentAt: ts, Body: "old",
			}},
		},
		{
			name: "own group message",
			chat: group,
			web: &waWeb.WebMessageInfo{
				Key:              &waCommon.MessageKey{RemoteJID: proto.String(group.String()), FromMe: proto.Bool(true), ID: proto.String("H2")},
				MessageTimestamp: &sec,
				Message:          &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("clip")}},
			},
			want: op{kind: opUpsert, chat: group, sender: me, msg: store.Message{
				ID: "120363000000000001@g.us/H2", ContainerID: "120363000000000001@g.us",
				SenderID: "15559990000@s.whatsapp.net", SentAt: ts, IsFromMe: true, Body: "clip", MediaType: "video",
			}},
		},
		{
			name: "group message from participant",
			chat: group,
			web: &waWeb.WebMessageInfo{
				Key:              &waCommon.MessageKey{RemoteJID: proto.String(group.String()), ID: proto.String("H3")},
				Participant:      proto.String(alice.String()),
				MessageTimestamp: &sec,
				Message:          &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("yo")}},
			},
			want: op{kind: opUpsert, chat: group, sender: alice, msg: store.Message{
				ID: "120363000000000001@g.us/H3", ContainerID: "120363000000000001@g.us",
				SenderID: "15550001111@s.whatsapp.net", SentAt: ts, Body: "yo",
			}},
		},
		{
			name: "revoked stub",
			chat: alice,
			web: &waWeb.WebMessageInfo{
				Key:              &waCommon.MessageKey{RemoteJID: proto.String(alice.String()), ID: proto.String("H4")},
				MessageTimestamp: &sec,
				MessageStubType:  waWeb.WebMessageInfo_REVOKE.Enum(),
			},
			want: op{kind: opDelete, chat: alice, sender: alice, target: "15550001111@s.whatsapp.net/H4", at: ts},
		},
		{
			name: "system stub skipped",
			chat: group,
			web: &waWeb.WebMessageInfo{
				Key:              &waCommon.MessageKey{RemoteJID: proto.String(group.String()), ID: proto.String("H5")},
				Participant:      proto.String(alice.String()),
				MessageTimestamp: &sec,
				MessageStubType:  waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD.Enum(),
			},
			wantN: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt, err := cli.ParseWebMessage(tt.chat, tt.web)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := convert(evt, resolver(nil))
			if tt.wantN {
				if ok {
					t.Fatalf("convert = %+v, want skipped", got)
				}
				return
			}
			if !ok {
				t.Fatal("convert skipped message")
			}
			if !got.msg.SentAt.IsZero() {
				got.msg.SentAt = got.msg.SentAt.UTC()
			}
			got.at = got.at.UTC()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("convert =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestHistoryChat(t *testing.T) {
	pn := resolver(map[types.JID]types.JID{aliceL: alice})
	tests := []struct {
		raw    types.JID
		want   types.JID
		wantOK bool
	}{
		{alice, alice, true},
		{aliceL, alice, true},
		{group, group, true},
		{types.StatusBroadcastJID, types.EmptyJID, false},
	}
	for _, tt := range tests {
		got, ok := historyChat(tt.raw, pn)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("historyChat(%s) = %s, %v; want %s, %v", tt.raw, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestContainers(t *testing.T) {
	pn := resolver(map[types.JID]types.JID{aliceL: alice})
	tests := []struct {
		name string
		got  store.Container
		want store.Container
	}{
		{
			name: "history group",
			got: historyContainer(group, &waHistorySync.Conversation{
				ID:          proto.String(group.String()),
				Name:        proto.String("Family"),
				Participant: []*waHistorySync.GroupParticipant{{UserJID: proto.String(aliceL.String())}},
			}, pn),
			want: store.Container{
				ID: "120363000000000001@g.us", Kind: "group", IsGroup: true, Name: "Family",
				Participants: []store.Participant{{ID: "15550001111@s.whatsapp.net"}},
			},
		},
		{
			name: "history dm dated by its conversation",
			got: historyContainer(alice, &waHistorySync.Conversation{
				ID: proto.String(alice.String()), DisplayName: proto.String("Alice"), ConversationTimestamp: proto.Uint64(1793385600),
			}, pn),
			want: store.Container{ID: "15550001111@s.whatsapp.net", Kind: "dm", Name: "Alice", LastMessageAt: time.Unix(1793385600, 0).UTC()},
		},
		{
			name: "group info query",
			got: groupContainer(&types.GroupInfo{
				JID:       group,
				GroupName: types.GroupName{Name: "Family"},
				Participants: []types.GroupParticipant{
					{JID: aliceL, PhoneNumber: alice, LID: aliceL},
					{JID: me},
				},
			}, pn, func(j types.JID) string {
				if j == alice {
					return "Alice A."
				}
				return ""
			}),
			want: store.Container{
				ID: "120363000000000001@g.us", Kind: "group", IsGroup: true, Name: "Family",
				Participants: []store.Participant{
					{ID: "15550001111@s.whatsapp.net", Name: "Alice A."},
					{ID: "15559990000@s.whatsapp.net"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(tt.got, tt.want) {
				t.Fatalf("got\n%+v\nwant\n%+v", tt.got, tt.want)
			}
		})
	}
}
