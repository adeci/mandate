package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func TestMedia(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.link()

	// encryptedImage encrypts data as a client would upload it to url.
	encryptedImage := func(eventID, url string, data []byte, ts int64) map[string]any {
		ef := attachment.NewEncryptedFile()
		f.hs.mu.Lock()
		ef.EncryptInPlace(data)
		f.hs.media[url] = data
		f.hs.mu.Unlock()
		evt, _ := f.encryptContent(f.element, vaultRoom, eventID, &event.MessageEventContent{
			MsgType: event.MsgImage, Body: "plan.png",
			Info: &event.FileInfo{MimeType: "image/png", Size: len(data)},
			File: &event.EncryptedFileInfo{EncryptedFile: *ef, URL: id.ContentURIString(url)},
		}, ts)
		return evt
	}
	f.hs.mu.Lock()
	f.hs.media["mxc://example.org/beach"] = []byte("plain bytes")
	f.hs.mu.Unlock()
	secret := encryptedImage("$secret", "mxc://example.org/secret", []byte("secret bytes"), 2000)
	tampered := encryptedImage("$tampered", "mxc://example.org/tampered", []byte("original"), 3000)
	f.hs.mu.Lock()
	f.hs.media["mxc://example.org/tampered"][0] ^= 1
	f.hs.mu.Unlock()
	// A message stored before mandate kept references, which only the
	// room's history still has.
	old := encryptedImage("$old", "mxc://example.org/old", []byte("old bytes"), 1000)
	f.hs.mu.Lock()
	f.hs.history[vaultRoom] = []map[string]any{old}
	f.hs.mu.Unlock()
	if err := f.st.UpsertMessage(ctx, store.Message{ID: "$old", ContainerID: vaultRoom.String(), SentAt: time.UnixMilli(1000), MediaType: "image"}); err != nil {
		t.Fatal(err)
	}

	n := f.hs.push(joined(map[id.RoomID]map[string]any{
		dmRoom: room([]map[string]any{member(owner, "Owner"), member(bob, "Bob")}, []map[string]any{
			msgEvent("$beach", bob, 1000, map[string]any{
				"msgtype": "m.image", "body": "beach.jpg", "url": "mxc://example.org/beach",
				"info": map[string]any{"mimetype": "image/jpeg", "size": 11},
			}),
		}),
		vaultRoom: room([]map[string]any{
			stateEvent("m.room.encryption", "", owner, map[string]any{"algorithm": "m.megolm.v1.aes-sha2"}),
			member(owner, "Owner"),
		}, []map[string]any{secret, tampered}),
	}))
	f.hs.waitSynced(n)

	for msg, want := range map[string]store.Media{
		"$beach":  {Type: "image/jpeg", Name: "beach.jpg", Size: 11},
		"$secret": {Type: "image/png", Name: "plan.png", Size: 12},
	} {
		got, err := f.st.Media(ctx, msg, 0)
		if err != nil {
			t.Fatalf("%s: %v", msg, err)
		}
		var ref mediaRef
		if err := json.Unmarshal(got.Ref, &ref); err != nil {
			t.Fatal(err)
		}
		if got.Type != want.Type || got.Name != want.Name || got.Size != want.Size {
			t.Errorf("%s = %+v, want %+v", msg, got, want)
		}
		if msg == "$beach" && ref.URL != "mxc://example.org/beach" || msg == "$secret" && (ref.File == nil || ref.File.Key.Key == "") {
			t.Errorf("%s ref = %s", msg, got.Ref)
		}
	}

	for msg, want := range map[string]string{"$beach": "plain bytes", "$secret": "secret bytes", "$old": "old bytes"} {
		file, err := f.c.Media(ctx, msg, 0)
		if err != nil {
			t.Fatalf("%s: %v", msg, err)
		}
		b, _ := io.ReadAll(file.Body)
		file.Body.Close()
		if string(b) != want || file.Size != int64(len(want)) {
			t.Errorf("%s = %q (size %d), want %q", msg, b, file.Size, want)
		}
	}

	_, err := f.c.Media(ctx, "$tampered", 0)
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Status != http.StatusBadGateway {
		t.Fatalf("tampered file: %v", err)
	}
	if _, err := f.c.Media(ctx, "$beach", 1); !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
		t.Fatalf("second file: %v", err)
	}
}
