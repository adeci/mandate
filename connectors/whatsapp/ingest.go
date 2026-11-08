package main

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/adeci/mandate/store"
)

const queryTimeout = 30 * time.Second

// Events arrive from the node handler and the history-sync loop, so caches
// are guarded by mu.
type ingester struct {
	cli *whatsmeow.Client
	st  *store.Store
	// writes keeps a message the store refuses as busy until it is free,
	// with the edits and deletions after it.
	writes *store.Queue
	log    *slog.Logger
	// groupInfo is cli.GetGroupInfo, unless a test answers for WhatsApp.
	groupInfo func(context.Context, types.JID) (*types.GroupInfo, error)

	mu    sync.Mutex
	seen  map[types.JID]bool      // containers upserted by this process
	dated map[types.JID]time.Time // the newest activity this process stored for each
	lidPN map[types.JID]types.JID // LID -> PN learned from history sync
}

func newIngester(cli *whatsmeow.Client, st *store.Store, writes *store.Queue, log *slog.Logger) *ingester {
	return &ingester{
		cli:       cli,
		st:        st,
		writes:    writes,
		log:       log,
		groupInfo: cli.GetGroupInfo,
		seen:      make(map[types.JID]bool),
		dated:     make(map[types.JID]time.Time),
		lidPN:     make(map[types.JID]types.JID),
	}
}

// Returning false for a message withholds its ack (SynchronousAck), so the
// server redelivers it after reconnecting. One the store refuses as busy is
// stored from the queue once the store is free; its ack is withheld still,
// so a restart before then loses nothing.
func (in *ingester) handle(ctx context.Context, evt any) bool {
	switch e := evt.(type) {
	case *events.Message:
		o, ok := convert(e, in.pn)
		if !ok {
			return true
		}
		return in.writes.Do("message "+e.Info.ID+" in "+e.Info.Chat.String(), func(ctx context.Context) error {
			return in.apply(ctx, o)
		})
	case *events.HistorySync:
		in.historySync(ctx, e.Data)
	case *events.PushName:
		in.renameDM(ctx, e.JID)
	case *events.Contact:
		if a := e.Action; a.GetFullName() != "" || a.GetFirstName() != "" {
			in.storeDM(ctx, e.JID)
		}
		in.renameDM(ctx, e.JID)
	case *events.GroupInfo:
		in.groupChanged(ctx, e)
	case *events.JoinedGroup:
		in.storeGroups(ctx, []*types.GroupInfo{&e.GroupInfo})
	case *events.AppStateSyncComplete:
		// A full sync stores contacts without a Contact event per contact.
		if e.Name == appstate.WAPatchCriticalUnblockLow {
			in.storeContacts(ctx)
			in.nameDMs(ctx)
		}
	case *events.IdentityChange:
		in.identityChanged(ctx, e)
	case *events.Connected:
		in.log.Info("connected")
		in.storeContacts(ctx)
		in.nameDMs(ctx)
		go in.syncGroups(ctx)
	case *events.Disconnected:
		in.log.Warn("disconnected; whatsmeow will reconnect")
	}
	return true
}

func (in *ingester) apply(ctx context.Context, o op) error {
	switch o.kind {
	case opUpsert:
		// A stanza ID is unique per author only, so a redelivery may update
		// the stored row but someone reusing another author's ID may not.
		if s, found, err := in.stored(ctx, o.msg.ID); err != nil || found && !in.sameAuthor(s, o) {
			return err
		}
		if err := in.ensureContainer(ctx, o.chat, store.Container{LastMessageAt: o.msg.SentAt}); err != nil {
			return err
		}
		in.enrichSender(ctx, &o)
		return in.st.UpsertMessage(ctx, o.msg)
	case opEdit, opDelete:
		ok, err := in.byAuthor(ctx, o)
		if err != nil || !ok {
			return err
		}
		if o.kind == opEdit {
			return in.st.EditMessage(ctx, o.target, o.body, o.at)
		}
		return in.st.DeleteMessage(ctx, o.target)
	}
	return nil
}

type storedMessage struct {
	author types.JID
	fromMe bool
	sentAt string
}

func (in *ingester) stored(ctx context.Context, id string) (storedMessage, bool, error) {
	var s storedMessage
	var author string
	err := in.st.DB().QueryRowContext(ctx,
		`SELECT COALESCE(sender_id, ''), is_from_me, sent_at FROM messages WHERE id = ?1`, id).
		Scan(&author, &s.fromMe, &s.sentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	} else if err != nil {
		return s, false, err
	}
	// An unparsable author is empty and matches no one.
	s.author, _ = types.ParseJID(author)
	return s, true, nil
}

// sameAuthor reports whether o comes from the author of s. Either side may
// name the user by LID.
func (in *ingester) sameAuthor(s storedMessage, o op) bool {
	if s.fromMe != o.fromMe {
		return false
	}
	return s.fromMe || !s.author.IsEmpty() && !o.sender.IsEmpty() && in.pn(s.author) == in.pn(o.sender)
}

// byAuthor reports whether the edit or delete o comes from the author of
// its target, and an edit also from within whatsmeow.EditWindow, which
// allows for delivery on top of the 15 minutes the apps give. Group admins
// may delete others' messages for everyone; those deletes are not applied.
func (in *ingester) byAuthor(ctx context.Context, o op) (bool, error) {
	s, found, err := in.stored(ctx, o.target)
	if err != nil || !found || !in.sameAuthor(s, o) {
		return false, err
	}
	if o.kind == opEdit {
		sent, err := time.Parse(time.RFC3339, s.sentAt)
		if err != nil {
			return false, fmt.Errorf("read sent time of %s: %w", o.target, err)
		}
		return o.at.Sub(sent) <= whatsmeow.EditWindow, nil
	}
	return true, nil
}

// Local lookups only (history-sync mappings, whatsmeow's LID store); no network.
func (in *ingester) pn(j types.JID) types.JID {
	j = j.ToNonAD()
	if j.Server != types.HiddenUserServer {
		return j
	}
	in.mu.Lock()
	p, ok := in.lidPN[j]
	in.mu.Unlock()
	if ok {
		return p
	}
	p, err := in.cli.Store.LIDs.GetPNForLID(context.Background(), j)
	if err == nil && !p.IsEmpty() {
		return p.ToNonAD()
	}
	return j
}

// contactName is an address-book name under any of ids, else a self-chosen
// push or business name marked with "~" as WhatsApp does, since anyone can
// pick those.
func (in *ingester) contactName(ctx context.Context, ids ...types.JID) string {
	own := in.cli.Store.GetJID()
	for _, j := range ids {
		if !own.IsEmpty() && j.User == own.User && j.Server == own.Server {
			return in.cli.Store.PushName
		}
	}
	var chosen string
	for _, j := range ids {
		c, err := in.cli.Store.Contacts.GetContact(ctx, j)
		if err != nil || !c.Found {
			continue
		}
		switch {
		case c.FullName != "":
			return c.FullName
		case c.FirstName != "":
			return c.FirstName
		case chosen == "" && c.PushName != "":
			chosen = "~" + c.PushName
		case chosen == "" && c.BusinessName != "":
			chosen = "~" + c.BusinessName
		}
	}
	return chosen
}

// messageYourself is what WhatsApp calls the owner's chat with themself.
const messageYourself = "Message yourself"

// chatName names the direct chat with the user under ids: the owner's own
// chat as WhatsApp does, any other as contactName under the user's phone
// number and LID, as whatsmeow keeps a name under the ID it came with.
func (in *ingester) chatName(ctx context.Context, ids ...types.JID) string {
	if slices.ContainsFunc(ids, in.own) {
		return messageYourself
	}
	all := make([]types.JID, 0, 2*len(ids))
	for _, j := range ids {
		all = append(all, j)
		if alt, err := in.cli.Store.GetAltJID(ctx, j); err == nil && !alt.IsEmpty() {
			all = append(all, alt)
		}
	}
	return in.contactName(ctx, all...)
}

// own says whether j is the owner, under their phone number or LID.
func (in *ingester) own(j types.JID) bool {
	for _, o := range []types.JID{in.cli.Store.GetJID(), in.cli.Store.GetLID()} {
		if !o.IsEmpty() && j.User == o.User && j.Server == o.Server {
			return true
		}
	}
	return false
}

// number is the phone number of the user j, their handle. Unlike a chosen
// name nobody can spoof it, so it also names a chat with them that has no
// other name. A LID has none.
func number(j types.JID) string {
	if j.Server != types.DefaultUserServer {
		return ""
	}
	return "+" + j.User
}

func (in *ingester) nameFunc(ctx context.Context) func(types.JID) string {
	return func(j types.JID) string { return in.contactName(ctx, j) }
}

// enrichSender names the sender as contactName does; the message's own push
// name is the fallback.
func (in *ingester) enrichSender(ctx context.Context, o *op) {
	if o.sender.IsEmpty() || o.msg.IsFromMe {
		return
	}
	c, err := in.cli.Store.Contacts.GetContact(ctx, o.sender)
	switch {
	case err == nil && c.FullName != "":
		o.msg.SenderName = c.FullName
	case err == nil && c.FirstName != "":
		o.msg.SenderName = c.FirstName
	case o.msg.SenderName != "":
		o.msg.SenderName = "~" + o.msg.SenderName
	default:
		o.msg.SenderName = in.contactName(ctx, o.sender)
	}
}

// ensureContainer stores chat the first time this process sees it, looking
// up a group base doesn't name. Later calls store only newer activity, and a
// name if the chat was stored without one, as when its lookup failed.
func (in *ingester) ensureContainer(ctx context.Context, chat types.JID, base store.Container) (err error) {
	in.mu.Lock()
	seen := in.seen[chat]
	newer := base.LastMessageAt.After(in.dated[chat])
	in.seen[chat] = true
	if newer {
		in.dated[chat] = base.LastMessageAt
	}
	in.mu.Unlock()
	defer func() {
		if err != nil {
			in.mu.Lock()
			delete(in.seen, chat)
			delete(in.dated, chat)
			in.mu.Unlock()
		}
	}()

	if seen {
		if err := in.nameIfNone(ctx, chat, base.Name); err != nil || !newer {
			return err
		}
		return in.st.UpsertContainer(ctx, store.Container{
			ID: chat.String(), Kind: kindFor(chat), IsGroup: chat.Server == types.GroupServer, LastMessageAt: base.LastMessageAt,
		})
	}

	c := base
	c.ID, c.Kind, c.IsGroup = chat.String(), kindFor(chat), chat.Server == types.GroupServer
	if c.IsGroup && c.Name == "" {
		if info, err := in.lookupGroup(ctx, chat); err == nil {
			c = groupContainer(info, in.pn, in.nameFunc(ctx), in.own)
			c.LastMessageAt = base.LastMessageAt
		}
	} else if !c.IsGroup {
		c.Name = cmp.Or(in.chatName(ctx, chat), c.Name)
		c.Participants = in.dmParticipants(chat, c.Name)
	}
	if err := in.st.UpsertContainer(ctx, c); err != nil || c.IsGroup || c.Name != "" {
		return err
	}
	return in.nameIfNone(ctx, chat, number(chat))
}

// nameIfNone names chat only if it has no name, unlike UpsertContainer.
func (in *ingester) nameIfNone(ctx context.Context, chat types.JID, name string) error {
	if name == "" {
		return nil
	}
	_, err := in.st.DB().ExecContext(ctx, `UPDATE containers SET name = ?2 WHERE id = ?1 AND name IS NULL`, chat.String(), name)
	return err
}

// lookupGroup asks WhatsApp about a group with GetGroupInfo, a read-only
// query it answers for members only.
func (in *ingester) lookupGroup(ctx context.Context, chat types.JID) (*types.GroupInfo, error) {
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	info, err := in.groupInfo(qctx, chat)
	if err != nil && !errors.Is(err, whatsmeow.ErrNotInGroup) && !errors.Is(err, whatsmeow.ErrGroupNotFound) {
		in.log.Warn("group info query failed", "chat", chat.String(), "err", err)
	}
	return info, err
}

// storeContacts stores a DM for the owner's own chat and every contact in
// the address book, so the owner can grant one that has had no messages
// since linking. Contacts with only a push name are group members the
// owner may never have written to.
func (in *ingester) storeContacts(ctx context.Context) {
	if own := in.cli.Store.GetJID(); !own.IsEmpty() {
		in.storeDM(ctx, own)
	}
	contacts, err := in.cli.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		in.log.Error("load contacts", "err", err)
		return
	}
	for j, c := range contacts {
		if c.FullName != "" || c.FirstName != "" {
			in.storeDM(ctx, j)
		}
	}
}

// storeDM stores the DM with the user j, under their phone number if known
// and named from either ID.
func (in *ingester) storeDM(ctx context.Context, j types.JID) {
	chat := in.pn(j)
	if chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer {
		return
	}
	if err := in.ensureContainer(ctx, chat, store.Container{Name: in.chatName(ctx, chat, j.ToNonAD())}); err != nil {
		in.log.Error("store dm", "chat", chat.String(), "err", err)
	}
}

// syncGroups stores every group the account is in, as storeContacts does
// contacts, then names the other groups it can.
func (in *ingester) syncGroups(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	groups, err := in.cli.GetJoinedGroups(qctx)
	cancel()
	if err != nil {
		in.log.Warn("joined groups query failed", "err", err)
		return
	}
	in.storeGroups(ctx, groups)
	in.nameGroups(ctx)
}

// nameGroups looks up the groups stored without a name, as when their
// lookup failed. WhatsApp describes a group to its members only, so one the
// owner left keeps none.
func (in *ingester) nameGroups(ctx context.Context) {
	rows, err := in.st.DB().QueryContext(ctx, `SELECT id FROM containers WHERE kind = 'group' AND name IS NULL`)
	if err != nil {
		in.log.Error("list unnamed groups", "err", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	var groups []*types.GroupInfo
	for _, id := range ids {
		j, err := types.ParseJID(id)
		if err != nil {
			continue
		}
		if info, err := in.lookupGroup(ctx, j); err == nil {
			groups = append(groups, info)
		}
	}
	in.storeGroups(ctx, groups)
	if len(ids) > 0 {
		in.log.Info("looked up unnamed groups", "count", len(ids), "found", len(groups))
	}
}

func (in *ingester) storeGroups(ctx context.Context, groups []*types.GroupInfo) {
	for _, g := range groups {
		in.mu.Lock()
		in.seen[g.JID.ToNonAD()] = true
		in.mu.Unlock()
		if err := in.st.UpsertContainer(ctx, groupContainer(g, in.pn, in.nameFunc(ctx), in.own)); err != nil {
			in.log.Error("store group", "chat", g.JID.String(), "err", err)
		}
	}
}

// Unseen users are ignored so group members don't become DMs.
func (in *ingester) renameDM(ctx context.Context, j types.JID) {
	chat := in.pn(j)
	in.mu.Lock()
	seen := in.seen[chat]
	in.mu.Unlock()
	if !seen || chat.Server == types.GroupServer {
		return
	}
	name := in.chatName(ctx, chat)
	if name == "" {
		return
	}
	c := store.Container{ID: chat.String(), Kind: "dm", Name: name, Participants: in.dmParticipants(chat, name)}
	if err := in.st.UpsertContainer(ctx, c); err != nil {
		in.log.Error("store dm name", "chat", chat.String(), "err", err)
	}
}

// dmParticipants lists the person of the DM chat by phone number where it is
// known, the ID their messages carry, so that hiding them hides the DM too;
// the number is their handle. The owner's own chat lists nobody.
func (in *ingester) dmParticipants(chat types.JID, name string) []store.Participant {
	j := in.pn(chat)
	if in.own(j) {
		return nil
	}
	return []store.Participant{{ID: j.String(), Name: name, Handle: number(j)}}
}

// nameDMs names every stored DM from the contact store, and one that has no
// name by phone number.
func (in *ingester) nameDMs(ctx context.Context) {
	rows, err := in.st.DB().QueryContext(ctx, `SELECT id, COALESCE(name, ''), participants_json FROM containers WHERE kind = 'dm'`)
	if err != nil {
		in.log.Error("list dms", "err", err)
		return
	}
	type dm struct{ id, name, participants string }
	var dms []dm
	for rows.Next() {
		var d dm
		if err := rows.Scan(&d.id, &d.name, &d.participants); err == nil {
			dms = append(dms, d)
		}
	}
	rows.Close()
	named := 0
	for _, d := range dms {
		j, err := types.ParseJID(d.id)
		if err != nil {
			continue
		}
		pn := in.pn(j)
		name := cmp.Or(in.chatName(ctx, pn, j), d.name, number(pn))
		parts := in.dmParticipants(j, name)
		if b, err := json.Marshal(parts); name == d.name && (parts == nil || err == nil && string(b) == d.participants) {
			continue
		}
		if err := in.st.UpsertContainer(ctx, store.Container{ID: d.id, Kind: "dm", Name: name, Participants: parts}); err != nil {
			in.log.Error("store dm name", "chat", d.id, "err", err)
			continue
		}
		named++
	}
	if named > 0 {
		in.log.Info("named direct chats", "count", named)
	}
}

func (in *ingester) groupChanged(ctx context.Context, e *events.GroupInfo) {
	chat := e.JID.ToNonAD()
	if len(e.Join) > 0 || len(e.Leave) > 0 {
		// Refresh participants on the next message.
		in.mu.Lock()
		delete(in.seen, chat)
		in.mu.Unlock()
	}
	if e.Name == nil || e.Name.Name == "" {
		return
	}
	c := store.Container{ID: chat.String(), Kind: "group", IsGroup: true, Name: e.Name.Name}
	if err := in.st.UpsertContainer(ctx, c); err != nil {
		in.log.Error("store group name", "chat", chat.String(), "err", err)
	}
}

func (in *ingester) historySync(ctx context.Context, hs *waHistorySync.HistorySync) {
	in.mu.Lock()
	for _, m := range hs.GetPhoneNumberToLidMappings() {
		lid, err1 := types.ParseJID(m.GetLidJID())
		pn, err2 := types.ParseJID(m.GetPnJID())
		if err1 == nil && err2 == nil && lid.Server == types.HiddenUserServer {
			pn.Server = types.DefaultUserServer // legacy c.us
			in.lidPN[lid.ToNonAD()] = pn.ToNonAD()
		}
	}
	for _, conv := range hs.GetConversations() {
		lid, err1 := types.ParseJID(conv.GetID())
		pn, err2 := types.ParseJID(conv.GetPnJID())
		if err1 == nil && err2 == nil && lid.Server == types.HiddenUserServer && pn.Server == types.DefaultUserServer {
			in.lidPN[lid.ToNonAD()] = pn.ToNonAD()
		}
	}
	in.mu.Unlock()

	// History comes once, so a busy store is waited out.
	var convs, stored, failed int
	for _, conv := range hs.GetConversations() {
		raw, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		chat, ok := historyChat(raw, in.pn)
		if !ok {
			continue
		}
		base := historyContainer(chat, conv, in.pn, in.own)
		if err := store.Retry(ctx, func() error { return in.ensureContainer(ctx, chat, base) }); err != nil {
			in.log.Error("store history container", "chat", chat.String(), "err", err)
			continue
		}
		convs++
		for _, hm := range conv.GetMessages() {
			wm := hm.GetMessage()
			if wm == nil {
				continue
			}
			evt, err := in.cli.ParseWebMessage(raw, wm)
			if err != nil {
				continue
			}
			o, ok := convert(evt, in.pn)
			if !ok {
				continue
			}
			if err := store.Retry(ctx, func() error { return in.apply(ctx, o) }); err != nil {
				failed++
				in.log.Error("store history message", "id", evt.Info.ID, "chat", chat.String(), "err", err)
				continue
			}
			stored++
		}
	}
	in.log.Info("history sync", "type", hs.GetSyncType().String(), "chunk", hs.GetChunkOrder(),
		"progress", hs.GetProgress(), "conversations", convs, "stored", stored, "failed", failed)
}
