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
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	})
	r := newRig(t, func(c *Config) { c.HTTPProxySocket = socket })

	req := httptest.NewRequest(http.MethodGet, "/v1/http/github/repos/a/b%2Fc/issues?per_page=5&q=x%20y", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"message":"Not Found"}` ||
		rec.Header().Get("Content-Type") != "application/vnd.github+json" || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("response: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	f := got.Load()
	if f == nil {
		t.Fatal("proxy not called")
	}
	want := forwarded{
		method: http.MethodGet, path: "/forward/github/repos/a/b%2Fc/issues", query: "per_page=5&q=x%20y",
		accept: "application/vnd.github+json",
	}
	if *f != want {
		t.Errorf("forwarded %+v\nwant      %+v", *f, want)
	}
}

func TestHTTPPassthroughRefusesWrites(t *testing.T) {
	t.Parallel()
	socket, got := fakeHTTPProxy(t, func(w http.ResponseWriter) {})
	r := newRig(t, func(c *Config) { c.HTTPProxySocket = socket })
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, "PURGE"} {
		rec := call(r.h, method, "/v1/http/github/graphql", `{"query":"mutation {}"}`)
		if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), "mandate http "+method+" github") {
			t.Errorf("%s: %d %s, want 405 pointing at a proposal", method, rec.Code, rec.Body)
		}
	}
	if got.Load() != nil {
		t.Fatal("a write reached the proxy")
	}
	if rec := call(r.h, http.MethodHead, "/v1/http/github/user", ""); rec.Code != http.StatusOK || got.Load().method != http.MethodHead {
		t.Errorf("HEAD: %d", rec.Code)
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
