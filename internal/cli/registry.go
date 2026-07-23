// Package cli is the TUI-free surface of the quack client: the server-context
// registry (which server chat/api/-p talk to), the /models discovery used by
// `server init`, and the quack.yaml emitter. It imports no bubbletea and no huh
// - the wizard (internal/wizard) wraps these in forms; print mode and api shell
// out here directly. Keeping this package terminal-free is what lets the pipe
// paths stay ANSI-clean and unit-testable with httptest.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ClientConfig is the CLI's client-side state: which servers exist and which is
// active. Lives at ~/.quack/servers.yaml ($QUACK_HOME) - distinct from a
// server's quack.yaml (its runtime config, in cwd). Two files, two concerns.
type ClientConfig struct {
	Active  string               `yaml:"active,omitempty"`
	Servers map[string]ServerRef `yaml:"servers"`
}

// ServerRef is one registered server the CLI can talk to.
type ServerRef struct {
	URL  string      `yaml:"url"`
	Auth *ServerAuth `yaml:"auth,omitempty"` // set by `quack server login`; nil if the server needs no auth
}

// ServerAuth is a server's stored OIDC session (from `quack server login`'s
// device flow): enough for NewClient to attach a bearer token and silently
// refresh it via the token endpoint when it's near expiry, without
// re-running the browser/device flow. ClientID+Scopes+TokenURL are cached
// from login so a refresh needs no re-discovery. There is no client secret
// field - the login flow only supports public OIDC clients (device grant,
// no secret), so there is never one to persist.
type ServerAuth struct {
	Issuer       string    `yaml:"issuer"`
	ClientID     string    `yaml:"client_id"`
	Scopes       []string  `yaml:"scopes,omitempty"`
	TokenURL     string    `yaml:"token_url"`
	AccessToken  string    `yaml:"access_token"`
	RefreshToken string    `yaml:"refresh_token,omitempty"`
	Expiry       time.Time `yaml:"expiry,omitempty"`
}

// configPath is the registry location: ~/.quack/servers.yaml ($QUACK_HOME).
func configPath() string { return filepath.Join(Home(), "servers.yaml") }

// LoadClient reads the registry, returning an empty (not nil) config when absent.
func LoadClient() (*ClientConfig, error) {
	b, err := os.ReadFile(configPath())
	if os.IsNotExist(err) {
		return &ClientConfig{Servers: map[string]ServerRef{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read client config: %w", err)
	}
	var c ClientConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse client config: %w", err)
	}
	if c.Servers == nil {
		c.Servers = map[string]ServerRef{}
	}
	return &c, nil
}

// Save writes the registry, creating the config dir as needed. Both are
// private (0700/0600) - the registry can hold OIDC access/refresh tokens
// (ServerAuth) once `quack server login` has run, and this is the cheap way
// to avoid leaving them in a world-readable file.
func (c *ClientConfig) Save() error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal client config: %w", err)
	}
	return os.WriteFile(configPath(), b, 0o600)
}

// AddServer registers name→url, erroring on a duplicate name (use Remove first).
func (c *ClientConfig) AddServer(name, url string) error {
	if name == "" {
		return fmt.Errorf("server name is required")
	}
	if url == "" {
		return fmt.Errorf("server url is required")
	}
	if _, exists := c.Servers[name]; exists {
		return fmt.Errorf("server %q already exists (remove it first)", name)
	}
	c.Servers[name] = ServerRef{URL: url}
	return nil
}

// RemoveServer drops name. No-op (not an error) if absent, so `remove` is idempotent.
func (c *ClientConfig) RemoveServer(name string) {
	delete(c.Servers, name)
	if c.Active == name {
		c.Active = ""
	}
}

// Use sets the active server, erroring if it isn't registered.
func (c *ClientConfig) Use(name string) error {
	if _, ok := c.Servers[name]; !ok {
		return fmt.Errorf("server %q is not registered (add it first)", name)
	}
	c.Active = name
	return nil
}

// ActiveURL resolves the remote server to talk to: the --server override, else
// the active server's URL. Returns "" when no remote is configured - the signal
// that the command should run the duck locally in-process (no separate server).
func (c *ClientConfig) ActiveURL(override string) string {
	if override != "" {
		return override
	}
	if c.Active != "" {
		if s, ok := c.Servers[c.Active]; ok {
			return s.URL
		}
	}
	return ""
}

// findByURL returns the registered server (name + ref) whose URL matches url
// (trailing-slash-insensitive) - how NewClient discovers a stored OIDC
// session for the server it's about to talk to, whether url came from the
// active registry entry or a literal --server override that happens to name
// a registered server.
func (c *ClientConfig) findByURL(url string) (string, ServerRef, bool) {
	url = strings.TrimRight(url, "/")
	for name, ref := range c.Servers {
		if strings.TrimRight(ref.URL, "/") == url {
			return name, ref, true
		}
	}
	return "", ServerRef{}, false
}

// SetAuth attaches (or replaces) the stored OIDC session on a registered
// server. Errors if name isn't registered - `server login` requires `server
// add` first, same as `server use`.
func (c *ClientConfig) SetAuth(name string, auth *ServerAuth) error {
	ref, ok := c.Servers[name]
	if !ok {
		return fmt.Errorf("server %q is not registered (add it first)", name)
	}
	ref.Auth = auth
	c.Servers[name] = ref
	return nil
}
