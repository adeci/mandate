package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
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
	}
	for _, c := range valid {
		if _, err := parse([]byte(c)); err != nil {
			t.Errorf("parse(%s): %v", c, err)
		}
	}

	invalid := map[string]string{
		"no services":       `{"services":{}}`,
		"rule object":       services(`"github"`, svc("", "", `[{"method":"GET","path":"/a"}]`, "")),
		"rule without path": services(`"github"`, svc("", "", `["GET"]`, "")),
		"slash in name":     services(`"a/b"`, svc("", "", "", "")),
		"dotdot name":       services(`".."`, svc("", "", "", "")),
		"ftp":               services(`"github"`, svc(`"ftp://api.github.com"`, "", "", "")),
		"no host":           services(`"github"`, svc(`"https:///x"`, "", "", "")),
		"userinfo":          services(`"github"`, svc(`"https://user:pw@api.github.com"`, "", "", "")),
		"query":             services(`"github"`, svc(`"https://api.github.com?access_token=x"`, "", "", "")),
		"trailing slash":    services(`"github"`, svc(`"https://api.github.com/"`, "", "", "")),
		"dotted base path":  services(`"github"`, svc(`"https://h/api/../x"`, "", "", "")),
		"bad header":        services(`"github"`, svc("", `{"header":"Auth orization"}`, "", "")),
		"host header":       services(`"github"`, svc("", `{"header":"host"}`, "", "")),
		"newline in prefix": services(`"github"`, svc("", `{"header":"Authorization","prefix":"Bearer\r\nX: "}`, "", "")),
		"no rules":          services(`"github"`, svc("", "", `[]`, `[]`)),
		"bad read rule":     services(`"github"`, svc("", "", `["get /a"]`, "")),
		"bad write rule":    services(`"github"`, svc("", "", "", `["POST /repos/../admin"]`)),
		"post read":         services(`"github"`, svc("", "", `["POST /graphql"]`, "")),
	}
	for name, c := range invalid {
		if _, err := parse([]byte(c)); err == nil {
			t.Errorf("%s: parse accepted %s", name, c)
		}
	}
}

func TestNewRefusesBadStoredTokens(t *testing.T) {
	t.Parallel()
	cfg, err := parse([]byte(services(`"github"`, svc("", "", "", ""))))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, content := range map[string]string{
		"short":   "abc\n",
		"newline": "abcdefghij\nklmnopqrst",
		"huge":    strings.Repeat("a", maxToken+1),
	} {
		env := newEnv(t)
		if err := env.Secret("github").Store([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if _, err := New(cfg, env.Secret, log); err == nil {
			t.Errorf("%s: New accepted the token", name)
		}
	}
}
