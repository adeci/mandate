package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/store"
)

const (
	// A sync stores the latest messages of the most recent conversations.
	syncConversations = 100
	syncMessages      = 50

	lookupTimeout = 20 * time.Second
)

// ingester stores what the phone reports. Its caches are filled by events,
// syncs and lookups, from libgm's goroutine and from our own.
type ingester struct {
	st  *store.Store
	log *slog.Logger

	mu    sync.Mutex
	convs map[string]*gmproto.Conversation
	sims  map[string]*gmproto.SIMPayload // by the owner's participant ID
	// pending holds messages waiting for their conversation's lookup.
	pending map[string][]*gmproto.Message
	// syncing is set while a sync runs; again asks it to run once more.
	syncing, again bool
	// phoneSyncing is set while the phone rebuilds its message database.
	phoneSyncing bool
}

func newIngester(st *store.Store, log *slog.Logger) *ingester {
	in := &ingester{st: st, log: log}
	in.reset()
	return in
}

// reset forgets what the previous pairing reported.
func (in *ingester) reset() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.convs = make(map[string]*gmproto.Conversation)
	in.sims = make(map[string]*gmproto.SIMPayload)
	in.pending = make(map[string][]*gmproto.Message)
	in.phoneSyncing = false
}

func (in *ingester) setPhoneSyncing(on bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.phoneSyncing = on
}

func (in *ingester) conversation(id string) (*gmproto.Conversation, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	conv, ok := in.convs[id]
	return conv, ok
}

func (in *ingester) remember(conv *gmproto.Conversation) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.convs[conv.GetConversationID()] = conv
}

// lookup returns conv from the cache, or asks the phone.
func (in *ingester) lookup(ctx context.Context, cli gmClient, id string) (*gmproto.Conversation, error) {
	if conv, ok := in.conversation(id); ok {
		return conv, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	conv, err := cli.GetConversation(ctx, id)
	if err != nil {
		return nil, err
	}
	if conv.GetConversationID() != id {
		return nil, fmt.Errorf("the phone has no conversation %s", id)
	}
	in.remember(conv)
	return conv, nil
}

func (in *ingester) settings(s *gmproto.Settings) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.sims = make(map[string]*gmproto.SIMPayload, len(s.GetSIMCards()))
	for _, sim := range s.GetSIMCards() {
		in.sims[sim.GetSIMParticipant().GetID()] = sim.GetSIMData().GetSIMPayload()
	}
}

// sim picks the SIM conv sends from.
func (in *ingester) sim(conv *gmproto.Conversation) *gmproto.SIMPayload {
	in.mu.Lock()
	sim := in.sims[conv.GetDefaultOutgoingID()]
	in.mu.Unlock()
	if sim != nil {
		return sim
	}
	return conv.GetSimCard().GetSIMData().GetSIMPayload()
}

func (in *ingester) conversationEvent(ctx context.Context, conv *gmproto.Conversation) {
	in.remember(conv)
	if err := in.st.UpsertContainer(ctx, container(conv)); err != nil {
		in.log.Error("store conversation", "conversation", conv.GetConversationID(), "err", err)
	}
}

// message stores m. libgm calls the event handler synchronously and asks that
// it not wait on the phone, so messages of a conversation not seen yet wait
// for its lookup in another goroutine.
func (in *ingester) message(ctx context.Context, cli gmClient, m *gmproto.Message) {
	id := m.GetConversationID()
	in.mu.Lock()
	if in.phoneSyncing && m.GetMessageStatus().GetStatus() == gmproto.MessageStatusType_MESSAGE_DELETED {
		// mautrix-gmessages ignores deletions reported while the phone
		// rebuilds its database.
		in.mu.Unlock()
		return
	}
	conv, known := in.convs[id]
	_, waiting := in.pending[id]
	if !known {
		in.pending[id] = append(in.pending[id], m)
	}
	in.mu.Unlock()
	switch {
	case known:
		in.store(ctx, m, conv)
	case !waiting:
		go in.resolve(ctx, cli, id)
	}
}

// resolve looks up conversation id and stores the messages waiting for it.
func (in *ingester) resolve(ctx context.Context, cli gmClient, id string) {
	conv, err := in.lookup(ctx, cli, id)
	in.mu.Lock()
	msgs := in.pending[id]
	delete(in.pending, id)
	in.mu.Unlock()
	if err != nil {
		// The next sync fetches them.
		in.log.Warn("look up conversation", "conversation", id, "err", err)
		return
	}
	if err := in.st.UpsertContainer(ctx, container(conv)); err != nil {
		in.log.Error("store conversation", "conversation", id, "err", err)
		return
	}
	for _, m := range msgs {
		in.store(ctx, m, conv)
	}
}

func (in *ingester) store(ctx context.Context, m *gmproto.Message, conv *gmproto.Conversation) {
	o, ok := convert(m, conv)
	if !ok {
		return
	}
	if err := in.apply(ctx, o); err != nil {
		in.log.Error("store message", "id", m.GetMessageID(), "conversation", m.GetConversationID(), "err", err)
	}
}

func (in *ingester) apply(ctx context.Context, o op) error {
	if o.kind == opDelete {
		return in.st.DeleteMessage(ctx, o.target)
	}
	return in.st.UpsertMessage(ctx, o.msg)
}

// sync stores the conversation list and fetches the latest messages of each
// conversation that has newer activity than stored. A sync requested while
// one runs runs once more after it.
func (in *ingester) sync(ctx context.Context, cli gmClient) {
	in.mu.Lock()
	if in.syncing {
		in.again = true
		in.mu.Unlock()
		return
	}
	in.syncing = true
	in.mu.Unlock()
	for {
		if err := in.syncOnce(ctx, cli); err != nil {
			in.log.Warn("sync", "err", err)
		}
		in.mu.Lock()
		if !in.again {
			in.syncing = false
			in.mu.Unlock()
			return
		}
		in.again = false
		in.mu.Unlock()
	}
}

func (in *ingester) syncOnce(ctx context.Context, cli gmClient) error {
	resp, err := cli.ListConversations(ctx, &gmproto.ListConversationsRequest{
		Count:  syncConversations,
		Folder: gmproto.ListConversationsRequest_INBOX,
	})
	if err != nil {
		return fmt.Errorf("list conversations: %w", err)
	}
	stored, err := in.lastActivity(ctx)
	if err != nil {
		return err
	}
	var fetched, msgs int
	for _, conv := range resp.GetConversations() {
		in.remember(conv)
		c := container(conv)
		last := c.LastMessageAt
		if !last.Truncate(time.Millisecond).After(stored[c.ID]) {
			if err := in.st.UpsertContainer(ctx, c); err != nil {
				return err
			}
			continue
		}
		// The activity is recorded once its messages are in, so a failed
		// fetch is retried by the next sync.
		c.LastMessageAt = time.Time{}
		if err := in.st.UpsertContainer(ctx, c); err != nil {
			return err
		}
		n, err := in.backfill(ctx, cli, conv)
		if errors.Is(err, libgm.ErrPhoneNotResponding) || errors.Is(err, libgm.ErrConnectionClosed) || ctx.Err() != nil {
			return err
		}
		if err != nil {
			in.log.Warn("fetch messages", "conversation", c.ID, "err", err)
			continue
		}
		c.LastMessageAt = last
		if err := in.st.UpsertContainer(ctx, c); err != nil {
			return err
		}
		fetched++
		msgs += n
	}
	in.log.Info("synced", "conversations", len(resp.GetConversations()), "fetched", fetched, "messages", msgs)
	return nil
}

func (in *ingester) backfill(ctx context.Context, cli gmClient, conv *gmproto.Conversation) (int, error) {
	resp, err := cli.FetchMessages(ctx, conv.GetConversationID(), syncMessages, nil)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range resp.GetMessages() {
		o, ok := convert(m, conv)
		if !ok {
			continue
		}
		if err := in.apply(ctx, o); err != nil {
			return n, fmt.Errorf("store message %s: %w", m.GetMessageID(), err)
		}
		n++
	}
	return n, nil
}

// lastActivity is the recorded activity of each stored conversation.
func (in *ingester) lastActivity(ctx context.Context) (map[string]time.Time, error) {
	rows, err := in.st.DB().QueryContext(ctx, `SELECT id, last_message_at FROM containers WHERE last_message_at IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("read stored conversations: %w", err)
	}
	defer rows.Close()
	out := make(map[string]time.Time)
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			continue
		}
		out[id] = t
	}
	return out, rows.Err()
}
