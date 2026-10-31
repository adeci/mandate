// Package httpproxy holds API tokens for HTTP services and lends them to
// agents without handing them over. Reads go through an allowlisted
// passthrough at /forward/{service}/{path}; writes are the "http" connector's
// "request" op and run only with the owner's approval. Tokens never leave
// this process: they are added to outgoing requests only, and scrubbed from
// everything that comes back.
package httpproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Service is one upstream API, as configured.
type Service struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Token   Token  `json:"token"`
	Reads   []Rule `json:"reads"`
	Writes  []Rule `json:"writes"`
}

// Token says where the token lives and how it is sent: Header: Prefix+token.
type Token struct {
	Header string `json:"header"`
	Prefix string `json:"prefix"`
	// File is a file name inside the credentials directory.
	File string `json:"file"`
}

// Rule allows one method on a path pattern. Patterns match whole segments:
// "*" is any one segment, a trailing "**" one or more.
type Rule struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// LoadConfig reads and validates a JSON array of services.
func LoadConfig(path string) ([]Service, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return ParseConfig(b)
}

// ParseConfig decodes and validates a JSON array of services. Unknown keys
// are errors so a typo can't silently widen or drop a rule.
func ParseConfig(b []byte) ([]Service, error) {
	var services []Service
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&services); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse config: trailing data")
	}
	if len(services) == 0 {
		return nil, errors.New("config has no services")
	}
	seen := map[string]bool{}
	for _, s := range services {
		if _, err := compile(s); err != nil {
			return nil, err
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("service %q is configured twice", s.Name)
		}
		seen[s.Name] = true
	}
	return services, nil
}

var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Headers the proxy sets or forwards itself; a token in one of them would be
// overwritten or would collide with what the agent sends.
var reservedHeaders = map[string]bool{
	"Accept": true, "Connection": true, "Content-Length": true, "Content-Type": true,
	"Host": true, "Te": true, "Transfer-Encoding": true, "Upgrade": true,
}

// service is a validated Service.
type service struct {
	name   string
	base   *url.URL
	header string
	prefix string
	file   string
	// value is the header value, prefix and token.
	value  string
	reads  []pattern
	writes []pattern
	rules  Service
}

func compile(s Service) (*service, error) {
	if !serviceName.MatchString(s.Name) {
		return nil, fmt.Errorf("service name %q must match %s", s.Name, serviceName)
	}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("service %s: %s", s.Name, fmt.Sprintf(format, a...))
	}
	base, err := parseBase(s.BaseURL)
	if err != nil {
		return nil, fail("base_url: %v", err)
	}
	t := s.Token
	if !isToken(t.Header) {
		return nil, fail("token.header %q is not a valid header name", t.Header)
	}
	if reservedHeaders[http.CanonicalHeaderKey(t.Header)] {
		return nil, fail("token.header %q is managed by the proxy", t.Header)
	}
	if strings.ContainsFunc(t.Prefix, isControl) {
		return nil, fail("token.prefix contains control characters")
	}
	if t.File == "" || t.File == "." || t.File == ".." || filepath.Base(t.File) != t.File {
		return nil, fail("token.file %q must be a plain file name", t.File)
	}
	if len(s.Reads)+len(s.Writes) == 0 {
		return nil, fail("no reads or writes")
	}
	svc := &service{name: s.Name, base: base, header: t.Header, prefix: t.Prefix, file: t.File, rules: s}
	for _, r := range s.Reads {
		// Reads run with the token and no approval, so they must not be able
		// to change anything; a POST (GraphQL included) can.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return nil, fail("reads: method %q is not allowed, reads are GET or HEAD only; use writes", r.Method)
		}
		p, err := compilePattern(r)
		if err != nil {
			return nil, fail("reads: %v", err)
		}
		svc.reads = append(svc.reads, p)
	}
	for _, r := range s.Writes {
		p, err := compilePattern(r)
		if err != nil {
			return nil, fail("writes: %v", err)
		}
		svc.writes = append(svc.writes, p)
	}
	return svc, nil
}

func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case u.Scheme != "https" && u.Scheme != "http":
		return nil, errors.New("scheme must be https or http")
	case u.Host == "" || u.Opaque != "":
		return nil, errors.New("missing host")
	case u.User != nil:
		return nil, errors.New("credentials belong in token, not the URL")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, errors.New("must not have a query or fragment")
	}
	if u.Path != "" {
		canon, _, err := normalize(u.EscapedPath())
		if err != nil {
			return nil, fmt.Errorf("path: %w", err)
		}
		if canon != u.EscapedPath() {
			return nil, fmt.Errorf("path must be written as %s", canon)
		}
	}
	return u, nil
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
