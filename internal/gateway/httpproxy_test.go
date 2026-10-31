package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type forwarded struct {
	method, path, query, auth, contentType, accept, other, body string
}

func fakeHTTPProxy(t *testing.T, reply func(w http.ResponseWriter)) (string, *atomic.Pointer[forwarded]) {
	t.Helper()
	var got atomic.Pointer[forwarded]
	socket := serveUnix(t, "proxy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.Store(&forwarded{
			method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"),
			accept: r.Header.Get("Accept"), other: r.Header.Get("X-Forwarded-For"), body: string(body),
		})
		reply(w)
	}))
	return socket, &got
}

func TestHTTPPassthrough(t *testing.T) {
	t.Parallel()
	socket, got := fakeHTTPProxy(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/vnd.github+json")
		w.Header().Set("Set-Cookie", "session=upstream")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"number":7}`))
	})
	r := newRig(t, func(c *Config) { c.HTTPProxySocket = socket })

	req := httptest.NewRequest(http.MethodPost, "/v1/http/github/repos/a/b%2Fc/issues?per_page=5&q=x%20y", strings.NewReader(`{"title":"t"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || rec.Body.String() != `{"number":7}` ||
		rec.Header().Get("Content-Type") != "application/vnd.github+json" || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("response: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	f := got.Load()
	if f == nil {
		t.Fatal("proxy not called")
	}
	want := forwarded{
		method: http.MethodPost, path: "/forward/github/repos/a/b%2Fc/issues", query: "per_page=5&q=x%20y",
		contentType: "application/json", accept: "application/vnd.github+json", body: `{"title":"t"}`,
	}
	if *f != want {
		t.Errorf("forwarded %+v\nwant      %+v", *f, want)
	}
}

func TestHTTPPassthroughCapsResponse(t *testing.T) {
	t.Parallel()
	socket, _ := fakeHTTPProxy(t, func(w http.ResponseWriter) {
		w.Write(make([]byte, maxForwardResponse+1))
	})
	r := newRig(t, func(c *Config) { c.HTTPProxySocket = socket })
	if rec := call(r.h, http.MethodGet, "/v1/http/github/big", ""); rec.Code != http.StatusBadGateway {
		t.Errorf("oversized response: %d", rec.Code)
	}
}

func TestHTTPPassthroughAbsentWithoutFlag(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	rec := call(r.h, http.MethodGet, "/v1/http/github/user", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not enabled") {
		t.Errorf("%d %s, want a clear 404", rec.Code, rec.Body)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/http/github/user", nil)
	rec = httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", rec.Code)
	}
}
