package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/adeci/mandate/store"
)

const self = "100"

var (
	ts    = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	alice = &apiUser{ID: "200", Username: "alice", GlobalName: "Alice A"}
	bob   = &apiUser{ID: "300", Username: "bob", Discriminator: "0042"}
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
			name: "attachments with caption",
			in: &apiMessage{
				ID: "1", ChannelID: "c", Content: "look", Timestamp: ts, Author: alice,
				Attachments: []apiAttachment{
					{ID: "a1", Filename: "rig.png", ContentType: "image/png", Size: 3, URL: "https://cdn.discordapp.com/attachments/1100000000000000002/1200000000000000001/rig.png?ex=1&is=2&hm=3&"},
					{ID: "a2", Size: 5},
				},
			},
			want: store.Message{
				ID: "1", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, Body: "look",
				Media: []store.Media{
					{
						Type: "image/png", Name: "rig.png", Size: 3,
						Ref: []byte(`{"channel_id":"c","message_id":"1","id":"a1","path":"/attachments/1100000000000000002/1200000000000000001/rig.png"}`),
					},
					{Type: "application/octet-stream", Size: 5, Ref: []byte(`{"channel_id":"c","message_id":"1","id":"a2"}`)},
				},
			},
			wantOK: true,
		},
		{
			name: "attachments present but none",
			in:   &apiMessage{ID: "1", ChannelID: "c", Content: "x", Timestamp: ts, Author: alice, Attachments: []apiAttachment{}},
			want: store.Message{ID: "1", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, Body: "x", Media: []store.Media{}}, wantOK: true,
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
				MessageReference: &apiMessageReference{MessageID: "1"},
			},
			want: store.Message{ID: "2", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, Body: "re", ReplyToID: "1"}, wantOK: true,
		},
		{
			name: "crosspost reference is not a reply",
			in: &apiMessage{
				ID: "2", ChannelID: "c", Content: "news", Timestamp: ts, Author: alice,
				MessageReference: &apiMessageReference{MessageID: "9"},
			},
			want: store.Message{ID: "2", ContainerID: "c", SenderID: "200", SenderName: "Alice A", SentAt: ts, Body: "news"}, wantOK: true,
		},
		{
			name: "forward uses snapshot content, filed under the forward",
			in: &apiMessage{
				ID: "2", ChannelID: "c", Timestamp: ts, Author: me,
				MessageReference: &apiMessageReference{Type: referenceForward, MessageID: "9"},
				MessageSnapshots: []apiSnapshot{{Message: &apiMessage{
					Content:     "fwd",
					Attachments: []apiAttachment{{ID: "a1", ContentType: "audio/ogg"}},
				}}},
			},
			want: store.Message{
				ID: "2", ContainerID: "c", SenderID: self, SenderName: "me", SentAt: ts, IsFromMe: true, Body: "fwd",
				Media: []store.Media{{Type: "audio/ogg", Ref: []byte(`{"channel_id":"c","message_id":"2","id":"a1"}`)}},
			},
			wantOK: true,
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
		{name: "system notice skipped", in: &apiMessage{ID: "1", ChannelID: "c", Timestamp: ts, Author: bob, Type: 21}},
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
		"attachments":[{"id":"1400000000000000001","filename":"p.png","content_type":"image/png","size":7,
			"url":"https://cdn.discordapp.com/attachments/1300000000000000010/1400000000000000001/p.png?ex=1&is=2&hm=3&"}],"embeds":[{"type":"rich"}],
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
		SentAt: time.Date(2026, 10, 1, 12, 0, 0, 123000000, time.UTC), Body: "re", ReplyToID: "1300000000000000001",
		Media: []store.Media{{
			Type: "image/png", Name: "p.png", Size: 7,
			Ref: []byte(`{"channel_id":"1300000000000000010","message_id":"1300000000000000002","id":"1400000000000000001",` +
				`"path":"/attachments/1300000000000000010/1400000000000000001/p.png"}`),
		}},
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
			in:   &apiChannel{ID: "c", GuildID: "g", Name: "general", Type: 0},
			want: store.Container{ID: "c", Kind: "channel", ParentID: "g", Name: "#general"}, wantOK: true,
		},
		{
			name: "public thread parents to its channel",
			in:   &apiChannel{ID: "t", GuildID: "g", ParentID: "c", Name: "topic", Type: channelPublicThread},
			want: store.Container{ID: "t", Kind: "thread", ParentID: "c", Name: "topic"}, wantOK: true,
		},
		{
			name: "dm uses the recipient's name and is dated by its last message",
			in:   &apiChannel{ID: "d", Type: channelDM, Recipients: []apiUser{*alice}, LastMessageID: "175928847299117063"},
			want: store.Container{
				ID: "d", Kind: "dm", Name: "Alice A", Participants: []store.Participant{{ID: "200", Name: "Alice A", Handle: "alice"}},
				LastMessageAt: time.Date(2016, 4, 30, 11, 18, 25, 796e6, time.UTC),
			}, wantOK: true,
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
				Participants: []store.Participant{{ID: "200", Name: "Alice A", Handle: "alice"}, {ID: "300", Name: "bob", Handle: "bob#0042"}},
			}, wantOK: true,
		},
		{
			name: "unnamed group dm uses member names",
			in:   &apiChannel{ID: "gd", Type: channelGroupDM, Recipients: []apiUser{*alice, *bob}},
			want: store.Container{
				ID: "gd", Kind: "group", Name: "Alice A, bob", IsGroup: true,
				Participants: []store.Participant{{ID: "200", Name: "Alice A", Handle: "alice"}, {ID: "300", Name: "bob", Handle: "bob#0042"}},
			}, wantOK: true,
		},
		{name: "category skipped", in: &apiChannel{ID: "cat", GuildID: "g", Type: channelGuildCategory}},
		{name: "guild channel without guild skipped", in: &apiChannel{ID: "c", Type: 0}},
		{name: "no id", in: &apiChannel{Type: channelDM}},
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
		{ID: "c", Type: 0, ParentID: "cat", Name: "general"}, // guild_id omitted
		{ID: "t", Type: channelPublicThread, ParentID: "c", Name: "topic"},
		{ID: "t2", Type: channelPublicThread, ParentID: "c2", Name: "elsewhere"},
	}
	// A thread's channel that was not listed is stored by ID, so the thread
	// still hangs under the guild.
	want := []store.Container{
		{ID: "c", Kind: "channel", ParentID: "g", Name: "#general"},
		{ID: "t", Kind: "thread", ParentID: "c", Name: "topic"},
		{ID: "c2", Kind: "channel", ParentID: "g"},
		{ID: "t2", Kind: "thread", ParentID: "c2", Name: "elsewhere"},
	}
	if got := guildChannelContainers("g", chans); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if chans[1].GuildID != "" {
		t.Error("input slice modified")
	}
}

func TestSnowflakeTime(t *testing.T) {
	// The example from Discord's API reference.
	got, ok := snowflakeTime("175928847299117063")
	if want := time.Date(2016, 4, 30, 11, 18, 25, 796e6, time.UTC); !ok || !got.Equal(want) {
		t.Errorf("snowflakeTime = %v, %v; want %v", got, ok, want)
	}
	if _, ok := snowflakeTime(""); ok {
		t.Error("empty ID has a time")
	}
	at := time.Date(2026, 10, 30, 18, 40, 0, 123e6, time.UTC)
	if got, ok := snowflakeTime(snowflakeAt(at)); !ok || !got.Equal(at) {
		t.Errorf("round trip = %v, want %v", got, at)
	}
}
