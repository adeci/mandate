package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	"google.golang.org/protobuf/proto"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// encryptMedia encrypts plain as a sender's WhatsApp app uploads it.
func encryptMedia(t *testing.T, plain, mediaKey []byte, kind whatsmeow.MediaType) []byte {
	t.Helper()
	keys := hkdfutil.SHA256(mediaKey, nil, []byte(kind), 112)
	iv, cipherKey, macKey := keys[:16], keys[16:48], keys[48:80]
	ciphertext, err := cbcutil.Encrypt(cipherKey, iv, plain)
	if err != nil {
		t.Fatal(err)
	}
	h := hmac.New(sha256.New, macKey)
	h.Write(iv)
	h.Write(ciphertext)
	return append(ciphertext, h.Sum(nil)[:10]...)
}

func TestMediaFetchesWhatIngestRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	plain := []byte("\xff\xd8\xff a photo of the crag")
	mediaKey := []byte(strings.Repeat("k", 32))
	blob := encryptMedia(t, plain, mediaKey, whatsmeow.MediaDocument)
	encSum, sum := sha256.Sum256(blob), sha256.Sum256(plain)
	f.cli.blobs = map[string][]byte{"/v/t62.7119-24/topo": blob}

	info := dmInfo("D1")
	info.Chat, info.Sender = farid, farid
	o, ok := convert(&events.Message{Info: info, Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Mimetype: proto.String("image/jpeg"), FileName: proto.String("topo.jpg"), Caption: proto.String("route"),
		DirectPath: proto.String("/v/t62.7119-24/topo"), MediaKey: mediaKey,
		FileSHA256: sum[:], FileEncSHA256: encSum[:], FileLength: proto.Uint64(uint64(len(plain))),
	}}}, func(j types.JID) types.JID { return j })
	if !ok {
		t.Fatal("document not converted")
	}
	if err := f.st.UpsertMessage(ctx, o.msg); err != nil {
		t.Fatal(err)
	}
	file, err := f.c.Media(ctx, o.msg.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file.Body)
	if err != nil || string(got) != string(plain) || file.Size != int64(len(plain)) || file.Type != "image/jpeg" || file.Name != "topo.jpg" {
		t.Errorf("file = %+v %q %v, want the decrypted photo", file, got, err)
	}

	// Stored before media was recorded, or expired on WhatsApp's servers.
	old := store.Message{ID: messageID(farid, "OLD"), ContainerID: farid.String(), SentAt: ts, MediaType: "image"}
	gone := store.Message{ID: messageID(farid, "GONE"), ContainerID: farid.String(), SentAt: ts, Media: []store.Media{{
		Type: "image/jpeg", Ref: []byte(`{"kind":"WhatsApp Image Keys","direct_path":"/v/gone"}`),
	}}}
	for want, m := range map[string]store.Message{"not available": old, "no longer": gone} {
		if err := f.st.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		var e *connector.Error
		if _, err := f.c.Media(ctx, m.ID, 0); !errors.As(err, &e) || e.Status != http.StatusNotFound || !strings.Contains(e.Message, want) {
			t.Errorf("%s: %v, want 404 saying %q", m.ID, err, want)
		}
	}
}
