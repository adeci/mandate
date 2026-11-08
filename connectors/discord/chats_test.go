package main

import (
	"path/filepath"
	"testing"

	"github.com/adeci/mandate/store"
	"github.com/adeci/mandate/store/storetest"
)

// The chats a typical account stores, kept in testdata/chats.json for the
// gateway's tests of agents' terms: two servers with a #general each, one
// with the server's own ID as old servers have, a thread, a category, a
// channel the account may not view, an announcement channel, a forum with
// a post, a voice channel with text and one without, a DM and a group DM,
// all with their pictures.
func TestChatsFixture(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var cs []store.Container
	for _, g := range []struct {
		guild apiGuild
		chans []apiChannel
	}{
		{apiGuild{ID: "1100000000000000001", Name: "main", Icon: "a_" + pictureHash}, []apiChannel{
			{ID: "1100000000000000010", Type: channelGuildCategory, Name: "Text Channels"},
			{ID: "1100000000000000001", Type: 0, Name: "general", ParentID: "1100000000000000010"},
			{ID: "1100000000000000012", Type: 0, Name: "releases"},
			{ID: "1100000000000000014", Type: 0, Name: hiddenName},
			{ID: "1100000000000000013", Type: channelPublicThread, Name: "v2 rollout", ParentID: "1100000000000000012"},
			{ID: "1100000000000000015", Type: channelAnnouncement, Name: "announcements"},
			{ID: "1100000000000000016", Type: channelVoice, Name: "General", LastMessageID: "1250000000000000000"},
			{ID: "1100000000000000017", Type: channelVoice, Name: "Lounge"},
			{ID: "1100000000000000018", Type: channelForum, Name: "help"},
			{ID: "1100000000000000019", Type: channelPublicThread, Name: "crash on start", ParentID: "1100000000000000018"},
		}},
		{apiGuild{ID: "1200000000000000001", Name: "other"}, []apiChannel{
			{ID: "1200000000000000011", Type: 0, Name: "general"},
		}},
	} {
		c, ok := guildContainer(&g.guild)
		if !ok {
			t.Fatal("guild not stored")
		}
		cs = append(cs, c)
		cs = append(cs, guildChannelContainers(g.guild.ID, g.chans)...)
	}
	for _, ch := range []apiChannel{
		{ID: "1300000000000000001", Type: channelDM, Recipients: []apiUser{{ID: "1000000000000000002", Username: "sam_p", GlobalName: "Sam Park", Avatar: pictureHash}}},
		{ID: "1300000000000000002", Type: channelGroupDM, Name: "climbing", Icon: pictureHash, Recipients: []apiUser{
			{ID: "1000000000000000002", Username: "sam_p", GlobalName: "Sam Park"}, {ID: "1000000000000000003", Username: "kim"},
		}},
	} {
		c, ok := channelContainer(&ch)
		if !ok {
			t.Fatal("dm not stored")
		}
		cs = append(cs, c)
	}
	if _, err := storeListed(t.Context(), st, cs); err != nil {
		t.Fatal(err)
	}
	storetest.Check(t, st.DB(), filepath.Join("testdata", "chats.json"))
}
