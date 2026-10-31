package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

const maxForwardResponse = 10 << 20

// Headers that pass between agent and connector on a raw read; the rest stay
// behind, above all the agent's Authorization, which carries the gateway
// token. Link and the rate-limit headers let agents page and pace themselves.
var (
	forwardRequestHeaders   = []string{"Accept", "If-None-Match", "If-Modified-Since"}
	forwardResponseHeaders  = []string{"Content-Type", "Link", "Etag", "Last-Modified", "Retry-After", "Location"}
	forwardResponsePrefixes = []string{"X-Ratelimit-", "Ratelimit"}
)

// forward relays a raw read to a connector that declares Forward. Anything
// but GET and HEAD changes something and must go through a proposal.
func (h *handler) forward(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	name := r.PathValue("connector")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf(
			"/v1/forward only reads (GET, HEAD); a %s needs the owner's approval: propose it with mandate propose %s ...", r.Method, name))
		return
	}
	spec, ok := h.spec(w, r, name)
	if !ok {
		return
	}
	if !spec.Forward {
		writeError(w, http.StatusNotFound, fmt.Sprintf("connector %s serves no raw reads; see mandate guide for its ops", name))
		return
	}
	if !h.healthy(w, r, name) {
		return
	}
	target, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/forward/"+name+"/")
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	header := http.Header{}
	for _, k := range forwardRequestHeaders {
		if v := r.Header.Values(k); len(v) > 0 {
			header[k] = v
		}
	}
	resp, err := h.connectors.reads[name].Forward(r.Context(), r.Method, target, header)
	attrs := []any{"remote", r.RemoteAddr, "method", r.Method, "connector", name}
	if err != nil {
		slog.Error("forward", append(attrs, "err", err)...)
		writeError(w, http.StatusServiceUnavailable, unavailable("connector "+name))
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxForwardResponse+1))
	slog.Info("forward", append(attrs, "status", resp.StatusCode, "bytes", len(data), "duration_ms", time.Since(start).Milliseconds())...)
	if err != nil {
		writeError(w, http.StatusBadGateway, "read connector response")
		return
	}
	if len(data) > maxForwardResponse {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("response exceeds %d MB", maxForwardResponse>>20))
		return
	}
	for k, v := range resp.Header {
		if slices.Contains(forwardResponseHeaders, k) || slices.ContainsFunc(forwardResponsePrefixes, func(p string) bool {
			return strings.HasPrefix(k, p)
		}) {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(data)
}
