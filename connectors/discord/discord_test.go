package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckToken(t *testing.T) {
	tests := []struct {
		name, content string
		ok            bool
	}{
		{"plain", "abc.def.ghi\n", true},
		{"empty", "  \n", false},
		{"bot token", "Bot abc.def.ghi", false},
		{"two tokens", "abc\ndef", false},
	}
	for _, tt := range tests {
		tok, err := checkToken(tt.content)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err %v", tt.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "abc") {
			t.Errorf("%s: error leaks the token: %v", tt.name, err)
		}
		if tt.ok && tok != "abc.def.ghi" {
			t.Errorf("%s: token %q", tt.name, tok)
		}
	}
}

// The browser's time zone is the machine's, as /etc/localtime names it.
func TestLocalZone(t *testing.T) {
	dir := t.TempDir()
	link := func(name, target string) string {
		p := filepath.Join(dir, name)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	file := filepath.Join(dir, "copied")
	if err := os.WriteFile(file, []byte("TZif"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		link("nixos", "/etc/zoneinfo/Europe/Berlin"):                    "Europe/Berlin",
		link("debian", "../usr/share/zoneinfo/America/Argentina/Salta"): "America/Argentina/Salta",
		link("unknown", "/usr/share/zoneinfo/Mars/Olympus"):             "",
		link("elsewhere", "/opt/zones/Europe/Berlin"):                   "",
		file:                          "",
		filepath.Join(dir, "missing"): "",
	} {
		if got := localZone(path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}
