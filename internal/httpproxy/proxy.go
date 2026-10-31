package httpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/connector"
)

const (
	maxRequest  = 1 << 20
	maxResponse = 10 << 20
	maxDetail   = 512
	// detailRead must exceed maxDetail by more than the longest token: a
	// token cut off at the read limit is only partly scrubbed, and that
	// part has to fall outside the Detail that is kept.
	detailRead = 64 << 10
	minToken   = 16
	maxToken   = 4096
	timeout    = 60 * time.Second
	redacted   = "[redacted]"
)

// Proxy serves reads and approved writes for the configured services.
type Proxy struct {
	services map[string]*service
	names    []string
	// secrets are every token in each form an upstream might echo it.
	secrets [][]byte
	read    *http.Client
	write   *http.Client
	log     *slog.Logger
}

// New loads each service's token from credDir.
func New(services []Service, credDir string, log *slog.Logger) (*Proxy, error) {
	p := &Proxy{
		services: map[string]*service{},
		read:     newClient(true),
		// A fresh connection per write: the transport only retries requests
		// on reused connections, and an approved write must never be resent.
		write: newClient(false),
		log:   log,
	}
	for _, s := range services {
		svc, err := compile(s)
		if err != nil {
			return nil, err
		}
		if _, dup := p.services[svc.name]; dup {
			return nil, fmt.Errorf("service %q is configured twice", svc.name)
		}
		token, err := readToken(filepath.Join(credDir, svc.file))
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", svc.name, err)
		}
		svc.value = svc.prefix + token
		p.services[svc.name] = svc
		p.names = append(p.names, svc.name)
		for _, form := range []string{token, url.QueryEscape(token), jsonEscape(token)} {
			if !slices.ContainsFunc(p.secrets, func(b []byte) bool { return string(b) == form }) {
				p.secrets = append(p.secrets, []byte(form))
			}
		}
	}
	slices.Sort(p.names)
	return p, nil
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	switch {
	case len(token) < minToken:
		// Scrubbing a short token would mangle responses without protecting it.
		return "", fmt.Errorf("token in %s is shorter than %d bytes", path, minToken)
	case len(token) > maxToken:
		return "", fmt.Errorf("token in %s is longer than %d bytes", path, maxToken)
	case strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return "", fmt.Errorf("token in %s contains whitespace or control characters", path)
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

func (p *Proxy) scrub(b []byte) []byte {
	for _, s := range p.secrets {
		b = bytes.ReplaceAll(b, s, []byte(redacted))
	}
	return b
}

func (p *Proxy) scrubString(s string) string { return string(p.scrub([]byte(s))) }

func (p *Proxy) leaks(s string) bool {
	return slices.ContainsFunc(p.secrets, func(b []byte) bool { return strings.Contains(s, string(b)) })
}

// target is the upstream URL for a normalized path below the service's base.
func (s *service) target(path string, segs []string, rawQuery string) string {
	u := *s.base
	u.Path = s.base.Path + "/" + strings.Join(segs, "/")
	u.RawPath = s.base.EscapedPath() + path
	u.RawQuery = rawQuery
	return u.String()
}

// Handler serves the read passthrough at /forward/ and the connector
// protocol from srv on everything else.
func (p *Proxy) Handler(srv *connector.Server) http.Handler {
	protocol := srv.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Route on the raw request target: ServeMux would clean "..", "//"
		// and redirect instead of refusing, and the decoded URL loses %2F.
		raw, _, _ := strings.Cut(r.RequestURI, "?")
		if rest, ok := strings.CutPrefix(raw, "/forward/"); ok {
			p.forward(w, r, rest)
			return
		}
		protocol.ServeHTTP(w, r)
	})
}

// Response headers that describe the upstream connection or session rather
// than the resource.
var dropResponse = map[string]bool{
	"Set-Cookie": true, "Set-Cookie2": true,
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Connection": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Content-Length": true,
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, rest string) {
	name, path, _ := strings.Cut(rest, "/")
	svc, ok := p.services[name]
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("unknown service %q", name))
		return
	}
	canon, segs, err := normalize("/" + path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !allowed(svc.reads, r.Method, segs) {
		writeErr(w, http.StatusForbidden, fmt.Sprintf("%s %s is not an allowed read for %s", r.Method, canon, svc.name))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequest))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, svc.target(canon, segs, r.URL.RawQuery), bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Only these come from the agent; Authorization, Cookie, Host,
	// X-Forwarded-* and the rest never reach the upstream.
	for _, h := range []string{"Accept", "Content-Type"} {
		if v := r.Header.Values(h); len(v) > 0 {
			req.Header[h] = v
		}
	}
	req.Header.Set(svc.header, svc.value)

	log := p.log.With("service", svc.name, "method", r.Method, "path", canon)
	resp, err := p.read.Do(req)
	if err != nil {
		msg := p.scrubString(err.Error())
		log.Warn("forward failed", "err", msg)
		writeErr(w, http.StatusBadGateway, msg)
		return
	}
	defer resp.Body.Close()
	// The transport decompresses gzip it asked for itself; any other
	// encoding can't be scrubbed.
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("upstream sent a %s-encoded body", ce))
		return
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		writeErr(w, http.StatusBadGateway, p.scrubString(err.Error()))
		return
	}
	if len(out) > maxResponse {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("upstream response exceeds %d bytes", maxResponse))
		return
	}
	out = p.scrub(out)
	for k, vs := range resp.Header {
		if dropResponse[k] || k == "Content-Encoding" || p.leaks(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, p.scrubString(v))
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	log.Info("forwarded", "status", resp.StatusCode, "bytes", len(out))
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(connector.Error{Message: msg})
}

const opRequest = "request"

func (p *Proxy) Spec() connector.Spec {
	return connector.Spec{Name: "http", Ops: []connector.Op{{
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

type write struct {
	svc    *service
	method string
	path   string
	segs   []string
	body   string
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
	return write{svc: svc, method: method, path: path, segs: segs, body: args["body"]}, nil
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
		return connector.Result{Status: connector.Failed, Detail: p.scrubString(err.Error())}
	}
	if w.body != "" {
		ct := "text/plain; charset=utf-8"
		if json.Valid([]byte(w.body)) {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set(w.svc.header, w.svc.value)

	resp, err := p.write.Do(req)
	if err != nil {
		status := connector.Failed
		if connected.Load() {
			status = connector.Unknown
		}
		msg := p.scrubString(err.Error())
		log.Warn("write failed", "status", status, "err", msg)
		return connector.Result{Status: status, Detail: msg}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, detailRead))
	detail := resp.Status
	if s := truncate(p.scrubString(string(out)), maxDetail); s != "" {
		detail += ": " + s
	}
	log.Info("write sent", "status", resp.StatusCode)
	return connector.Result{Status: connector.Done, Ref: strconv.Itoa(resp.StatusCode), Detail: detail}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Drops a rune cut in half at n.
	return strings.ToValidUTF8(s[:n], "") + "…"
}
