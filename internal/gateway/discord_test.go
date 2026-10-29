package gateway

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adeci/sieve/internal/discordapi"
	"github.com/adeci/sieve/internal/store"
)

func fakeDiscord(t *testing.T, handler http.HandlerFunc) (socket string, calls *atomic.Int32) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	calls = &atomic.Int32{}
	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			handler(w, r)
		})},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return socket, calls
}

func discordHandler(t *testing.T, socket string) *handler {
	t.Helper()
	exposure := filepath.Join(t.TempDir(), "exposure.toml")
	mustDo(t, os.WriteFile(exposure, []byte(testExposure), 0o600))
	return newHandler(Config{
		Sources: fixture(t), ExposureFile: exposure, MaxRows: 100,
		Timeout: 2 * time.Second, DiscordSocket: socket,
	}, strings.Repeat("t", 40))
}

func post(h *handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 40))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDiscordProxyRefusesHiddenAndUnknownWithoutUpstreamCall(t *testing.T) {
	t.Parallel()
	socket, calls := fakeDiscord(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, discordapi.MessagesResponse{})
	})
	h := discordHandler(t, socket)

	for name, tc := range map[string]struct{ path, body string }{
		"hidden guild search":                {"/v1/discord/search", `{"guild_id":"g-work","content":"x"}`},
		"channel in hidden guild":            {"/v1/discord/fetch", `{"channel_id":"c-general"}`},
		"unknown channel":                    {"/v1/discord/fetch", `{"channel_id":"999"}`},
		"hidden channel filter in visible g": {"/v1/discord/search", `{"guild_id":"g-fun","channel_id":"c-general"}`},
		"hidden guild sync":                  {"/v1/discord/sync", `{"guild_id":"g-work"}`},
	} {
		rec := post(h, tc.path, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d %s, want 404", name, rec.Code, rec.Body)
		}
	}
	if rec := post(h, "/v1/discord/fetch", `{"channel_id":"c-memes","evil":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: status %d, want 400", rec.Code)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("refused requests reached the Discord service %d times", n)
	}
}

func TestDiscordProxyAnswersThroughFilteredViews(t *testing.T) {
	t.Parallel()
	var sources map[string]string
	socket, calls := fakeDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		var req discordapi.FetchRequest
		mustDo(t, json.NewDecoder(r.Body).Decode(&req))
		if r.URL.Path != discordapi.PathFetch || req.ChannelID != "c-memes" {
			t.Errorf("unexpected call %s %+v", r.URL.Path, req)
		}
		// Simulate the service storing new messages, one of them an OTP and
		// one from a hidden guild's channel (must not leak via the answer).
		st, err := store.Open(r.Context(), sources["discord"])
		mustDo(t, err)
		defer st.Close()
		at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
		mustDo(t, st.UpsertMessage(r.Context(), store.Message{ID: "d3", ContainerID: "c-memes", SenderID: "u2", Body: "Your login code is 482913", SentAt: at}))
		mustDo(t, st.UpsertMessage(r.Context(), store.Message{ID: "d4", ContainerID: "c-memes", SenderID: "u2", Body: "mounting brackets", SentAt: at}))
		writeJSON(w, http.StatusOK, discordapi.MessagesResponse{MessageIDs: []string{"d4", "d1", "d3", "d2"}})
	})
	h := discordHandler(t, socket)
	sources = h.cfg.Sources

	rec := post(h, "/v1/discord/fetch", `{"channel_id":"c-memes","limit":50}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch: %d %s", rec.Code, rec.Body)
	}
	var res queryResult
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &res))
	col := map[string]int{}
	for i, c := range res.Columns {
		col[c] = i
	}
	var ids []string
	for _, row := range res.Rows {
		ids = append(ids, row[col["id"]].(string))
		if row[col["id"]] == "d3" && row[col["body"]] != nil {
			t.Errorf("OTP body served: %v", row[col["body"]])
		}
	}
	if got := strings.Join(ids, ","); got != "d4,d3,d2" {
		t.Errorf("ids = %s, want d4,d3,d2 (service order, hidden-guild message d1 dropped)", got)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestDiscordProxyRelaysPacingRefusal(t *testing.T) {
	t.Parallel()
	socket, _ := fakeDiscord(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusTooManyRequests, discordapi.ErrorResponse{Error: "daily budget used", RetryAfterSeconds: 3600})
	})
	h := discordHandler(t, socket)
	rec := post(h, "/v1/discord/search", `{"guild_id":"g-fun","content":"rig"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	var e discordapi.ErrorResponse
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &e))
	if e.RetryAfterSeconds != 3600 {
		t.Errorf("retry_after_seconds = %d, want 3600", e.RetryAfterSeconds)
	}
}

func TestDiscordEndpointsAbsentWithoutSocket(t *testing.T) {
	t.Parallel()
	h := discordHandler(t, "")
	if rec := post(h, "/v1/discord/search", `{"guild_id":"g-fun"}`); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want endpoint absent", rec.Code)
	}
}
