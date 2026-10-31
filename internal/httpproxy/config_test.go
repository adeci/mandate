package httpproxy

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// svc renders one service with fragments substituted into a valid default.
func svc(name, base, token, reads, writes string) string {
	if name == "" {
		name = `"github"`
	}
	if base == "" {
		base = `"https://api.github.com"`
	}
	if token == "" {
		token = `{"header":"Authorization","prefix":"Bearer ","file":"github"}`
	}
	if reads == "" {
		reads = `[{"method":"GET","path":"/repos/*/*/issues"},{"method":"HEAD","path":"/repos/*/*/issues"}]`
	}
	if writes == "" {
		writes = `[{"method":"POST","path":"/repos/*/*/issues"}]`
	}
	return `{"name":` + name + `,"base_url":` + base + `,"token":` + token + `,"reads":` + reads + `,"writes":` + writes + `}`
}

func TestParseConfig(t *testing.T) {
	t.Parallel()
	valid := []string{
		"[" + svc("", "", "", "", "") + "]",
		"[" + svc(`"ha"`, `"http://homeassistant.lan:8123/api"`, `{"header":"Authorization","prefix":"Bearer ","file":"ha"}`, `[{"method":"GET","path":"/states/**"}]`, `[]`) + "]",
		"[" + svc("", "", `{"header":"X-Api-Key","file":"github"}`, `[]`, "") + "]",
	}
	for _, c := range valid {
		if _, err := ParseConfig([]byte(c)); err != nil {
			t.Errorf("ParseConfig(%s): %v", c, err)
		}
	}

	invalid := map[string]string{
		"empty":             `[]`,
		"not an array":      svc("", "", "", "", ""),
		"trailing data":     "[" + svc("", "", "", "", "") + "] []",
		"unknown key":       `[{"name":"github","base_url":"https://api.github.com","token":{"header":"Authorization","file":"github"},"reads":[{"method":"GET","path":"/a"}],"writes":[],"allow_all":true}]`,
		"unknown token key": "[" + svc("", "", `{"header":"Authorization","file":"github","value":"secret"}`, "", "") + "]",
		"unknown rule key":  "[" + svc("", "", "", `[{"method":"GET","path":"/a","query":"*"}]`, "") + "]",
		"duplicate":         "[" + svc("", "", "", "", "") + "," + svc("", "", "", "", "") + "]",
		"bad name":          "[" + svc(`"Git Hub"`, "", "", "", "") + "]",
		"slash in name":     "[" + svc(`"a/b"`, "", "", "", "") + "]",
		"empty name":        "[" + svc(`""`, "", "", "", "") + "]",
		"ftp":               "[" + svc("", `"ftp://api.github.com"`, "", "", "") + "]",
		"no host":           "[" + svc("", `"https:///x"`, "", "", "") + "]",
		"relative":          "[" + svc("", `"/api"`, "", "", "") + "]",
		"userinfo":          "[" + svc("", `"https://user:pw@api.github.com"`, "", "", "") + "]",
		"query":             "[" + svc("", `"https://api.github.com?access_token=x"`, "", "", "") + "]",
		"fragment":          "[" + svc("", `"https://api.github.com#x"`, "", "", "") + "]",
		"trailing slash":    "[" + svc("", `"https://api.github.com/"`, "", "", "") + "]",
		"dotted base path":  "[" + svc("", `"https://h/api/../x"`, "", "", "") + "]",
		"no header":         "[" + svc("", "", `{"header":"","file":"github"}`, "", "") + "]",
		"bad header":        "[" + svc("", "", `{"header":"Auth orization","file":"github"}`, "", "") + "]",
		"host header":       "[" + svc("", "", `{"header":"host","file":"github"}`, "", "") + "]",
		"content-type":      "[" + svc("", "", `{"header":"Content-Type","file":"github"}`, "", "") + "]",
		"newline in prefix": "[" + svc("", "", `{"header":"Authorization","prefix":"Bearer\r\nX: ","file":"github"}`, "", "") + "]",
		"no file":           "[" + svc("", "", `{"header":"Authorization"}`, "", "") + "]",
		"file path":         "[" + svc("", "", `{"header":"Authorization","file":"../etc/shadow"}`, "", "") + "]",
		"file dotdot":       "[" + svc("", "", `{"header":"Authorization","file":".."}`, "", "") + "]",
		"no rules":          "[" + svc("", "", "", `[]`, `[]`) + "]",
		"lower method":      "[" + svc("", "", "", `[{"method":"get","path":"/a"}]`, "") + "]",
		"partial wildcard":  "[" + svc("", "", "", `[{"method":"GET","path":"/repos/a*"}]`, "") + "]",
		"inner globstar":    "[" + svc("", "", "", "", `[{"method":"POST","path":"/**/issues"}]`) + "]",
		"dot in rule":       "[" + svc("", "", "", "", `[{"method":"POST","path":"/repos/../admin"}]`) + "]",
		"relative rule":     "[" + svc("", "", "", `[{"method":"GET","path":"repos"}]`, "") + "]",
		"empty segment":     "[" + svc("", "", "", `[{"method":"GET","path":"/repos//x"}]`, "") + "]",
		"post read":         "[" + svc("", "", "", `[{"method":"POST","path":"/graphql"}]`, "") + "]",
		"delete read":       "[" + svc("", "", "", `[{"method":"DELETE","path":"/repos/*/*"}]`, "") + "]",
	}
	for name, c := range invalid {
		if _, err := ParseConfig([]byte(c)); err == nil {
			t.Errorf("%s: ParseConfig accepted %s", name, c)
		}
	}
}

func TestNewRefusesBadTokens(t *testing.T) {
	t.Parallel()
	services, err := ParseConfig([]byte("[" + svc("", "", "", "", "") + "]"))
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
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "github"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(services, dir, log); err == nil {
			t.Errorf("%s: New accepted the token", name)
		}
	}
	if _, err := New(services, t.TempDir(), log); err == nil {
		t.Error("New accepted a missing token file")
	}
}
