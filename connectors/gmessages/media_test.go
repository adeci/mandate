package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func fetch(t *testing.T, c *gmConnector, message string, index int) (connector.MediaFile, string, error) {
	t.Helper()
	f, err := c.Media(context.Background(), message, index)
	if err != nil {
		return f, "", err
	}
	defer f.Body.Close()
	body, err := io.ReadAll(f.Body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != f.Size {
		t.Fatalf("%s #%d: %d bytes, size %d", message, index, len(body), f.Size)
	}
	return f, string(body), nil
}

func wantRefused(t *testing.T, err error, status int, says string) {
	t.Helper()
	var e *connector.Error
	if !errors.As(err, &e) || e.Status != status || e.Message != says {
		t.Fatalf("err = %v, want %d %q", err, status, says)
	}
}

func TestMedia(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, true)
	f.activate()
	cli := f.cli()
	cli.emit(faridDM())

	photo := withMedia(message("m8", "1", "11", incoming, ts(8), "look"), gmproto.MediaFormats_IMAGE_JPEG, "md1", "IMG_1.jpg")
	mms := message("m10", "1", "11", gmproto.MessageStatusType_INCOMING_YET_TO_MANUAL_DOWNLOAD, ts(10), "")
	downloaded := withMedia(message("m10", "1", "11", incoming, ts(10), ""), gmproto.MediaFormats_VIDEO_MP4, "md10", "")
	old := withMedia(message("m11", "1", "11", incoming, ts(11), ""), gmproto.MediaFormats_IMAGE_PNG, "md11", "")
	f.phone.set(func(p *phone) {
		p.files["md1"], p.files["md10"], p.files["md11"] = []byte("jpeg bytes"), []byte("mp4"), []byte("png!")
		p.msgs["1"] = []*gmproto.Message{photo, mms, old}
	})

	// Ingest records what fetching needs.
	cli.emit(&libgm.WrappedMessage{Message: photo})
	item, err := f.st.Media(ctx, "m8", 0)
	if err != nil || item.Type != "image/jpeg" || item.Name != "IMG_1.jpg" || item.Size != 1234 || len(item.Ref) == 0 {
		t.Fatalf("stored media %+v, %v", item, err)
	}
	file, body, err := fetch(t, f.c, "m8", 0)
	if err != nil || body != "jpeg bytes" || file.Type != "image/jpeg" || file.Name != "IMG_1.jpg" {
		t.Fatalf("fetched %+v %q, %v", file, body, err)
	}
	_, _, err = fetch(t, f.c, "m8", 1)
	wantRefused(t, err, http.StatusNotFound, noFile)

	// An MMS the phone has not downloaded has nothing to fetch until it has.
	cli.emit(&libgm.WrappedMessage{Message: mms})
	_, _, err = fetch(t, f.c, "m10", 0)
	wantRefused(t, err, http.StatusNotFound, notDownloaded)
	f.phone.set(func(p *phone) { p.msgs["1"] = []*gmproto.Message{photo, downloaded, old} })
	if file, body, err := fetch(t, f.c, "m10", 0); err != nil || body != "mp4" || file.Type != "video/mp4" {
		t.Fatalf("fetched %+v %q, %v", file, body, err)
	}
	if item, err := f.st.Media(ctx, "m10", 0); err != nil || item.Type != "video/mp4" {
		t.Fatalf("the phone's answer was not stored: %+v, %v", item, err)
	}

	// A message stored before files were recorded is looked up again.
	for _, id := range []string{"m11", "m12"} {
		if err := f.st.UpsertMessage(ctx, store.Message{ID: id, ContainerID: "1", SentAt: time.UnixMicro(ts(11)), MediaType: "image"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, body, err := fetch(t, f.c, "m11", 0); err != nil || body != "png!" {
		t.Fatalf("old message: %q, %v", body, err)
	}
	_, _, err = fetch(t, f.c, "m12", 0)
	wantRefused(t, err, http.StatusNotFound, noFile)

	// Stored files come from Google's servers without the phone; lookups
	// need it.
	cli.emit(&events.PhoneNotResponding{})
	if _, body, err := fetch(t, f.c, "m8", 0); err != nil || body != "jpeg bytes" {
		t.Fatalf("with the phone off: %q, %v", body, err)
	}
	_, _, err = fetch(t, f.c, "m12", 0)
	wantStatus(t, err, http.StatusServiceUnavailable)

	if err := f.c.Unlink(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, err = fetch(t, f.c, "m8", 0)
	wantStatus(t, err, http.StatusServiceUnavailable)
}

// A JPEG's first bytes, so the preview's type can be told from them.
const jpeg = "\xff\xd8\xff\xe0\x00\x10JFIF\x00"

func mediaMessage(id string, at int64, mc *gmproto.MediaContent) *gmproto.Message {
	m := message(id, "1", "11", incoming, at, "")
	m.MessageInfo = []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MediaContent{MediaContent: mc}}}
	return m
}

func TestMediaWithoutFileID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, true)
	f.activate()
	cli := f.cli()
	cli.emit(faridDM())

	// Inline data is the file only when it is the whole of it.
	whole := mediaMessage("m20", ts(20), &gmproto.MediaContent{Format: gmproto.MediaFormats_TEXT_VCARD, MediaName: "193", Size: 11, MediaData: []byte("BEGIN:VCARD")})
	preview := mediaMessage("m21", ts(21), &gmproto.MediaContent{Format: gmproto.MediaFormats_IMAGE_JPEG, Size: 185505, MediaData: []byte(jpeg)})
	thumb := mediaMessage("m22", ts(22), &gmproto.MediaContent{
		Format: gmproto.MediaFormats_VIDEO_MP4, Size: 9000, MediaName: "clip.mp4",
		ThumbnailMediaID: "th22", ThumbnailDecryptionKey: []byte("key-th22"),
	})
	f.phone.set(func(p *phone) {
		p.files["th22"] = []byte(jpeg + "thumbnail")
		p.msgs["1"] = []*gmproto.Message{whole, preview, thumb}
	})
	for _, m := range []*gmproto.Message{whole, preview, thumb} {
		cli.emit(&libgm.WrappedMessage{Message: m})
	}
	if item, err := f.st.Media(ctx, "m20", 0); err != nil || item.Name != "" || item.Type != "text/vcard" {
		t.Fatalf("a part number kept as the name: %+v, %v", item, err)
	}
	if _, body, err := fetch(t, f.c, "m20", 0); err != nil || body != "BEGIN:VCARD" {
		t.Fatalf("whole inline file: %q, %v", body, err)
	}

	// A small preview within the message is not the file, nor is one stored
	// that way before.
	_, _, err := fetch(t, f.c, "m21", 0)
	wantRefused(t, err, http.StatusNotFound, notDownloaded)
	stale := []store.Media{{Type: "image/jpeg", Size: 185505, Ref: []byte(`{"data":"/9j/4AAQ"}`)}}
	if err := f.st.UpsertMessage(ctx, store.Message{ID: "m21", ContainerID: "1", SentAt: time.UnixMicro(ts(21)), Media: stale}); err != nil {
		t.Fatal(err)
	}
	_, _, err = fetch(t, f.c, "m21", 0)
	wantRefused(t, err, http.StatusNotFound, notDownloaded)

	// Once the phone has uploaded the file, that is what is served.
	uploaded := mediaMessage("m21", ts(21), &gmproto.MediaContent{
		Format: gmproto.MediaFormats_IMAGE_JPEG, Size: 185505, MediaID: "md21", DecryptionKey: []byte("key-md21"), MediaData: []byte(jpeg),
	})
	f.phone.set(func(p *phone) {
		p.files["md21"] = []byte(jpeg + "the whole photo")
		p.msgs["1"] = []*gmproto.Message{whole, uploaded, thumb}
	})
	if file, body, err := fetch(t, f.c, "m21", 0); err != nil || body != jpeg+"the whole photo" || file.Name != "" {
		t.Fatalf("uploaded file: %+v %q, %v", file, body, err)
	}

	// Without the file, a preview is served as one.
	file, body, err := fetch(t, f.c, "m22", 0)
	if err != nil || body != jpeg+"thumbnail" || file.Type != "image/jpeg" || file.Name != "preview" {
		t.Fatalf("preview: %+v %q, %v", file, body, err)
	}
}
