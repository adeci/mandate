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
