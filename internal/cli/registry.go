// Package cli is the terminal-free client surface (server registry, /models discovery, quack.yaml emitter),
// so pipe paths stay ANSI-clean and testable with httptest; internal/wizard wraps it in forms.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ClientConfig is the CLI's server registry at ~/.quack/servers.yaml ($QUACK_HOME),
// distinct from a server's own quack.yaml.
type ClientConfig struct {
	Active  string               `yaml:"active,omitempty"`
	Servers map[string]ServerRef `yaml:"servers"`
}

// ServerRef is one registered server the CLI can talk to.
type ServerRef struct {
	URL  string      `yaml:"url"`
	Auth *ServerAuth `yaml:"auth,omitempty"` // set by `quack server login`; nil if the server needs no auth
}

// ServerAuth is a server's stored OIDC session, cached with ClientID/Scopes/TokenURL so a refresh needs
// no re-discovery. No client secret: login supports only public PKCE clients.
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

// Save writes the registry 0600 in a 0700 dir, creating it as needed:
// it can hold OIDC access/refresh tokens.
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
// Activates it when it's the first server registered.
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
	first := len(c.Servers) == 0
	c.Servers[name] = ServerRef{URL: url}
	if first {
		c.Active = name
	}
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

// ActiveURL resolves the server to talk to: the --server override, else the active server's URL.
// "" means no remote is configured, so the command runs the duck in-process.
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

// findByURL returns the registered server whose URL matches url (trailing-slash-insensitive),
// so a literal --server still finds its stored OIDC session.
func (c *ClientConfig) findByURL(url string) (string, ServerRef, bool) {
	url = strings.TrimRight(url, "/")
	for name, ref := range c.Servers {
		if strings.TrimRight(ref.URL, "/") == url {
			return name, ref, true
		}
	}
	return "", ServerRef{}, false
}

// SetAuth attaches or replaces a registered server's OIDC session;
// errors if name isn't registered (`server add` comes first).
func (c *ClientConfig) SetAuth(name string, auth *ServerAuth) error {
	ref, ok := c.Servers[name]
	if !ok {
		return fmt.Errorf("server %q is not registered (add it first)", name)
	}
	ref.Auth = auth
	c.Servers[name] = ref
	return nil
}
