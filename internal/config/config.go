// Package config reads /etc/mandate/mandate.toml and names the fixed paths
// every mandate process uses.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Listen     string               `toml:"listen"`
	TLS        TLS                  `toml:"tls"`
	Connectors map[string]Connector `toml:"connectors"`
}

type TLS struct {
	Mode   string `toml:"mode"`   // own, acme or cert
	Domain string `toml:"domain"` // acme
	Cert   string `toml:"cert"`   // cert
	Key    string `toml:"key"`    // cert
}

type Connector struct {
	Command string `toml:"command"`
	// Config is passed to the connector as JSON through --config.
	Config map[string]any `toml:"config"`
}

// The name becomes the user mandate-NAME, which useradd caps at 32
// characters, and part of the connector's paths.
var connectorName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	cfg := Config{Listen: ":8443", TLS: TLS{Mode: "own"}}
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(names, ", "))
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	t := c.TLS
	switch t.Mode {
	case "own":
		if t.Domain != "" || t.Cert != "" || t.Key != "" {
			return errors.New(`tls: mode "own" takes no domain, cert or key`)
		}
	case "acme":
		if t.Domain == "" || t.Cert != "" || t.Key != "" {
			return errors.New(`tls: mode "acme" takes a domain and no cert or key`)
		}
	case "cert":
		if t.Domain != "" || t.Cert == "" || t.Key == "" {
			return errors.New(`tls: mode "cert" takes cert and key and no domain`)
		}
	default:
		return fmt.Errorf("tls: unknown mode %q, want own, acme or cert", t.Mode)
	}
	for name, conn := range c.Connectors {
		if !connectorName.MatchString(name) {
			return fmt.Errorf("connector %q: name must be a-z, 0-9 and -, start with a letter, at most 24 characters", name)
		}
		if slices.Contains([]string{"access", "connectors", "join", "proposals"}, name) {
			return fmt.Errorf("connector %q: the name is taken by an agent API route", name)
		}
		if !filepath.IsAbs(conn.Command) {
			return fmt.Errorf("connector %s: command must be an absolute path", name)
		}
		if conn.Config == nil {
			conn.Config = map[string]any{}
			c.Connectors[name] = conn
		}
	}
	return nil
}

// root prefixes the fixed paths; only tests set it.
func root(path string) string {
	return filepath.Join(os.Getenv("MANDATE_ROOT"), path)
}

// File is the config file every process reads.
func File() string { return root("/etc/mandate/mandate.toml") }

// StateDir is the state directory of front, gateway or owner.
func StateDir(process string) string { return root("/var/lib/mandate/" + process) }

// Socket is where gateway or owner serves HTTP.
func Socket(process string) string { return root("/run/mandate/" + process + ".sock") }

// SetupPasswordFile holds the setup password in plain text, for root only.
func SetupPasswordFile() string { return root("/var/lib/mandate/setup-password") }

// ConnectorDir is the connector's --state directory.
func ConnectorDir(name string) string { return root("/var/lib/mandate/connectors/" + name) }

// ConnectorSocket is the connector's --socket.
func ConnectorSocket(name string) string { return root("/run/mandate/connectors/" + name + ".sock") }
