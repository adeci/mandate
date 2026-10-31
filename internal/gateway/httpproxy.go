package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	maxForwardRequest  = 1 << 20
	maxForwardResponse = 10 << 20
)

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

// forward passes only method, path, query, body, Content-Type and Accept. The
// agent's Authorization header carries the gateway token and must not reach
// the proxy, which adds the service's own credentials.
func (p *httpProxy) forward(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/http/")
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.ContentLength > maxForwardRequest {
		writeError(w, http.StatusRequestEntityTooLarge, "request too large")
		return
	}
	target := "http://proxy/forward/" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	var body io.Reader
	if r.ContentLength != 0 {
		body = http.MaxBytesReader(w, r.Body, maxForwardRequest)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	req.ContentLength = max(r.ContentLength, -1)
	for _, k := range []string{"Content-Type", "Accept"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := p.client.Do(req)
	attrs := []any{"remote", r.RemoteAddr, "method", r.Method, "service", r.PathValue("service")}
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		slog.Error("http proxy", append(attrs, "err", err)...)
		writeError(w, http.StatusServiceUnavailable, "http proxy is unavailable")
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
