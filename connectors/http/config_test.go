package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// parse decodes and validates a --config the way connector.Main does.
func parse(b []byte) (Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

// svc renders one service with fragments substituted into a valid default.
func svc(base, token, reads, writes string) string {
	if base == "" {
		base = `"https://api.github.com"`
	}
	if token == "" {
		token = `{"header":"Authorization","prefix":"Bearer "}`
	}
	if reads == "" {
		reads = `["GET /repos/*/*/issues","HEAD /repos/*/*/issues"]`
	}
	if writes == "" {
		writes = `["POST /repos/*/*/issues"]`
	}
	return `{"base_url":` + base + `,"token":` + token + `,"reads":` + reads + `,"writes":` + writes + `}`
}

func services(name, svc string) string {
	return `{"services":{` + name + `:` + svc + `}}`
}

func TestConfig(t *testing.T) {
	t.Parallel()
	valid := []string{
		services(`"github"`, svc("", "", "", "")),
		services(`"ha"`, svc(`"http://homeassistant.lan:8123/api"`, "", `["GET /states/**"]`, `[]`)),
		services(`"github"`, svc("", `{"header":"X-Api-Key"}`, `[]`, "")),
		`{"services":{"a":` + svc("", "", "", "") + `,"b":` + svc("", "", "", "") + `}}`,
	}
	for _, c := range valid {
		if _, err := parse([]byte(c)); err != nil {
			t.Errorf("parse(%s): %v", c, err)
		}
	}

	invalid := map[string]string{
		"no config":          `{}`,
		"no services":        `{"services":{}}`,
		"not an object":      `{"services":[` + svc("", "", "", "") + `]}`,
		"unknown key":        services(`"github"`, `{"base_url":"https://api.github.com","token":{"header":"Authorization"},"reads":["GET /a"],"writes":[],"allow_all":true}`),
		"unknown token key":  services(`"github"`, svc("", `{"header":"Authorization","value":"secret"}`, "", "")),
		"token file":         services(`"github"`, svc("", `{"header":"Authorization","file":"github"}`, "", "")),
		"rule object":        services(`"github"`, svc("", "", `[{"method":"GET","path":"/a"}]`, "")),
		"rule without path":  services(`"github"`, svc("", "", `["GET"]`, "")),
		"bad name":           services(`"Git Hub"`, svc("", "", "", "")),
		"slash in name":      services(`"a/b"`, svc("", "", "", "")),
		"dotdot name":        services(`".."`, svc("", "", "", "")),
		"empty name":         services(`""`, svc("", "", "", "")),
		"ftp":                services(`"github"`, svc(`"ftp://api.github.com"`, "", "", "")),
		"no host":            services(`"github"`, svc(`"https:///x"`, "", "", "")),
		"relative":           services(`"github"`, svc(`"/api"`, "", "", "")),
		"userinfo":           services(`"github"`, svc(`"https://user:pw@api.github.com"`, "", "", "")),
		"query":              services(`"github"`, svc(`"https://api.github.com?access_token=x"`, "", "", "")),
		"fragment":           services(`"github"`, svc(`"https://api.github.com#x"`, "", "", "")),
		"trailing slash":     services(`"github"`, svc(`"https://api.github.com/"`, "", "", "")),
		"dotted base path":   services(`"github"`, svc(`"https://h/api/../x"`, "", "", "")),
		"no header":          services(`"github"`, svc("", `{"header":""}`, "", "")),
		"bad header":         services(`"github"`, svc("", `{"header":"Auth orization"}`, "", "")),
		"host header":        services(`"github"`, svc("", `{"header":"host"}`, "", "")),
		"content-type":       services(`"github"`, svc("", `{"header":"Content-Type"}`, "", "")),
		"newline in prefix":  services(`"github"`, svc("", `{"header":"Authorization","prefix":"Bearer\r\nX: "}`, "", "")),
		"no rules":           services(`"github"`, svc("", "", `[]`, `[]`)),
		"lower method":       services(`"github"`, svc("", "", `["get /a"]`, "")),
		"double space":       services(`"github"`, svc("", "", `["GET  /a"]`, "")),
		"partial wildcard":   services(`"github"`, svc("", "", `["GET /repos/a*"]`, "")),
		"inner globstar":     services(`"github"`, svc("", "", "", `["POST /**/issues"]`)),
		"dot in rule":        services(`"github"`, svc("", "", "", `["POST /repos/../admin"]`)),
		"relative rule":      services(`"github"`, svc("", "", `["GET repos"]`, "")),
		"empty segment":      services(`"github"`, svc("", "", `["GET /repos//x"]`, "")),
		"post read":          services(`"github"`, svc("", "", `["POST /graphql"]`, "")),
		"delete read":        services(`"github"`, svc("", "", `["DELETE /repos/*/*"]`, "")),
		"unknown top key":    `{"services":{"github":` + svc("", "", "", "") + `},"listen":"/tmp/x"}`,
		"services not a map": `{"services":"github"}`,
	}
	for name, c := range invalid {
		if _, err := parse([]byte(c)); err == nil {
			t.Errorf("%s: parse accepted %s", name, c)
		}
	}
}

func TestNewRefusesBadTokens(t *testing.T) {
	t.Parallel()
	cfg, err := parse([]byte(services(`"github"`, svc("", "", "", ""))))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, content := range map[string]string{
		"short":   "abc\n",
		"empty":   "",
		"space":   "abcdefgh ijklmnopqrst",
		"newline": "abcdefghij\nklmnopqrst",
		"huge":    strings.Repeat("a", maxToken+1),
	} {
		cred := func(string) ([]byte, error) { return []byte(content), nil }
		if _, err := New(cfg, cred, log); err == nil {
			t.Errorf("%s: New accepted the token", name)
		}
	}
	var asked string
	missing := func(name string) ([]byte, error) { asked = name; return nil, os.ErrNotExist }
	if _, err := New(cfg, missing, log); !errors.Is(err, os.ErrNotExist) || asked != "github" {
		t.Errorf("missing credential: asked %q, err %v", asked, err)
	}
}
