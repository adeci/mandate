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
	Listen      string               `toml:"listen"`
	AgentListen string               `toml:"agent_listen"`
	TLS         TLS                  `toml:"tls"`
	Connectors  map[string]Connector `toml:"connectors"`
}

type TLS struct {
	Mode   string `toml:"mode"`
	Domain string `toml:"domain"`
	Cert   string `toml:"cert"`
	Key    string `toml:"key"`
}

type Connector struct {
	Command string         `toml:"command"`
	Config  map[string]any `toml:"config"`
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
	if c.AgentListen != "" {
		if _, _, err := net.SplitHostPort(c.AgentListen); err != nil {
			return fmt.Errorf("agent_listen: %w", err)
		}
		if c.AgentListen == c.Listen {
			return errors.New("agent_listen: same as listen")
		}
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
		// Agent API routes and CLI commands share the namespace with connectors.
		if slices.Contains([]string{"access", "connectors", "help", "join", "login", "proposals", "request", "reset", "serve", "setup", "status", "store-acl"}, name) {
			return fmt.Errorf("connector %q: the name is taken by mandate itself", name)
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

func File() string { return root("/etc/mandate/mandate.toml") }

func StateDir(process string) string { return root("/var/lib/mandate/" + process) }

// Each process has a runtime directory of its own, since a dynamic user
// can't share one with others.
func Socket(process string) string { return root("/run/mandate/" + process + "/socket") }

func SetupPasswordFile() string { return root("/var/lib/mandate/setup-password") }

func ConnectorDir(name string) string { return root("/var/lib/mandate/connectors/" + name) }

func ConnectorSocket(name string) string { return root("/run/mandate/connectors/" + name + "/socket") }
