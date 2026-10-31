package discord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/adeci/mandate/internal/connector"
	"github.com/adeci/mandate/internal/store"
)

const (
	listTTL        = 24 * time.Hour // guild list, DM list, guild channel lists
	selfTTL        = 24 * time.Hour
	searchTTL      = time.Hour
	searchPageSize = 25
	maxSearchQuery = 512 // runes
	maxSearchOff   = 9975
	defaultLimit   = 50
	maxLimit       = 100
)

var searchHas = map[string]bool{"link": true, "embed": true, "file": true, "video": true, "image": true, "sound": true}

// badRequestError is an invalid gateway call; nothing was sent upstream.
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &badRequestError{msg: fmt.Sprintf(format, args...)}
}

// syncRequest refreshes the guild list and DM channels; with GuildID set it
// also refreshes that guild's channels and threads.
type syncRequest struct {
	GuildID string
}

type searchRequest struct {
	GuildID, Content, AuthorID, ChannelID string
	Has                                   string
	Offset                                int
}

type fetchRequest struct {
	ChannelID, Before, After, Around string
	Limit                            int
}

// args consumes op arguments; anything left over is refused, so a typo never
// silently widens a query.
type args struct {
	m   map[string]string
	err error
}

func newArgs(m map[string]string) *args {
	return &args{m: maps.Clone(m)}
}

func (a *args) str(name string) string {
	v := a.m[name]
	delete(a.m, name)
	return v
}

func (a *args) int(name string) int {
	v, ok := a.m[name]
	delete(a.m, name)
	if !ok || v == "" || a.err != nil {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		a.err = badRequest("%s must be an integer", name)
	}
	return n
}

func (a *args) done() error {
	if a.err != nil {
		return a.err
	}
	for k := range a.m {
		return badRequest("unknown argument %q", k)
	}
	return nil
}

func parseSync(m map[string]string) (syncRequest, error) {
	a := newArgs(m)
	r := syncRequest{GuildID: a.str("guild_id")}
	if err := a.done(); err != nil {
		return r, err
	}
	return r, validateSync(&r)
}

func parseSearch(m map[string]string) (searchRequest, error) {
	a := newArgs(m)
	r := searchRequest{
		GuildID: a.str("guild_id"), Content: a.str("content"), AuthorID: a.str("author_id"),
		ChannelID: a.str("channel_id"), Has: a.str("has"), Offset: a.int("offset"),
	}
	if err := a.done(); err != nil {
		return r, err
	}
	return r, validateSearch(&r)
}

func parseFetch(m map[string]string) (fetchRequest, error) {
	a := newArgs(m)
	r := fetchRequest{
		ChannelID: a.str("channel_id"), Before: a.str("before"), After: a.str("after"),
		Around: a.str("around"), Limit: a.int("limit"),
	}
	if err := a.done(); err != nil {
		return r, err
	}
	return r, validateFetch(&r)
}

// Only Discord-ID-shaped strings are ever put into upstream URLs.
func isSnowflake(s string) bool {
	if len(s) < 15 || len(s) > 21 {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func checkID(field, v string, required bool) error {
	if v == "" && !required {
		return nil
	}
	if !isSnowflake(v) {
		return badRequest("%s must be a Discord snowflake (15-21 digits)", field)
	}
	return nil
}

func validateSync(r *syncRequest) error {
	return checkID("guild_id", r.GuildID, false)
}

func validateSearch(r *searchRequest) error {
	if err := checkID("guild_id", r.GuildID, true); err != nil {
		return err
	}
	if err := checkID("author_id", r.AuthorID, false); err != nil {
		return err
	}
	if err := checkID("channel_id", r.ChannelID, false); err != nil {
		return err
	}
	if r.Has != "" && !searchHas[r.Has] {
		return badRequest("has must be one of link, embed, file, video, image, sound")
	}
	if !utf8.ValidString(r.Content) || utf8.RuneCountInString(r.Content) > maxSearchQuery {
		return badRequest("content must be valid UTF-8 of at most %d characters", maxSearchQuery)
	}
	if r.Offset < 0 || r.Offset > maxSearchOff {
		return badRequest("offset must be between 0 and %d", maxSearchOff)
	}
	if r.Content == "" && r.AuthorID == "" && r.ChannelID == "" && r.Has == "" {
		return badRequest("search needs content, author_id, channel_id, or has")
	}
	return nil
}

func validateFetch(r *fetchRequest) error {
	if err := checkID("channel_id", r.ChannelID, true); err != nil {
		return err
	}
	anchors := 0
	for _, a := range []struct{ field, v string }{{"before", r.Before}, {"after", r.After}, {"around", r.Around}} {
		if err := checkID(a.field, a.v, false); err != nil {
			return err
		}
		if a.v != "" {
			anchors++
		}
	}
	if anchors > 1 {
		return badRequest("before, after, and around are mutually exclusive")
	}
	if r.Limit < 0 || r.Limit > maxLimit {
		return badRequest("limit must be between 1 and %d", maxLimit)
	}
	return nil
}

type cachedList[T any] struct {
	at time.Time
	v  []T
}

func (c *cachedList[T]) fresh(now time.Time) bool { return !c.at.IsZero() && now.Sub(c.at) < listTTL }

type cachedSearch struct {
	at    time.Time
	ids   []string
	total int
}

// Methods run only on the worker goroutine, so the caches need no locking.
type service struct {
	st    *store.Store
	up    *upstream
	clock clock

	guilds   cachedList[apiGuild]
	dms      cachedList[apiChannel]
	channels map[string]*cachedList[apiChannel] // by guild ID
	searches map[string]cachedSearch
}

func newService(st *store.Store, up *upstream, c clock) *service {
	return &service{
		st:       st,
		up:       up,
		clock:    c,
		channels: make(map[string]*cachedList[apiChannel]),
		searches: make(map[string]cachedSearch),
	}
}

const refererHome = "https://discord.com/channels/@me"

func channelURL(guildID, channelID string) string {
	if guildID == "" {
		guildID = "@me"
	}
	return "https://discord.com/channels/" + guildID + "/" + channelID
}

func (s *service) loadGuilds(ctx context.Context, dl time.Time) ([]apiGuild, error) {
	if s.guilds.fresh(s.clock.Now()) {
		return s.guilds.v, nil
	}
	var gs []apiGuild
	r := route{template: "/users/@me/guilds", path: "/users/@me/guilds", referer: refererHome}
	if err := s.up.get(ctx, dl, r, &gs); err != nil {
		return nil, err
	}
	s.guilds = cachedList[apiGuild]{at: s.clock.Now(), v: gs}
	return gs, nil
}

func (s *service) loadDMs(ctx context.Context, dl time.Time) ([]apiChannel, error) {
	if s.dms.fresh(s.clock.Now()) {
		return s.dms.v, nil
	}
	var cs []apiChannel
	r := route{template: "/users/@me/channels", path: "/users/@me/channels", referer: refererHome}
	if err := s.up.get(ctx, dl, r, &cs); err != nil {
		return nil, err
	}
	s.dms = cachedList[apiChannel]{at: s.clock.Now(), v: cs}
	return cs, nil
}

func (s *service) loadChannels(ctx context.Context, dl time.Time, guildID string) ([]apiChannel, error) {
	if c := s.channels[guildID]; c != nil && c.fresh(s.clock.Now()) {
		return c.v, nil
	}
	var cs []apiChannel
	r := route{
		template: "/guilds/{guild_id}/channels",
		path:     "/guilds/" + guildID + "/channels",
		major:    guildID,
		referer:  refererHome,
		deny:     "guild:" + guildID,
		denyWhat: "guild " + guildID,
	}
	if err := s.up.get(ctx, dl, r, &cs); err != nil {
		return nil, err
	}
	for i := range cs {
		if cs[i].GuildID == "" {
			cs[i].GuildID = guildID
		}
	}
	s.channels[guildID] = &cachedList[apiChannel]{at: s.clock.Now(), v: cs}
	return cs, nil
}

func (s *service) selfID(ctx context.Context, dl time.Time) (string, error) {
	st := s.up.st
	if st.SelfID != "" && s.clock.Now().Sub(st.SelfCheckedAt) < selfTTL {
		return st.SelfID, nil
	}
	var me apiUser
	r := route{template: "/users/@me", path: "/users/@me", referer: refererHome}
	if err := s.up.get(ctx, dl, r, &me); err != nil {
		return "", err
	}
	if !isSnowflake(me.ID) {
		return "", &upstreamError{status: 200, msg: ": /users/@me without a user ID"}
	}
	st.SelfID, st.SelfCheckedAt = me.ID, s.clock.Now()
	return me.ID, s.up.saveState()
}

// Requests are never made for guilds the account is not in.
func (s *service) joinedGuild(ctx context.Context, dl time.Time, guildID string) (*apiGuild, error) {
	gs, err := s.loadGuilds(ctx, dl)
	if err != nil {
		return nil, err
	}
	for i := range gs {
		if gs[i].ID == guildID {
			return &gs[i], nil
		}
	}
	return nil, &deniedError{status: 404, what: "guild " + guildID + ": the account has not joined it"}
}

func (s *service) upsertGuild(ctx context.Context, g *apiGuild, chans []apiChannel) (int, error) {
	n := 0
	if c, ok := guildContainer(g); ok {
		if err := s.st.UpsertContainer(ctx, c); err != nil {
			return n, err
		}
		n++
	}
	for _, c := range guildChannelContainers(g.ID, chans) {
		if err := s.st.UpsertContainer(ctx, c); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *service) sync(ctx context.Context, dl time.Time, req syncRequest) (connector.ReadResult, error) {
	var resp connector.ReadResult
	gs, err := s.loadGuilds(ctx, dl)
	if err != nil {
		return resp, err
	}
	for i := range gs {
		if c, ok := guildContainer(&gs[i]); ok {
			if err := s.st.UpsertContainer(ctx, c); err != nil {
				return resp, err
			}
			resp.Containers++
		}
	}
	dms, err := s.loadDMs(ctx, dl)
	if err != nil {
		return resp, err
	}
	for i := range dms {
		if c, ok := channelContainer(&dms[i]); ok {
			if err := s.st.UpsertContainer(ctx, c); err != nil {
				return resp, err
			}
			resp.Containers++
		}
	}
	if req.GuildID == "" {
		return resp, nil
	}
	g, err := s.joinedGuild(ctx, dl, req.GuildID)
	if err != nil {
		return resp, err
	}
	chans, err := s.loadChannels(ctx, dl, g.ID)
	if err != nil {
		return resp, err
	}
	for _, c := range guildChannelContainers(g.ID, chans) {
		if err := s.st.UpsertContainer(ctx, c); err != nil {
			return resp, err
		}
		resp.Containers++
	}
	return resp, nil
}

func searchKey(req searchRequest) string {
	b, _ := json.Marshal(req)
	return string(b)
}

func (s *service) search(ctx context.Context, dl time.Time, req searchRequest) (connector.ReadResult, error) {
	now := s.clock.Now()
	key := searchKey(req)
	if err := s.up.denied("guild:"+req.GuildID, "guild "+req.GuildID); err != nil {
		return connector.ReadResult{}, err
	}
	if req.ChannelID != "" {
		if err := s.up.denied("channel:"+req.ChannelID, "channel "+req.ChannelID); err != nil {
			return connector.ReadResult{}, err
		}
	}
	if c, ok := s.searches[key]; ok && now.Sub(c.at) < searchTTL {
		return connector.ReadResult{MessageIDs: slices.Clone(c.ids), TotalResults: c.total}, nil
	}
	g, err := s.joinedGuild(ctx, dl, req.GuildID)
	if err != nil {
		return connector.ReadResult{}, err
	}
	chans, err := s.loadChannels(ctx, dl, g.ID)
	if err != nil {
		return connector.ReadResult{}, err
	}
	if _, err := s.upsertGuild(ctx, g, chans); err != nil {
		return connector.ReadResult{}, err
	}
	self, err := s.selfID(ctx, dl)
	if err != nil {
		return connector.ReadResult{}, err
	}

	q := url.Values{}
	if req.Content != "" {
		q.Set("content", req.Content)
	}
	if req.AuthorID != "" {
		q.Set("author_id", req.AuthorID)
	}
	if req.ChannelID != "" {
		q.Set("channel_id", req.ChannelID)
	}
	if req.Has != "" {
		q.Set("has", req.Has)
	}
	if req.Offset > 0 {
		q.Set("offset", strconv.Itoa(req.Offset))
	}
	q.Set("limit", strconv.Itoa(searchPageSize))
	r := route{
		template: "/guilds/{guild_id}/messages/search",
		path:     "/guilds/" + g.ID + "/messages/search",
		query:    q,
		major:    g.ID,
		referer:  searchReferer(g.ID, req.ChannelID, chans),
		deny:     "guild:" + g.ID,
		denyWhat: "guild " + g.ID,
	}
	if req.ChannelID != "" {
		r.deny, r.denyWhat = "channel:"+req.ChannelID, "channel "+req.ChannelID
	}
	var res apiSearchResult
	if err := s.up.get(ctx, dl, r, &res); err != nil {
		return connector.ReadResult{}, err
	}

	known := make(map[string]store.Container)
	for _, c := range guildChannelContainers(g.ID, chans) {
		known[c.ID] = c
	}
	for _, c := range guildChannelContainers(g.ID, res.Threads) {
		known[c.ID] = c
	}
	stored := make(map[string]bool)
	ids := make([]string, 0, len(res.Messages))
	for _, group := range res.Messages {
		m := searchHit(group)
		msg, ok := toMessage(m, self)
		if !ok {
			continue
		}
		if !stored[msg.ContainerID] {
			c, ok := known[msg.ContainerID]
			if !ok {
				c = fallbackContainer(msg.ContainerID, g.ID)
			}
			if err := s.st.UpsertContainer(ctx, c); err != nil {
				return connector.ReadResult{}, err
			}
			stored[msg.ContainerID] = true
		}
		if err := s.st.UpsertMessage(ctx, msg); err != nil {
			return connector.ReadResult{}, err
		}
		ids = append(ids, msg.ID)
	}
	for k, c := range s.searches {
		if now.Sub(c.at) >= searchTTL {
			delete(s.searches, k)
		}
	}
	s.searches[key] = cachedSearch{at: s.clock.Now(), ids: ids, total: res.TotalResults}
	return connector.ReadResult{MessageIDs: slices.Clone(ids), TotalResults: res.TotalResults}, nil
}

// searchReferer is the channel page a search would be typed on: the
// filtered channel, else the guild's first text channel.
func searchReferer(guildID, channelID string, chans []apiChannel) string {
	if channelID != "" {
		return channelURL(guildID, channelID)
	}
	first := -1
	for i := range chans {
		if chans[i].Type == 0 && (first < 0 || chans[i].Position < chans[first].Position) {
			first = i
		}
	}
	if first < 0 {
		return refererHome
	}
	return channelURL(guildID, chans[first].ID)
}

func (s *service) resolveContainer(ctx context.Context, dl time.Time, channelID string) (c store.Container, upsert bool, err error) {
	for gid, list := range s.channels {
		for i := range list.v {
			if list.v[i].ID == channelID {
				if cc, ok := channelContainer(&list.v[i]); ok {
					return cc, true, nil
				}
				return fallbackContainer(channelID, gid), true, nil
			}
		}
	}
	findDM := func() (store.Container, bool) {
		for i := range s.dms.v {
			if s.dms.v[i].ID == channelID {
				return channelContainer(&s.dms.v[i])
			}
		}
		return store.Container{}, false
	}
	if cc, ok := findDM(); ok {
		return cc, true, nil
	}
	stored, err := s.storedContainer(ctx, channelID)
	if err != nil || stored.ID != "" {
		return stored, false, err
	}
	if !s.dms.fresh(s.clock.Now()) {
		if _, err := s.loadDMs(ctx, dl); err != nil {
			return store.Container{}, false, err
		}
		if cc, ok := findDM(); ok {
			return cc, true, nil
		}
	}
	return fallbackContainer(channelID, ""), true, nil
}

func (s *service) storedContainer(ctx context.Context, id string) (store.Container, error) {
	var c store.Container
	var parent sql.NullString
	err := s.st.DB().QueryRowContext(ctx, `SELECT id, kind, parent_id FROM containers WHERE id = ?1`, id).
		Scan(&c.ID, &c.Kind, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Container{}, nil
	}
	c.ParentID = parent.String
	return c, err
}

func (s *service) fetch(ctx context.Context, dl time.Time, req fetchRequest) (connector.ReadResult, error) {
	if err := s.up.denied("channel:"+req.ChannelID, "channel "+req.ChannelID); err != nil {
		return connector.ReadResult{}, err
	}
	c, upsert, err := s.resolveContainer(ctx, dl, req.ChannelID)
	if err != nil {
		return connector.ReadResult{}, err
	}
	self, err := s.selfID(ctx, dl)
	if err != nil {
		return connector.ReadResult{}, err
	}
	limit := req.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	q := url.Values{}
	switch {
	case req.Before != "":
		q.Set("before", req.Before)
	case req.After != "":
		q.Set("after", req.After)
	case req.Around != "":
		q.Set("around", req.Around)
	}
	q.Set("limit", strconv.Itoa(limit))
	guild := ""
	if c.Kind == kindChannel || c.Kind == kindThread {
		guild = c.ParentID
	}
	referer := channelURL(guild, req.ChannelID)
	r := route{
		template: "/channels/{channel_id}/messages",
		path:     "/channels/" + req.ChannelID + "/messages",
		query:    q,
		major:    req.ChannelID,
		referer:  referer,
		deny:     "channel:" + req.ChannelID,
		denyWhat: "channel " + req.ChannelID,
	}
	var msgs []apiMessage
	if err := s.up.get(ctx, dl, r, &msgs); err != nil {
		return connector.ReadResult{}, err
	}
	if upsert {
		if err := s.st.UpsertContainer(ctx, c); err != nil {
			return connector.ReadResult{}, err
		}
	}
	ids := make([]string, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		if m.ChannelID == "" {
			m.ChannelID = req.ChannelID
		}
		if m.ChannelID != req.ChannelID {
			continue // never file a message under a container it was not fetched from
		}
		msg, ok := toMessage(m, self)
		if !ok {
			continue
		}
		if err := s.st.UpsertMessage(ctx, msg); err != nil {
			return connector.ReadResult{}, err
		}
		ids = append(ids, msg.ID)
	}
	return connector.ReadResult{MessageIDs: ids}, nil
}
