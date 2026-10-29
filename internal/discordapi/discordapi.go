// Package discordapi is the protocol between the gateway and the Discord
// service's Unix socket. The service upserts fetched messages and containers
// before responding, so the gateway answers from its filtered views by ID.
package discordapi

const (
	PathSync   = "/sync"
	PathSearch = "/search"
	PathFetch  = "/fetch"
)

// SyncRequest refreshes the guild list and DM channels; with GuildID set it
// also refreshes that guild's channels and threads.
type SyncRequest struct {
	GuildID string `json:"guild_id,omitempty"`
}

type SyncResponse struct {
	Containers int `json:"containers"`
}

type SearchRequest struct {
	GuildID   string `json:"guild_id"`
	Content   string `json:"content,omitempty"`
	AuthorID  string `json:"author_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	// Has filters by attachment kind: link, embed, file, video, image, sound.
	Has string `json:"has,omitempty"`
	// Offset pages through results, 25 at a time (Discord's page size).
	Offset int `json:"offset,omitempty"`
}

// FetchRequest reads up to Limit (1..100) messages from a channel or DM,
// optionally anchored Before, After, or Around a message ID.
type FetchRequest struct {
	ChannelID string `json:"channel_id"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
	Around    string `json:"around,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// IDs are in the order Discord returned them; TotalResults is set for searches.
type MessagesResponse struct {
	MessageIDs   []string `json:"message_ids"`
	TotalResults int      `json:"total_results,omitempty"`
}

// Status 429 means the service's pacing budget is exhausted.
type ErrorResponse struct {
	Error             string `json:"error"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}
