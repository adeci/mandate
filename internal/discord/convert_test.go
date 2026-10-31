package discord

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/adeci/mandate/internal/store"
)

const self = "100"

var (
	ts    = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	alice = &apiUser{ID: "200", Username: "alice", GlobalName: "Alice A"}
	bob   = &apiUser{ID: "300", Username: "bob"}
	me    = &apiUser{ID: self, Username: "me"}
)

func TestToMessage(t *testing.T) {
	tests := []struct {
		name   string
		in     *apiMessage
		want   store.Message
		wantOK bool
	}{
		{
			name: "plain",
			in:   &apiMessage{ID: "1", ChannelID: "c", Content: "hi", Timestamp: ts, Author: alice},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, Body: "hi"}, wantOK: true,
		},
		{
			name: "username when no global name",
			in:   &apiMessage{ID: "1", ChannelID: "c", Content: "hi", Timestamp: ts, Author: bob},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: "300", SenderName: "bob", SentAt: ts, Body: "hi"}, wantOK: true,
		},
		{
			name: "from me",
			in:   &apiMessage{ID: "1", ChannelID: "c", Content: "yo", Timestamp: ts, Author: me},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: self, SenderName: "me", SentAt: ts, IsFromMe: true, Body: "yo"}, wantOK: true,
		},
		{
			name: "attachment with content type and caption",
			in: &apiMessage{
				ID: "1", ChannelID: "c", Content: "look", Timestamp: ts, Author: alice,
				Attachments: []apiAttachment{{ID: "a", ContentType: "image/png"}, {ID: "b", ContentType: "video/mp4"}},
			},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, Body: "look", MediaType: "image/png"}, wantOK: true,
		},
		{
			name: "attachment without content type",
			in: &apiMessage{
				ID: "1", ChannelID: "c", Timestamp: ts, Author: alice,
				Attachments: []apiAttachment{{ID: "a", Filename: "x.bin"}},
			},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, MediaType: "attachment"}, wantOK: true,
		},
		{
			name: "sticker",
			in:   &apiMessage{ID: "1", ChannelID: "c", Timestamp: ts, Author: alice, StickerItems: []struct{}{{}}},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, MediaType: "sticker"}, wantOK: true,
		},
		{
			name: "embed only",
			in:   &apiMessage{ID: "1", ChannelID: "c", Timestamp: ts, Author: bob, Embeds: []struct{}{{}}},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: "300", SenderName: "bob", SentAt: ts, MediaType: "embed"}, wantOK: true,
		},
		{
			name: "link with unfurl embed is plain text",
			in:   &apiMessage{ID: "1", ChannelID: "c", Content: "https://x", Timestamp: ts, Author: bob, Embeds: []struct{}{{}}},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: "300", SenderName: "bob", SentAt: ts, Body: "https://x"}, wantOK: true,
		},
		{
			name: "reply",
			in: &apiMessage{
				ID: "2", ChannelID: "c", Content: "re", Timestamp: ts, Author: alice, Type: messageReply,
				MessageReference: &apiMessageReference{MessageID: "1", ChannelID: "c"},
			},
			want: store.Message{ID: "2", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, Body: "re", ReplyToID: "1"}, wantOK: true,
		},
		{
			name: "crosspost reference is not a reply",
			in: &apiMessage{
				ID: "2", ChannelID: "c", Content: "news", Timestamp: ts, Author: alice,
				MessageReference: &apiMessageReference{MessageID: "9", ChannelID: "other"},
			},
			want: store.Message{ID: "2", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, Body: "news"}, wantOK: true,
		},
		{
			name: "forward uses snapshot content",
			in: &apiMessage{
				ID: "2", ChannelID: "c", Timestamp: ts, Author: me,
				MessageReference: &apiMessageReference{Type: referenceForward, MessageID: "9", ChannelID: "other"},
				MessageSnapshots: []apiSnapshot{{Message: &apiMessage{
					Content:     "fwd",
					Attachments: []apiAttachment{{ContentType: "audio/ogg"}},
				}}},
			},
			want: store.Message{ID: "2", ContainerID: "c", SenderID: self, SenderName: "me", SentAt: ts, IsFromMe: true, Body: "fwd", MediaType: "audio/ogg"}, wantOK: true,
		},
		{
			name: "timestamp from snowflake",
			// (1<<22) ms after the Discord epoch.
			in:   &apiMessage{ID: "17592186044416", ChannelID: "c", Content: "x", Author: bob},
			want: store.Message{ID: "17592186044416", ContainerID: "c", SenderID: "300", SenderName: "bob", SentAt: time.UnixMilli(1420070400000 + 1<<22), Body: "x"}, wantOK: true,
		},
		{
			name: "slash command response",
			in:   &apiMessage{ID: "1", ChannelID: "c", Content: "pong", Timestamp: ts, Author: bob, Type: messageChatInputCommand},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: "300", SenderName: "bob", SentAt: ts, Body: "pong"}, wantOK: true,
		},
		{
			name: "no author",
			in:   &apiMessage{ID: "1", ChannelID: "c", Content: "x", Timestamp: ts},
			want: store.Message{ID: "1", ContainerID: "c", SentAt: ts, Body: "x"}, wantOK: true,
		},
		{name: "system join notice skipped", in: &apiMessage{ID: "1", ChannelID: "c", Timestamp: ts, Author: bob, Type: 7}},
		{name: "pin notice skipped", in: &apiMessage{ID: "1", ChannelID: "c", Timestamp: ts, Author: bob, Type: 6}},
		{name: "thread starter skipped", in: &apiMessage{ID: "1", ChannelID: "c", Timestamp: ts, Author: bob, Type: 21}},
		{name: "missing channel", in: &apiMessage{ID: "1", Content: "x", Timestamp: ts}},
		{name: "unparseable id without timestamp", in: &apiMessage{ID: "x", ChannelID: "c", Content: "x"}},
		{name: "nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toMessage(tt.in, self)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !got.SentAt.Equal(tt.want.SentAt) {
				t.Errorf("SentAt = %v, want %v", got.SentAt, tt.want.SentAt)
			}
			got.SentAt, tt.want.SentAt = time.Time{}, time.Time{}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestMessageJSON(t *testing.T) {
	raw := `{"id":"1300000000000000002","channel_id":"1300000000000000010","type":19,"content":"re",
		"timestamp":"2026-10-01T12:00:00.123000+00:00","edited_timestamp":null,
		"author":{"id":"200","username":"alice","global_name":"Alice A"},
		"attachments":[{"id":"a","filename":"p.png","content_type":"image/png"}],"embeds":[{"type":"rich"}],
		"sticker_items":[{"id":"s"}],"message_reference":{"type":0,"message_id":"1300000000000000001","channel_id":"1300000000000000010"},
		"message_snapshots":[{"message":{"content":"fwd"}}],"hit":true}`
	var m apiMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	got, ok := toMessage(&m, "")
	if !ok {
		t.Fatal("not converted")
	}
	want := store.Message{
		ID: "1300000000000000002", ContainerID: "1300000000000000010", SenderID: "200", SenderName: "Alice A",
		SentAt: time.Date(2026, 10, 1, 12, 0, 0, 123000000, time.UTC), Body: "re", MediaType: "image/png", ReplyToID: "1300000000000000001",
	}
	if !got.SentAt.Equal(want.SentAt) {
		t.Errorf("SentAt = %v, want %v", got.SentAt, want.SentAt)
	}
	got.SentAt, want.SentAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if !m.Hit || len(m.StickerItems) != 1 || len(m.Embeds) != 1 || len(m.MessageSnapshots) != 1 {
		t.Errorf("fields not decoded: %+v", m)
	}
}

func TestSearchHit(t *testing.T) {
	tests := []struct {
		name  string
		group []apiMessage
		want  string
	}{
		{name: "single", group: []apiMessage{{ID: "a"}}, want: "a"},
		{name: "context group picks hit", group: []apiMessage{{ID: "a"}, {ID: "b", Hit: true}, {ID: "c"}}, want: "b"},
		{name: "empty", group: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := searchHit(tt.group)
			if (got == nil) != (tt.want == "") || (got != nil && got.ID != tt.want) {
				t.Errorf("got %+v, want %q", got, tt.want)
			}
		})
	}
}

func TestChannelContainer(t *testing.T) {
	tests := []struct {
		name   string
		in     *apiChannel
		want   store.Container
		wantOK bool
	}{
		{
			name: "text channel",
			in:   &apiChannel{ID: "c", GuildID: "g", Name: "general", Type: 0, ParentID: "cat"},
			want: store.Container{ID: "c", Kind: "channel", ParentID: "g", Name: "general"}, wantOK: true,
		},
		{
			name: "voice channel text chat",
			in:   &apiChannel{ID: "v", GuildID: "g", Name: "lounge", Type: 2},
			want: store.Container{ID: "v", Kind: "channel", ParentID: "g", Name: "lounge"}, wantOK: true,
		},
		{
			name: "public thread parents to guild, not channel",
			in:   &apiChannel{ID: "t", GuildID: "g", ParentID: "c", Name: "topic", Type: channelPublicThread},
			want: store.Container{ID: "t", Kind: "thread", ParentID: "g", Name: "topic"}, wantOK: true,
		},
		{
			name: "private thread",
			in:   &apiChannel{ID: "t", GuildID: "g", ParentID: "c", Name: "secret", Type: channelPrivateThread},
			want: store.Container{ID: "t", Kind: "thread", ParentID: "g", Name: "secret"}, wantOK: true,
		},
		{
			name: "news thread",
			in:   &apiChannel{ID: "t", GuildID: "g", Type: channelNewsThread},
			want: store.Container{ID: "t", Kind: "thread", ParentID: "g"}, wantOK: true,
		},
		{
			name: "dm uses the recipient's name",
			in:   &apiChannel{ID: "d", Type: channelDM, Recipients: []apiUser{*alice}},
			want: store.Container{ID: "d", Kind: "dm", Name: "Alice A", Participants: []store.Participant{{ID: "200", Name: "Alice A"}}}, wantOK: true,
		},
		{
			name: "dm without recipients leaves participants unknown",
			in:   &apiChannel{ID: "d", Type: channelDM},
			want: store.Container{ID: "d", Kind: "dm"}, wantOK: true,
		},
		{
			name: "named group dm",
			in:   &apiChannel{ID: "gd", Name: "crew", Type: channelGroupDM, Recipients: []apiUser{*alice, *bob}},
			want: store.Container{
				ID: "gd", Kind: "group", Name: "crew", IsGroup: true,
				Participants: []store.Participant{{ID: "200", Name: "Alice A"}, {ID: "300", Name: "bob"}},
			}, wantOK: true,
		},
		{
			name: "unnamed group dm uses member names",
			in:   &apiChannel{ID: "gd", Type: channelGroupDM, Recipients: []apiUser{*alice, *bob}},
			want: store.Container{
				ID: "gd", Kind: "group", Name: "Alice A, bob", IsGroup: true,
				Participants: []store.Participant{{ID: "200", Name: "Alice A"}, {ID: "300", Name: "bob"}},
			}, wantOK: true,
		},
		{name: "category skipped", in: &apiChannel{ID: "cat", GuildID: "g", Type: channelGuildCategory}},
		{name: "guild channel without guild skipped", in: &apiChannel{ID: "c", Type: 0}},
		{name: "no id", in: &apiChannel{Type: channelDM}},
		{name: "nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := channelContainer(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestGuildContainers(t *testing.T) {
	g := &apiGuild{ID: "g", Name: "Club"}
	gc, ok := guildContainer(g)
	if want := (store.Container{ID: "g", Kind: "guild", Name: "Club"}); !ok || !reflect.DeepEqual(gc, want) {
		t.Errorf("guild = %+v, %v; want %+v", gc, ok, want)
	}
	if _, ok := guildContainer(&apiGuild{ID: "g", Unavailable: true}); ok {
		t.Error("unavailable guild converted")
	}
	chans := []apiChannel{
		{ID: "cat", GuildID: "g", Type: channelGuildCategory, Name: "Text"},
		{ID: "c", Type: 0, Name: "general"}, // guild_id omitted
		{ID: "t", ParentID: "c", Type: channelPublicThread, Name: "topic"},
	}
	want := []store.Container{
		{ID: "c", Kind: "channel", ParentID: "g", Name: "general"},
		{ID: "t", Kind: "thread", ParentID: "g", Name: "topic"},
	}
	if got := guildChannelContainers("g", chans); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if chans[1].GuildID != "" {
		t.Error("input slice modified")
	}
}
