package gmessages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/sieve/internal/exitcode"
	"github.com/adeci/sieve/internal/store"
)

const (
	// opsBuffer bounds queued store writes; a full queue blocks the event
	// handler (backpressure) rather than dropping events.
	opsBuffer = 4096
	// conversationPageSize matches the bridge's initial_chat_sync_count.
	conversationPageSize = 25
	maxConversationPages = 40
	// conversationFetchCooldown matches the bridge's chatInfoFetchCooldown.
	conversationFetchCooldown = 5 * time.Minute
)

type fatalError struct {
	err          error
	clearPairing bool
}

// Exactly one field is set.
type op struct {
	conv        *gmproto.Conversation
	msg         *gmproto.Message
	fetchFailed string // conversation ID whose metadata fetch failed
}

type ingester struct {
	ctx    context.Context // cancelled by stop; used for upstream requests
	cancel context.CancelFunc
	dbCtx  context.Context // outlives ctx so queued writes drain on shutdown
	cli    *libgm.Client
	st     *store.Store
	sess   *sessionFile
	log    *slog.Logger

	backfillCount int64
	reclaim       bool

	ops         chan op
	stopping    chan struct{} // closed by stop: writer drains and exits
	done        chan struct{} // closed when the writer has exited
	stopOnce    sync.Once
	backfillReq chan struct{}
	fatal       chan fatalError

	backfilling    atomic.Bool
	mobileSyncing  atomic.Bool
	reclaiming     atomic.Bool
	hackyResetDone atomic.Bool

	mu              sync.Mutex // guards the session state below
	ready           bool       // a BROWSER_ACTIVE alert has been received
	inactive        bool       // another web client took the active session
	phoneResponding bool
	sessionID       string
	noDataRecently  bool

	// Writer-goroutine state.
	draining      bool
	convs         map[string]*gmproto.Conversation
	known         map[string]bool // containers known to exist in the store
	pending       map[string][]*gmproto.Message
	fetching      map[string]bool
	fetchFailedAt map[string]time.Time
}

func newIngester(parent context.Context, cli *libgm.Client, st *store.Store, sess *sessionFile, log *slog.Logger, backfillCount int64, reclaim bool) *ingester {
	ctx, cancel := context.WithCancel(parent)
	return &ingester{
		ctx:             ctx,
		cancel:          cancel,
		dbCtx:           context.WithoutCancel(parent),
		cli:             cli,
		st:              st,
		sess:            sess,
		log:             log,
		backfillCount:   backfillCount,
		reclaim:         reclaim,
		ops:             make(chan op, opsBuffer),
		stopping:        make(chan struct{}),
		done:            make(chan struct{}),
		backfillReq:     make(chan struct{}, 1),
		fatal:           make(chan fatalError, 1),
		phoneResponding: true,
		convs:           make(map[string]*gmproto.Conversation),
		known:           make(map[string]bool),
		pending:         make(map[string][]*gmproto.Message),
		fetching:        make(map[string]bool),
		fetchFailedAt:   make(map[string]time.Time),
	}
}

// Call after the libgm client is disconnected.
func (in *ingester) stop() {
	in.stopOnce.Do(func() {
		in.cancel()
		close(in.stopping)
		<-in.done
	})
}

func (in *ingester) fail(err error, clearPairing bool) {
	select {
	case in.fatal <- fatalError{err: err, clearPairing: clearPairing}:
	default:
		// Typing, settings, and similar events are not stored.
	}
}

func (in *ingester) enqueue(o op) {
	select {
	case in.ops <- o:
	case <-in.done:
	}
}

func (in *ingester) requestBackfill() {
	select {
	case in.backfillReq <- struct{}{}:
	default:
	}
}

// handleEvent is libgm's event handler. libgm calls it synchronously from its
// polling and pinging goroutines, so it must not make requests to the phone;
// those run on separate goroutines.
func (in *ingester) handleEvent(raw any) {
	switch evt := raw.(type) {
	case *events.ClientReady:
		// Not emitted by this libgm version (readiness is the BROWSER_ACTIVE
		// alert below); handled in case a future version does.
		for _, c := range evt.Conversations {
			in.enqueue(op{conv: c})
		}
		in.requestBackfill()
	case *events.AuthTokenRefreshed:
		if err := in.sess.save(); err != nil {
			in.log.Error("save session after token refresh", "err", err)
		}
	case *events.ListenFatalError:
		if errors.Is(evt.Error, events.ErrInvalidCredentials) || evt.Error.Error() == "http 401 while polling" {
			in.fail(fmt.Errorf("session rejected by Google (%w); re-pair with \"sieve messages pair\": %w", evt.Error, exitcode.ErrNeedsOwner), false)
		} else {
			in.fail(fmt.Errorf("listen: %w", evt.Error), false)
		}
	case *events.ListenTemporaryError:
		in.log.Warn("connection to Google interrupted, retrying", "err", evt.Error)
	case *events.ListenRecovered:
		in.log.Info("connection to Google restored")
	case *events.PhoneNotResponding:
		in.mu.Lock()
		in.phoneResponding = false
		in.mu.Unlock()
		in.log.Warn("phone is not responding; messages arrive once it is back online")
	case *events.PhoneRespondingAgain:
		in.mu.Lock()
		in.phoneResponding = true
		in.mu.Unlock()
		in.log.Info("phone is responding again")
	case *events.PingFailed:
		if errors.Is(evt.Error, events.ErrRequestedEntityNotFound) {
			in.fail(fmt.Errorf("phone no longer recognizes this session (%w); %w", evt.Error, errNotPaired), true)
		} else {
			in.log.Warn("ping to phone failed", "err", evt.Error, "count", evt.ErrorCount)
		}
	case *events.HackySetActiveMayFail:
		go in.hackyResetActive()
	case *gmproto.RevokePairData:
		in.fail(fmt.Errorf("pairing revoked from the phone; %w", errNotPaired), true)
	case *events.GaiaLoggedOut:
		in.fail(fmt.Errorf("logged out by Google; %w", errNotPaired), true)
	case *events.AccountChange:
		if evt.IsFake {
			break
		}
		if evt.GetEnabled() {
			in.log.Warn("phone switched to Google-account pairing; this QR session may stop receiving until switched back")
		} else {
			in.log.Info("phone switched back to QR pairing")
			in.mu.Lock()
			in.ready = true
			in.mu.Unlock()
		}
	case *events.NoDataReceived:
		in.mu.Lock()
		in.noDataRecently = true
		in.mu.Unlock()
	case *events.PairSuccessful:
		in.log.Warn("unexpected pair success event while running")
	case *gmproto.UserAlertEvent:
		in.handleAlert(evt.GetAlertType())
	case *gmproto.Conversation:
		in.dataReceived()
		in.enqueue(op{conv: evt})
	case *libgm.WrappedMessage:
		in.dataReceived()
		if isDeletion(evt.Message) && (in.mobileSyncing.Load() || in.backfilling.Load()) {
			// The bridge ignores these: a resync can report spurious deletions.
			in.log.Debug("ignoring deletion during resync", "message_id", evt.GetMessageID())
			return
		}
		in.enqueue(op{msg: evt.Message})
	default:
	}
}

// dataReceived mirrors the bridge's resyncAfterDataResume: after a quiet
// period flagged by NoDataReceived, check for events missed meanwhile.
func (in *ingester) dataReceived() {
	in.mu.Lock()
	resync := in.noDataRecently
	in.noDataRecently = false
	in.mu.Unlock()
	if resync {
		in.requestBackfill()
	}
}

func (in *ingester) handleAlert(alert gmproto.AlertType) {
	switch alert {
	case gmproto.AlertType_BROWSER_ACTIVE:
		in.mu.Lock()
		wasInactive := in.inactive || !in.ready
		in.inactive, in.ready = false, true
		sid := in.cli.CurrentSessionID()
		changed := sid != in.sessionID
		in.sessionID = sid
		noData := in.noDataRecently
		in.noDataRecently = false
		in.mu.Unlock()
		if changed || wasInactive || noData {
			in.log.Info("this client is the active Messages for Web session; syncing")
			in.requestBackfill()
		}
	case gmproto.AlertType_BROWSER_INACTIVE,
		gmproto.AlertType_BROWSER_INACTIVE_FROM_TIMEOUT,
		gmproto.AlertType_BROWSER_INACTIVE_FROM_INACTIVITY:
		in.mu.Lock()
		in.inactive = true
		in.mu.Unlock()
		if in.reclaim {
			in.log.Warn("another Messages for Web session became active; reclaiming", "alert", alert.String())
			go in.reclaimActive()
		} else {
			in.log.Warn("another Messages for Web session became active; not receiving until restart", "alert", alert.String())
		}
	case gmproto.AlertType_MOBILE_DATABASE_SYNC_STARTED:
		in.mobileSyncing.Store(true)
	case gmproto.AlertType_MOBILE_DATABASE_SYNC_COMPLETE:
		in.mobileSyncing.Store(false)
		in.requestBackfill()
	}
}

// reclaimActive is the bridge's aggressiveSetActive: re-take the active
// session with increasing delays unless it comes back on its own.
func (in *ingester) reclaimActive() {
	if !in.reclaiming.CompareAndSwap(false, true) {
		return
	}
	defer in.reclaiming.Store(false)
	for _, delay := range []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second} {
		if !sleepCtx(in.ctx, delay) {
			return
		}
		in.mu.Lock()
		inactive := in.inactive
		in.mu.Unlock()
		if !inactive {
			return
		}
		in.log.Info("setting this client as the active session")
		if err := in.cli.SetActiveSession(in.ctx); err != nil {
			in.log.Warn("set active session failed", "err", err)
			continue
		}
		return
	}
}

// hackyResetActive is the bridge's response to HackySetActiveMayFail: if the
// phone does not confirm this session as active, ask again, then reconnect.
func (in *ingester) hackyResetActive() {
	if !in.hackyResetDone.CompareAndSwap(false, true) {
		return
	}
	notReady := func() bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return !in.ready && in.phoneResponding
	}
	if !sleepCtx(in.ctx, 7*time.Second) || !notReady() {
		return
	}
	in.log.Warn("session still not active, setting active session again")
	if err := in.cli.SetActiveSession(in.ctx); err != nil {
		in.log.Warn("set active session failed", "err", err)
	}
	if !sleepCtx(in.ctx, 7*time.Second) || !notReady() {
		return
	}
	in.log.Warn("session still not active, reconnecting")
	if err := in.cli.Reconnect(in.ctx); err != nil {
		in.log.Error("reconnect failed", "err", err)
	}
}

func (in *ingester) backfillLoop() {
	for {
		select {
		case <-in.ctx.Done():
			return
		case <-in.backfillReq:
			in.backfill()
		}
	}
}

// Fetches recent messages only for conversations whose latest message is not
// stored yet.
func (in *ingester) backfill() {
	in.backfilling.Store(true)
	defer in.backfilling.Store(false)
	start := time.Now()
	var listed, fetched int
	for _, folder := range []gmproto.ListConversationsRequest_Folder{
		gmproto.ListConversationsRequest_INBOX,
		gmproto.ListConversationsRequest_ARCHIVE,
	} {
		convs, err := in.listConversations(folder)
		if in.ctx.Err() != nil {
			return
		} else if err != nil {
			in.log.Warn("list conversations failed", "folder", folder.String(), "err", err)
		}
		listed += len(convs)
		for _, c := range convs {
			in.enqueue(op{conv: c})
		}
		if in.backfillCount == 0 {
			continue
		}
		for _, c := range convs {
			latest := c.GetLatestMessageID()
			if latest == "" {
				continue
			}
			if stored, err := in.messageStored(latest); err != nil {
				in.log.Warn("check stored message", "err", err)
			} else if stored {
				continue
			}
			resp, err := in.cli.FetchMessages(in.ctx, c.GetConversationID(), in.backfillCount, nil)
			if in.ctx.Err() != nil {
				return
			} else if err != nil {
				in.log.Warn("fetch messages failed", "conversation_id", c.GetConversationID(), "err", err)
				continue
			}
			fetched++
			// Newest first from the phone; write oldest first.
			msgs := resp.GetMessages()
			for i := len(msgs) - 1; i >= 0; i-- {
				in.enqueue(op{msg: msgs[i]})
			}
		}
	}
	in.log.Info("sync finished", "conversations", listed, "conversations_fetched", fetched, "took", time.Since(start).Round(time.Millisecond))
}

func (in *ingester) listConversations(folder gmproto.ListConversationsRequest_Folder) ([]*gmproto.Conversation, error) {
	var out []*gmproto.Conversation
	seen := make(map[string]bool)
	var cursor *gmproto.Cursor
	for range maxConversationPages {
		resp, err := in.cli.ListConversations(in.ctx, &gmproto.ListConversationsRequest{
			Count:  conversationPageSize,
			Folder: folder,
			Cursor: cursor,
		})
		if err != nil {
			return out, err
		}
		added := 0
		for _, c := range resp.GetConversations() {
			if id := c.GetConversationID(); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, c)
				added++
			}
		}
		if added == 0 || len(resp.GetConversations()) < conversationPageSize || resp.GetCursor() == nil {
			break
		}
		cursor = resp.GetCursor()
	}
	return out, nil
}

func (in *ingester) messageStored(id string) (bool, error) {
	var one int
	err := in.st.DB().QueryRowContext(in.ctx, `SELECT 1 FROM messages WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (in *ingester) containerStored(id string) (bool, error) {
	var one int
	err := in.st.DB().QueryRowContext(in.dbCtx, `SELECT 1 FROM containers WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// As the bridge's getChatInfoWithFetch does.
func (in *ingester) fetchConversation(id string) {
	conv, err := in.cli.GetConversation(in.ctx, id)
	if err != nil || conv == nil {
		in.log.Warn("fetch conversation failed", "conversation_id", id, "err", err)
		in.enqueue(op{fetchFailed: id})
		return
	}
	in.enqueue(op{conv: conv})
}

// writeLoop is the only goroutine that writes to the store, so container and
// message writes stay ordered.
func (in *ingester) writeLoop() {
	defer close(in.done)
	for {
		select {
		case o := <-in.ops:
			in.apply(o)
		case <-in.stopping:
			in.draining = true
			for {
				select {
				case o := <-in.ops:
					in.apply(o)
				default:
					for id := range in.pending {
						in.flushPending(id, in.convs[id])
					}
					return
				}
			}
		}
	}
}

func (in *ingester) apply(o op) {
	switch {
	case o.conv != nil:
		id := o.conv.GetConversationID()
		if id == "" {
			return
		}
		in.convs[id] = o.conv
		delete(in.fetching, id)
		delete(in.fetchFailedAt, id)
		if err := in.st.UpsertContainer(in.dbCtx, conversationToContainer(o.conv)); err != nil {
			in.log.Error("upsert container", "conversation_id", id, "err", err)
		} else {
			in.known[id] = true
		}
		in.flushPending(id, o.conv)
	case o.msg != nil:
		in.applyMessage(o.msg)
	case o.fetchFailed != "":
		delete(in.fetching, o.fetchFailed)
		in.fetchFailedAt[o.fetchFailed] = time.Now()
		in.flushPending(o.fetchFailed, nil)
	}
}

func (in *ingester) applyMessage(m *gmproto.Message) {
	if isDeletion(m) {
		if err := in.st.DeleteMessage(in.dbCtx, m.GetMessageID()); err != nil {
			in.log.Error("delete message", "message_id", m.GetMessageID(), "err", err)
		}
		return
	}
	if _, ok := messageToStore(m, nil); !ok {
		return // nothing to store yet; a later status update may carry content
	}
	id := m.GetConversationID()
	conv := in.convs[id]
	if conv == nil && !in.draining && time.Since(in.fetchFailedAt[id]) > conversationFetchCooldown {
		// Hold the message until the participant list is known: sender_id is
		// never rewritten once stored.
		in.pending[id] = append(in.pending[id], m)
		if !in.fetching[id] {
			in.fetching[id] = true
			go in.fetchConversation(id)
		}
		return
	}
	in.writeMessage(m, conv)
}

func (in *ingester) flushPending(id string, conv *gmproto.Conversation) {
	msgs := in.pending[id]
	delete(in.pending, id)
	for _, m := range msgs {
		in.writeMessage(m, conv)
	}
}

func (in *ingester) writeMessage(m *gmproto.Message, conv *gmproto.Conversation) {
	sm, ok := messageToStore(m, conv)
	if !ok {
		return
	}
	if !in.known[sm.ContainerID] {
		// Never clobber a stored container (e.g. a group's kind) with the
		// fallback guess.
		exists, err := in.containerStored(sm.ContainerID)
		if err != nil {
			in.log.Error("check container", "conversation_id", sm.ContainerID, "err", err)
			return
		}
		if !exists {
			if err := in.st.UpsertContainer(in.dbCtx, fallbackContainer(sm.ContainerID)); err != nil {
				in.log.Error("upsert container", "conversation_id", sm.ContainerID, "err", err)
				return
			}
		}
		in.known[sm.ContainerID] = true
	}
	if err := in.st.UpsertMessage(in.dbCtx, sm); err != nil {
		in.log.Error("upsert message", "message_id", sm.ID, "err", err)
	}
}
