package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func TestMediaFetchesWhatIngestRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	plain := []byte("\xff\xd8\xff a photo of the crag")
	key := bytes.Repeat([]byte("k"), 64)
	f.c.download = func(_ context.Context, p *signalpb.AttachmentPointer) ([]byte, error) {
		if p.GetCdnKey() == "gone" {
			return nil, signalmeow.ErrAttachmentNotFound
		}
		if p.GetCdnNumber() != 3 || p.GetCdnKey() != "topo" || !bytes.Equal(p.GetKey(), key) || string(p.GetDigest()) != "digest" {
			return nil, errors.New("unexpected pointer " + p.String())
		}
		return plain, nil
	}

	const ts = 1798000000000
	dm := farid.String()
	in := newIngester(f.cli(), f.c.dev, f.st, f.sess, discard)
	if !in.handle(ctx, &events.ChatEvent{Info: events.MessageInfo{Sender: farid, ChatID: dm}, Event: &signalpb.DataMessage{
		Body: new("route"), Timestamp: new(uint64(ts)),
		Attachments: []*signalpb.AttachmentPointer{{
			AttachmentIdentifier: &signalpb.AttachmentPointer_CdnKey{CdnKey: "topo"}, CdnNumber: new(uint32(3)),
			Key: key, Digest: []byte("digest"), Size: new(uint32(len(plain))),
			ContentType: new("image/jpeg"), FileName: new("topo.jpg"),
		}},
	}}) {
		t.Fatal("handle failed")
	}
	id := messageID(dm, farid, ts)
	file, err := f.c.Media(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file.Body)
	if err != nil || !bytes.Equal(got, plain) || file.Size != int64(len(plain)) || file.Type != "image/jpeg" || file.Name != "topo.jpg" {
		t.Errorf("file = %+v %q %v, want the decrypted photo", file, got, err)
	}

	// Stored before media was recorded, or expired on Signal's CDN.
	at := time.UnixMilli(ts).UTC()
	old := store.Message{ID: messageID(dm, farid, 1), ContainerID: dm, SentAt: at, MediaType: "image"}
	gone := store.Message{ID: messageID(dm, farid, 2), ContainerID: dm, SentAt: at, Media: []store.Media{{
		Type: "image/jpeg", Ref: []byte(`{"cdn_number":3,"cdn_key":"gone"}`),
	}}}
	for want, m := range map[string]store.Message{"not available": old, "no longer": gone} {
		if err := f.st.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		_, err := f.c.Media(ctx, m.ID, 0)
		var cerr *connector.Error
		if !errors.As(err, &cerr) || cerr.Status != http.StatusNotFound || !strings.Contains(cerr.Message, want) {
			t.Errorf("Media(%s) = %v, want 404 %q", m.ID, err, want)
		}
	}
}
