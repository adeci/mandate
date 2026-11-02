package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/web"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func TestMediaFetchesWhatIngestRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	plain := []byte("\xff\xd8\xff a photo of the crag")
	key := bytes.Repeat([]byte("k"), 64)
	f.c.download = func(_ context.Context, p *signalpb.AttachmentPointer, into *os.File) error {
		if p.GetCdnKey() == "gone" {
			return signalmeow.ErrAttachmentNotFound
		}
		if p.GetCdnNumber() != 3 || p.GetCdnKey() != "topo" || !bytes.Equal(p.GetKey(), key) || string(p.GetDigest()) != "digest" {
			return errors.New("unexpected pointer " + p.String())
		}
		_, err := into.Write(plain)
		return err
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

// signalmeow indexes its CDN host list with the pointer's CDN number.
func TestUnknownCDN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.c.download = func(context.Context, *signalpb.AttachmentPointer, *os.File) error {
		t.Error("downloaded from an unknown CDN")
		return nil
	}
	p := &signalpb.AttachmentPointer{
		AttachmentIdentifier: &signalpb.AttachmentPointer_CdnKey{CdnKey: "x"}, CdnNumber: new(uint32(len(web.CDNHosts))),
		Key: bytes.Repeat([]byte("k"), 64), Digest: []byte("digest"),
	}
	if _, ok := attachment(p); ok {
		t.Error("recorded an attachment on an unknown CDN")
	}
	dm := farid.String()
	m := store.Message{ID: messageID(dm, farid, 1), ContainerID: dm, SentAt: time.UnixMilli(1798000000000).UTC(), Media: []store.Media{{
		Type: "image/jpeg", Ref: []byte(`{"cdn_number":4,"cdn_key":"x"}`),
	}}}
	if err := f.st.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	var cerr *connector.Error
	if _, err := f.c.Media(ctx, m.ID, 0); !errors.As(err, &cerr) || cerr.Status != http.StatusBadGateway {
		t.Errorf("Media = %v, want 502", err)
	}
}

// Signal's CDN states a length, and signalmeow allocates it and reads the
// body whole; both stop at the limit. Not parallel: it replaces
// signalmeow's HTTP transport.
func TestMediaStopsAtLimit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.c.download = download
	var served atomic.Int64
	chunked := false
	orig := web.SignalHTTPClient.Transport
	t.Cleanup(func() { web.SignalHTTPClient.Transport = orig })
	web.SignalHTTPClient.Transport = capMedia{roundTrip(func(req *http.Request) (*http.Response, error) {
		const size = 4 << 20
		body := io.NopCloser(io.TeeReader(io.LimitReader(zeros{}, size), writerFunc(func(p []byte) { served.Add(int64(len(p))) })))
		resp := &http.Response{StatusCode: http.StatusOK, Body: body, Header: http.Header{}, Request: req, ContentLength: size}
		if chunked {
			resp.ContentLength = -1
		}
		return resp, nil
	})}
	dm := farid.String()
	m := store.Message{ID: messageID(dm, farid, 1), ContainerID: dm, SentAt: time.UnixMilli(1798000000000).UTC(), Media: []store.Media{{
		Type: "application/pdf", Size: 1, Ref: []byte(`{"cdn_number":2,"cdn_key":"big","key":"` +
			base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 64)) + `","digest":"ZGlnZXN0","size":1}`),
	}}}
	if err := f.st.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	const limit = 64 << 10
	for _, chunked = range []bool{false, true} {
		served.Store(0)
		var cerr *connector.Error
		if _, err := f.c.Media(connector.WithMediaLimit(ctx, limit), m.ID, 0); !errors.As(err, &cerr) || cerr.Status != http.StatusRequestEntityTooLarge {
			t.Errorf("chunked %v: Media = %v, want 413", chunked, err)
		}
		if n := served.Load(); n > 2*limit {
			t.Errorf("chunked %v: read %d bytes with a %d byte limit", chunked, n, limit)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
