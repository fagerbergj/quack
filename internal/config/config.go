// Package config loads Quack's YAML configuration. Secrets interpolated from env via ${VAR}.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Providers    map[string]ProviderConfig `yaml:"providers"`
	Stores       map[string]StoreConfig    `yaml:"stores"`
	Session      SessionConfig             `yaml:"session"`
	Orchestrator OrchestratorConfig        `yaml:"orchestrator"`
	Agents       map[string]AgentConfig    `yaml:"agents"`
	Tools        map[string]ToolConfig     `yaml:"tools"`
	Gates        GatesConfig               `yaml:"gates"`
	Dag          DagConfig                 `yaml:"dag"`
	Server       ServerConfig              `yaml:"server"`
	Workspace    WorkspaceConfig           `yaml:"workspace"`
	Skills       SkillsConfig              `yaml:"skills"`
	// Workflows is a top-level key, not nested under skills: - it's a
	// binding mechanism onto the DAG planner, not a skill-library concern
	// (skills.plugins is a different axis entirely).
	Workflows     []WorkflowShape     `yaml:"workflows"`
	Extensions    ExtensionsConfig    `yaml:"extensions"`
	Observability ObservabilityConfig `yaml:"observability"`
	Auth          *InboundAuthConfig  `yaml:"auth"`
	Artifacts     ArtifactsConfig     `yaml:"artifacts"`
	// Revision identifies the loaded config's content (sha256 of the raw file,
	// short form) - a deployment-authored workflow shape's provenance stamps
	// this as its version, so a shape changes version only when quack.yaml does.
	Revision string `yaml:"-"`
}

// SkillsConfig names skill-library plugin roots beyond quack's own shipped
// skills/ - each resolved at startup via internal/plugin's Agent Plugins /
// Codex discovery order. A root that fails to resolve is a startup warning,
// never an error. Order is preserved and never deduped.
type SkillsConfig struct {
	Plugins []string `yaml:"plugins"`
}

// WorkflowShape teaches plan-work's "Common workflows" table a deployment-
// specific DAG shape (issue #805) - a house-standard node chain (document
// ingestion, reMarkable notes, ...) that isn't in the shipped catalog.
// Trigger and Shape render as the table's two columns verbatim; Agents is
// the subset of that prose the config layer can actually validate.
//
// Nodes is optional (workflow binding): when present, a dispatch
// naming this shape gets Nodes built into a dag.Plan directly - no planner
// LLM call - instead of Trigger/Shape staying a planner hint. Trigger/Shape
// still render in the table either way, so the shape stays discoverable to
// an ordinary chat request too.
type WorkflowShape struct {
	Name    string         `yaml:"name"`    // short id for logs/warnings; also the future storage key (#806)
	Trigger string         `yaml:"trigger"` // "Request" column - when this shape applies
	Shape   string         `yaml:"shape"`   // "DAG shape" column - node chain + what the terminal node produces
	Agents  []string       `yaml:"agents"`  // every agent name Shape mentions, checked against Agents below
	Nodes   []WorkflowNode `yaml:"nodes,omitempty"`
}

// WorkflowNode is one node of a bound workflow shape's fixed DAG. Task may
// contain the literal token "{{ask}}", substituted verbatim with the
// dispatching Ask.Message - the only templating this supports, deliberately.
type WorkflowNode struct {
	ID        string   `yaml:"id"`
	Agent     string   `yaml:"agent"`
	Task      string   `yaml:"task"`
	DependsOn []string `yaml:"depends_on,omitempty"`
	Rubric    string   `yaml:"rubric,omitempty"`
}

// validateWorkflows drops structurally incomplete shapes with a warning
// (test case 4: never takes down planning) but hard-fails the whole config
// when a structurally valid shape names an agent that isn't configured (test
// case 3: never let a plan reach a node the executor can't run), or when a
// bound shape's node list is malformed - a shape a dispatch can bind to must
// never fail loud only at dispatch time.
func (c *Config) validateWorkflows() error {
	valid := make([]WorkflowShape, 0, len(c.Workflows))
	for i, w := range c.Workflows {
		id := w.Name
		if id == "" {
			id = fmt.Sprintf("workflows[%d]", i)
		}
		if w.Name == "" || w.Trigger == "" || w.Shape == "" || len(w.Agents) == 0 {
			slog.Warn("config: skipping malformed workflow shape (needs name, trigger, shape, and at least one agent); catalog composition continues",
				"component", "config", "shape", id)
			continue
		}
		for _, a := range w.Agents {
			if _, ok := c.Agents[a]; !ok {
				return fmt.Errorf("config: workflow shape %q names agent %q which is not configured under agents", w.Name, a)
			}
		}
		if len(w.Nodes) > 0 {
			if err := validateWorkflowNodes(w.Name, w.Nodes, c.Agents); err != nil {
				return err
			}
		}
		valid = append(valid, w)
	}
	c.Workflows = valid
	return nil
}

// validateWorkflowNodes checks a bound shape's node list is a well-formed,
// acyclic DAG naming only configured agents - the whole structural surface a
// dispatch's Bind() will later rely on without re-checking.
func validateWorkflowNodes(shapeName string, nodes []WorkflowNode, agents map[string]AgentConfig) error {
	ids := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.ID == "" || n.Agent == "" || n.Task == "" {
			return fmt.Errorf("config: workflow shape %q: every bound node needs id, agent, and task", shapeName)
		}
		if ids[n.ID] {
			return fmt.Errorf("config: workflow shape %q: duplicate bound node id %q", shapeName, n.ID)
		}
		ids[n.ID] = true
		if _, ok := agents[n.Agent]; !ok {
			return fmt.Errorf("config: workflow shape %q: bound node %q names agent %q which is not configured under agents", shapeName, n.ID, n.Agent)
		}
	}
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("config: workflow shape %q: bound node %q depends on unknown node %q", shapeName, n.ID, dep)
			}
		}
	}
	if !workflowNodesAcyclic(nodes) {
		return fmt.Errorf("config: workflow shape %q: bound nodes contain a dependency cycle", shapeName)
	}
	return nil
}

// workflowNodesAcyclic: Kahn's algorithm - every node must reach zero
// in-degree, or some subset only depends on itself/each other (a cycle).
func workflowNodesAcyclic(nodes []WorkflowNode) bool {
	indeg := make(map[string]int, len(nodes))
	dependents := map[string][]string{}
	for _, n := range nodes {
		indeg[n.ID] = len(n.DependsOn)
		for _, d := range n.DependsOn {
			dependents[d] = append(dependents[d], n.ID)
		}
	}
	queue := make([]string, 0, len(nodes))
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	placed := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		placed++
		for _, dep := range dependents[id] {
			indeg[dep]--
			if indeg[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	return placed == len(nodes)
}

var defaultSkillPlugins = []string{".agents/vendor/dotagents", ".agents/vendor/ponytail"}

type ObservabilityConfig struct {
	Otel      OtelConfig      `yaml:"otel"`
	Recording RecordingConfig `yaml:"recording"`
	// ADKDebug mounts ADK's REST debug console at /debug/adk. DANGER: /run,
	// /run_sse and /run_live execute any loaded agent WITHOUT the trust gate -
	// protected only by quack's auth + this flag. MUST stay off in production.
	ADKDebug bool `yaml:"adk_debug"`
}

type RecordingConfig struct {
	Enabled       *bool  `yaml:"enabled"`
	Store         string `yaml:"store"`
	RetentionDays int    `yaml:"retention_days"`
	CloneSnapshot bool   `yaml:"clone_snapshot"`
}

func (r RecordingConfig) IsEnabled(otelEnabled bool) bool {
	if !otelEnabled {
		return false
	}
	if r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

// ArtifactsConfig selects the artifact.Service backend, always wired. Empty
// Store means in-memory (lost on restart); a named store must be postgres
// (config.validate enforces it) - its large-object backend, durable.
type ArtifactsConfig struct {
	Store string `yaml:"store"`
}

type InboundAuthConfig struct {
	OIDC           *OIDCConfig           `yaml:"oidc"`
	TrustedHeaders *TrustedHeadersConfig `yaml:"trusted_headers"`
}

type OIDCConfig struct {
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`
	JWKSURL  string `yaml:"jwks_url"`
}

type TrustedHeadersConfig struct {
	User   string `yaml:"user"`
	Groups string `yaml:"groups"`
}

type OtelConfig struct {
	Enabled      *bool   `yaml:"enabled"`
	OTLPEndpoint string  `yaml:"otlp_endpoint"`
	Sample       float64 `yaml:"sample"`
	// Logs opts into OTLP log export. Off by default: logs need a collector
	// logs pipeline most deployments don't run (slog/stdout is the sink).
	Logs bool `yaml:"logs"`
}

const otelDefaultSample = 1.0

func (o OtelConfig) IsEnabled() bool { return o.Enabled == nil || *o.Enabled }

func (o *OtelConfig) applyDefaults() error {
	if o.Sample == 0 {
		o.Sample = otelDefaultSample
	}
	if o.Sample < 0 || o.Sample > 1 {
		return fmt.Errorf("config: otel.sample must be in (0,1]")
	}
	return nil
}

// ExtensionsConfig is the extensions: block - every top-level key is opaque
// (internal/serve resolves each against sdk.Registered() and hands the raw
// node to its Factory). GitHub used to be typed and strict here
// (GitHubExtensionConfig); now that internal/github is an SDK module like
// any other, its config lives entirely in quack-extensions/github and quack
// itself never parses it.
type ExtensionsConfig struct {
	Modules map[string]yaml.Node `yaml:",inline"`
}

const (
	defaultWorkspaceRoot           = "./workspace"
	defaultWorkspaceMaxReadKB      = 256
	defaultWorkspaceMaxWriteKB     = 2048
	defaultWorkspaceMaxResults     = 200
	defaultWorkspaceMaxListEntries = 500
	defaultWorkspaceTimeoutSeconds = 60
	defaultWorkspaceSandbox        = "bwrap"
	defaultWorkspaceAddressSpaceMB = 8192
	defaultWorkspaceMaxProcs       = 512
	defaultWorkspaceMaxFileSizeMB  = 1024
	defaultGCChatTTLHours          = 168
	defaultGCScratchTTLHours       = 6
	defaultGCIntervalHours         = 1
	defaultGCHomeMaxMB             = 500
)

var defaultCheckCommands = []string{"go build", "go vet", "go test", "npm run", "npm test", "npx tsc", "make", "gofmt", "npx prettier", "./gradlew"}

type WorkspaceConfig struct {
	Root           string                `yaml:"root"`
	MaxReadKB      int                   `yaml:"max_read_kb"`
	MaxWriteKB     int                   `yaml:"max_write_kb"`
	MaxResults     int                   `yaml:"max_results"`
	MaxListEntries int                   `yaml:"max_list_entries"`
	TimeoutSeconds int                   `yaml:"timeout_seconds"`
	CheckCommands  []string              `yaml:"check_commands"`
	CheckSetup     []string              `yaml:"check_setup"`
	ExecPath       []string              `yaml:"exec_path"`
	Env            map[string]string     `yaml:"env"`
	GitCredentials []GitCredentialConfig `yaml:"git_credentials"`
	Guards         map[string]string     `yaml:"guards"`
	Sandbox        string                `yaml:"sandbox"`
	Limits         WorkspaceLimits       `yaml:"limits"`
	GC             WorkspaceGCConfig     `yaml:"gc"`
}

type WorkspaceGCConfig struct {
	Enabled         *bool `yaml:"enabled"`
	ChatTTLHours    int   `yaml:"chat_ttl_hours"`
	ScratchTTLHours int   `yaml:"scratch_ttl_hours"`
	IntervalHours   int   `yaml:"interval_hours"`
	// HomeMaxMB bounds the ACP agent's shared $HOME (opencode.db, snapshot,
	// tool-output, log) - the one directory nothing else ever collects. Reset
	// whole, only when none of the user's chats have a round in flight.
	HomeMaxMB int `yaml:"home_max_mb"`
}

func (g WorkspaceGCConfig) IsEnabled() bool { return g.Enabled == nil || *g.Enabled }

type WorkspaceLimits struct {
	AddressSpaceMB int `yaml:"address_space_mb"`
	MaxProcs       int `yaml:"max_procs"`
	MaxFileSizeMB  int `yaml:"max_file_size_mb"`
}

type GitCredentialConfig struct {
	Host     string `yaml:"host"`
	Username string `yaml:"username"`
	Token    string `yaml:"token"`
}

const defaultGitCredentialUsername = "x-access-token"

var validGuardTiers = map[string]bool{"none": true, "judge": true, "confirm": true, "judge+confirm": true}

type SessionConfig struct {
	Store      string           `yaml:"store"`
	Compaction CompactionConfig `yaml:"compaction"`
}

type ProviderModel struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type CompactionConfig struct {
	Enabled            bool   `yaml:"enabled"`
	Provider           string `yaml:"provider"`
	Model              string `yaml:"model"`
	TokenThreshold     int    `yaml:"token_threshold"`
	EventRetentionSize int    `yaml:"event_retention_size"`
}

type DagConfig struct {
	MaxActiveNodes int `yaml:"max_active_nodes"`
	MaxActiveRuns  int `yaml:"max_active_runs"`
}

type GatesConfig struct {
	ConstitutionPath    string      `yaml:"constitution_path"`
	Constitution        string      `yaml:"constitution"`
	RubricPath          string      `yaml:"rubric_path"`
	Rubric              string      `yaml:"rubric"`
	DeterministicChecks StageConfig `yaml:"deterministic_checks"`
	Judge               JudgeConfig `yaml:"judge"`
}

type StageConfig struct {
	MaxRounds int `yaml:"max_rounds"`
}

type JudgeConfig struct {
	Provider      string  `yaml:"provider"`
	Model         string  `yaml:"model"`
	MaxRounds     int     `yaml:"max_rounds"`
	Threshold     float64 `yaml:"threshold"`
	MaxIterations int     `yaml:"max_iterations"`
	ContextWindow int     `yaml:"context_window"`
	Skeptics      int     `yaml:"skeptics"`
}

func (g GatesConfig) JudgeEnabled() bool { return g.Judge.Model != "" && g.Judge.MaxRounds > 0 }

func (g GatesConfig) Enabled() bool {
	return g.DeterministicChecks.MaxRounds > 0 || g.JudgeEnabled()
}

type AgentConfig struct {
	Bundle        string          `yaml:"bundle"`
	Provider      string          `yaml:"provider"`
	Model         string          `yaml:"model"`
	ContextWindow int             `yaml:"context_window"`
	Tools         []string        `yaml:"tools"`
	Inputs        []string        `yaml:"inputs"`
	Gated         *bool           `yaml:"gated"`
	JudgeRounds   int             `yaml:"judge_rounds"`
	Judge         *bool           `yaml:"judge"`
	Memory        MemoryConfig    `yaml:"memory"`
	Skills        []string        `yaml:"skills"`
	Acp           *AcpAgentConfig `yaml:"acp"`
}

type MemoryConfig struct {
	Bucket string `yaml:"bucket"`
}

type AcpAgentConfig struct {
	Command    []string          `yaml:"command"`
	Env        map[string]string `yaml:"env"`
	McpServers []string          `yaml:"mcp_servers"`
	ReadOnly   bool              `yaml:"read_only"`
}

func (a AgentConfig) IsGated() bool { return a.Gated == nil || *a.Gated }

type ToolConfig struct {
	Kind       string      `yaml:"kind"`
	URL        string      `yaml:"url"`
	Auth       *AuthConfig `yaml:"auth"`
	Store      string      `yaml:"store"`
	Collection string      `yaml:"collection"`
	TopK       int         `yaml:"top_k"`
	MinScore   *float32    `yaml:"min_score"`
}

type AuthConfig struct {
	Kind   string `yaml:"kind"`
	APIKey string `yaml:"api_key"`
}

const authKindAPIKey = "api_key"

func (t ToolConfig) APIKey() string {
	if t.Auth != nil && t.Auth.Kind == authKindAPIKey {
		return t.Auth.APIKey
	}
	return ""
}

type ProviderConfig struct {
	Kind     string          `yaml:"kind"`
	Endpoint string          `yaml:"endpoint"`
	APIKey   string          `yaml:"api_key"`
	Bundle   string          `yaml:"bundle"`
	ForkMode string          `yaml:"fork_mode"`
	ForkFrom string          `yaml:"fork_from"`
	Live     *ProviderConfig `yaml:"live"`
	// Models: optional per-model USD price table, keyed by model name as
	// passed to inference.NewModel. A model absent here gets no cost metric -
	// quack never guesses a price.
	Models map[string]ModelPricing `yaml:"models"`
}

// ModelPricing: USD per million tokens. Zero value (a model absent from
// ProviderConfig.Models) means "no pricing configured", not "free".
type ModelPricing struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
}

type StoreConfig struct {
	Kind          string               `yaml:"kind"`
	URL           string               `yaml:"url"`
	Extends       string               `yaml:"extends"`
	Embedder      *ProviderModel       `yaml:"embedder"`
	Consolidation *ConsolidationConfig `yaml:"consolidation"`
	TopK          int                  `yaml:"top_k"`
	MinScore      *float32             `yaml:"min_score"`
	Collection    string               `yaml:"collection"`
	Root          string               `yaml:"root"`
}

// ConsolidationConfig binds the model driving both the gated-commit reconcile
// and the periodic sweep (design doc docs/memory-lifecycle.md §4(c)), plus
// the sweep's own schedule. IntervalMinutes 0/absent disables the periodic
// sweep entirely - matching ledger.RunRetentionSweep's retentionDays<=0
// convention, not this struct's own RetentionDays. RetentionDays 0 keeps
// invalidated points and memory_ops rows forever (explicit, not implicit -
// see design doc §6).
type ConsolidationConfig struct {
	Provider        string `yaml:"provider"`
	Model           string `yaml:"model"`
	IntervalMinutes int    `yaml:"interval_minutes"`
	RetentionDays   int    `yaml:"retention_days"`
}

const defaultLedgerRoot = "./recordings"

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
			return StoreConfig{}, false
		}
	}
	parent, ok := c.resolveStore(s.Extends, append(seen, name))
	if !ok {
		return StoreConfig{}, false
	}
	return mergeStore(parent, s), true
}

type ResolvedMemory struct {
	Kind          string
	URL           string
	Embedder      ProviderModel
	Consolidation ConsolidationConfig
	Collection    string
	TopK          int
	MinScore      float32
}

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
		return ResolvedMemory{}, false
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
	if child.Collection != "" {
		out.Collection = child.Collection
	}
	return out
}

type OrchestratorConfig struct {
	Provider       string               `yaml:"provider"`
	Model          string               `yaml:"model"`
	Tools          []string             `yaml:"tools"`
	Skills         []string             `yaml:"skills"`
	UserMemoryHook UserMemoryHookConfig `yaml:"user_memory_hook"`
}

type UserMemoryHookConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type ServerConfig struct {
	Addr     string `yaml:"addr"`
	Topology string `yaml:"topology"`
	// ShutdownGraceSeconds bounds how long SIGTERM waits for in-flight runs
	// to finish before force-cancelling them (see serve.DrainActiveRuns).
	ShutdownGraceSeconds int `yaml:"shutdown_grace_seconds"`
}

const (
	TopologyEmbedded = "embedded"
	TopologyManaged  = "managed"
	TopologyExternal = "external"
)

func (s ServerConfig) Managed() bool { return s.Topology == TopologyManaged }

var literalTokenRe = regexp.MustCompile(`(?m)^\s*(?:token|private_key|webhook_secret):\s*(.+?)\s*$`)

var envRefRe = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// validateNoLiteralTokens checks raw config for secret values that must be ${VAR} references.
func validateNoLiteralTokens(raw string) error {
	for _, m := range literalTokenRe.FindAllStringSubmatch(raw, -1) {
		val := strings.Trim(m[1], `"'`)
		if val == "" {
			continue
		}
		if !envRefRe.MatchString(val) {
			return fmt.Errorf("config: secret values (token / private_key / webhook_secret) must be an ${VAR} env reference, not a literal value (got %q)", m[1])
		}
	}
	return nil
}

var knownRenames = map[string]string{
	"memory_role": "memory.bucket",
}

func scanForKnownRenames(raw string) error {
	for oldKey := range knownRenames {
		re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(oldKey) + `:`)
		if re.MatchString(raw) {
			return fmt.Errorf("config: unknown field %q — use %s instead", oldKey, knownRenames[oldKey])
		}
	}
	return nil
}

const (
	coderModelFallbackEnv = "QUACK_CODER_MODEL"
	researcherModelEnv    = "QUACK_RESEARCHER_MODEL"
)

func expandEnv(key string) string {
	if key == coderModelFallbackEnv {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return os.Getenv(researcherModelEnv)
	}
	return os.Getenv(key)
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := validateNoLiteralTokens(string(raw)); err != nil {
		return nil, err
	}
	if err := scanForKnownRenames(string(raw)); err != nil {
		return nil, err
	}
	expanded := os.Expand(string(raw), expandEnv)

	var c Config
	dec := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	c.Revision = hex.EncodeToString(sum[:])[:12]
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
		switch p.Kind {
		case "openai":
			if p.Endpoint == "" {
				return fmt.Errorf("config: provider %q has empty endpoint", name)
			}
		case "replay":
			if p.Bundle == "" {
				return fmt.Errorf("config: provider %q (kind %q) has empty bundle", name, "replay")
			}
			switch p.ForkMode {
			case "", "fork":
			default:
				return fmt.Errorf("config: provider %q has unsupported fork_mode %q (only \"\" and \"fork\")", name, p.ForkMode)
			}
			if p.ForkMode == "fork" {
				if p.Live == nil {
					return fmt.Errorf("config: provider %q has fork_mode: fork but no live provider config", name)
				}
				if p.Live.Kind != "openai" {
					return fmt.Errorf("config: provider %q live config must be kind %q, got %q", name, "openai", p.Live.Kind)
				}
				if p.Live.Endpoint == "" {
					return fmt.Errorf("config: provider %q live config has empty endpoint", name)
				}
			}
		default:
			return fmt.Errorf("config: provider %q has unsupported kind %q (only %q and %q are implemented)", name, p.Kind, "openai", "replay")
		}
	}
	if _, ok := c.Providers[c.Orchestrator.Provider]; !ok {
		return fmt.Errorf("config: orchestrator.provider %q is not defined under providers", c.Orchestrator.Provider)
	}
	if c.Orchestrator.Model == "" {
		return fmt.Errorf("config: orchestrator.model is empty")
	}
	if c.Orchestrator.UserMemoryHook.Enabled {
		h := c.Orchestrator.UserMemoryHook
		if _, ok := c.Providers[h.Provider]; !ok {
			return fmt.Errorf("config: orchestrator.user_memory_hook.provider %q is not defined under providers", h.Provider)
		}
		if h.Model == "" {
			return fmt.Errorf("config: orchestrator.user_memory_hook.model is empty")
		}
	}
	for name, a := range c.Agents {
		switch a.Memory.Bucket {
		case "", "coding", "research":
		default:
			return fmt.Errorf("config: agent %q has unknown memory.bucket %q (known: coding, research)", name, a.Memory.Bucket)
		}
	}
	for name := range c.Stores {
		s, ok := c.Store(name)
		if !ok {
			return fmt.Errorf("config: store %q has an unknown or cyclic extends", name)
		}
		switch s.Kind {
		case "postgres", "qdrant", "sqlite", "filesystem":
		default:
			return fmt.Errorf("config: store %q has unsupported kind %q (known: postgres, qdrant, sqlite, filesystem)", name, s.Kind)
		}
	}
	for name, s := range c.Stores {
		if s.Kind != "filesystem" {
			continue
		}
		if s.Root == "" {
			s.Root = defaultLedgerRoot
			c.Stores[name] = s
		}
	}
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
	for name, s := range c.Stores {
		if s.Consolidation == nil {
			continue
		}
		if s.Consolidation.IntervalMinutes < 0 {
			return fmt.Errorf("config: store %q consolidation.interval_minutes must be >= 0", name)
		}
		if s.Consolidation.RetentionDays < 0 {
			return fmt.Errorf("config: store %q consolidation.retention_days must be >= 0", name)
		}
	}
	if ss, ok := c.Store(c.Session.Store); !ok {
		return fmt.Errorf("config: session.store %q is not defined under stores", c.Session.Store)
	} else if ss.Kind != "postgres" && ss.Kind != "sqlite" {
		return fmt.Errorf("config: session.store %q must be a postgres or sqlite store, got kind %q", c.Session.Store, ss.Kind)
	} else if ss.URL == "" {
		return fmt.Errorf("config: session.store %q has empty url", c.Session.Store)
	}
	if c.Artifacts.Store != "" {
		as, ok := c.Store(c.Artifacts.Store)
		if !ok {
			return fmt.Errorf("config: artifacts.store %q is not defined under stores", c.Artifacts.Store)
		} else if as.Kind != "postgres" {
			return fmt.Errorf("config: artifacts.store %q must be a postgres store (large-object backend), got kind %q", c.Artifacts.Store, as.Kind)
		} else if as.URL == "" {
			return fmt.Errorf("config: artifacts.store %q has empty url", c.Artifacts.Store)
		}
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
		if a.Acp != nil && len(a.Acp.Command) == 0 {
			return fmt.Errorf("config: agent %q has an acp block with an empty command", name)
		}
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
			if g.Judge.Skeptics < 0 {
				return fmt.Errorf("config: gates.judge.skeptics must be >= 0")
			}
		}
	}
	if c.Session.Compaction.Enabled {
		cc := c.Session.Compaction
		if cc.Model != "" {
			if _, ok := c.Providers[cc.Provider]; !ok {
				return fmt.Errorf("config: session.compaction.provider %q is not defined under providers", cc.Provider)
			}
		}
	}
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
	if c.Dag.MaxActiveRuns == 0 {
		c.Dag.MaxActiveRuns = 3
	}
	if c.Dag.MaxActiveRuns < 1 {
		return fmt.Errorf("config: dag.max_active_runs must be >= 1")
	}
	if c.Dag.MaxActiveNodes < 1 {
		return fmt.Errorf("config: dag.max_active_nodes must be >= 1")
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.ShutdownGraceSeconds == 0 {
		c.Server.ShutdownGraceSeconds = 20
	}
	if c.Server.ShutdownGraceSeconds < 0 {
		return fmt.Errorf("config: server.shutdown_grace_seconds must be >= 0")
	}
	switch c.Server.Topology {
	case "", TopologyEmbedded, TopologyManaged, TopologyExternal:
	default:
		return fmt.Errorf("config: server.topology %q is unknown (use embedded, managed, or external)", c.Server.Topology)
	}
	if err := c.Workspace.applyDefaults(); err != nil {
		return err
	}
	if c.Skills.Plugins == nil {
		c.Skills.Plugins = append([]string{}, defaultSkillPlugins...)
	}
	if err := c.validateWorkflows(); err != nil {
		return err
	}
	if err := c.Observability.Otel.applyDefaults(); err != nil {
		return err
	}
	if err := c.Observability.Recording.validate(c, c.Observability.Otel.IsEnabled()); err != nil {
		return err
	}
	if err := c.Auth.validate(); err != nil {
		return err
	}
	return nil
}

func (r RecordingConfig) validate(c *Config, otelEnabled bool) error {
	if r.RetentionDays < 0 {
		return fmt.Errorf("config: observability.recording.retention_days must be >= 0")
	}
	if !r.IsEnabled(otelEnabled) || r.Store == "" {
		return nil
	}
	s, ok := c.Store(r.Store)
	if !ok {
		return fmt.Errorf("config: observability.recording.store %q is not defined under stores", r.Store)
	}
	if s.Kind != "filesystem" {
		return fmt.Errorf("config: observability.recording.store %q must be a filesystem store, got kind %q", r.Store, s.Kind)
	}
	return nil
}

func (a *InboundAuthConfig) validate() error {
	if a == nil {
		return nil
	}
	if a.OIDC == nil && a.TrustedHeaders == nil {
		return fmt.Errorf("config: auth section is present but has neither oidc nor trusted_headers configured")
	}
	if a.OIDC != nil {
		if a.OIDC.Issuer == "" {
			return fmt.Errorf("config: auth.oidc.issuer is empty")
		}
		if a.OIDC.Audience == "" {
			return fmt.Errorf("config: auth.oidc.audience is empty")
		}
	}
	if a.TrustedHeaders != nil && a.TrustedHeaders.User == "" {
		return fmt.Errorf("config: auth.trusted_headers.user is empty")
	}
	return nil
}

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
	if w.Sandbox == "" {
		w.Sandbox = defaultWorkspaceSandbox
	}
	if w.CheckCommands == nil {
		w.CheckCommands = append([]string{}, defaultCheckCommands...)
	}
	if w.Sandbox != "bwrap" && w.Sandbox != "landlock" && w.Sandbox != "none" {
		return fmt.Errorf("config: workspace.sandbox is %q (want bwrap, landlock, or none)", w.Sandbox)
	}
	if w.Limits.AddressSpaceMB == 0 {
		w.Limits.AddressSpaceMB = defaultWorkspaceAddressSpaceMB
	}
	if w.Limits.MaxProcs == 0 {
		w.Limits.MaxProcs = defaultWorkspaceMaxProcs
	}
	if w.Limits.MaxFileSizeMB == 0 {
		w.Limits.MaxFileSizeMB = defaultWorkspaceMaxFileSizeMB
	}
	if w.Limits.AddressSpaceMB < 0 || w.Limits.MaxProcs < 0 || w.Limits.MaxFileSizeMB < 0 {
		return fmt.Errorf("config: workspace.limits must be >= 0")
	}
	if w.GC.ChatTTLHours == 0 {
		w.GC.ChatTTLHours = defaultGCChatTTLHours
	}
	if w.GC.ScratchTTLHours == 0 {
		w.GC.ScratchTTLHours = defaultGCScratchTTLHours
	}
	if w.GC.IntervalHours == 0 {
		w.GC.IntervalHours = defaultGCIntervalHours
	}
	if w.GC.HomeMaxMB == 0 {
		w.GC.HomeMaxMB = defaultGCHomeMaxMB
	}
	if w.GC.ChatTTLHours < 0 || w.GC.ScratchTTLHours < 0 || w.GC.IntervalHours < 0 {
		return fmt.Errorf("config: workspace.gc hours must be >= 0")
	}
	if w.GC.HomeMaxMB < 0 {
		return fmt.Errorf("config: workspace.gc.home_max_mb must be >= 0")
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
	for k := range w.Env {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("config: workspace.env has an empty variable name")
		}
		if k == "PATH" || k == "HOME" {
			return fmt.Errorf("config: workspace.env must not set %q (use workspace.exec_path for PATH; HOME is the jail's isolated per-user home)", k)
		}
	}
	return nil
}

func (c *Config) Provider(name string) (ProviderConfig, bool) {
	p, ok := c.Providers[name]
	return p, ok
}
