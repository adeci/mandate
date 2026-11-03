package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, tt := range []struct {
		name, toml string
		ok         bool
	}{
		{"empty", ``, true},
		{"connector", `
[connectors.whatsapp]
command = "/usr/lib/mandate/connectors/whatsapp"
config = { phone = "+4912345", retries = 3 }
`, true},
		{"acme", "[tls]\nmode = \"acme\"\ndomain = \"box.example\"", true},
		{"cert", "[tls]\nmode = \"cert\"\ncert = \"/c.pem\"\nkey = \"/k.pem\"", true},
		{"unknown top-level key", `listn = ":443"`, false},
		{"unknown tls key", "[tls]\nmode = \"own\"\ndomian = \"box.example\"", false},
		{"unknown connector key", "[connectors.signal]\ncommand = \"/bin/signal\"\nargs = []", false},
		{"acme without domain", "[tls]\nmode = \"acme\"", false},
		{"cert without key", "[tls]\nmode = \"cert\"\ncert = \"/c.pem\"", false},
		{"own with cert", "[tls]\ncert = \"/c.pem\"\nkey = \"/k.pem\"", false},
		{"unknown mode", "[tls]\nmode = \"self\"", false},
		{"listen without port", `listen = "box.example"`, false},
		{"connector name as path", "[connectors.\"../x\"]\ncommand = \"/bin/x\"", false},
		{"relative command", "[connectors.signal]\ncommand = \"signal\"", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mandate.toml")
			if err := os.WriteFile(path, []byte(tt.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if tt.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("accepted")
			}
		})
	}
}
