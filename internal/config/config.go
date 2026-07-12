// Package config loads Quack's declarative YAML configuration. Structure
// (providers, stores, orchestrator model) lives in the YAML; secrets are
// interpolated from the environment via ${VAR}. Providers and stores are
// pluggable by a `kind` discriminator; M0 implements only `openai` + `postgres`.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

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
	Workspace    WorkspaceConfig           `yaml:"workspace"`  // agents' working disk (filesystem/git tools)
	Extensions   ExtensionsConfig          `yaml:"extensions"` // bundled inbound+outbound integrations (e.g. GitHub App)
}

// ExtensionsConfig holds the optional bundled integrations. Each is off unless
// its sub-section is present (a nil pointer = not built, no tools, no route).
type ExtensionsConfig struct {
	GitHub *GitHubExtensionConfig `yaml:"github"`
}

// GitHubExtensionConfig configures the GitHub App extension (internal/github).
// Secrets (private_key, webhook_secret) MUST be ${VAR} env references in the raw
// YAML — a literal is a startup error (validateNoLiteralTokens). Provide the
// private key EITHER inline via private_key (${VAR} whose value is the PEM) OR
// by private_key_path (a filesystem path to the .pem).
type GitHubExtensionConfig struct {
	AppID          int64  `yaml:"app_id"`
	PrivateKey     string `yaml:"private_key"`      // PEM contents via ${VAR}
	PrivateKeyPath string `yaml:"private_key_path"` // path to a .pem file (alternative to private_key)
	WebhookSecret  string `yaml:"webhook_secret"`   // ${VAR}
	Mention        string `yaml:"mention"`          // trigger phrase, default "@quack"
}

// defaultMention is the trigger phrase when github.mention is unset.
const defaultMention = "@quack"

// Workspace defaults (see WorkspaceConfig). Every field is optional; a
// config with no workspace: section at all still gets a working (default)
// filesystem jail rooted at ./workspace.
const (
	defaultWorkspaceRoot           = "./workspace"
	defaultWorkspaceMaxReadKB      = 256
	defaultWorkspaceMaxWriteKB     = 2048
	defaultWorkspaceMaxResults     = 200
	defaultWorkspaceMaxListEntries = 500
	defaultWorkspaceTimeoutSeconds = 60
)

// WorkspaceConfig is the agents' working disk: one configured root, with a
// per-user jail under it (<root>/<user_id>/ — see internal/workspace.Jail)
// that filesystem and git tools resolve every path through. Only root + the
// caps are consumed by this PR's filesystem tools; check_commands is parsed
// and validated for shape here but its ENFORCEMENT (the orchestrator-set
// deterministic gate checks) is a later PR — see .quack/plan-pr5-tool-schemas.md §4.
type WorkspaceConfig struct {
	Root string `yaml:"root"` // default ./workspace (compose: the volume mountpoint)
	// Caps — all optional with defaults below; a capped result sets
	// truncated:true, it never errors (write_file is the one exception: an
	// oversized write errors, since its result carries no truncated field).
	MaxReadKB      int `yaml:"max_read_kb"`      // default 256
	MaxWriteKB     int `yaml:"max_write_kb"`     // default 2048
	MaxResults     int `yaml:"max_results"`      // default 200 (grep/glob hits per call)
	MaxListEntries int `yaml:"max_list_entries"` // default 500 (list_dir entries per call)
	TimeoutSeconds int `yaml:"timeout_seconds"`  // default 60; per git/check invocation (later PRs)
	// CheckCommands are the allowed command PREFIXES the planner may complete
	// into per-node checks (§4 of the design doc). Empty (default) means checks
	// are unavailable; consumed by a later PR, not this one.
	CheckCommands []string `yaml:"check_commands"`
	// ExecPath lists extra directories appended (first) to the hermetic child
	// PATH for run_command, gate checks, and git children — the operator's
	// knob for toolchains living outside the fixed system dirs (nvm, asdf,
	// custom prefixes). Empty (default) = the fixed PATH alone.
	ExecPath []string `yaml:"exec_path"`
	// GitCredentials are deployment-level per-host HTTPS git credentials (one
	// identity per host — a PAT, configured like every other secret). Empty
	// (default) ⇒ public repos only. Token MUST be an ${VAR} env reference in
	// the raw YAML — see validateNoLiteralTokens.
	GitCredentials []GitCredentialConfig `yaml:"git_credentials"`
	// GitPush gates the git_push tool (default false) — the one outward-facing,
	// non-undoable git operation in the set.
	GitPush bool `yaml:"git_push"`
	// Guards maps a tool name to its guard-ladder tier: none (default,
	// unlisted) | judge | confirm | judge+confirm. See §4b of the design doc.
	Guards map[string]string `yaml:"guards"`
}

// GitCredentialConfig is one deployment-level per-host HTTPS git credential.
type GitCredentialConfig struct {
	Host     string `yaml:"host"`
	Username string `yaml:"username"` // default "x-access-token" (GitHub PATs use this)
	Token    string `yaml:"token"`    // MUST be ${VAR} in the raw YAML — never a literal secret
}

// defaultGitCredentialUsername is used when a git_credentials entry omits
// username (GitHub PATs authenticate with any non-empty username by
// convention; x-access-token is GitHub's own recommended placeholder).
const defaultGitCredentialUsername = "x-access-token"

// validGuardTiers are the only values workspace.guards may map a tool to.
var validGuardTiers = map[string]bool{"none": true, "judge": true, "confirm": true, "judge+confirm": true}

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

// GatesConfig configures the trust gate that wraps every agent, each stage with
// its own round budget (a stage with max_rounds 0 is skipped):
//
//   - deterministic_checks — free code checks (citation backing, length) that
//     drive cheap targeted revisions before any expensive stage runs.
//   - judge — an independent model scores the answer against the rubric and the
//     worker revises on a fail.
//
// The advisor (agents/advisor) is NOT a gate stage: it's the ask_advisor tool,
// which a worker calls at its own discretion (see internal/tools/ask_advisor.go).
// It reuses the judge's provider/model and is only registered on worker
// bundles whose tools: list it when the judge is enabled — not a separate
// gates.* toggle (see internal/serve's advisorAgent + resolveToolNames).
//
// The gate is optional: when no stage is active it is disabled and agents are
// served unwrapped. constitution/rubric are shared by the advisor and judge.
type GatesConfig struct {
	ConstitutionPath    string      `yaml:"constitution_path"`    // global principles file (optional)
	Constitution        string      `yaml:"constitution"`         // inline constitution (alternative to path)
	RubricPath          string      `yaml:"rubric_path"`          // scoring guide file
	Rubric              string      `yaml:"rubric"`               // inline rubric (alternative to path)
	DeterministicChecks StageConfig `yaml:"deterministic_checks"` // free citation/length checks + cheap revises
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
	return g.DeterministicChecks.MaxRounds > 0 || g.JudgeEnabled()
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
	JudgeRounds   int      `yaml:"judge_rounds"`   // per-agent judge/revise round budget; 0 ⇒ inherit gates.judge.max_rounds
}

// IsGated reports whether the agent runs under the trust gate (default true).
func (a AgentConfig) IsGated() bool { return a.Gated == nil || *a.Gated }

// ToolConfig configures one built-in tool. A tool with a dedicated external
// service declares it inline (`kind` + `url`, e.g. web_search→searxng). A tool
// backed by shared infrastructure references a named store (`store`) and may
// override its namespace/tuning (`collection`/`schema`/`top_k`/`min_score`) — the
// store provides the connection + adapter `kind`, so the tool needs no `kind`.
type ToolConfig struct {
	Kind       string      `yaml:"kind"`       // store-less tool: adapter selector (empty = default)
	URL        string      `yaml:"url"`        // store-less tool: backend endpoint
	Auth       *AuthConfig `yaml:"auth"`       // optional backend auth (e.g. web_search kind exa → Exa REST)
	Store      string      `yaml:"store"`      // store-backed tool: name of a stores[] entry
	Collection string      `yaml:"collection"` // vector namespace override
	Schema     string      `yaml:"schema"`     // relational namespace override
	TopK       int         `yaml:"top_k"`      // recall override
	MinScore   *float32    `yaml:"min_score"`  // recall override
}

// AuthConfig is how a tool's backend authenticates. `kind` selects the scheme and
// the remaining fields are scheme-specific. Only `api_key` is implemented today;
// `oauth` (client credentials + token URL) is a planned kind, added here without
// disturbing callers.
type AuthConfig struct {
	Kind   string `yaml:"kind"`    // api_key (oauth planned)
	APIKey string `yaml:"api_key"` // kind: api_key — the key value (interpolate from env)
}

// authKindAPIKey is the only implemented auth scheme.
const authKindAPIKey = "api_key"

// APIKey returns the configured API key when the tool uses api_key auth, else "".
func (t ToolConfig) APIKey() string {
	if t.Auth != nil && t.Auth.Kind == authKindAPIKey {
		return t.Auth.APIKey
	}
	return ""
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
// the store URL is empty (QUACK_QDRANT_URL unset ⇒ memory self-disables) — so a
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
	Addr     string `yaml:"addr"`
	Topology string `yaml:"topology"` // embedded | managed | external (empty ⇒ external)
}

// Topology values. Only "managed" changes serve's behaviour (it brings up the
// Postgres + Qdrant stores via an embedded compose file, waits healthy, then
// runs; `quack server stop` tears them down). "embedded" (sqlite, no containers)
// and "external" (user-managed stores) just run the process — the label exists
// for the `server init` wizard and docs; serve treats both as "just run".
const (
	TopologyEmbedded = "embedded"
	TopologyManaged  = "managed"
	TopologyExternal = "external"
)

// Managed reports whether serve should orchestrate the stores via docker compose.
func (s ServerConfig) Managed() bool { return s.Topology == TopologyManaged }

// literalTokenRe matches a raw secret mapping entry's value (before ${VAR}
// expansion), e.g. `  token: ${QUACK_GITHUB_TOKEN}` or `webhook_secret: "abc"`.
// Covers every field that must be an env reference, never a literal:
// git-credential `token`, plus the GitHub extension's `private_key` and
// `webhook_secret`. Only the value is captured.
var literalTokenRe = regexp.MustCompile(`(?m)^\s*(?:token|private_key|webhook_secret):\s*(.+?)\s*$`)

// envRefRe matches a bare ${VAR_NAME} env reference with nothing else around it.
var envRefRe = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// validateNoLiteralTokens is a MECHANICAL, syntactic check on the RAW config
// text (before os.Expand interpolates ${VAR} references) — a git credential's
// `token:` value must be exactly an ${VAR} env reference, so a literal secret
// pasted into quack.yaml is a startup error, never a silent leak. This must
// run on the raw text: by the time YAML is parsed, os.Expand has already
// replaced ${VAR} with its value (or "" if unset), so the distinction between
// "a literal token" and "an env reference" would already be lost.
func validateNoLiteralTokens(raw string) error {
	for _, m := range literalTokenRe.FindAllStringSubmatch(raw, -1) {
		val := strings.Trim(m[1], `"'`)
		if val == "" {
			continue // an empty token: line is not a literal secret
		}
		if !envRefRe.MatchString(val) {
			return fmt.Errorf("config: secret values (token / private_key / webhook_secret) must be an ${VAR} env reference, not a literal value (got %q)", m[1])
		}
	}
	return nil
}

// coderModelFallbackEnv/researcherModelEnv implement agents.code-implementer's
// documented model fallback (config/quack.yaml, §6 of
// .quack/plan-pr5-tool-schemas.md): QUACK_CODER_MODEL, or QUACK_RESEARCHER_MODEL
// when unset, so a deployment that hasn't picked a dedicated coder model still
// gets a working code-implementer for free. Go's os.Expand has no ${A:-$B}
// syntax (unlike docker-compose's ${VAR:-default}, which QUACK_MEDIA_MODEL
// uses for a hardcoded literal default), so the chaining is done in the
// mapping function itself.
const (
	coderModelFallbackEnv = "QUACK_CODER_MODEL"
	researcherModelEnv    = "QUACK_RESEARCHER_MODEL"
)

// expandEnv is Load's os.Expand mapping function: every ${VAR} resolves to
// its environment value, except QUACK_CODER_MODEL, which falls back to
// QUACK_RESEARCHER_MODEL when unset (see coderModelFallbackEnv).
func expandEnv(key string) string {
	if key == coderModelFallbackEnv {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return os.Getenv(researcherModelEnv)
	}
	return os.Getenv(key)
}

// Load reads the YAML at path, expands ${ENV} references, and validates it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := validateNoLiteralTokens(string(raw)); err != nil {
		return nil, err
	}
	expanded := os.Expand(string(raw), expandEnv)

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
		case "postgres", "qdrant", "sqlite":
		default:
			return fmt.Errorf("config: store %q has unsupported kind %q (known: postgres, qdrant, sqlite)", name, s.Kind)
		}
	}
	// Default + range-check vector-store recall tuning so consumers don't repeat it.
	// Applies to any vector backend that carries an embedder (qdrant or sqlite).
	for name, s := range c.Stores {
		if s.Embedder == nil || s.URL == "" {
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
	// Session: must reference a relational store (postgres or sqlite) with a URL.
	if ss, ok := c.Store(c.Session.Store); !ok {
		return fmt.Errorf("config: session.store %q is not defined under stores", c.Session.Store)
	} else if ss.Kind != "postgres" && ss.Kind != "sqlite" {
		return fmt.Errorf("config: session.store %q must be a postgres or sqlite store, got kind %q", c.Session.Store, ss.Kind)
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
		if g.DeterministicChecks.MaxRounds < 0 || g.Judge.MaxRounds < 0 {
			return fmt.Errorf("config: gates.*.max_rounds must be >= 0")
		}
		if g.ConstitutionPath != "" && g.Constitution != "" {
			return fmt.Errorf("config: gates sets both constitution_path and constitution; use one")
		}
		if g.RubricPath != "" && g.Rubric != "" {
			return fmt.Errorf("config: gates sets both rubric_path and rubric; use one")
		}
		// The advisor and judge score against the rubric; deterministic checks don't.
		if g.JudgeEnabled() && g.RubricPath == "" && g.Rubric == "" {
			return fmt.Errorf("config: gates needs one of rubric_path or rubric when judge is enabled")
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
	// where memory is actually built — a store with QUACK_QDRANT_URL unset self-disables.)
	for name, t := range c.Tools {
		if t.Store != "" {
			if _, ok := c.Store(t.Store); !ok {
				return fmt.Errorf("config: tool %q references unknown store %q", name, t.Store)
			}
		}
		if t.Auth != nil && t.Auth.Kind != authKindAPIKey {
			return fmt.Errorf("config: tool %q has unsupported auth kind %q (only %q is implemented)", name, t.Auth.Kind, authKindAPIKey)
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
	switch c.Server.Topology {
	case "", TopologyEmbedded, TopologyManaged, TopologyExternal:
		// "" behaves as external (just run); only managed orchestrates.
	default:
		return fmt.Errorf("config: server.topology %q is unknown (use embedded, managed, or external)", c.Server.Topology)
	}
	if err := c.Workspace.applyDefaults(); err != nil {
		return err
	}
	if err := c.Extensions.GitHub.applyDefaults(); err != nil {
		return err
	}
	return nil
}

// applyDefaults validates and defaults the GitHub extension config. A nil
// receiver (no extensions.github section) is a no-op — the extension is off.
func (g *GitHubExtensionConfig) applyDefaults() error {
	if g == nil {
		return nil
	}
	if g.AppID == 0 {
		return fmt.Errorf("config: extensions.github.app_id is required")
	}
	if g.PrivateKey == "" && g.PrivateKeyPath == "" {
		return fmt.Errorf("config: extensions.github needs one of private_key or private_key_path")
	}
	if g.PrivateKey != "" && g.PrivateKeyPath != "" {
		return fmt.Errorf("config: extensions.github sets both private_key and private_key_path; use one")
	}
	if g.WebhookSecret == "" {
		return fmt.Errorf("config: extensions.github.webhook_secret is required")
	}
	if g.Mention == "" {
		g.Mention = defaultMention
	}
	return nil
}

// applyDefaults fills in unset workspace caps and validates the ones that
// were set. Every field is optional — a config with no workspace: section at
// all still ends up with a fully-defaulted WorkspaceConfig.
func (w *WorkspaceConfig) applyDefaults() error {
	if w.Root == "" {
		w.Root = defaultWorkspaceRoot
	}
	if w.MaxReadKB == 0 {
		w.MaxReadKB = defaultWorkspaceMaxReadKB
	}
	if w.MaxWriteKB == 0 {
		w.MaxWriteKB = defaultWorkspaceMaxWriteKB
	}
	if w.MaxResults == 0 {
		w.MaxResults = defaultWorkspaceMaxResults
	}
	if w.MaxListEntries == 0 {
		w.MaxListEntries = defaultWorkspaceMaxListEntries
	}
	if w.TimeoutSeconds == 0 {
		w.TimeoutSeconds = defaultWorkspaceTimeoutSeconds
	}
	if w.MaxReadKB < 0 || w.MaxWriteKB < 0 || w.MaxResults < 0 || w.MaxListEntries < 0 || w.TimeoutSeconds < 0 {
		return fmt.Errorf("config: workspace caps must be >= 0")
	}
	for i, gc := range w.GitCredentials {
		if strings.TrimSpace(gc.Host) == "" {
			return fmt.Errorf("config: workspace.git_credentials[%d] has an empty host", i)
		}
		if gc.Username == "" {
			w.GitCredentials[i].Username = defaultGitCredentialUsername
		}
	}
	for tool, tier := range w.Guards {
		if !validGuardTiers[tier] {
			return fmt.Errorf("config: workspace.guards[%q] has unknown tier %q (want none, judge, confirm, or judge+confirm)", tool, tier)
		}
	}
	return nil
}

// Provider returns the named provider config.
func (c *Config) Provider(name string) (ProviderConfig, bool) {
	p, ok := c.Providers[name]
	return p, ok
}
