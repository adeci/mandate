package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

type forwarded struct {
	method, path, query, auth, accept, other string
}

type fakeForwarder struct {
	got    atomic.Pointer[forwarded]
	health atomic.Pointer[connector.Health]
	reply  func(w http.ResponseWriter)
}

func (f *fakeForwarder) Spec() connector.Spec {
	return connector.Spec{Name: "api", Ops: []connector.Op{}}
}

func (f *fakeForwarder) Health(context.Context) connector.Health {
	if h := f.health.Load(); h != nil {
		return *h
	}
	return connector.Health{State: connector.Healthy}
}

func (f *fakeForwarder) Prepare(context.Context, string, map[string]string) (connector.Prepared, error) {
	return connector.Prepared{}, connector.ErrUnknownOp
}

func (f *fakeForwarder) Execute(context.Context, approval.Record) connector.Result {
	return connector.Result{Status: connector.Failed}
}

func (f *fakeForwarder) Forward(w http.ResponseWriter, r *http.Request, path string) {
	f.got.Store(&forwarded{
		method: r.Method, path: path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
		accept: r.Header.Get("Accept"), other: r.Header.Get("X-Forwarded-For"),
	})
	f.reply(w)
}

func forwardRig(t *testing.T, reply func(w http.ResponseWriter)) (*rig, *fakeForwarder) {
	t.Helper()
	f := &fakeForwarder{reply: reply}
	srv := &connector.Server{Connector: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return newRig(t, func(c *Config) { c.Connectors["api"] = serveUnix(t, "api", srv.Handler()) }), f
}

func TestForwardRelaysOnlyAllowedParts(t *testing.T) {
	t.Parallel()
	r, f := forwardRig(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/vnd.github+json")
		w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="next"`)
		w.Header().Set("X-Ratelimit-Remaining", "42")
		w.Header().Set("Set-Cookie", "session=upstream")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/forward/api/github/repos/a/b%2Fc/issues?per_page=5&q=x%20y", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"message":"Not Found"}` ||
		rec.Header().Get("Content-Type") != "application/vnd.github+json" || rec.Header().Get("Set-Cookie") != "" ||
		rec.Header().Get("Link") == "" || rec.Header().Get("X-Ratelimit-Remaining") != "42" {
		t.Errorf("response: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	got := f.got.Load()
	if got == nil {
		t.Fatal("connector not called")
	}
	want := forwarded{
		method: http.MethodGet, path: "github/repos/a/b%2Fc/issues", query: "per_page=5&q=x%20y",
		accept: "application/vnd.github+json",
	}
	if *got != want {
		t.Errorf("forwarded %+v\nwant      %+v", *got, want)
	}
}

func TestForwardRefusesWrites(t *testing.T) {
	t.Parallel()
	r, f := forwardRig(t, func(http.ResponseWriter) {})
	for _, method := range []string{http.MethodPost, http.MethodDelete, "PURGE"} {
		rec := call(r.h, method, "/v1/forward/api/github/graphql", `{"query":"mutation {}"}`)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d %s, want 405", method, rec.Code, rec.Body)
		}
	}
	if f.got.Load() != nil {
		t.Fatal("a write reached the connector")
	}
	if rec := call(r.h, http.MethodHead, "/v1/forward/api/github/user", ""); rec.Code != http.StatusOK || f.got.Load().method != http.MethodHead {
		t.Errorf("HEAD: %d", rec.Code)
	}
}

func TestForwardCapsResponse(t *testing.T) {
	t.Parallel()
	r, _ := forwardRig(t, func(w http.ResponseWriter) { w.Write(make([]byte, maxForwardResponse+1)) })
	if rec := call(r.h, http.MethodGet, "/v1/forward/api/big", ""); rec.Code != http.StatusBadGateway {
		t.Errorf("oversized response: %d", rec.Code)
	}
}

func TestForwardOnlyToConnectorsThatDeclareIt(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	rec := call(r.h, http.MethodGet, "/v1/forward/discord/channels/1", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("connector without Forward: %d %s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/forward/discord/channels/1", nil)
	rec = httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", rec.Code)
	}
}

func TestForwardRefusesUnhealthyConnector(t *testing.T) {
	t.Parallel()
	r, f := forwardRig(t, func(http.ResponseWriter) {})
	f.health.Store(&connector.Health{State: connector.NeedsSetup, Detail: "token revoked"})
	rec := call(r.h, http.MethodGet, "/v1/forward/api/github/user", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("%d %s, want 503", rec.Code, rec.Body)
	}
	if f.got.Load() != nil {
		t.Fatal("unhealthy connector was called")
	}
}
