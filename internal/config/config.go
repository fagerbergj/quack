// Package config loads Quack's declarative YAML configuration. Structure
// (providers, stores, orchestrator model) lives in the YAML; secrets are
// interpolated from the environment via ${VAR}. Providers and stores are
// pluggable by a `kind` discriminator; M0 implements only `openai` + `postgres`.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level declarative configuration.
type Config struct {
	Providers    map[string]ProviderConfig `yaml:"providers"`
	Stores       StoresConfig              `yaml:"stores"`
	Orchestrator OrchestratorConfig        `yaml:"orchestrator"`
	Agents       map[string]AgentConfig    `yaml:"agents"`
	Tools        ToolsConfig               `yaml:"tools"`
	Adversarial  AdversarialConfig         `yaml:"adversarial"`
	Server       ServerConfig              `yaml:"server"`
}

// AdversarialConfig configures the trust gate that wraps every agent: a
// self-refine pre-pass followed by an independent judge that scores the answer
// against a standing rubric, looping up to MaxRounds or until Threshold is met.
// It is optional — when Model is empty the gate is disabled and agents are served
// unwrapped. The judge is platform-invoked (a dedicated model), so agent bundles
// stay simple.
type AdversarialConfig struct {
	Provider           string  `yaml:"provider"`             // inference provider for the judge model
	Model              string  `yaml:"model"`                // judge model (empty ⇒ vetting disabled)
	MaxRounds          int     `yaml:"max_rounds"`           // judge/revise rounds before giving up (default 2)
	Threshold          float64 `yaml:"threshold"`            // pass score in (0,1] (default 0.7)
	SelfRefine         bool    `yaml:"self_refine"`          // run the same-model self-refine pre-pass
	JudgeMaxIterations int     `yaml:"judge_max_iterations"` // cap on the agentic judge's model turns per round (default 6)
	ConstitutionPath   string  `yaml:"constitution_path"`    // path to global principles file (optional)
	Constitution       string  `yaml:"constitution"`         // inline constitution (alternative to constitution_path)
	RubricPath         string  `yaml:"rubric_path"`          // path to the default scoring guide
	Rubric             string  `yaml:"rubric"`               // inline rubric (alternative to rubric_path)
}

// Enabled reports whether the trust gate should wrap agents.
func (a AdversarialConfig) Enabled() bool { return a.Model != "" }

// AgentConfig binds a declarative agent bundle (a directory holding an
// agent-card.json + prompt.md) to a provider/model and a selection of built-in
// tools. Defining a new agent is adding a bundle directory plus one of these.
type AgentConfig struct {
	Bundle   string   `yaml:"bundle"`   // path to the agent bundle directory
	Provider string   `yaml:"provider"` // inference provider name
	Model    string   `yaml:"model"`    // model served to this agent
	Tools    []string `yaml:"tools"`    // built-in tool names (kind: builtin)
}

// ToolsConfig holds backend bindings for the built-in tools that need them.
type ToolsConfig struct {
	WebSearch ToolBackend `yaml:"web_search"`
	Fetch     ToolBackend `yaml:"web_fetch"`
}

// ToolBackend is the backend endpoints a built-in tool talks to. Both are
// keyless, internal services in M1.
type ToolBackend struct {
	Backend       string `yaml:"backend"`        // web_search: SearXNG base URL
	RenderBackend string `yaml:"render_backend"` // web_fetch: crawl4ai base URL
}

// ProviderConfig is one named inference provider. `kind` selects the adapter
// (the API protocol); the endpoint picks the actual server.
type ProviderConfig struct {
	Kind     string `yaml:"kind"`     // e.g. openai
	Endpoint string `yaml:"endpoint"` // OpenAI-compatible base URL
	APIKey   string `yaml:"api_key"`
}

// StoresConfig groups the store roles. M0 needs only the relational store.
type StoresConfig struct {
	Relational StoreConfig `yaml:"relational"`
}

// StoreConfig is one store backend; `kind` selects it (postgres in M0).
type StoreConfig struct {
	Kind string `yaml:"kind"`
	URL  string `yaml:"url"`
}

// OrchestratorConfig binds the orchestrator to a provider + model.
type OrchestratorConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Addr string `yaml:"addr"`
}

// Load reads the YAML at path, expands ${ENV} references, and validates it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	expanded := os.Expand(string(raw), os.Getenv)

	var c Config
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: no providers defined")
	}
	for name, p := range c.Providers {
		if p.Kind != "openai" {
			return fmt.Errorf("config: provider %q has unsupported kind %q (only %q is implemented)", name, p.Kind, "openai")
		}
		if p.Endpoint == "" {
			return fmt.Errorf("config: provider %q has empty endpoint", name)
		}
	}
	if _, ok := c.Providers[c.Orchestrator.Provider]; !ok {
		return fmt.Errorf("config: orchestrator.provider %q is not defined under providers", c.Orchestrator.Provider)
	}
	if c.Orchestrator.Model == "" {
		return fmt.Errorf("config: orchestrator.model is empty")
	}
	if c.Stores.Relational.Kind != "postgres" {
		return fmt.Errorf("config: stores.relational.kind %q unsupported (only %q is implemented)", c.Stores.Relational.Kind, "postgres")
	}
	if c.Stores.Relational.URL == "" {
		return fmt.Errorf("config: stores.relational.url is empty")
	}
	for name, a := range c.Agents {
		if _, ok := c.Providers[a.Provider]; !ok {
			return fmt.Errorf("config: agent %q provider %q is not defined under providers", name, a.Provider)
		}
		if a.Bundle == "" {
			return fmt.Errorf("config: agent %q has empty bundle path", name)
		}
		if a.Model == "" {
			return fmt.Errorf("config: agent %q has empty model", name)
		}
		// Tool names are resolved (and unknown ones rejected) when the agent's
		// tools are built at startup; see internal/tools.Build.
	}
	if c.Adversarial.Enabled() {
		if _, ok := c.Providers[c.Adversarial.Provider]; !ok {
			return fmt.Errorf("config: adversarial.provider %q is not defined under providers", c.Adversarial.Provider)
		}
		if c.Adversarial.ConstitutionPath != "" && c.Adversarial.Constitution != "" {
			return fmt.Errorf("config: adversarial sets both constitution_path and constitution; use one")
		}
		if c.Adversarial.RubricPath == "" && c.Adversarial.Rubric == "" {
			return fmt.Errorf("config: adversarial needs one of rubric_path or rubric")
		}
		if c.Adversarial.RubricPath != "" && c.Adversarial.Rubric != "" {
			return fmt.Errorf("config: adversarial sets both rubric_path and rubric; use one")
		}
		if c.Adversarial.MaxRounds == 0 {
			c.Adversarial.MaxRounds = 2
		}
		if c.Adversarial.MaxRounds < 1 {
			return fmt.Errorf("config: adversarial.max_rounds must be >= 1")
		}
		if c.Adversarial.Threshold == 0 {
			c.Adversarial.Threshold = 0.7
		}
		if c.Adversarial.Threshold <= 0 || c.Adversarial.Threshold > 1 {
			return fmt.Errorf("config: adversarial.threshold must be in (0,1]")
		}
		if c.Adversarial.JudgeMaxIterations == 0 {
			c.Adversarial.JudgeMaxIterations = 6
		}
		if c.Adversarial.JudgeMaxIterations < 1 {
			return fmt.Errorf("config: adversarial.judge_max_iterations must be >= 1")
		}
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	return nil
}

// Provider returns the named provider config.
func (c *Config) Provider(name string) (ProviderConfig, bool) {
	p, ok := c.Providers[name]
	return p, ok
}
