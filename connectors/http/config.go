package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Config is the connector's --config. Each service's token is the credential
// of the same name.
type Config struct {
	Services map[string]Service `json:"services"`
}

type Service struct {
	BaseURL string `json:"base_url"`
	Token   Token  `json:"token"`
	Reads   []Rule `json:"reads"`
	Writes  []Rule `json:"writes"`
}

// Token says how the token is sent, as Header: Prefix+token. Omitted, it is
// Authorization: Bearer.
type Token struct {
	Header string `json:"header"`
	Prefix string `json:"prefix"`
}

// Rule allows one method on a path pattern, written "METHOD /path". Patterns
// match whole segments: "*" is any one segment, a trailing "**" one or more.
type Rule struct {
	Method string
	Path   string
}

func (r *Rule) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	method, path, ok := strings.Cut(s, " ")
	if !ok {
		return fmt.Errorf("rule %q must be \"METHOD /path\"", s)
	}
	*r = Rule{Method: method, Path: path}
	return nil
}

func (c Config) Validate() error {
	if len(c.Services) == 0 {
		return errors.New("no services")
	}
	for name, s := range c.Services {
		if _, err := compile(name, s); err != nil {
			return err
		}
	}
	return nil
}

var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Headers the proxy sets or forwards itself; a token in one of them would be
// overwritten or would collide with what the agent sends.
var reservedHeaders = map[string]bool{
	"Accept": true, "Connection": true, "Content-Length": true, "Content-Type": true,
	"Host": true, "Te": true, "Transfer-Encoding": true, "Upgrade": true,
}

type service struct {
	name   string
	base   *url.URL
	header string
	prefix string
	// value is prefix + token.
	value  string
	reads  []pattern
	writes []pattern
	rules  Service
}

func compile(name string, s Service) (*service, error) {
	// The name is also the token's credential file name.
	if !serviceName.MatchString(name) {
		return nil, fmt.Errorf("service name %q must match %s", name, serviceName)
	}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("service %s: %s", name, fmt.Sprintf(format, a...))
	}
	base, err := parseBase(s.BaseURL)
	if err != nil {
		return nil, fail("base_url: %v", err)
	}
	t := s.Token
	if t == (Token{}) {
		t = Token{Header: "Authorization", Prefix: "Bearer "}
	}
	if !isToken(t.Header) {
		return nil, fail("token.header %q is not a valid header name", t.Header)
	}
	if reservedHeaders[http.CanonicalHeaderKey(t.Header)] {
		return nil, fail("token.header %q is managed by the proxy", t.Header)
	}
	if strings.ContainsFunc(t.Prefix, isControl) {
		return nil, fail("token.prefix contains control characters")
	}
	if len(s.Reads)+len(s.Writes) == 0 {
		return nil, fail("no reads or writes")
	}
	svc := &service{name: name, base: base, header: t.Header, prefix: t.Prefix, rules: s}
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
