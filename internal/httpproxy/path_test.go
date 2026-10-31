package httpproxy

import (
	"slices"
	"testing"
)

func TestPatternMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rule   Rule
		method string
		path   string
		want   bool
	}{
		{Rule{"GET", "/repos/*/*/issues"}, "GET", "/repos/a/b/issues", true},
		{Rule{"GET", "/repos/*/*/issues"}, "POST", "/repos/a/b/issues", false},
		{Rule{"GET", "/repos/*/*/issues"}, "get", "/repos/a/b/issues", false},
		{Rule{"GET", "/repos/*/*/issues"}, "GET", "/repos/a/issues", false},
		{Rule{"GET", "/repos/*/*/issues"}, "GET", "/repos/a/b/issues/1", false},
		{Rule{"GET", "/repos/*/*/issues"}, "GET", "/repos/a/b/pulls", false},
		{Rule{"GET", "/repos/*/*/issues"}, "GET", "/Repos/a/b/issues", false},
		{Rule{"GET", "/repos/*/*/issues"}, "GET", "/repos/%61/b/issues", true},
		{Rule{"GET", "/repos/*/*/issues"}, "GET", "/repos/a%20b/c/issues", true},
		{Rule{"GET", "/repos/*/*/contents/**"}, "GET", "/repos/a/b/contents/x", true},
		{Rule{"GET", "/repos/*/*/contents/**"}, "GET", "/repos/a/b/contents/x/y/z.md", true},
		{Rule{"GET", "/repos/*/*/contents/**"}, "GET", "/repos/a/b/contents", false},
		{Rule{"GET", "/api/states/light.kitchen"}, "GET", "/api/states/light.kitchen", true},
		{Rule{"GET", "/api/states/light.kitchen"}, "GET", "/api/states/light.hall", false},
		{Rule{"GET", "/**"}, "GET", "/anything/at/all", true},
		{Rule{"POST", "/graphql"}, "POST", "/graphql", true},
	}
	for _, tt := range tests {
		p, err := compilePattern(tt.rule)
		if err != nil {
			t.Fatalf("%v: %v", tt.rule, err)
		}
		_, segs, err := normalize(tt.path)
		if err != nil {
			t.Fatalf("%s: %v", tt.path, err)
		}
		if got := p.match(tt.method, segs); got != tt.want {
			t.Errorf("%v matching %s %s = %v, want %v", tt.rule, tt.method, tt.path, got, tt.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	ok := []struct{ in, canon string }{
		{"/repos/a/b/issues", "/repos/a/b/issues"},
		{"/repos/%61/b", "/repos/a/b"},
		{"/a%20b/c:d+e@f", "/a%20b/c:d+e@f"},
		{"/caf%C3%A9", "/caf%C3%A9"},
		{"/100%25", "/100%25"},
	}
	for _, tt := range ok {
		canon, _, err := normalize(tt.in)
		if err != nil || canon != tt.canon {
			t.Errorf("normalize(%q) = %q, %v; want %q", tt.in, canon, err, tt.canon)
		}
		again, _, err := normalize(canon)
		if err != nil || again != canon {
			t.Errorf("normalize not idempotent on %q: %q, %v", canon, again, err)
		}
	}
	bad := []string{
		"", "repos/a", "/", "/repos/", "/repos//a",
		"/repos/a/../admin", "/repos/./a", "/..", "/repos/%2e%2e/admin", "/repos/%2E%2e/admin",
		"/repos/a%2e", "/repos/a%2Fb", "/repos/a%2fb",
		`/repos/a\b`, "/repos/a%5cb", "/repos/a%5Cb",
		"/repos/a?x=1", "/repos/a#x", "/repos/%zz", "/repos/a%00b", "/repos/a%0d%0aX-Evil:1",
	}
	for _, in := range bad {
		if canon, _, err := normalize(in); err == nil {
			t.Errorf("normalize(%q) = %q, want error", in, canon)
		}
	}
}

func TestCompilePatternRefuses(t *testing.T) {
	t.Parallel()
	for _, r := range []Rule{
		{"get", "/a"},
		{"", "/a"},
		{"GE T", "/a"},
		{"GET", "a"},
		{"GET", "/"},
		{"GET", "/a//b"},
		{"GET", "/a/"},
		{"GET", "/a/../b"},
		{"GET", "/a/%2e%2e"},
		{"GET", `/a\b`},
		{"GET", "/a*"},
		{"GET", "/**/a"},
		{"GET", "/a/***"},
	} {
		if _, err := compilePattern(r); err == nil {
			t.Errorf("compilePattern(%v) succeeded", r)
		}
	}
	p, err := compilePattern(Rule{"GET", "/a%20b/*/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.segs, []string{"a b", "*"}) || !p.rest {
		t.Fatalf("compiled %+v", p)
	}
}
