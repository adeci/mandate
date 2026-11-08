package main

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/adeci/mandate/store"
)

// Forum and media channels hold only posts, which are threads.
const (
	kindDM           = "dm"
	kindGroup        = "group"
	kindGuild        = "guild"
	kindChannel      = "channel"
	kindAnnouncement = "announcement"
	kindVoice        = "voice"
	kindStage        = "stage"
	kindForum        = "forum"
	kindMedia        = "media"
	kindThread       = "thread"
)

// Embed-only messages (bot or webhook cards) are stored with an empty body.
const mediaEmbed = "embed"

// Discord channel types (https://docs.discord.food/resources/channel).
const (
	channelDM            = 1
	channelVoice         = 2
	channelGroupDM       = 3
	channelGuildCategory = 4
	channelAnnouncement  = 5
	channelNewsThread    = 10
	channelPublicThread  = 11
	channelPrivateThread = 12
	channelStage         = 13
	channelForum         = 15
	channelMedia         = 16
)

// guildKinds are the kinds of guild channels other than text channels and
// threads; types without one are stored as text channels.
var guildKinds = map[int]string{
	channelAnnouncement: kindAnnouncement,
	channelVoice:        kindVoice,
	channelStage:        kindStage,
	channelForum:        kindForum,
	channelMedia:        kindMedia,
}

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
	ID            string `json:"id"`
	Username      string `json:"username"`
	Discriminator string `json:"discriminator"`
	GlobalName    string `json:"global_name"`
	Avatar        string `json:"avatar"`
}

type apiGuild struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Unavailable bool   `json:"unavailable"`
}

type apiChannel struct {
	ID            string    `json:"id"`
	Type          int       `json:"type"`
	GuildID       string    `json:"guild_id"`
	ParentID      string    `json:"parent_id"`
	Name          string    `json:"name"`
	Icon          string    `json:"icon"` // a group DM's
	Position      int       `json:"position"`
	Recipients    []apiUser `json:"recipients"`
	LastMessageID string    `json:"last_message_id"`
}

func (c *apiChannel) isThread() bool {
	return c.Type == channelNewsThread || c.Type == channelPublicThread || c.Type == channelPrivateThread
}

type apiAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
}

type apiMessageReference struct {
	Type      int    `json:"type"`
	MessageID string `json:"message_id"`
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

// displayName is the name u chose for themselves, marked ~.
func displayName(u *apiUser) string {
	if u == nil {
		return ""
	}
	return chosen(cmp.Or(u.GlobalName, u.Username))
}

// chosen marks name ~, once.
func chosen(name string) string {
	if name == "" {
		return ""
	}
	return "~" + strings.TrimLeft(name, "~")
}

// handle is the unique @name Discord shows for u; accounts not yet moved to
// unique usernames, such as bots, keep a #discriminator.
func handle(u *apiUser) string {
	if u == nil || u.Username == "" {
		return ""
	}
	if u.Discriminator != "" && u.Discriminator != "0" {
		return "@" + u.Username + "#" + u.Discriminator
	}
	return "@" + u.Username
}

// participant is u as a chat lists them.
func participant(u *apiUser) store.Participant {
	return store.Participant{ID: u.ID, Name: displayName(u), Handle: handle(u)}
}

// guildChat is a guild's chat ID. A guild's first channel may have the
// guild's own ID, so guilds are stored apart, as mailboxes are.
func guildChat(id string) string {
	if id == "" {
		return ""
	}
	return "guild:" + id
}

// hiddenName is what Discord names a channel the account may not view.
const hiddenName = "___hidden___"

// pictureRef is the shape of a stored picture, the CDN path of a guild's
// icon, a group DM's or a person's avatar by its hash, a_ marking an
// animated one. Icon fetches nothing else.
var pictureRef = regexp.MustCompile(`^(?:icons|channel-icons|avatars)/\d{15,21}/(?:a_)?[0-9a-f]{32}$`)

// picture is the reference to store for id's picture hash under dir, "" for none.
func picture(dir, id, hash string) string {
	if ref := dir + "/" + id + "/" + hash; pictureRef.MatchString(ref) {
		return ref
	}
	return ""
}

// avatar is u's picture, "" while u has Discord's default one.
func avatar(u *apiUser) string { return picture("avatars", u.ID, u.Avatar) }

func guildContainer(g *apiGuild) (store.Container, bool) {
	if g == nil || g.ID == "" || g.Unavailable {
		return store.Container{}, false
	}
	return store.Container{ID: guildChat(g.ID), Kind: kindGuild, Name: g.Name, Icon: picture("icons", g.ID, g.Icon)}, true
}

// guildChannelContainers converts chans, which default to guildID. The
// channel of a thread that chans leave out comes too, by ID alone and with
// no kind, so that hiding the guild hides the thread through it;
// storeListed never stores it over what is known of the channel.
func guildChannelContainers(guildID string, chans []apiChannel) []store.Container {
	out := make([]store.Container, 0, len(chans))
	listed := make(map[string]bool, len(chans))
	for i := range chans {
		listed[chans[i].ID] = true
	}
	for i := range chans {
		c := chans[i]
		if c.GuildID == "" {
			c.GuildID = guildID
		}
		cc, ok := channelContainer(&c)
		if !ok {
			continue
		}
		if c.isThread() && cc.ParentID != "" && !listed[cc.ParentID] {
			listed[cc.ParentID] = true
			out = append(out, store.Container{ID: cc.ParentID, ParentID: guildChat(c.GuildID)})
		}
		out = append(out, cc)
	}
	return out
}

// A thread's parent is its channel, whose parent is the guild, so hiding
// either hides the thread. Discord leaves parent_id out of some thread
// payloads; an empty ParentID keeps the stored one.
// Channels are named as Discord shows them: "#name" for text and
// announcement channels, the name alone for the others. The gateway adds
// the guild's name. A DM's picture is its person's.
// last_message_id dates the chat for the chats list before any message is fetched.
func channelContainer(c *apiChannel) (store.Container, bool) {
	if c == nil || c.ID == "" {
		return store.Container{}, false
	}
	last, _ := snowflakeTime(c.LastMessageID)
	switch {
	case c.Type == channelDM:
		ps := participants(c)
		name := c.Name
		if name == "" && len(ps) > 0 {
			name = ps[0].Name
		}
		name = chosen(name)
		var icon string
		if len(c.Recipients) > 0 {
			icon = avatar(&c.Recipients[0])
		}
		return store.Container{ID: c.ID, Kind: kindDM, Name: name, Participants: ps, LastMessageAt: last, Icon: icon}, true
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
		return store.Container{
			ID: c.ID, Kind: kindGroup, Name: name, IsGroup: true, Participants: ps, LastMessageAt: last,
			Icon: picture("channel-icons", c.ID, c.Icon),
		}, true
	case c.Type == channelGuildCategory, c.GuildID == "", c.Name == hiddenName:
		return store.Container{}, false
	case c.isThread():
		return store.Container{ID: c.ID, Kind: kindThread, ParentID: c.ParentID, Name: c.Name, LastMessageAt: last}, true
	default:
		kind, name := cmp.Or(guildKinds[c.Type], kindChannel), c.Name
		if name != "" && (kind == kindChannel || kind == kindAnnouncement) {
			name = "#" + name
		}
		return store.Container{ID: c.ID, Kind: kind, ParentID: guildChat(c.GuildID), Name: name, LastMessageAt: last}, true
	}
}

// The account itself is never a recipient. nil means unknown and leaves
// stored participants untouched.
func participants(c *apiChannel) []store.Participant {
	var ps []store.Participant
	for i := range c.Recipients {
		if u := &c.Recipients[i]; u.ID != "" {
			ps = append(ps, participant(u))
		}
	}
	return ps
}

func fallbackContainer(channelID, guildID string) store.Container {
	return store.Container{ID: channelID, Kind: kindChannel, ParentID: guildChat(guildID)}
}

func snowflakeTime(id string) (time.Time, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(n>>22) + discordEpochMS).UTC(), true
}

// snowflakeAt is the smallest snowflake of t's millisecond, as clients make nonces.
func snowflakeAt(t time.Time) string {
	return strconv.FormatUint(uint64(t.UnixMilli()-discordEpochMS)<<22, 10)
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
	content := carried(m)
	out.Body = content.Content
	out.MediaType = mediaType(content)
	out.Media = storedFiles(m.ChannelID, m.ID, content.Attachments)
	return out, true
}

// carried is the message whose content m carries: a forward's is the
// snapshot of the forwarded message.
func carried(m *apiMessage) *apiMessage {
	if m.MessageReference != nil && m.MessageReference.Type == referenceForward &&
		len(m.MessageSnapshots) > 0 && m.MessageSnapshots[0].Message != nil {
		return m.MessageSnapshots[0].Message
	}
	return m
}

// A message with files is marked by them.
func mediaType(m *apiMessage) string {
	switch {
	case len(m.Attachments) > 0:
		return ""
	case len(m.StickerItems) > 0:
		return "sticker"
	case m.Content == "" && len(m.Embeds) > 0:
		return mediaEmbed
	}
	return ""
}
