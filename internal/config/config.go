// Package config loads Quack's YAML configuration. Secrets interpolated from env via ${VAR}.
package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Providers     map[string]ProviderConfig `yaml:"providers"`
	Stores        map[string]StoreConfig    `yaml:"stores"`
	Session       SessionConfig             `yaml:"session"`
	Orchestrator  OrchestratorConfig        `yaml:"orchestrator"`
	Agents        map[string]AgentConfig    `yaml:"agents"`
	Tools         map[string]ToolConfig     `yaml:"tools"`
	Gates         GatesConfig               `yaml:"gates"`
	Dag           DagConfig                 `yaml:"dag"`
	Server        ServerConfig              `yaml:"server"`
	Workspace     WorkspaceConfig           `yaml:"workspace"`
	Skills        SkillsConfig              `yaml:"skills"`
	Extensions    ExtensionsConfig          `yaml:"extensions"`
	Observability ObservabilityConfig       `yaml:"observability"`
	Auth          *InboundAuthConfig        `yaml:"auth"`
}

// SkillsConfig names skill-library plugin roots beyond quack's own shipped
// skills/ - each resolved at startup via internal/plugin's Agent Plugins /
// Codex discovery order. A root that fails to resolve is a startup warning,
// never an error. Order is preserved and never deduped.
type SkillsConfig struct {
	Plugins []string `yaml:"plugins"`
}

var defaultSkillPlugins = []string{".agents/vendor/dotagents", ".agents/vendor/ponytail"}

type ObservabilityConfig struct {
	Otel      OtelConfig      `yaml:"otel"`
	Recording RecordingConfig `yaml:"recording"`
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

type ExtensionsConfig struct {
	GitHub *GitHubExtensionConfig `yaml:"github"`
}

// GitHubExtensionConfig configures the GitHub App extension (internal/github).
// Secrets (private_key, webhook_secret) must be ${VAR} env references in raw YAML.
type GitHubExtensionConfig struct {
	ClientID           string       `yaml:"client_id"`
	AppID              int64        `yaml:"app_id"`
	PrivateKey         string       `yaml:"private_key"`
	PrivateKeyPath     string       `yaml:"private_key_path"`
	WebhookSecret      string       `yaml:"webhook_secret"`
	Mention            string       `yaml:"mention"`
	Triggers           []string     `yaml:"triggers"`
	AutoReviewLabel    string       `yaml:"auto_review_label"`
	AllowedUsers       []string     `yaml:"allowed_users"`
	Labels             GitHubLabels `yaml:"labels"`
	RunTimeoutMinutes  int          `yaml:"run_timeout_minutes"`
	AutoArchiveOnMerge bool         `yaml:"auto_archive_on_merge"`
}

type GitHubLabels struct {
	Plan       string `yaml:"plan"`
	Implement  string `yaml:"implement"`
	Review     string `yaml:"review"`
	Merge      string `yaml:"merge"`
	PartialFix string `yaml:"partial_fix"`
	Fix        string `yaml:"fix"`
}

const defaultMention = "/quack"
const defaultAutoReviewLabel = "quack-auto-review"
const defaultPlanLabel = "quack:plan"
const defaultImplementLabel = "quack:implement"
const defaultMergeLabel = "quack:merge"
const defaultPartialFixLabel = "quack:partial-fix"
const defaultFixLabel = "quack:fix"

var validGitHubTriggers = map[string]bool{
	"mention": true, "pr_opened": true, "label": true,
	"issue_plan": true, "issue_implement": true, "merge": true,
	"ci_fix": true,
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
	Schema     string           `yaml:"schema"`
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
	Schema     string      `yaml:"schema"`
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
}

type StoreConfig struct {
	Kind          string         `yaml:"kind"`
	URL           string         `yaml:"url"`
	Extends       string         `yaml:"extends"`
	Embedder      *ProviderModel `yaml:"embedder"`
	Consolidation *ProviderModel `yaml:"consolidation"`
	TopK          int            `yaml:"top_k"`
	MinScore      *float32       `yaml:"min_score"`
	Schema        string         `yaml:"schema"`
	Collection    string         `yaml:"collection"`
	Root          string         `yaml:"root"`
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
	Consolidation ProviderModel
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
	if child.Schema != "" {
		out.Schema = child.Schema
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
	if err := c.Extensions.GitHub.applyDefaults(); err != nil {
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

func (g *GitHubExtensionConfig) applyDefaults() error {
	if g == nil {
		return nil
	}
	switch {
	case g.ClientID == "" && g.AppID == 0:
		return fmt.Errorf("config: extensions.github needs one of client_id (recommended) or app_id")
	case g.ClientID != "" && g.AppID != 0:
		return fmt.Errorf("config: extensions.github sets both client_id and app_id; use one (client_id recommended)")
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
	if g.RunTimeoutMinutes <= 0 {
		g.RunTimeoutMinutes = 120
	}
	if g.Mention == "" {
		g.Mention = defaultMention
	}
	if len(g.Triggers) == 0 {
		g.Triggers = []string{"mention"}
	}
	for _, t := range g.Triggers {
		if !validGitHubTriggers[t] {
			return fmt.Errorf("config: extensions.github.triggers has unknown entry %q (want mention, pr_opened, label, issue_plan, issue_implement, merge, or ci_fix)", t)
		}
	}
	if g.Labels.Review == "" {
		g.Labels.Review = g.AutoReviewLabel
	}
	if g.Labels.Review == "" {
		g.Labels.Review = defaultAutoReviewLabel
	}
	if g.Labels.Plan == "" {
		g.Labels.Plan = defaultPlanLabel
	}
	if g.Labels.Implement == "" {
		g.Labels.Implement = defaultImplementLabel
	}
	if g.Labels.Merge == "" {
		g.Labels.Merge = defaultMergeLabel
	}
	if g.Labels.PartialFix == "" {
		g.Labels.PartialFix = defaultPartialFixLabel
	}
	if g.Labels.Fix == "" {
		g.Labels.Fix = defaultFixLabel
	}
	if len(g.AllowedUsers) == 0 {
		slog.Warn("config: extensions.github.allowed_users is empty; DENYING every human-invoked trigger " +
			"(mention comments, quack:plan/implement/merge labels) until it is set — auto-review is unaffected")
	}
	return nil
}

func (g *GitHubExtensionConfig) Issuer() string {
	if g.ClientID != "" {
		return g.ClientID
	}
	return fmt.Sprintf("%d", g.AppID)
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
