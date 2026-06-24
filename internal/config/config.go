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
	Stores       map[string]StoreConfig    `yaml:"stores"`  // named backend registry (like providers)
	Session      SessionConfig             `yaml:"session"` // ADK session/chat store + compaction
	Orchestrator OrchestratorConfig        `yaml:"orchestrator"`
	Agents       map[string]AgentConfig    `yaml:"agents"`
	Tools        map[string]ToolConfig     `yaml:"tools"`
	Gates        GatesConfig               `yaml:"gates"`
	Dag          DagConfig                 `yaml:"dag"`
	Server       ServerConfig              `yaml:"server"`
}

// SessionConfig binds the ADK session + chat persistence to a named store and
// holds context-compaction settings (compaction operates over session history).
type SessionConfig struct {
	Store      string           `yaml:"store"`      // name of a stores[] entry (a relational store)
	Schema     string           `yaml:"schema"`     // reserved: ADK's session service exposes no schema param yet
	Compaction CompactionConfig `yaml:"compaction"` // automatic context compaction
}

// ProviderModel binds a named provider to a model — used by a vector store's
// embedder and consolidation model.
type ProviderModel struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// CompactionConfig configures automatic context compaction. When enabled, every
// gated agent gets a BeforeModelCallback that prunes old tool outputs and, if a
// request is still over budget, summarises the older conversation via the named
// summariser model (ports sst/opencode's prune + compaction).
type CompactionConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Provider string `yaml:"provider"` // inference provider for the summariser model
	Model    string `yaml:"model"`    // summariser model
	Prune    *bool  `yaml:"prune"`    // blank old tool outputs before summarising (default true)
}

// PruneEnabled reports whether the cheap tool-output prune pass runs (default true).
func (c CompactionConfig) PruneEnabled() bool { return c.Prune == nil || *c.Prune }

// DagConfig tunes how the orchestrator's DAG is executed.
type DagConfig struct {
	// MaxActiveNodes caps how many nodes run concurrently (default 2). Nodes whose
	// dependencies are met still queue behind this cap, so a wide layer doesn't
	// fire many heavy model requests at the single worker at once.
	MaxActiveNodes int `yaml:"max_active_nodes"`
}

// GatesConfig configures the trust gate that wraps every agent. Three stages run
// lowest-effort-first, each with its own round budget (a stage with max_rounds 0
// is skipped):
//
//   - deterministic_checks — free code checks (citation backing, length) that
//     drive cheap targeted revisions before any expensive stage runs.
//   - self_critique — the worker critiques and revises its own draft.
//   - judge — an independent model scores the answer against the rubric and the
//     worker revises on a fail.
//
// The gate is optional: when no stage is active it is disabled and agents are
// served unwrapped. constitution/rubric are shared by self_critique and judge.
type GatesConfig struct {
	ConstitutionPath    string      `yaml:"constitution_path"`    // global principles file (optional)
	Constitution        string      `yaml:"constitution"`         // inline constitution (alternative to path)
	RubricPath          string      `yaml:"rubric_path"`          // scoring guide file
	Rubric              string      `yaml:"rubric"`               // inline rubric (alternative to path)
	DeterministicChecks StageConfig `yaml:"deterministic_checks"` // free citation/length checks + cheap revises
	SelfCritique        StageConfig `yaml:"self_critique"`        // worker self-improvement passes
	Judge               JudgeConfig `yaml:"judge"`                // expensive model-judge stage
}

// StageConfig is a gate stage bounded by a round budget. max_rounds 0 disables it.
type StageConfig struct {
	MaxRounds int `yaml:"max_rounds"`
}

// JudgeConfig configures the model-judge stage. Model empty (or max_rounds 0)
// disables the judge; the other stages can still run.
type JudgeConfig struct {
	Provider      string  `yaml:"provider"`       // inference provider for the judge model
	Model         string  `yaml:"model"`          // judge model (empty ⇒ judge disabled)
	MaxRounds     int     `yaml:"max_rounds"`     // judge/revise rounds
	Threshold     float64 `yaml:"threshold"`      // pass score in (0,1] (default 0.7)
	MaxIterations int     `yaml:"max_iterations"` // cap on the agentic judge's model turns per round (default 6)
}

// JudgeEnabled reports whether the model-judge stage runs.
func (g GatesConfig) JudgeEnabled() bool { return g.Judge.Model != "" && g.Judge.MaxRounds > 0 }

// Enabled reports whether the trust gate should wrap agents (any stage active).
func (g GatesConfig) Enabled() bool {
	return g.DeterministicChecks.MaxRounds > 0 || g.SelfCritique.MaxRounds > 0 || g.JudgeEnabled()
}

// AgentConfig binds a declarative agent bundle (a directory holding an
// agent-card.json + prompt.md) to a provider/model and a selection of built-in
// tools. Defining a new agent is adding a bundle directory plus one of these.
type AgentConfig struct {
	Bundle        string   `yaml:"bundle"`         // path to the agent bundle directory
	Provider      string   `yaml:"provider"`       // inference provider name
	Model         string   `yaml:"model"`          // model served to this agent
	ContextWindow int      `yaml:"context_window"` // model's per-request context window in tokens (0 ⇒ no compaction)
	Tools         []string `yaml:"tools"`          // built-in tool names (kind: builtin)
	Inputs        []string `yaml:"inputs"`         // accepted input modalities: "text", "image", "audio" (text assumed if empty)
	Gated         *bool    `yaml:"gated"`          // wrap in the trust gate? default true; set false for side-effecting/action agents
}

// IsGated reports whether the agent runs under the trust gate (default true).
func (a AgentConfig) IsGated() bool { return a.Gated == nil || *a.Gated }

// ToolConfig configures one built-in tool. A tool with a dedicated external
// service declares it inline (`kind` + `url`, e.g. web_search→searxng). A tool
// backed by shared infrastructure references a named store (`store`) and may
// override its namespace/tuning (`collection`/`schema`/`top_k`/`min_score`) — the
// store provides the connection + adapter `kind`, so the tool needs no `kind`.
type ToolConfig struct {
	Kind       string   `yaml:"kind"`       // store-less tool: adapter selector (empty = default)
	URL        string   `yaml:"url"`        // store-less tool: backend endpoint
	Store      string   `yaml:"store"`      // store-backed tool: name of a stores[] entry
	Collection string   `yaml:"collection"` // vector namespace override
	Schema     string   `yaml:"schema"`     // relational namespace override
	TopK       int      `yaml:"top_k"`      // recall override
	MinScore   *float32 `yaml:"min_score"`  // recall override
}

// ProviderConfig is one named inference provider. `kind` selects the adapter
// (the API protocol); the endpoint picks the actual server.
type ProviderConfig struct {
	Kind     string `yaml:"kind"`     // e.g. openai
	Endpoint string `yaml:"endpoint"` // OpenAI-compatible base URL
	APIKey   string `yaml:"api_key"`
}

// StoreConfig is one named backend in the stores registry. `kind` selects the
// adapter (the portability seam, like providers); `url` is its endpoint.
// `extends` inherits another store's fields (child overrides), so e.g. a document
// store can reuse a base postgres connection. The remaining fields are
// store-type-specific: a vector store carries an embedder + consolidation model
// and recall defaults; relational/search stores ignore them.
type StoreConfig struct {
	Kind          string         `yaml:"kind"`
	URL           string         `yaml:"url"`
	Extends       string         `yaml:"extends"`       // inherit fields from another named store
	Embedder      *ProviderModel `yaml:"embedder"`      // vector store: how text is vectorized
	Consolidation *ProviderModel `yaml:"consolidation"` // vector store: extract/vet/consolidate model
	TopK          int            `yaml:"top_k"`         // vector store: neighbours per recall/consolidation
	MinScore      *float32       `yaml:"min_score"`     // vector store: min cosine similarity for a recall hit
	Schema        string         `yaml:"schema"`        // relational namespace default (overridable per tool)
	Collection    string         `yaml:"collection"`    // vector namespace default (overridable per tool)
}

// Store resolves a named store, applying `extends` inheritance (parent fields
// first, child overrides). Returns false if the name (or an ancestor) is unknown
// or the extends chain cycles.
func (c *Config) Store(name string) (StoreConfig, bool) {
	return c.resolveStore(name, nil)
}

func (c *Config) resolveStore(name string, seen []string) (StoreConfig, bool) {
	s, ok := c.Stores[name]
	if !ok {
		return StoreConfig{}, false
	}
	if s.Extends == "" {
		return s, true
	}
	for _, n := range seen {
		if n == name {
			return StoreConfig{}, false // cycle
		}
	}
	parent, ok := c.resolveStore(s.Extends, append(seen, name))
	if !ok {
		return StoreConfig{}, false
	}
	return mergeStore(parent, s), true
}

// ResolvedMemory is a vector store resolved for a memory tool binding:
// connection + models + namespace, ready to pass to memory.New.
type ResolvedMemory struct {
	Kind          string
	URL           string
	Embedder      ProviderModel
	Consolidation ProviderModel
	Collection    string
	TopK          int
	MinScore      float32
}

// MemoryStore resolves the vector store bound to a memory tool (e.g.
// "stage_memory" → task memory, "commit_memory" → user memory). It returns false
// when the tool is unconfigured, its store is missing embedder/consolidation, or
// the store URL is empty (QDRANT_URL unset ⇒ memory self-disables) — so a
// qdrant-less run keeps working. Per-tool collection/top_k/min_score override the
// store defaults.
func (c *Config) MemoryStore(toolName string) (ResolvedMemory, bool) {
	t, ok := c.Tools[toolName]
	if !ok || t.Store == "" {
		return ResolvedMemory{}, false
	}
	s, ok := c.Store(t.Store)
	if !ok || s.URL == "" || s.Embedder == nil || s.Consolidation == nil {
		return ResolvedMemory{}, false
	}
	coll := s.Collection
	if t.Collection != "" {
		coll = t.Collection
	}
	if coll == "" {
		return ResolvedMemory{}, false // a memory tool needs a collection
	}
	topK := s.TopK
	if t.TopK != 0 {
		topK = t.TopK
	}
	minScore := float32(0.5)
	if s.MinScore != nil {
		minScore = *s.MinScore
	}
	if t.MinScore != nil {
		minScore = *t.MinScore
	}
	return ResolvedMemory{
		Kind: s.Kind, URL: s.URL,
		Embedder: *s.Embedder, Consolidation: *s.Consolidation,
		Collection: coll, TopK: topK, MinScore: minScore,
	}, true
}

// mergeStore overlays child's set (non-zero) fields onto parent.
func mergeStore(parent, child StoreConfig) StoreConfig {
	out := parent
	out.Extends = ""
	if child.Kind != "" {
		out.Kind = child.Kind
	}
	if child.URL != "" {
		out.URL = child.URL
	}
	if child.Embedder != nil {
		out.Embedder = child.Embedder
	}
	if child.Consolidation != nil {
		out.Consolidation = child.Consolidation
	}
	if child.TopK != 0 {
		out.TopK = child.TopK
	}
	if child.MinScore != nil {
		out.MinScore = child.MinScore
	}
	if child.Schema != "" {
		out.Schema = child.Schema
	}
	if child.Collection != "" {
		out.Collection = child.Collection
	}
	return out
}

// OrchestratorConfig binds the orchestrator to a provider + model and lists its
// optional tools. The orchestrator's core tools (plan/execute/clarify) are
// intrinsic; this list adds opt-in capabilities — notably, including
// `commit_memory` turns on user memory (the orchestrator then recalls + commits
// personal facts).
type OrchestratorConfig struct {
	Provider string   `yaml:"provider"`
	Model    string   `yaml:"model"`
	Tools    []string `yaml:"tools"`
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
	// Stores registry: every entry must resolve (extends acyclic) and use a
	// supported kind.
	for name := range c.Stores {
		s, ok := c.Store(name)
		if !ok {
			return fmt.Errorf("config: store %q has an unknown or cyclic extends", name)
		}
		switch s.Kind {
		case "postgres", "qdrant", "opensearch":
		default:
			return fmt.Errorf("config: store %q has unsupported kind %q (known: postgres, qdrant, opensearch)", name, s.Kind)
		}
	}
	// Default + range-check vector-store recall tuning so consumers don't repeat it.
	for name, s := range c.Stores {
		if s.Kind != "qdrant" || s.URL == "" {
			continue
		}
		if s.TopK == 0 {
			s.TopK = 5
		}
		if s.TopK < 1 {
			return fmt.Errorf("config: store %q top_k must be >= 1", name)
		}
		if s.MinScore == nil {
			d := float32(0.5)
			s.MinScore = &d
		}
		if *s.MinScore < 0 || *s.MinScore > 1 {
			return fmt.Errorf("config: store %q min_score must be in [0,1]", name)
		}
		c.Stores[name] = s
	}
	// Session: must reference a postgres store with a URL.
	if ss, ok := c.Store(c.Session.Store); !ok {
		return fmt.Errorf("config: session.store %q is not defined under stores", c.Session.Store)
	} else if ss.Kind != "postgres" {
		return fmt.Errorf("config: session.store %q must be a postgres store, got kind %q", c.Session.Store, ss.Kind)
	} else if ss.URL == "" {
		return fmt.Errorf("config: session.store %q has empty url", c.Session.Store)
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
	if c.Gates.Enabled() {
		g := &c.Gates
		if g.DeterministicChecks.MaxRounds < 0 || g.SelfCritique.MaxRounds < 0 || g.Judge.MaxRounds < 0 {
			return fmt.Errorf("config: gates.*.max_rounds must be >= 0")
		}
		if g.ConstitutionPath != "" && g.Constitution != "" {
			return fmt.Errorf("config: gates sets both constitution_path and constitution; use one")
		}
		if g.RubricPath != "" && g.Rubric != "" {
			return fmt.Errorf("config: gates sets both rubric_path and rubric; use one")
		}
		// self_critique and judge score against the rubric; deterministic checks don't.
		if (g.SelfCritique.MaxRounds > 0 || g.JudgeEnabled()) && g.RubricPath == "" && g.Rubric == "" {
			return fmt.Errorf("config: gates needs one of rubric_path or rubric when self_critique or judge is enabled")
		}
		if g.JudgeEnabled() {
			if _, ok := c.Providers[g.Judge.Provider]; !ok {
				return fmt.Errorf("config: gates.judge.provider %q is not defined under providers", g.Judge.Provider)
			}
			if g.Judge.Threshold == 0 {
				g.Judge.Threshold = 0.7
			}
			if g.Judge.Threshold <= 0 || g.Judge.Threshold > 1 {
				return fmt.Errorf("config: gates.judge.threshold must be in (0,1]")
			}
			if g.Judge.MaxIterations == 0 {
				g.Judge.MaxIterations = 6
			}
			if g.Judge.MaxIterations < 1 {
				return fmt.Errorf("config: gates.judge.max_iterations must be >= 1")
			}
		}
	}
	if c.Session.Compaction.Enabled {
		cc := c.Session.Compaction
		if _, ok := c.Providers[cc.Provider]; !ok {
			return fmt.Errorf("config: session.compaction.provider %q is not defined under providers", cc.Provider)
		}
		if cc.Model == "" {
			return fmt.Errorf("config: session.compaction.enabled is true but session.compaction.model is empty")
		}
	}
	// Tools: a store-backed tool must reference a defined store. (Embedder /
	// consolidation on the referenced vector store are validated at wiring time,
	// where memory is actually built — a store with QDRANT_URL unset self-disables.)
	for name, t := range c.Tools {
		if t.Store != "" {
			if _, ok := c.Store(t.Store); !ok {
				return fmt.Errorf("config: tool %q references unknown store %q", name, t.Store)
			}
		}
	}
	if c.Dag.MaxActiveNodes == 0 {
		c.Dag.MaxActiveNodes = 2
	}
	if c.Dag.MaxActiveNodes < 1 {
		return fmt.Errorf("config: dag.max_active_nodes must be >= 1")
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
