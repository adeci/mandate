package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	sgstore "go.mau.fi/mautrix-signal/pkg/signalmeow/store"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"

	"github.com/adeci/mandate/store"
)

const queryTimeout = 10 * time.Second

// lookup is what ingest asks Signal: read-only profile and group fetches,
// cached by signalmeow.
type lookup interface {
	ContactByACI(ctx context.Context, aci uuid.UUID) (*types.Recipient, error)
	RetrieveGroupByID(ctx context.Context, gid types.GroupIdentifier, revision uint32) (*signalmeow.Group, *signalmeow.SendEndorsementCache, error)
}

type ingester struct {
	cli    lookup
	dev    *sgstore.Device
	st     *store.Store
	timers *timers
	log    *slog.Logger

	mu   sync.Mutex
	seen map[string]uint32 // chats upserted by this process, with the group revision
}

func newIngester(cli lookup, dev *sgstore.Device, st *store.Store, t *timers, log *slog.Logger) *ingester {
	return &ingester{cli: cli, dev: dev, st: st, timers: t, log: log, seen: make(map[string]uint32)}
}

// Returning false for a message leaves it unacknowledged, so the server
// delivers it again.
func (in *ingester) handle(ctx context.Context, evt events.SignalEvent) bool {
	switch e := evt.(type) {
	case *events.ChatEvent:
		return in.chatEvent(ctx, e)
	case *events.ContactList:
		in.renameDMs(ctx, e.Contacts)
	}
	return true
}

func (in *ingester) chatEvent(ctx context.Context, e *events.ChatEvent) bool {
	chat := in.chatID(ctx, e.Info.ChatID)
	if dm, ok := e.Event.(*signalpb.DataMessage); ok {
		if !isGroup(chat) {
			if err := in.timers.observe(ctx, chat, dm); err != nil {
				in.log.Error("store disappearing timer", "chat", chat, "err", err)
				return false
			}
		}
		if dm.GetGroupV2().GetGroupChange() != nil {
			if err := in.ensureContainer(ctx, chat, e.Info.GroupRevision, time.Time{}); err != nil {
				in.log.Error("store group", "chat", chat, "err", err)
			}
		}
	}
	o, ok := convert(e, chat, in.dev.ACI, func(aci uuid.UUID) string {
		if n := in.localName(ctx, aci); n != "" {
			return n
		}
		return aci.String()
	})
	if !ok {
		return true
	}
	var err error
	switch o.kind {
	case opUpsert:
		if err = in.ensureContainer(ctx, chat, e.Info.GroupRevision, o.msg.SentAt); err != nil {
			break
		}
		if !o.msg.IsFromMe {
			o.msg.SenderName = in.contactName(ctx, o.sender)
		}
		err = in.st.UpsertMessage(ctx, o.msg)
	case opEdit:
		err = in.st.EditMessage(ctx, o.target, o.body, o.at)
	case opDelete:
		err = in.st.DeleteMessage(ctx, o.target)
	}
	if err != nil {
		in.log.Error("store message", "chat", chat, "err", err)
		return false
	}
	return true
}

// chatID names a DM by the user's ACI where known; messages the owner sent
// from the phone may name the recipient by phone number identity (PNI).
func (in *ingester) chatID(ctx context.Context, raw string) string {
	if !strings.HasPrefix(raw, "PNI:") {
		return raw
	}
	sid, err := libsignalgo.ServiceIDFromString(raw)
	if err != nil {
		return raw
	}
	r, err := in.dev.RecipientStore.LoadAndUpdateRecipient(ctx, uuid.Nil, sid.UUID, nil)
	if err != nil || r.ACI == uuid.Nil {
		return raw
	}
	return r.ACI.String()
}

// displayName is the owner's nickname or address-book name for r, else the
// profile name marked with "~", since anyone can pick that. The owner's own
// profile name is unmarked.
func displayName(r *types.Recipient, own uuid.UUID) string {
	switch {
	case r == nil:
		return ""
	case r.ACI == own && own != uuid.Nil:
		return r.Profile.Name
	case r.Nickname != "":
		return r.Nickname
	case r.ContactName != "":
		return r.ContactName
	case r.Profile.Name != "":
		return "~" + r.Profile.Name
	}
	return ""
}

// contactName may fetch the profile, at most hourly per user.
func (in *ingester) contactName(ctx context.Context, aci uuid.UUID) string {
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	r, err := in.cli.ContactByACI(qctx, aci)
	if err != nil {
		in.log.Warn("contact lookup failed", "aci", aci.String(), "err", err)
		return ""
	}
	return displayName(r, in.dev.ACI)
}

func (in *ingester) localName(ctx context.Context, aci uuid.UUID) string {
	r, err := in.dev.RecipientStore.LoadAndUpdateRecipient(ctx, aci, uuid.Nil, nil)
	if err != nil {
		return ""
	}
	return displayName(r, in.dev.ACI)
}

// Once per process and group revision, except that later calls still
// record the date. Groups are looked up with RetrieveGroupByID, a read-only
// query.
func (in *ingester) ensureContainer(ctx context.Context, chat string, revision uint32, at time.Time) error {
	group := isGroup(chat)
	in.mu.Lock()
	known, seen := in.seen[chat]
	if seen && revision <= known {
		in.mu.Unlock()
		if at.IsZero() {
			return nil
		}
		return in.st.UpsertContainer(ctx, store.Container{ID: chat, Kind: kindFor(chat), IsGroup: group, LastMessageAt: at})
	}
	in.seen[chat] = revision
	in.mu.Unlock()

	c := store.Container{ID: chat, Kind: kindFor(chat), IsGroup: group, LastMessageAt: at}
	if group {
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		g, _, err := in.cli.RetrieveGroupByID(qctx, types.GroupIdentifier(chat), revision)
		cancel()
		if err != nil {
			in.log.Warn("group lookup failed", "chat", chat, "err", err)
		} else {
			c.Name = g.Title
			c.Participants = make([]store.Participant, 0, len(g.Members))
			for _, m := range g.Members {
				c.Participants = append(c.Participants, store.Participant{ID: m.ACI.String(), Name: in.localName(ctx, m.ACI)})
			}
		}
	} else if sid, err := libsignalgo.ServiceIDFromString(chat); err == nil && sid.Type == libsignalgo.ServiceIDTypeACI {
		c.Name = in.contactName(ctx, sid.UUID)
	}
	if err := in.st.UpsertContainer(ctx, c); err != nil {
		in.mu.Lock()
		delete(in.seen, chat)
		in.mu.Unlock()
		return err
	}
	return nil
}

// renameDMs names the stored DMs of contacts the owner's devices synced.
func (in *ingester) renameDMs(ctx context.Context, contacts []*types.Recipient) {
	for _, r := range contacts {
		name := displayName(r, in.dev.ACI)
		if r.ACI == uuid.Nil || name == "" {
			continue
		}
		var old string
		err := in.st.DB().QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM containers WHERE id = ?1 AND kind = 'dm'`, r.ACI.String()).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) || err == nil && old == name {
			continue
		}
		if err == nil {
			err = in.st.UpsertContainer(ctx, store.Container{ID: r.ACI.String(), Kind: "dm", Name: name})
		}
		if err != nil {
			in.log.Error("store dm name", "chat", r.ACI.String(), "err", err)
		}
	}
}

const timersSchema = `
CREATE TABLE IF NOT EXISTS mandate_timers (
    chat    TEXT PRIMARY KEY,
    seconds INTEGER NOT NULL,
    version INTEGER NOT NULL
)`

// timers remembers each DM's disappearing-message timer, which every
// message carries: one sent without it can turn the timer off for the
// recipient. Group timers are part of the group.
type timers struct{ db *sql.DB }

func (t *timers) observe(ctx context.Context, chat string, m *signalpb.DataMessage) error {
	update := m.GetFlags()&uint32(signalpb.DataMessage_EXPIRATION_TIMER_UPDATE) != 0
	if !update && m.ExpireTimer == nil {
		return nil
	}
	_, err := t.db.ExecContext(ctx, `
INSERT INTO mandate_timers (chat, seconds, version) VALUES (?1, ?2, ?3)
ON CONFLICT(chat) DO UPDATE SET seconds = excluded.seconds, version = excluded.version
WHERE excluded.version >= mandate_timers.version`,
		chat, m.GetExpireTimer(), m.GetExpireTimerVersion())
	return err
}

func (t *timers) get(ctx context.Context, chat string) (seconds, version uint32, err error) {
	err = t.db.QueryRowContext(ctx, `SELECT seconds, version FROM mandate_timers WHERE chat = ?1`, chat).Scan(&seconds, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return seconds, version, err
}
