package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/adeci/sieve/internal/discordapi"
)

// discordCallTimeout bounds one proxied call. The Discord service holds a call
// for at most 90 s waiting for its paced slot, then answers 429.
const discordCallTimeout = 110 * time.Second

// Every target guild/channel must already be visible through the filtered
// views, so hidden containers never cause an upstream request.
type discordProxy struct {
	h      *handler
	client *http.Client
}

func newDiscordProxy(h *handler, socket string) *discordProxy {
	return &discordProxy{
		h: h,
		client: &http.Client{
			Timeout: discordCallTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

func (p *discordProxy) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/discord/sync", p.h.authed(p.sync))
	mux.HandleFunc("POST /v1/discord/search", p.h.authed(p.search))
	mux.HandleFunc("POST /v1/discord/fetch", p.h.authed(p.fetch))
}

func (p *discordProxy) sync(w http.ResponseWriter, r *http.Request) {
	var req discordapi.SyncRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.GuildID != "" && !p.visible(w, req.GuildID) {
		return
	}
	var resp discordapi.SyncResponse
	if p.call(w, r, discordapi.PathSync, req, &resp) {
		writeJSON(w, http.StatusOK, resp)
	}
}

func (p *discordProxy) search(w http.ResponseWriter, r *http.Request) {
	var req discordapi.SearchRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !p.visible(w, req.GuildID) || (req.ChannelID != "" && !p.visible(w, req.ChannelID)) {
		return
	}
	var resp discordapi.MessagesResponse
	if p.call(w, r, discordapi.PathSearch, req, &resp) {
		p.answer(w, r, resp)
	}
}

func (p *discordProxy) fetch(w http.ResponseWriter, r *http.Request) {
	var req discordapi.FetchRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !p.visible(w, req.ChannelID) {
		return
	}
	var resp discordapi.MessagesResponse
	if p.call(w, r, discordapi.PathFetch, req, &resp) {
		p.answer(w, r, resp)
	}
}

// Unknown and hidden look the same to the client.
func (p *discordProxy) visible(w http.ResponseWriter, id string) bool {
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing id")
		return false
	}
	q, err := p.h.open()
	if err != nil {
		slog.Error("open query connection", "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable")
		return false
	}
	defer q.close()
	res, err := q.run(context.Background(), `SELECT 1 FROM discord_containers WHERE id = ?1`, []any{id}, 1)
	if err == nil && len(res.Rows) == 1 {
		return true
	}
	writeError(w, http.StatusNotFound,
		fmt.Sprintf("discord container %s is unknown or hidden; POST /v1/discord/sync (with guild_id for its channels) and check discord_containers", id))
	return false
}

func (p *discordProxy) call(w http.ResponseWriter, r *http.Request, path string, body, out any) bool {
	start := time.Now()
	payload, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode request")
		return false
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://discord"+path, bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "build request")
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		slog.Error("discord service", "path", path, "err", err)
		writeError(w, http.StatusServiceUnavailable, "discord service unavailable")
		return false
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	slog.Info("discord", "remote", r.RemoteAddr, "path", path, "status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds())
	if err != nil {
		writeError(w, http.StatusBadGateway, "read discord service response")
		return false
	}
	if resp.StatusCode/100 != 2 {
		var e discordapi.ErrorResponse
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		writeJSON(w, resp.StatusCode, e)
		return false
	}
	if err := json.Unmarshal(data, out); err != nil {
		writeError(w, http.StatusBadGateway, "decode discord service response")
		return false
	}
	return true
}

// Hidden senders and containers drop out; auth messages come back with a NULL
// body, as with /v1/query.
func (p *discordProxy) answer(w http.ResponseWriter, r *http.Request, resp discordapi.MessagesResponse) {
	out := struct {
		*queryResult
		TotalResults int `json:"total_results,omitempty"`
	}{queryResult: &queryResult{Columns: []string{}, Rows: [][]any{}}, TotalResults: resp.TotalResults}
	if len(resp.MessageIDs) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	q, err := p.h.open()
	if err != nil {
		slog.Error("open query connection", "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable")
		return
	}
	defer q.close()

	params := make([]any, len(resp.MessageIDs))
	for i, id := range resp.MessageIDs {
		params[i] = id
	}
	sql := `SELECT * FROM discord_messages WHERE id IN (?` + strings.Repeat(",?", len(params)-1) + `)`
	ctx, cancel := context.WithTimeout(r.Context(), p.h.cfg.Timeout)
	defer cancel()
	res, err := q.run(ctx, sql, params, len(params))
	if err != nil {
		slog.Error("read back discord messages", "err", err)
		writeError(w, http.StatusInternalServerError, "read back messages")
		return
	}
	order := make(map[string]int, len(resp.MessageIDs))
	for i, id := range resp.MessageIDs {
		order[id] = i
	}
	idCol := -1
	for i, c := range res.Columns {
		if c == "id" {
			idCol = i
		}
	}
	if idCol < 0 {
		writeError(w, http.StatusInternalServerError, "view has no id column")
		return
	}
	slices.SortFunc(res.Rows, func(a, b []any) int {
		return order[a[idCol].(string)] - order[b[idCol].(string)]
	})
	out.queryResult = res
	writeJSON(w, http.StatusOK, out)
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		}
		return false
	}
	return true
}
