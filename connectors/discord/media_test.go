package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	msgFile  = "1300000000000000010"
	attPlan  = "1400000000000000001"
	attPhoto = "1400000000000000002"
	planPath = "/attachments/" + dmID + "/" + attPlan + "/plan.pdf"
	plan     = "%PDF-1.7 rig plan"
)

func attachment(id, name, ct string, size int, query string) map[string]any {
	return map[string]any{
		"id": id, "filename": name, "content_type": ct, "size": size,
		"url":       "https://cdn.discordapp.com/attachments/" + dmID + "/" + id + "/" + name + query,
		"proxy_url": "https://media.discordapp.net/attachments/" + dmID + "/" + id + "/" + name + query,
	}
}

func fileMessage(attachments ...map[string]any) map[string]any {
	return map[string]any{
		"id": msgFile, "channel_id": dmID, "type": 0, "content": "", "timestamp": "2026-10-28T12:30:00.000000+00:00",
		"author": map[string]any{"id": aliceUser, "username": "alice"}, "attachments": attachments,
	}
}

// serveFile answers the CDN for path while the link carries signature.
func (h *harness) serveFile(path, signature, body string) {
	h.discord.handle(path, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Query().Get("hm") != signature {
			http.Error(w, "This content is no longer available.", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, body)
	})
}

func (h *harness) media(message string, index int) (string, http.Header, error) {
	h.t.Helper()
	resp, err := h.client.Media(context.Background(), message, index)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b), resp.Header, nil
}

func (h *harness) mediaErr(message string, index int, status int) {
	h.t.Helper()
	_, _, err := h.media(message, index)
	var e *connector.Error
	if !errors.As(err, &e) || e.Status != status {
		h.t.Fatalf("media %s #%d: got %v, want status %d", message, index, err, status)
	}
}

func TestMediaRefreshesTheStoredLink(t *testing.T) {
	h := newHarness(t, nil)
	h.discord.reply(api("/channels/"+dmID+"/messages"), 200, []map[string]any{
		fileMessage(attachment(attPlan, "plan.pdf", "application/pdf", len(plan), "?ex=1&is=2&hm=old&")),
	})
	h.start()
	var res connector.ReadResult
	h.callOK("fetch", map[string]string{"channel_id": dmID}, &res)
	row := h.queryRow(`SELECT type, name, size, ref FROM media WHERE message_id = ? AND idx = 0`, msgFile)
	wantRef := `{"channel_id":"` + dmID + `","message_id":"` + msgFile + `","id":"` + attPlan + `","path":"` + planPath + `"}`
	if row[0] != "application/pdf" || row[1] != "plan.pdf" || row[2] != int64(len(plan)) || string(row[3].([]byte)) != wantRef {
		t.Fatalf("media row %v %s", row[:3], row[3])
	}

	h.discord.handle(api(refreshPath), func(w http.ResponseWriter, r *http.Request, _ int) {
		var req struct {
			URLs []string `json:"attachment_urls"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || !slices.Equal(req.URLs, []string{h.srv.URL + planPath}) {
			t.Errorf("refresh body %v", req.URLs)
		}
		connector.WriteJSON(w, 200, map[string]any{"refreshed_urls": []map[string]any{{
			"original": req.URLs[0], "refreshed": "https://cdn.discordapp.com" + planPath + "?ex=3&is=4&hm=new&",
		}}})
	})
	h.serveFile(planPath, "new", plan)
	mark, before := len(h.discord.all()), len(h.state().Requests)
	got, hd, err := h.media(msgFile, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != plan || hd.Get("Content-Type") != "application/pdf" || hd.Get("Content-Length") != "17" ||
		!strings.Contains(hd.Get("Content-Disposition"), "plan.pdf") {
		t.Errorf("file %q, headers %v", got, hd)
	}
	hits := h.discord.all()[mark:]
	if len(hits) != 2 || hits[0].method != http.MethodPost || hits[0].path != api(refreshPath) || hits[1].path != planPath {
		t.Fatalf("upstream requests %v", h.discord.paths()[mark:])
	}
	if ref := hits[0].header.Get("Referer"); ref != "https://discord.com/channels/@me/"+dmID || hits[0].header.Get("Authorization") != testToken {
		t.Errorf("refresh referer %q", ref)
	}
	cdn := hits[1].header
	if cdn.Get("Authorization") != "" || cdn.Get("Sec-Fetch-Dest") != "document" || cdn.Get("User-Agent") != userAgent {
		t.Errorf("download headers %v", cdn)
	}
	if gap := hits[0].at.Sub(h.discord.all()[mark-1].at); gap < gapMin {
		t.Errorf("refresh %v after the fetch", gap)
	}
	if d := hits[1].at.Sub(hits[0].at); d < followMin || d > followMax {
		t.Errorf("download %v after the refresh", d)
	}
	if n := len(h.state().Requests) - before; n != 2 {
		t.Errorf("%d requests counted, want 2", n)
	}

	// Deleted from Discord: the CDN no longer has it.
	h.discord.reply(planPath, http.StatusNotFound, map[string]any{})
	h.mediaErr(msgFile, 0, http.StatusNotFound)
}

func TestMediaOfOldMessagesIsLookedUpAgain(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.st.UpsertContainer(ctx, store.Container{ID: dmID, Kind: kindDM}); err != nil {
		t.Fatal(err)
	}
	sent := time.Date(2026, 10, 28, 12, 30, 0, 0, time.UTC)
	for _, m := range []store.Message{
		{ID: msgFile, ContainerID: dmID, SentAt: sent, MediaType: "image/png"},
		{ID: msg4, ContainerID: dmID, SentAt: sent, MediaType: "sticker"},
	} {
		if err := h.st.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	photoPath := "/attachments/" + dmID + "/" + attPhoto + "/rig.png"
	h.discord.reply(api("/channels/"+dmID+"/messages"), 200, []map[string]any{
		{"id": "1300000000000000011", "channel_id": dmID, "type": 0, "content": "next", "timestamp": "2026-10-28T12:31:00.000000+00:00"},
		fileMessage(
			attachment(attPlan, "plan.pdf", "application/pdf", len(plan), "?ex=1&is=2&hm=a&"),
			attachment(attPhoto, "rig.png", "image/png", 4, "?ex=1&is=2&hm=b&"),
		),
	})
	h.serveFile(photoPath, "b", "\x89PNG")
	h.start()

	got, hd, err := h.media(msgFile, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != "\x89PNG" || hd.Get("Content-Type") != "image/png" || !strings.Contains(hd.Get("Content-Disposition"), "rig.png") {
		t.Errorf("file %q, headers %v", got, hd)
	}
	hits := h.discord.all()
	if len(hits) != 2 || hits[1].path != photoPath {
		t.Fatalf("upstream requests %v", h.discord.paths())
	}
	if q := hits[0].query; q.Get("around") != msgFile || q.Get("limit") != "50" || len(q) != 2 {
		t.Errorf("lookup query %v", q)
	}

	h.mediaErr(msgFile, 2, http.StatusNotFound)
	h.mediaErr(msg4, 0, http.StatusNotFound)
	if n := len(h.discord.all()); n != 3 {
		t.Errorf("%d requests, want 3: a sticker has no file to look up", n)
	}
}
