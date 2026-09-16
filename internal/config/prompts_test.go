package config

import (
	"strings"
	"testing"
	"time"
)

// TestPromptsDefaultsToStatic: no prompts: block is today's behaviour - no
// store, so internal/artifactsrc resolves every artifact from the shipped file.
func TestPromptsDefaultsToStatic(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Prompts.Store != "" {
		t.Errorf("prompts.store = %q, want unset", c.Prompts.Store)
	}
	if got := c.Prompts.CacheTTLDuration(); got != 60*time.Second {
		t.Errorf("cache_ttl = %v, want 60s", got)
	}
	if got := c.Prompts.Label(); got != "production" {
		t.Errorf("pin_label = %q, want production", got)
	}
}

func TestPromptsLangfuseStore(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
  langfuse:
    kind: langfuse
    url: http://langfuse:3000
    public_key: pk-1
    secret_key: sk-1
session: { store: main }
orchestrator: { provider: default, model: m }
prompts:
  store: langfuse
  cache_ttl: 5s
  pin_label: staging
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Prompts.CacheTTLDuration() != 5*time.Second || c.Prompts.Label() != "staging" {
		t.Errorf("prompts = %+v", c.Prompts)
	}
	s, _ := c.Store("langfuse")
	if s.PublicKey != "pk-1" || s.SecretKey != "sk-1" {
		t.Errorf("langfuse store keys not read: %+v", s)
	}
}

func TestPromptsValidation(t *testing.T) {
	for _, c := range []struct{ name, yaml, want string }{
		{"unknown store", "\nprompts: { store: nope }\n", "not defined under stores"},
		{"wrong kind", "\nprompts: { store: main }\n", "must be a langfuse store"},
		{"bad ttl", "\nprompts: { cache_ttl: soon }\n", "not a duration"},
		{"zero ttl", "\nprompts: { cache_ttl: 0s }\n", "must be > 0"},
	} {
		_, err := Load(writeTemp(t, baseConfig+c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
}

// TestPromptsStoreNeedsCredentials: a half-populated langfuse store falls back
// to static on every name and looks exactly like Langfuse holding no prompts,
// so it must fail the config rather than boot into a silent no-op.
func TestPromptsStoreNeedsCredentials(t *testing.T) {
	const head = `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
  langfuse:
    kind: langfuse
`
	const tail = `
session: { store: main }
orchestrator: { provider: default, model: m }
prompts: { store: langfuse }
`
	for _, c := range []struct{ name, store, want string }{
		{"no url", "    public_key: pk\n    secret_key: sk\n", "empty url"},
		{"no public key", "    url: http://lf\n    secret_key: sk\n", "empty public_key"},
		{"no secret key", "    url: http://lf\n    public_key: pk\n", "empty secret_key"},
	} {
		_, err := Load(writeTemp(t, head+c.store+tail))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
}
