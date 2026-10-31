package whatsapp

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
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
	log *slog.Logger

	mu    sync.Mutex
	seen  map[types.JID]bool      // containers upserted by this process
	lidPN map[types.JID]types.JID // LID -> PN learned from history sync
}

func newIngester(cli *whatsmeow.Client, st *store.Store, log *slog.Logger) *ingester {
	return &ingester{
		cli:   cli,
		st:    st,
		log:   log,
		seen:  make(map[types.JID]bool),
		lidPN: make(map[types.JID]types.JID),
	}
}

// Returning false for a message withholds its ack (SynchronousAck), so the
// server redelivers it after reconnecting.
func (in *ingester) handle(ctx context.Context, evt any) bool {
	switch e := evt.(type) {
	case *events.Message:
		o, ok := convert(e, in.pn)
		if !ok {
			return true
		}
		if err := in.apply(ctx, o, ""); err != nil {
			in.log.Error("store message", "id", e.Info.ID, "chat", e.Info.Chat.String(), "err", err)
			return false
		}
	case *events.HistorySync:
		in.historySync(ctx, e.Data)
	case *events.PushName:
		in.renameDM(ctx, e.JID)
	case *events.Contact:
		in.renameDM(ctx, e.JID)
	case *events.GroupInfo:
		in.groupChanged(ctx, e)
	case *events.JoinedGroup:
		in.mu.Lock()
		in.seen[e.JID.ToNonAD()] = true
		in.mu.Unlock()
		if err := in.st.UpsertContainer(ctx, groupContainer(&e.GroupInfo, in.pn, in.nameFunc(ctx))); err != nil {
			in.log.Error("store group", "chat", e.JID.String(), "err", err)
		}
	case *events.Connected:
		in.log.Info("connected")
	case *events.Disconnected:
		in.log.Warn("disconnected; whatsmeow will reconnect")
	}
	return true
}

func (in *ingester) apply(ctx context.Context, o op, nameHint string) error {
	switch o.kind {
	case opUpsert:
		if err := in.ensureContainer(ctx, o.chat, store.Container{Name: nameHint}); err != nil {
			return err
		}
		in.enrichSender(ctx, &o)
		return in.st.UpsertMessage(ctx, o.msg)
	case opEdit:
		return in.st.EditMessage(ctx, o.target, o.body, o.at)
	case opDelete:
		return in.st.DeleteMessage(ctx, o.target)
	}
	return nil
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

func (in *ingester) contactName(ctx context.Context, j types.JID) string {
	if own := in.cli.Store.GetJID(); !own.IsEmpty() && j.User == own.User && j.Server == own.Server {
		return in.cli.Store.PushName
	}
	c, err := in.cli.Store.Contacts.GetContact(ctx, j)
	if err != nil || !c.Found {
		return ""
	}
	for _, n := range []string{c.FullName, c.FirstName, c.PushName, c.BusinessName} {
		if n != "" {
			return n
		}
	}
	return ""
}

func (in *ingester) nameFunc(ctx context.Context) func(types.JID) string {
	return func(j types.JID) string { return in.contactName(ctx, j) }
}

func (in *ingester) enrichSender(ctx context.Context, o *op) {
	if o.sender.IsEmpty() {
		return
	}
	c, err := in.cli.Store.Contacts.GetContact(ctx, o.sender)
	if err == nil && c.Found {
		if c.FullName != "" {
			o.msg.SenderName = c.FullName
			return
		}
		if c.FirstName != "" {
			o.msg.SenderName = c.FirstName
			return
		}
	}
	if o.msg.SenderName == "" {
		o.msg.SenderName = in.contactName(ctx, o.sender)
	}
}

// Once per process. Groups without a name hint are looked up with
// GetGroupInfo, a read-only query.
func (in *ingester) ensureContainer(ctx context.Context, chat types.JID, base store.Container) error {
	in.mu.Lock()
	if in.seen[chat] {
		in.mu.Unlock()
		return nil
	}
	in.seen[chat] = true
	in.mu.Unlock()

	c := base
	c.ID, c.Kind, c.IsGroup = chat.String(), kindFor(chat), chat.Server == types.GroupServer
	if c.IsGroup && c.Name == "" {
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		info, err := in.cli.GetGroupInfo(qctx, chat)
		cancel()
		if err != nil {
			in.log.Warn("group info query failed", "chat", chat.String(), "err", err)
		} else {
			c = groupContainer(info, in.pn, in.nameFunc(ctx))
		}
	} else if !c.IsGroup {
		if n := in.contactName(ctx, chat); n != "" {
			c.Name = n
		}
	}
	if err := in.st.UpsertContainer(ctx, c); err != nil {
		in.mu.Lock()
		delete(in.seen, chat)
		in.mu.Unlock()
		return err
	}
	return nil
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
	name := in.contactName(ctx, chat)
	if name == "" {
		return
	}
	if err := in.st.UpsertContainer(ctx, store.Container{ID: chat.String(), Kind: "dm", Name: name}); err != nil {
		in.log.Error("store dm name", "chat", chat.String(), "err", err)
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
		base := historyContainer(chat, conv, in.pn)
		if err := in.ensureContainer(ctx, chat, base); err != nil {
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
			if err := in.apply(ctx, o, base.Name); err != nil {
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
