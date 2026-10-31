package discord

import (
	"strconv"
	"strings"
	"time"

	"github.com/adeci/mandate/internal/store"
)

const (
	kindDM      = "dm"
	kindGroup   = "group"
	kindGuild   = "guild"
	kindChannel = "channel"
	kindThread  = "thread"
)

// Embed-only messages (bot or webhook cards) are stored with an empty body.
const mediaEmbed = "embed"

// Discord channel types (https://docs.discord.food/resources/channel).
const (
	channelDM            = 1
	channelGroupDM       = 3
	channelGuildCategory = 4
	channelNewsThread    = 10
	channelPublicThread  = 11
	channelPrivateThread = 12
)

const (
	messageDefault            = 0
	messageReply              = 19
	messageChatInputCommand   = 20
	messageContextMenuCommand = 23

	referenceForward = 1
)

// Every other type is a system notice (join, pin, boost, call, ...) and is not stored.
var storedTypes = map[int]bool{
	messageDefault:            true,
	messageReply:              true,
	messageChatInputCommand:   true,
	messageContextMenuCommand: true,
}

// discordEpochMS is the first millisecond of 2015, the snowflake epoch.
const discordEpochMS = 1420070400000

type apiUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

type apiGuild struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Unavailable bool   `json:"unavailable"`
}

type apiChannel struct {
	ID         string    `json:"id"`
	Type       int       `json:"type"`
	GuildID    string    `json:"guild_id"`
	ParentID   string    `json:"parent_id"`
	Name       string    `json:"name"`
	Position   int       `json:"position"`
	Recipients []apiUser `json:"recipients"`
}

func (c *apiChannel) isThread() bool {
	return c.Type == channelNewsThread || c.Type == channelPublicThread || c.Type == channelPrivateThread
}

type apiAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}

type apiMessageReference struct {
	Type      int    `json:"type"`
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
}

type apiSnapshot struct {
	Message *apiMessage `json:"message"`
}

type apiMessage struct {
	ID               string               `json:"id"`
	ChannelID        string               `json:"channel_id"`
	Type             int                  `json:"type"`
	Content          string               `json:"content"`
	Timestamp        time.Time            `json:"timestamp"`
	Author           *apiUser             `json:"author"`
	Attachments      []apiAttachment      `json:"attachments"`
	Embeds           []struct{}           `json:"embeds"`
	StickerItems     []struct{}           `json:"sticker_items"`
	MessageReference *apiMessageReference `json:"message_reference"`
	MessageSnapshots []apiSnapshot        `json:"message_snapshots"`
	// Hit marks the matching message in old-style search context groups.
	Hit bool `json:"hit"`
}

// Each group holds the hit and, in older API shapes, surrounding context.
type apiSearchResult struct {
	TotalResults int            `json:"total_results"`
	Messages     [][]apiMessage `json:"messages"`
	Threads      []apiChannel   `json:"threads"`
}

func searchHit(group []apiMessage) *apiMessage {
	for i := range group {
		if group[i].Hit {
			return &group[i]
		}
	}
	if len(group) == 0 {
		return nil
	}
	return &group[0]
}

func displayName(u *apiUser) string {
	if u == nil {
		return ""
	}
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

func guildContainer(g *apiGuild) (store.Container, bool) {
	if g == nil || g.ID == "" || g.Unavailable {
		return store.Container{}, false
	}
	return store.Container{ID: g.ID, Kind: kindGuild, Name: g.Name}, true
}

func guildChannelContainers(guildID string, chans []apiChannel) []store.Container {
	out := make([]store.Container, 0, len(chans))
	for i := range chans {
		c := chans[i]
		if c.GuildID == "" {
			c.GuildID = guildID
		}
		if cc, ok := channelContainer(&c); ok {
			out = append(out, cc)
		}
	}
	return out
}

// Threads also get ParentID = guild ID, so hiding a guild hides everything in it.
func channelContainer(c *apiChannel) (store.Container, bool) {
	if c == nil || c.ID == "" {
		return store.Container{}, false
	}
	switch {
	case c.Type == channelDM:
		ps := participants(c)
		name := c.Name
		if name == "" && len(ps) > 0 {
			name = ps[0].Name
		}
		return store.Container{ID: c.ID, Kind: kindDM, Name: name, Participants: ps}, true
	case c.Type == channelGroupDM:
		ps := participants(c)
		name := c.Name
		if name == "" {
			names := make([]string, 0, len(ps))
			for _, p := range ps {
				if p.Name != "" {
					names = append(names, p.Name)
				}
			}
			name = strings.Join(names, ", ")
		}
		return store.Container{ID: c.ID, Kind: kindGroup, Name: name, IsGroup: true, Participants: ps}, true
	case c.Type == channelGuildCategory, c.GuildID == "":
		return store.Container{}, false
	case c.isThread():
		return store.Container{ID: c.ID, Kind: kindThread, ParentID: c.GuildID, Name: c.Name}, true
	default:
		return store.Container{ID: c.ID, Kind: kindChannel, ParentID: c.GuildID, Name: c.Name}, true
	}
}

// The account itself is never a recipient. nil means unknown and leaves
// stored participants untouched.
func participants(c *apiChannel) []store.Participant {
	var ps []store.Participant
	for i := range c.Recipients {
		if u := &c.Recipients[i]; u.ID != "" {
			ps = append(ps, store.Participant{ID: u.ID, Name: displayName(u)})
		}
	}
	return ps
}

func fallbackContainer(channelID, guildID string) store.Container {
	return store.Container{ID: channelID, Kind: kindChannel, ParentID: guildID}
}

func snowflakeTime(id string) (time.Time, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(n>>22) + discordEpochMS), true
}

func toMessage(m *apiMessage, selfID string) (store.Message, bool) {
	if m == nil || m.ID == "" || m.ChannelID == "" || !storedTypes[m.Type] {
		return store.Message{}, false
	}
	sent := m.Timestamp
	if sent.IsZero() {
		var ok bool
		if sent, ok = snowflakeTime(m.ID); !ok {
			return store.Message{}, false
		}
	}
	out := store.Message{
		ID:          m.ID,
		ContainerID: m.ChannelID,
		SentAt:      sent,
	}
	if m.Author != nil {
		out.SenderID = m.Author.ID
		out.SenderName = displayName(m.Author)
		out.IsFromMe = selfID != "" && m.Author.ID == selfID
	}
	if m.Type == messageReply && m.MessageReference != nil {
		out.ReplyToID = m.MessageReference.MessageID
	}
	// A forward's payload is the snapshot of the forwarded message.
	content := m
	if m.MessageReference != nil && m.MessageReference.Type == referenceForward &&
		len(m.MessageSnapshots) > 0 && m.MessageSnapshots[0].Message != nil {
		content = m.MessageSnapshots[0].Message
	}
	out.Body = content.Content
	out.MediaType = mediaType(content)
	return out, true
}

func mediaType(m *apiMessage) string {
	switch {
	case len(m.Attachments) > 0:
		if ct := m.Attachments[0].ContentType; ct != "" {
			return ct
		}
		return "attachment"
	case len(m.StickerItems) > 0:
		return "sticker"
	case m.Content == "" && len(m.Embeds) > 0:
		return mediaEmbed
	}
	return ""
}
