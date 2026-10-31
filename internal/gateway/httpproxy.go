package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

const maxForwardResponse = 10 << 20

type httpProxy struct {
	client *http.Client
}

func newHTTPProxy(socket string) *httpProxy {
	return &httpProxy{client: &http.Client{
		Timeout: readTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
		// The proxy answers; redirects are relayed, never followed here.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// forward is the read passthrough: GET and HEAD with path, query and Accept.
// Anything else changes something and must go through a proposal. The agent's
// Authorization header carries the gateway token and must not reach the
// proxy, which adds the service's own credentials.
func (p *httpProxy) forward(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	service := r.PathValue("service")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf(
			"/v1/http only reads (GET, HEAD); a %s needs the owner's approval: run mandate http %s %s PATH --data BODY, which proposes it",
			r.Method, r.Method, service))
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/http/")
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	target := "http://proxy/forward/" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if v := r.Header.Get("Accept"); v != "" {
		req.Header.Set("Accept", v)
	}
	resp, err := p.client.Do(req)
	attrs := []any{"remote", r.RemoteAddr, "method", r.Method, "service", service}
	if err != nil {
		slog.Error("http proxy", append(attrs, "err", err)...)
		writeError(w, http.StatusServiceUnavailable, unavailable("http proxy"))
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxForwardResponse+1))
	slog.Info("http", append(attrs, "status", resp.StatusCode, "bytes", len(data), "duration_ms", time.Since(start).Milliseconds())...)
	if err != nil {
		writeError(w, http.StatusBadGateway, "read http proxy response")
		return
	}
	if len(data) > maxForwardResponse {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("response exceeds %d MB", maxForwardResponse>>20))
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(data)
}
