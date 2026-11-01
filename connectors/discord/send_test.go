package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	groupID = "1100000000000000006"
	sentID  = "1300000000000000099"
	// Holds secretBody, so the harness fails if it is ever logged; the HTML
	// characters go out unescaped, as from a browser.
	sentText = "rig build log is up <b>&</b>"
)

// newSendHarness stores the chats the owner has synced.
func newSendHarness(t *testing.T, seed func(*state)) *harness {
	t.Helper()
	h := newHarness(t, seed)
	for _, c := range []store.Container{
		{ID: guildID, Kind: kindGuild, Name: "Club"},
		{ID: chanGeneral, Kind: kindChannel, ParentID: guildID, Name: "#general"},
		{ID: threadID, Kind: kindThread, ParentID: guildID, Name: "rig thread"},
		{ID: dmID, Kind: kindDM, Name: "Alice A"},
		{ID: groupID, Kind: kindGroup, Name: "crew", IsGroup: true},
	} {
		if err := h.st.UpsertContainer(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	h.discord.handle(api("/channels/"+dmID+"/typing"), func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusNoContent)
	})
	return h
}

// posts records the bodies POSTed to a chat and answers each with reply.
type posts struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (p *posts) handle(h *harness, chat string, reply func(w http.ResponseWriter)) {
	h.discord.handle(api("/channels/"+chat+"/messages"), func(w http.ResponseWriter, r *http.Request, _ int) {
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &b); err != nil {
			h.t.Errorf("body %q: %v", raw, err)
		}
		if strings.Contains(string(raw), `\u003c`) {
			h.t.Errorf("body escapes HTML: %s", raw)
		}
		p.mu.Lock()
		p.bodies = append(p.bodies, b)
		p.mu.Unlock()
		reply(w)
	})
}

func (p *posts) all() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.bodies...)
}

func sentMessage(w http.ResponseWriter) {
	connector.WriteJSON(w, http.StatusOK, map[string]any{
		"id": sentID, "channel_id": dmID, "type": 0, "content": sentText, "timestamp": "2026-10-29T16:00:00.000000+00:00",
		"author": map[string]any{"id": selfUser, "username": "me"},
	})
}

func sendRecord(chat string) approval.Record {
	return approval.Record{ID: "p1", Connector: "discord", Op: "send", Args: map[string]string{"chat": chat, "text": sentText}}
}

func TestPrepareSend(t *testing.T) {
	h := newSendHarness(t, nil)
	h.start()
	ctx := context.Background()
	text := strings.Repeat("😀", maxMessageLength) // Discord counts characters, not bytes
	for chat, want := range map[string]approval.Field{
		dmID:        {Label: "To", Value: "Alice A"},
		groupID:     {Label: "Group", Value: "crew"},
		chanGeneral: {Label: "Channel", Value: "Club / #general"},
		threadID:    {Label: "Thread", Value: "Club / rig thread"},
	} {
		p, err := h.client.Prepare(ctx, "send", map[string]string{"chat": chat, "text": text})
		if err != nil {
			t.Fatalf("%s: %v", chat, err)
		}
		wantP := connector.Prepared{
			Args:      map[string]string{"chat": chat, "text": text},
			Display:   []approval.Field{want, {Label: "Text", Value: text, Param: "text"}},
			Container: chat,
		}
		if !reflect.DeepEqual(p, wantP) {
			t.Errorf("%s: prepared display %+v", chat, p.Display[0])
		}
		if again, err := h.client.Prepare(ctx, "send", p.Args); err != nil || !reflect.DeepEqual(again, p) {
			t.Errorf("%s: not idempotent: %v", chat, err)
		}
	}

	for _, chat := range []string{otherChan, "Alice A", ""} {
		_, err := h.client.Prepare(ctx, "send", map[string]string{"chat": chat, "text": "hi"})
		if e, ok := errors.AsType[*connector.Error](err); !ok || e.Status != http.StatusBadRequest || !strings.Contains(e.Message, "mandate discord chats --search") {
			t.Errorf("%q: %v, want 400 with the chats hint", chat, err)
		}
	}
	for name, args := range map[string]map[string]string{
		"guild":     {"chat": guildID, "text": "hi"},
		"blank":     {"chat": dmID, "text": " \n"},
		"too long":  {"chat": dmID, "text": strings.Repeat("é", maxMessageLength+1)},
		"extra arg": {"chat": dmID, "text": "hi", "reply_to": msg4},
	} {
		if _, err := h.client.Prepare(ctx, "send", args); errStatus(err) != http.StatusBadRequest {
			t.Errorf("%s: %v, want 400", name, err)
		}
	}
	if hits := h.discord.all(); len(hits) != 0 {
		t.Errorf("prepare reached Discord: %v", h.discord.paths())
	}
}

func TestExecuteSend(t *testing.T) {
	tests := []struct {
		name   string
		typing int // status of the typing indicator, 0 for 204
		reply  func(w http.ResponseWriter)
		want   connector.Status
		detail string
	}{
		{name: "sent", reply: sentMessage, want: connector.Done},
		{name: "typing indicator failed", typing: http.StatusInternalServerError, reply: sentMessage, want: connector.Done},
		{
			name: "missing access",
			reply: func(w http.ResponseWriter) {
				connector.WriteJSON(w, http.StatusForbidden, map[string]any{"message": "Missing Access", "code": 50001})
			},
			want: connector.Failed, detail: "Missing Access (code 50001)",
		},
		{
			name: "rate limited",
			reply: func(w http.ResponseWriter) {
				connector.WriteJSON(w, http.StatusTooManyRequests, map[string]any{"message": "You are being rate limited.", "retry_after": 2.5, "global": false})
			},
			want: connector.Failed, detail: "retry in 3s",
		},
		{
			name: "connection lost after sending",
			reply: func(w http.ResponseWriter) {
				c, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					c.Close()
				}
			},
			want: connector.Unknown,
		},
		{
			name: "server error",
			reply: func(w http.ResponseWriter) {
				connector.WriteJSON(w, http.StatusBadGateway, map[string]any{"message": "upstream"})
			},
			want: connector.Unknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A reading break is under way; the owner's send does not wait for it.
			h := newSendHarness(t, func(s *state) {
				s.LastRequest, s.BreakUntil = noon.Add(-30*time.Second), noon.Add(10*time.Minute)
			})
			if tt.typing != 0 {
				h.discord.reply(api("/channels/"+dmID+"/typing"), tt.typing, map[string]any{"message": "oops"})
			}
			var p posts
			p.handle(h, dmID, tt.reply)
			h.start()

			res := h.w.Execute(context.Background(), sendRecord(dmID))
			if res.Status != tt.want || !strings.Contains(res.Detail, tt.detail) {
				t.Fatalf("result %+v, want %s with %q", res, tt.want, tt.detail)
			}
			// The typing indicator, then the message once typing it took long
			// enough; only the message is the send.
			hits := h.discord.all()
			if len(hits) != 2 || hits[0].path != api("/channels/"+dmID+"/typing") || hits[1].method != http.MethodPost || len(p.all()) != 1 {
				t.Fatalf("upstream %v, want typing and exactly one message POST", h.discord.paths())
			}
			if tt.want != connector.Done {
				if row := h.queryRow(`SELECT count(*) FROM messages`); row[0] != int64(0) {
					t.Errorf("stored %v messages", row[0])
				}
				return
			}

			typing, hit, body := hits[0], hits[1], p.all()[0]
			if !typing.at.Equal(noon) {
				t.Errorf("typing at %v, want %v", typing.at, noon)
			}
			if want := (&pacer{rng: fixedRand(0.5)}).typingDelay(sentText); hit.at.Sub(typing.at) != want {
				t.Errorf("message %v after typing, want %v", hit.at.Sub(typing.at), want)
			}
			nonce, _ := body["nonce"].(string)
			if at, ok := snowflakeTime(nonce); !ok || !at.Equal(hit.at) {
				t.Errorf("nonce %q dates %v, want %v", nonce, at, hit.at)
			}
			delete(body, "nonce")
			if want := map[string]any{"mobile_network_type": "unknown", "content": sentText, "tts": false, "flags": float64(0)}; !reflect.DeepEqual(body, want) {
				t.Errorf("body %v, want %v", body, want)
			}
			for k, v := range map[string]string{
				"Authorization":        testToken,
				"Content-Type":         "application/json",
				"Origin":               "https://discord.com",
				"Referer":              "https://discord.com/channels/@me/" + dmID,
				"X-Context-Properties": "eyJsb2NhdGlvbiI6ImNoYXRfaW5wdXQifQ==",
				"X-Installation-Id":    testInstallation,
			} {
				if got := hit.header.Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			for k, v := range map[string]string{
				"Authorization": testToken, "Origin": "https://discord.com", "Content-Length": "0",
				"Content-Type": "", "X-Context-Properties": "", "Referer": "https://discord.com/channels/@me/" + dmID,
			} {
				if got := typing.header.Get(k); got != v {
					t.Errorf("typing %s = %q, want %q", k, got, v)
				}
			}
			if res.Ref != sentID {
				t.Errorf("ref %q", res.Ref)
			}
			if row := h.queryRow(`SELECT container_id, sender_id, is_from_me, body FROM messages WHERE id = ?`, sentID); row[0] != dmID ||
				row[1] != selfUser || row[2] != int64(1) || row[3] != sentText {
				t.Errorf("stored %v", row)
			}
		})
	}
}

// A typing indicator that cannot go out yet holds the message too, so the
// send never skips the pacing gap.
func TestSendWaitsForTypingSlot(t *testing.T) {
	h := newSendHarness(t, func(s *state) { s.CooldownUntil = noon.Add(time.Hour) })
	var p posts
	p.handle(h, dmID, sentMessage)
	h.start()
	res := h.w.Execute(context.Background(), sendRecord(dmID))
	if res.Status != connector.Failed || !strings.Contains(res.Detail, "cooldown") {
		t.Errorf("result %+v", res)
	}
	if n := len(h.discord.all()); n != 0 {
		t.Errorf("%d requests", n)
	}
}

func TestSendRefusedWhileStoppedOrUnlinked(t *testing.T) {
	h := newSendHarness(t, nil)
	var p posts
	p.handle(h, dmID, func(w http.ResponseWriter) {
		connector.WriteJSON(w, http.StatusBadRequest, map[string]any{"message": "verify", "code": 40002})
	})
	h.start()
	ctx := context.Background()
	if res := h.w.Execute(ctx, sendRecord(dmID)); res.Status != connector.Failed {
		t.Fatalf("first send %+v, want failed", res)
	}
	// The account needs the owner now, so nothing more goes out.
	if res := h.w.Execute(ctx, sendRecord(dmID)); res.Status != connector.Failed || !strings.Contains(res.Detail, "verification") {
		t.Errorf("send while stopped %+v", res)
	}
	if _, err := h.client.Prepare(ctx, "send", sendRecord(dmID).Args); errStatus(err) != http.StatusServiceUnavailable {
		t.Errorf("prepare while stopped: %v", err)
	}
	if err := h.client.Unlink(ctx); err != nil {
		t.Fatal(err)
	}
	if res := h.w.Execute(ctx, sendRecord(dmID)); res.Status != connector.Failed || !strings.Contains(res.Detail, "not linked") {
		t.Errorf("send while unlinked %+v", res)
	}
	if _, err := h.client.Prepare(ctx, "send", sendRecord(dmID).Args); errStatus(err) != http.StatusServiceUnavailable {
		t.Errorf("prepare while unlinked: %v", err)
	}
	if n := len(p.all()); n != 1 {
		t.Errorf("%d POSTs, want 1", n)
	}
}

func TestApprovedSendRunsOnce(t *testing.T) {
	const rpID, origin = "box.example.ts.net", "https://box.example.ts.net:8781"
	h := newSendHarness(t, nil)
	var p posts
	p.handle(h, dmID, sentMessage)
	key := approvaltest.New(t, rpID, origin)
	h.keys = approvaltest.Keys(t, key)
	seen, err := approval.OpenSeen(filepath.Join(t.TempDir(), "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.seen = seen
	h.start()
	ctx := context.Background()

	pr, err := h.client.Prepare(ctx, "send", map[string]string{"chat": dmID, "text": sentText})
	if err != nil {
		t.Fatal(err)
	}
	approve := func(op string, args map[string]string, display []approval.Field) approval.Approval {
		b, err := approval.Encode(approval.Record{
			ID: "p-" + op, Connector: "discord", Op: op, Args: args, Display: display,
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		return key.Approve(b, time.Now())
	}
	a := approve("send", pr.Args, pr.Display)
	res, err := h.client.Execute(ctx, a)
	if err != nil || res.Status != connector.Done || res.Ref != sentID {
		t.Fatalf("execute: %+v %v", res, err)
	}
	if _, err := h.client.Execute(ctx, a); errStatus(err) != http.StatusConflict {
		t.Fatalf("replay: %v", err)
	}
	// A signed approval for a read op is no way to make a write.
	if _, err := h.client.Execute(ctx, approve("fetch", map[string]string{"channel_id": dmID}, nil)); errStatus(err) != http.StatusForbidden {
		t.Errorf("approved read: %v", err)
	}
	if n := len(p.all()); n != 1 || len(h.discord.all()) != 2 {
		t.Errorf("upstream %v, want typing and one POST", h.discord.paths())
	}
}
