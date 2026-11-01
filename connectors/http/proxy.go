package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

const (
	maxResponse = 10 << 20
	maxOutput   = 64 << 10
	minToken    = 16
	maxToken    = 4096
	timeout     = 60 * time.Second
	redacted    = "[redacted]"
)

type Proxy struct {
	services map[string]*service
	names    []string
	secret   func(name string) connector.Secret
	read     *http.Client
	write    *http.Client
	log      *slog.Logger

	// mu guards tokens and serializes Setup and Unlink.
	mu     sync.Mutex
	tokens *tokens
}

// tokens are the tokens in use. Setup and Unlink replace them whole, so a
// request keeps the set it started with.
type tokens struct {
	// values are prefix + token by service name.
	values map[string]string
	// secrets are every token in each form an upstream might echo it.
	secrets [][]byte
}

var errNotLinked = &connector.Error{Status: http.StatusServiceUnavailable, Message: "http is not linked: set the API tokens in the console"}

func New(cfg Config, secret func(name string) connector.Secret, log *slog.Logger) (*Proxy, error) {
	p := &Proxy{
		services: map[string]*service{},
		secret:   secret,
		read:     newClient(true),
		// A fresh connection per write: the transport only retries requests
		// on reused connections, and an approved write must never be resent.
		write: newClient(false),
		log:   log,
	}
	stored := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Services)) {
		svc, err := compile(name, cfg.Services[name])
		if err != nil {
			return nil, err
		}
		p.services[name] = svc
		p.names = append(p.names, name)
		raw, err := secret(name).Load()
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		if raw == nil {
			continue
		}
		token, err := checkToken(string(raw))
		if err != nil {
			return nil, fmt.Errorf("service %s: stored %w", name, err)
		}
		stored[name] = token
	}
	p.use(stored)
	return p, nil
}

// use switches to tokens by service name; the caller holds mu or owns p.
func (p *Proxy) use(byName map[string]string) {
	t := &tokens{values: map[string]string{}}
	for _, name := range p.names {
		token, ok := byName[name]
		if !ok {
			continue
		}
		t.values[name] = p.services[name].prefix + token
		for _, form := range []string{token, url.QueryEscape(token), jsonEscape(token)} {
			if !slices.ContainsFunc(t.secrets, func(b []byte) bool { return string(b) == form }) {
				t.secrets = append(t.secrets, []byte(form))
			}
		}
	}
	p.tokens = t
}

func (p *Proxy) current() *tokens {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokens
}

// linked is whether every service has a token; the caller holds mu.
func (p *Proxy) linked() bool { return len(p.tokens.values) == len(p.services) }

func (p *Proxy) Health(context.Context) connector.Health {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.linked() {
		return connector.Health{State: connector.NeedsSetup, Detail: "needs API tokens"}
	}
	return connector.Health{State: connector.Healthy}
}

// Setup takes a token for every service, checks them all, then stores them.
func (p *Proxy) Setup(_ context.Context, args map[string]string) (connector.SetupStep, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.linked() {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "already linked"}
	}
	for k := range args {
		if _, ok := p.services[k]; !ok {
			return connector.SetupStep{}, connector.Invalid("unknown arg %q", k)
		}
	}
	byName := map[string]string{}
	for _, name := range p.names {
		raw, ok := args[name]
		if !ok {
			return connector.SetupStep{}, connector.Invalid("service %s: token missing", name)
		}
		token, err := checkToken(raw)
		if err != nil {
			return connector.SetupStep{}, connector.Invalid("service %s: %v", name, err)
		}
		byName[name] = token
	}
	for _, name := range p.names {
		if err := p.secret(name).Store([]byte(byName[name])); err != nil {
			return connector.SetupStep{}, fmt.Errorf("service %s: %w", name, err)
		}
	}
	p.use(byName)
	return connector.SetupStep{Done: true}, nil
}

func (p *Proxy) Unlink(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.use(nil)
	var errs []error
	for _, name := range p.names {
		if err := p.secret(name).Delete(); err != nil {
			errs = append(errs, fmt.Errorf("service %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func checkToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	switch {
	case len(token) < minToken:
		// Scrubbing a short token would mangle responses without protecting it.
		return "", fmt.Errorf("token is shorter than %d bytes", minToken)
	case len(token) > maxToken:
		return "", fmt.Errorf("token is longer than %d bytes", maxToken)
	case strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return "", errors.New("token contains whitespace or control characters")
	}
	return token, nil
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func newClient(keepAlive bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Proxy is nil: tokens never go through an environment proxy.
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			DisableKeepAlives:     !keepAlive,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (t *tokens) scrub(b []byte) []byte {
	for _, s := range t.secrets {
		b = bytes.ReplaceAll(b, s, []byte(redacted))
	}
	return b
}

func (t *tokens) scrubString(s string) string { return string(t.scrub([]byte(s))) }

func (s *service) target(path string, segs []string, rawQuery string) string {
	u := *s.base
	u.Path = s.base.Path + "/" + strings.Join(segs, "/")
	u.RawPath = s.base.EscapedPath() + path
	u.RawQuery = rawQuery
	return u.String()
}

// Response headers an agent needs to page, cache and back off; every other
// upstream header is dropped.
var passResponse = []string{"Content-Type", "Location", "Link", "Etag", "Last-Modified", "Retry-After"}

func passHeader(k string) bool {
	return slices.Contains(passResponse, k) ||
		strings.HasPrefix(k, "X-Ratelimit-") || strings.HasPrefix(k, "Ratelimit-") || k == "Ratelimit"
}

func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request, rest string) {
	name, path, _ := strings.Cut(rest, "/")
	svc, ok := p.services[name]
	if !ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusNotFound, Message: fmt.Sprintf("unknown service %q", name)})
		return
	}
	t := p.current()
	value, ok := t.values[name]
	if !ok {
		connector.WriteError(w, errNotLinked)
		return
	}
	canon, segs, err := normalize("/" + path)
	if err != nil {
		connector.WriteError(w, &connector.Error{Status: http.StatusBadRequest, Message: err.Error()})
		return
	}
	if !allowed(svc.reads, r.Method, segs) {
		connector.WriteError(w, &connector.Error{Status: http.StatusForbidden, Message: fmt.Sprintf("%s %s is not an allowed read for %s", r.Method, canon, svc.name)})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, svc.target(canon, segs, r.URL.RawQuery), nil)
	if err != nil {
		connector.WriteError(w, &connector.Error{Status: http.StatusBadRequest, Message: err.Error()})
		return
	}
	// Only these come from the agent; Authorization, Cookie, Host,
	// X-Forwarded-* and the rest never reach the upstream.
	for _, h := range []string{"Accept", "If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Values(h); len(v) > 0 {
			req.Header[h] = v
		}
	}
	req.Header.Set(svc.header, value)

	log := p.log.With("service", svc.name, "method", r.Method, "path", canon)
	resp, err := p.read.Do(req)
	if err != nil {
		msg := t.scrubString(err.Error())
		log.Warn("forward failed", "err", msg)
		connector.WriteError(w, &connector.Error{Status: http.StatusBadGateway, Message: msg})
		return
	}
	defer resp.Body.Close()
	// The transport decompresses gzip it asked for itself; any other
	// encoding can't be scrubbed.
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		connector.WriteError(w, &connector.Error{Status: http.StatusBadGateway, Message: fmt.Sprintf("upstream sent a %s-encoded body", ce)})
		return
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		connector.WriteError(w, &connector.Error{Status: http.StatusBadGateway, Message: t.scrubString(err.Error())})
		return
	}
	if len(out) > maxResponse {
		connector.WriteError(w, &connector.Error{Status: http.StatusBadGateway, Message: fmt.Sprintf("upstream response exceeds %d bytes", maxResponse)})
		return
	}
	out = t.scrub(out)
	for k, vs := range resp.Header {
		if !passHeader(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, t.scrubString(v))
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	log.Info("forwarded", "status", resp.StatusCode, "bytes", len(out))
}

const opRequest = "request"

func (p *Proxy) Spec() connector.Spec {
	setup := make([]connector.Param, 0, len(p.names))
	for _, name := range p.names {
		setup = append(setup, connector.Param{Name: name, Doc: "token for " + p.services[name].base.Host, Required: true, Secret: true})
	}
	return connector.Spec{Name: "http", Doc: p.doc(), Setup: setup, Ops: []connector.Op{{
		Name:    opRequest,
		Kind:    connector.Write,
		Summary: "Send an HTTP request to a configured API with its stored token",
		Params: []connector.Param{
			{Name: "service", Doc: "one of: " + strings.Join(p.names, ", "), Required: true},
			{Name: "method", Doc: "HTTP method, e.g. POST", Required: true},
			{Name: "path", Doc: "path below the service's base URL, e.g. /repos/OWNER/REPO/issues; no query", Required: true},
			{Name: "body", Doc: "request body; JSON is sent as application/json, anything else as text/plain", Editable: true},
		},
	}}}
}

func (p *Proxy) doc() string {
	var b strings.Builder
	b.WriteString("Reads: `mandate http get <service>/<path>`, e.g. `mandate http get github/repos/OWNER/REPO/issues?state=open`.\n")
	b.WriteString("Writes: `mandate http request service=… method=… path=… body=…`, which the owner approves.\n")
	for _, name := range p.names {
		s := p.services[name]
		fmt.Fprintf(&b, "\n## %s (%s)\n", name, s.base.Host)
		for _, set := range []struct {
			title string
			rules []Rule
		}{{"Reads", s.rules.Reads}, {"Writes", s.rules.Writes}} {
			if len(set.rules) == 0 {
				continue
			}
			fmt.Fprintf(&b, "%s:\n", set.title)
			for _, r := range set.rules {
				fmt.Fprintf(&b, "- `%s %s`\n", r.Method, r.Path)
			}
		}
	}
	return b.String()
}

type write struct {
	svc    *service
	method string
	path   string
	segs   []string
	body   string
	tokens *tokens
	value  string
}

func (p *Proxy) resolve(op string, args map[string]string) (write, error) {
	if op != opRequest {
		return write{}, fmt.Errorf("%w %q", connector.ErrUnknownOp, op)
	}
	for k := range args {
		if k != "service" && k != "method" && k != "path" && k != "body" {
			return write{}, connector.Invalid("unknown arg %q", k)
		}
	}
	svc, ok := p.services[args["service"]]
	if !ok {
		return write{}, connector.Invalid("unknown service %q", args["service"])
	}
	t := p.current()
	value, ok := t.values[svc.name]
	if !ok {
		return write{}, errNotLinked
	}
	method := strings.ToUpper(args["method"])
	if !isToken(method) {
		return write{}, connector.Invalid("invalid method %q", args["method"])
	}
	path, segs, err := normalize(args["path"])
	if err != nil {
		return write{}, connector.Invalid("%v", err)
	}
	if !allowed(svc.writes, method, segs) {
		return write{}, &connector.Error{
			Status:  http.StatusForbidden,
			Message: fmt.Sprintf("%s %s is not an allowed write for %s", method, path, svc.name),
		}
	}
	return write{svc: svc, method: method, path: path, segs: segs, body: args["body"], tokens: t, value: value}, nil
}

func (p *Proxy) Prepare(_ context.Context, op string, args map[string]string) (connector.Prepared, error) {
	w, err := p.resolve(op, args)
	if err != nil {
		return connector.Prepared{}, err
	}
	out := map[string]string{"service": w.svc.name, "method": w.method, "path": w.path}
	if w.body != "" {
		out["body"] = w.body
	}
	return connector.Prepared{
		Args: out,
		Display: []approval.Field{
			{Label: "Service", Value: w.svc.name},
			{Label: "Request", Value: w.method + " " + w.path},
			{Label: "Body", Value: pretty(w.body), Param: "body"},
		},
	}, nil
}

// pretty indents JSON for the approval page. json.Indent only touches
// whitespace, so duplicate keys and number spellings show as they are sent.
func pretty(body string) string {
	var b bytes.Buffer
	if json.Indent(&b, []byte(body), "", "  ") != nil {
		return body
	}
	return b.String()
}

func (p *Proxy) Execute(ctx context.Context, rec approval.Record) connector.Result {
	w, err := p.resolve(rec.Op, rec.Args)
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: err.Error()}
	}
	log := p.log.With("id", rec.ID, "service", w.svc.name, "method", w.method, "path", w.path)
	// The owner approved this write; finish it even if the caller hangs up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	// Dialing, DNS and TLS happen before GotConn; once a connection is
	// handed over, request bytes may be on the wire.
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	var body io.Reader
	if w.body != "" {
		body = strings.NewReader(w.body)
	}
	req, err := http.NewRequestWithContext(ctx, w.method, w.svc.target(w.path, w.segs, ""), body)
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: w.tokens.scrubString(err.Error())}
	}
	if w.body != "" {
		ct := "text/plain; charset=utf-8"
		if json.Valid([]byte(w.body)) {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set(w.svc.header, w.value)

	resp, err := p.write.Do(req)
	if err != nil {
		status := connector.Failed
		if connected.Load() {
			status = connector.Unknown
		}
		msg := w.tokens.scrubString(err.Error())
		log.Warn("write failed", "status", status, "err", msg)
		return connector.Result{Status: status, Detail: msg}
	}
	defer resp.Body.Close()
	// Scrub the whole body before cutting it, so no partial token survives
	// at the cut.
	out, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	status := connector.Unknown
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		status = connector.Done
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		status = connector.Failed
	}
	log.Info("write sent", "status", resp.StatusCode, "result", status)
	return connector.Result{
		Status: status,
		Ref:    strconv.Itoa(resp.StatusCode),
		Detail: w.tokens.scrubString(resp.Status),
		Output: truncate(string(w.tokens.scrub(out)), maxOutput),
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Drops a rune cut in half at n.
	return strings.ToValidUTF8(s[:n], "") + fmt.Sprintf("\n[truncated: %d of %d bytes shown]", n, len(s))
}
