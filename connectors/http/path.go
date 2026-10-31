package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// normalize checks an escaped request path and returns its canonical escaped
// form and its decoded segments. Rules match the decoded segments and the
// canonical form is what goes upstream, so what was checked is what is sent.
//
// Anything an upstream or an intermediary might resolve differently is
// refused rather than cleaned: dot segments, encoded slashes, dots and
// backslashes, empty segments.
func normalize(escaped string) (string, []string, error) {
	if !strings.HasPrefix(escaped, "/") {
		return "", nil, errors.New("path must start with /")
	}
	if strings.ContainsAny(escaped, `\?#`) {
		return "", nil, errors.New("path must not contain a backslash, query, or fragment")
	}
	raws := strings.Split(escaped[1:], "/")
	segs := make([]string, len(raws))
	var b strings.Builder
	for i, raw := range raws {
		if raw == "" {
			return "", nil, errors.New("path has an empty segment")
		}
		lower := strings.ToLower(raw)
		if strings.Contains(lower, "%2f") || strings.Contains(lower, "%2e") || strings.Contains(lower, "%5c") {
			return "", nil, errors.New("path has an encoded slash, dot, or backslash")
		}
		seg, err := url.PathUnescape(raw)
		if err != nil {
			return "", nil, fmt.Errorf("path has a bad escape: %w", err)
		}
		if seg == "." || seg == ".." {
			return "", nil, errors.New("path has a dot segment")
		}
		if strings.ContainsFunc(seg, isControl) {
			return "", nil, errors.New("path has control characters")
		}
		segs[i] = seg
		b.WriteByte('/')
		b.WriteString(url.PathEscape(seg))
	}
	return b.String(), segs, nil
}

type pattern struct {
	method string
	// segs holds decoded literals and "*" for any one segment.
	segs []string
	// rest is a trailing "**": one or more further segments.
	rest bool
}

func compilePattern(r Rule) (pattern, error) {
	if r.Method == "" || strings.ToUpper(r.Method) != r.Method || !isToken(r.Method) {
		return pattern{}, fmt.Errorf("method %q must be an upper-case HTTP method", r.Method)
	}
	if !strings.HasPrefix(r.Path, "/") {
		return pattern{}, fmt.Errorf("path %q must start with /", r.Path)
	}
	p := pattern{method: r.Method}
	parts := strings.Split(r.Path[1:], "/")
	for i, part := range parts {
		switch {
		case part == "**" && i == len(parts)-1:
			p.rest = true
		case part == "*":
			p.segs = append(p.segs, part)
		case strings.Contains(part, "*"):
			return pattern{}, fmt.Errorf("path %q: wildcards must be whole segments, and ** only last", r.Path)
		default:
			_, segs, err := normalize("/" + part)
			if err != nil {
				return pattern{}, fmt.Errorf("path %q: %w", r.Path, err)
			}
			p.segs = append(p.segs, segs[0])
		}
	}
	return p, nil
}

func (p pattern) match(method string, segs []string) bool {
	if method != p.method {
		return false
	}
	if p.rest && len(segs) <= len(p.segs) || !p.rest && len(segs) != len(p.segs) {
		return false
	}
	for i, s := range p.segs {
		if s != "*" && s != segs[i] {
			return false
		}
	}
	return true
}

func allowed(rules []pattern, method string, segs []string) bool {
	for _, p := range rules {
		if p.match(method, segs) {
			return true
		}
	}
	return false
}
