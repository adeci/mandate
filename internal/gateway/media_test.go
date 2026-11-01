package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

type fakeMedia struct {
	files map[string][]byte // message ID -> content

	mu    sync.Mutex
	asked []string
}

func (f *fakeMedia) Spec() connector.Spec {
	return connector.Spec{Name: "whatsapp", Ops: []connector.Op{}}
}

func (f *fakeMedia) Prepare(context.Context, string, map[string]string) (connector.Prepared, error) {
	return connector.Prepared{}, connector.ErrUnknownOp
}

func (f *fakeMedia) Execute(context.Context, approval.Record) connector.Result {
	return connector.Result{Status: connector.Failed}
}

func (f *fakeMedia) Media(_ context.Context, message string, _ int) (connector.MediaFile, error) {
	f.mu.Lock()
	f.asked = append(f.asked, message)
	f.mu.Unlock()
	b := f.files[message]
	return connector.MediaFile{Type: "image/jpeg", Name: "cat.jpg", Size: int64(len(b)), Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func TestMediaFollowsHideRules(t *testing.T) {
	t.Parallel()
	sources := fixture(t)
	st, err := store.Open(context.Background(), sources["whatsapp"])
	mustDo(t, err)
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	photo := []store.Media{{Type: "image/jpeg", Name: "cat.jpg", Size: 5}}
	for _, m := range []store.Message{
		{ID: "p7", ContainerID: "conv-family", SenderID: "+15550001", Body: "look", Media: photo},
		{ID: "p8", ContainerID: "conv-family", SenderID: "+15550002", Media: photo},
		{ID: "p9", ContainerID: "conv-private", SenderID: "+15550003", Media: photo},
		{ID: "p10", ContainerID: "conv-family", SenderID: "+15550001", Media: []store.Media{{Type: "video/mp4", Size: 2000}}},
		{ID: "p11", ContainerID: "conv-family", SenderID: "+15550001", Media: []store.Media{{Type: "image/png"}, {Type: "application/pdf", Name: "plan.pdf"}}},
	} {
		m.SentAt = at
		mustDo(t, st.UpsertMessage(context.Background(), m))
	}
	mustDo(t, st.Close())

	f := &fakeMedia{files: map[string][]byte{"p7": []byte("jpeg!"), "p11": bytes.Repeat([]byte("x"), 2000)}}
	exposure := filepath.Join(t.TempDir(), "exposure.toml")
	mustDo(t, os.WriteFile(exposure, []byte(testExposure), 0o600))
	srv := &connector.Server{Connector: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := newHandler(Config{
		Sources: sources, ExposureFile: exposure, Timeout: 2 * time.Second, MaxMedia: 1000,
		Connectors: map[string]string{"whatsapp": serveUnix(t, "whatsapp", srv.Handler())},
	}, testToken)

	_, res, _ := getTable(t, h, "/v1/whatsapp/messages?chat=conv-family&limit=3")
	if got, want := column(t, res, "body"), []any{"[#0 image/png] [#1 application/pdf plan.pdf]", "[video/mp4]", "[image/jpeg cat.jpg] look"}; !slices.Equal(got, want) {
		t.Errorf("bodies = %q, want %q", got, want)
	}

	rec := call(h, http.MethodGet, "/v1/whatsapp/media?chat=conv-family&seq=7", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "jpeg!" || rec.Header().Get("Content-Type") != "image/jpeg" ||
		rec.Header().Get("Content-Length") != "5" || rec.Header().Get("Content-Disposition") != "attachment; filename=cat.jpg" {
		t.Errorf("visible photo: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	for query, want := range map[string]int{
		"chat=conv-family&seq=5":          http.StatusForbidden,             // login code
		"chat=conv-family&seq=8":          http.StatusNotFound,              // hidden sender
		"chat=conv-private&seq=9":         http.StatusNotFound,              // hidden chat
		"chat=conv-family&seq=9":          http.StatusNotFound,              // not in that chat
		"chat=conv-family&seq=1":          http.StatusNotFound,              // text only
		"chat=conv-family&seq=7&index=1":  http.StatusNotFound,              // one file
		"chat=conv-family&seq=10":         http.StatusRequestEntityTooLarge, // stored size
		"chat=conv-family&seq=11&index=1": http.StatusRequestEntityTooLarge, // size the connector sent
		"chat=conv-family":                http.StatusBadRequest,
		"chat=conv-family&seq=7&index=-1": http.StatusBadRequest,
	} {
		rec := call(h, http.MethodGet, "/v1/whatsapp/media?"+query, "")
		if rec.Code != want {
			t.Errorf("%s: %d %s, want %d", query, rec.Code, rec.Body, want)
		}
		if want == http.StatusRequestEntityTooLarge && !strings.Contains(rec.Body.String(), "limit") {
			t.Errorf("%s: %s, want the limit explained", query, rec.Body)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.asked, []string{"p7", "p11"}) {
		t.Errorf("connector asked for %v, want only the visible files within the limit or of unknown size", f.asked)
	}
}
