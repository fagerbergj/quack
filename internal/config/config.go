// Package config loads Quack's declarative YAML configuration. Structure
// (providers, stores, orchestrator model) lives in the YAML; secrets are
// interpolated from the environment via ${VAR}. Providers and stores are
// pluggable by a `kind` discriminator; M0 implements only `openai` + `postgres`.
package config

import (
	"fmt"
	"log/slog"
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
	Otel         OtelConfig                `yaml:"otel"`       // OpenTelemetry tracing/metrics (emission-only)
	Auth         *InboundAuthConfig        `yaml:"auth"`       // inbound request auth; nil (section absent) = disabled, open
}

// InboundAuthConfig gates the API surface (REST + MCP). Absent entirely (nil),
// auth is disabled — today's behavior. Present, it needs at least one of its
// two sub-blocks: trusted_headers takes priority per-request over oidc when
// both are configured, since it's the gateway-fronted path. Named distinctly
// from the tool-level AuthConfig (ToolConfig.Auth, e.g. web_search's Exa key) —
// unrelated schemes that happen to share a domain name.
type InboundAuthConfig struct {
	OIDC           *OIDCConfig           `yaml:"oidc"`
	TrustedHeaders *TrustedHeadersConfig `yaml:"trusted_headers"`
}

// OIDCConfig verifies a bearer token against an OIDC issuer. JWKSURL is an
// optional override of the jwks_uri that discovery (<issuer>/.well-known/
// openid-configuration) would otherwise resolve — set it only when discovery
// is unavailable or blocked.
type OIDCConfig struct {
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`
	JWKSURL  string `yaml:"jwks_url"`
}

// TrustedHeadersConfig names the headers a forward-auth gateway (e.g. Traefik +
// Authentik) injects after authenticating the request itself; quack trusts
// them as-is rather than re-verifying. Groups is optional.
type TrustedHeadersConfig struct {
	User   string `yaml:"user"`
	Groups string `yaml:"groups"`
}

// OtelConfig configures OTel tracing/metrics — emission-only: quack keeps no
// local trace/metric store of its own (Tempo/Grafana, the home-server
// monitoring stack, own trace/metric viewing). Enabled defaults to true
// (spans/metrics are always recorded against the SDK's providers); set false
// for a minimal-overhead deployment that wants neither. OTLPEndpoint is what
// actually ships them anywhere — unset, providers are built but nothing is
// exported.
type OtelConfig struct {
	Enabled      *bool   `yaml:"enabled"`       // default true
	OTLPEndpoint string  `yaml:"otlp_endpoint"` // OTLP/http endpoint; empty = providers built, nothing exported
	Sample       float64 `yaml:"sample"`        // trace sample ratio in (0,1]; default 1.0
}

// otelDefaultSample is OtelConfig.Sample's default when unset (zero value).
const otelDefaultSample = 1.0

// IsEnabled reports whether OTel tracing/metrics should be wired up. nil
// (section absent or enabled unset) defaults to true.
func (o OtelConfig) IsEnabled() bool { return o.Enabled == nil || *o.Enabled }

// applyDefaults fills in unset OtelConfig fields and validates the ones set.
func (o *OtelConfig) applyDefaults() error {
	if o.Sample == 0 {
		o.Sample = otelDefaultSample
	}
	if o.Sample < 0 || o.Sample > 1 {
		return fmt.Errorf("config: otel.sample must be in (0,1]")
	}
	return nil
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
	// Exactly one of ClientID or AppID identifies the App as the JWT issuer.
	// ClientID (e.g. "Iv23li…") is GitHub's recommended issuer and the credential
	// it surfaces most prominently; AppID (numeric) is the backward-compatible
	// alternative. Neither is a secret — literals are fine. See Issuer().
	ClientID       string `yaml:"client_id"`        // GitHub App Client ID; recommended JWT issuer
	AppID          int64  `yaml:"app_id"`           // numeric App ID; legacy issuer (alternative to client_id)
	PrivateKey     string `yaml:"private_key"`      // PEM contents via ${VAR}
	PrivateKeyPath string `yaml:"private_key_path"` // path to a .pem file (alternative to private_key)
	WebhookSecret  string `yaml:"webhook_secret"`   // ${VAR}
	Mention        string `yaml:"mention"`          // trigger phrase, default "@quack"

	// Triggers selects which webhook events fire a run: "mention" (default),
	// "pr_opened" (auto-review on PR open), "label" (auto-review when
	// Labels.Review is applied), "issue_plan" (plan an issue when Labels.Plan is
	// applied), "ci_fix" (auto-heal a Labels.Monitor PR when CI fails),
	// "pr_fix" (fix a PR when Labels.Fix is applied).
	Triggers        []string `yaml:"triggers"`
	AutoReviewLabel string   `yaml:"auto_review_label"` // deprecated alias for labels.review

	// AllowedUsers is the allowlist of GitHub logins permitted to INVOKE quack
	// (a mention comment, or a workflow label applied by a human) — matched
	// case-insensitively. Empty (default) is DENY-ALL, a secure default:
	// applyDefaults logs a startup WARN so an operator who forgot to seed it
	// notices before assuming quack "just doesn't run" is a bug. Does NOT gate
	// the synthetic pr_opened/label auto-review (no human invoker) — see
	// internal/github/webhook.go's isInvokerAllowed callers.
	AllowedUsers []string `yaml:"allowed_users"`

	// Labels names the labels that drive the label-based workflow. Each label
	// only acts when its trigger is enabled (see Triggers).
	Labels GitHubLabels `yaml:"labels"`

	// FixAttempts bounds the CI auto-heal loop ("ci_fix" trigger): how many fix
	// runs quack dispatches per red streak on one PR before it comments and
	// stops. Re-applying Labels.Monitor resets the counter. Default 3.
	FixAttempts int `yaml:"fix_attempts"`

	// RunTimeoutMinutes bounds a single webhook-driven run. Default 120. Size it
	// to the DEPLOYMENT's model speed: a ~30 tok/s local coder with judge+revise
	// rounds can legitimately need hours on an implement run — a too-small value
	// kills the run mid-push (dogfood: died at exactly 2h, before the branch
	// was pushed).
	RunTimeoutMinutes int `yaml:"run_timeout_minutes"`
}

// GitHubLabels names the repo labels that drive quack's label-based workflow.
// Applying a label requires repo write access, so labels double as the
// permission model — no separate allowlist.
type GitHubLabels struct {
	Plan       string `yaml:"plan"`        // on an issue: post an implementation plan ("issue_plan" trigger)
	Implement  string `yaml:"implement"`   // on an issue: implement the plan, open a PR ("issue_implement" trigger)
	Review     string `yaml:"review"`      // on a PR: review it once ("label" trigger; alias auto_review_label)
	Merge      string `yaml:"merge"`       // on a PR: merge IF quack's latest review approved ("merge" trigger)
	PartialFix string `yaml:"partial_fix"` // signals a partial fix — suppresses unconditional Closes #N in acks and PR bodies
	Monitor    string `yaml:"monitor"`     // on a PR: opt in to CI auto-heal ("ci_fix" trigger); no label = never auto-heal
	Fix        string `yaml:"fix"`         // on a PR: fix the currently-failing checks once ("pr_fix" trigger)
}

// defaultMention is the trigger phrase when github.mention is unset.
const defaultMention = "@quack"

// defaultAutoReviewLabel is the label name when github.auto_review_label is unset.
const defaultAutoReviewLabel = "quack-auto-review"

// defaultPlanLabel is the label name when github.labels.plan is unset.
const defaultPlanLabel = "quack:plan"

// defaultImplementLabel is the label name when github.labels.implement is unset.
const defaultImplementLabel = "quack:implement"

// defaultMergeLabel is the label name when github.labels.merge is unset.
const defaultMergeLabel = "quack:merge"

const defaultPartialFixLabel = "quack:partial-fix"

// defaultMonitorLabel is the label name when github.labels.monitor is unset.
const defaultMonitorLabel = "quack:monitor"

// defaultFixLabel is the label name when github.labels.fix is unset.
const defaultFixLabel = "quack:fix"

// defaultFixAttempts bounds the CI auto-heal loop when github.fix_attempts is unset.
const defaultFixAttempts = 3

// validGitHubTriggers is the whitelist for github.triggers entries.
var validGitHubTriggers = map[string]bool{
	"mention": true, "pr_opened": true, "label": true,
	"issue_plan": true, "issue_implement": true, "merge": true,
	"ci_fix": true, "pr_fix": true,
}

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
	// defaultWorkspaceSandbox: children get a real OS boundary unless the
	// operator explicitly opts out. A host without bubblewrap therefore fails to
	// START (with an install-it-or-say-none error) rather than quietly running
	// agent child processes with the server user's full filesystem authority.
	defaultWorkspaceSandbox = "bwrap"
	// Per-child resource limits (see workspace.Limits). RLIMIT_AS is generous on
	// purpose: it is per PROCESS and Node's V8 reserves a huge virtual region at
	// startup, so a tight limit doesn't shrink a build, it stops `node` running.
	defaultWorkspaceAddressSpaceMB = 8192
	defaultWorkspaceMaxProcs       = 512
	defaultWorkspaceMaxFileSizeMB  = 1024
)

// defaultCheckCommands is the check-prefix allowlist an UNSET
// workspace.check_commands defaults to — the commands vetting.deriveChecks can
// complete for the repos quack actually builds (go, npm, make), each further
// gated on the binary existing on the host. Explicit `check_commands: []`
// still means "checks disabled".
var defaultCheckCommands = []string{"go build", "go vet", "go test", "npm run", "npm test", "npx tsc", "make"}

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
	// Env is EXTRA environment handed to every workspace child process (gate
	// checks, git probes, run_command) AND the ACP coding-agent subprocess —
	// the half exec_path doesn't cover: a toolchain that must be FOUND, not
	// just be on PATH (JAVA_HOME for Gradle, ANDROID_HOME/ANDROID_SDK_ROOT for
	// AGP, a GOROOT outside /usr). Deployment-wide; a matching key in an
	// agent's own acp.env WINS (AcpAgentConfig.Env) — the agent-specific
	// override is more specific. PATH and HOME are reserved (exec_path and the
	// jail's isolated per-user home already own those) and rejected here.
	// This is deployment config, not a secrets vault: values interpolate
	// ${VAR} like the rest of quack.yaml, but anything actually sensitive
	// belongs in a provider/tool's own auth block, not here. Under
	// sandbox: bwrap, a directory an env value POINTS AT still needs its own
	// exec_path entry to be bind-mounted into the child — see exec_path's doc.
	Env map[string]string `yaml:"env"`
	// GitCredentials are deployment-level per-host HTTPS git credentials (one
	// identity per host — a PAT, configured like every other secret). Empty
	// (default) ⇒ public repos only. Token MUST be an ${VAR} env reference in
	// the raw YAML — see validateNoLiteralTokens.
	GitCredentials []GitCredentialConfig `yaml:"git_credentials"`
	// Guards maps a tool name to its guard-ladder tier: none (default,
	// unlisted) | judge | confirm | judge+confirm. See §4b of the design doc.
	Guards map[string]string `yaml:"guards"`
	// Sandbox is the OS boundary run_command/gate-check CHILD PROCESSES run
	// inside: "bwrap" (default — a bubblewrap mount/pid/user namespace: nothing
	// outside the child's cwd and its isolated $HOME exists in its filesystem)
	// or "none" (the child runs with the server user's full filesystem
	// authority). "bwrap" on a host without a working bwrap is a startup ERROR,
	// never a silent fallback; "none" logs a loud WARN. See
	// internal/workspace.ResolveSandbox.
	Sandbox string `yaml:"sandbox"`
	// Limits are the per-child-process resource limits (setrlimit) — a runaway
	// build must not be able to take the host down with it.
	Limits WorkspaceLimits `yaml:"limits"`
}

// WorkspaceLimits are the per-child rlimits (see internal/workspace.Limits for
// what each one means and why RLIMIT_NPROC only applies inside the sandbox).
// Each is optional; 0 means "inherit the server's limit" (no limit).
type WorkspaceLimits struct {
	AddressSpaceMB int `yaml:"address_space_mb"` // RLIMIT_AS, default 8192 (per process)
	MaxProcs       int `yaml:"max_procs"`        // RLIMIT_NPROC, default 512 (sandboxed children only)
	MaxFileSizeMB  int `yaml:"max_file_size_mb"` // RLIMIT_FSIZE, default 1024
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
// gated agent gets a BeforeModelCallback that, once a request would overflow the
// model's context window, summarises the older conversation into an anchored
// summary and drops those turns. Compaction runs on the active run/node's OWN
// worker model — it's already resident, so this is swap-free by construction.
// Provider/Model below are an optional FALLBACK, used only when no active
// worker model is available (e.g. a standalone compaction).
type CompactionConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Provider string `yaml:"provider"` // inference provider for the fallback summariser model
	Model    string `yaml:"model"`    // fallback summariser model; empty ⇒ no fallback, active model only

	// TokenThreshold is the trigger budget in tokens. 0 ⇒ derive per-agent from
	// the agent's context_window (context_window - a fixed output reserve).
	TokenThreshold int `yaml:"token_threshold"`
	// EventRetentionSize is the minimum number of trailing request contents
	// compaction never folds into the summary, however far over threshold the
	// request is. 0 ⇒ 20 (agent.defaultEventRetentionSize).
	EventRetentionSize int `yaml:"event_retention_size"`
}

// DagConfig tunes how the orchestrator's DAG is executed.
type DagConfig struct {
	// MaxActiveNodes caps how many nodes run concurrently (default 2). Nodes whose
	// dependencies are met still queue behind this cap, so a wide layer doesn't
	// fire many heavy model requests at the single worker at once.
	MaxActiveNodes int `yaml:"max_active_nodes"`
	// MaxActiveRuns caps how many orchestrator RUNS execute concurrently server-wide
	// (default 3). max_active_nodes bounds nodes within ONE plan; this bounds the
	// number of plans, so a burst of webhook/REST requests queues instead of all
	// piling onto one model. 0 ⇒ default; extras block until a slot frees.
	MaxActiveRuns int `yaml:"max_active_runs"`
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
	ContextWindow int     `yaml:"context_window"` // judge model's context window in tokens; budgets the assembled judge prompt (0 ⇒ vetting falls back to a conservative default)
	// Skeptics is N — how many independent adversarial skeptics (#370) each
	// load-bearing PASSING judge criterion faces before the gate trusts it; a
	// STRICT MAJORITY refuting kills the finding. 0 (default) disables the
	// stage: it costs N extra model calls per qualifying criterion, so it's
	// opt-in rather than always-on.
	Skeptics int `yaml:"skeptics"`
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
	Bundle        string       `yaml:"bundle"`         // path to the agent bundle directory
	Provider      string       `yaml:"provider"`       // inference provider name
	Model         string       `yaml:"model"`          // model served to this agent
	ContextWindow int          `yaml:"context_window"` // model's per-request context window in tokens (0 ⇒ no compaction)
	Tools         []string     `yaml:"tools"`          // built-in tool names (kind: builtin)
	Inputs        []string     `yaml:"inputs"`         // accepted input modalities: "text", "image", "audio" (text assumed if empty)
	Gated         *bool        `yaml:"gated"`          // wrap in the trust gate? default true; set false for side-effecting/action agents
	JudgeRounds   int          `yaml:"judge_rounds"`   // per-agent judge/revise round budget; 0 ⇒ inherit gates.judge.max_rounds
	Judge         *bool        `yaml:"judge"`          // run the independent judge? default true; set false when the judge model cannot evaluate this output (a text judge cannot see media)
	Memory        MemoryConfig `yaml:"memory"`         // shared-memory bucket binding (empty Bucket ⇒ none)
	Skills        []string     `yaml:"skills"`         // built-in skill names this agent may load_skill (empty ⇒ none); project skills discovered in its jailed repo stay additive regardless (internal/skillsource)
	// Acp, when set, replaces the local llmagent worker with an EXTERNAL coding
	// agent subprocess speaking the Agent Client Protocol (internal/acp) —
	// opencode, claude-agent-acp, gemini-cli. provider/model still bind the
	// model (injected into the subprocess via OPENCODE_CONFIG_CONTENT); tools
	// is ignored (the external agent brings its own).
	Acp *AcpAgentConfig `yaml:"acp"`
}

// MemoryConfig binds an agent into a shared-memory role bucket.
type MemoryConfig struct {
	Bucket string `yaml:"bucket"` // role-bucket family for shared memory: "coding" | "research" (empty ⇒ no bucket)
}

// AcpAgentConfig configures an external ACP agent subprocess.
type AcpAgentConfig struct {
	Command []string          `yaml:"command"` // argv, e.g. ["opencode", "acp"]
	Env     map[string]string `yaml:"env"`     // extra subprocess environment; wins over a matching workspace.env key (WorkspaceConfig.Env)
	// McpServers is a list of MCP server URLs that the ACP agent will connect
	// to (e.g. context7). Passes straight through into opencode's mcp config
	// block in OPENCODE_CONFIG_CONTENT — zero Go abstraction, just plumbing.
	McpServers []string `yaml:"mcp_servers"`
	// ReadOnly marks an ACP agent that never commits/delivers code (a reviewer
	// or explorer): the gate then skips the commit/push delivery demand exactly
	// as it does for native read-only agents (vetting.Config.ReadOnly).
	ReadOnly bool `yaml:"read_only"`
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
	Provider       string               `yaml:"provider"`
	Model          string               `yaml:"model"`
	Tools          []string             `yaml:"tools"`
	Skills         []string             `yaml:"skills"` // built-in skill names the orchestrator may load_skill (see AgentConfig.Skills)
	UserMemoryHook UserMemoryHookConfig `yaml:"user_memory_hook"`
}

// UserMemoryHookConfig binds the end-of-turn user-memory extraction hook
// (#262): a dedicated memory-agent bundle (agents/memory-agent) reads a
// user's message and mines durable preferences/facts, run fire-and-forget so
// it never blocks the response. The orchestrator model reliably calling
// commit_memory itself turned out not to hold up, and hardcoded regex rules
// (#542) were rejected as too brittle - this replaces both with a real
// extraction model. Off by default: it costs one model call per qualifying
// turn, so it is opt-in like the judge and compaction.
type UserMemoryHookConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Provider string `yaml:"provider"` // inference provider for the memory-agent model
	Model    string `yaml:"model"`    // memory-agent model (required when enabled)
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
	if c.Orchestrator.UserMemoryHook.Enabled {
		h := c.Orchestrator.UserMemoryHook
		if _, ok := c.Providers[h.Provider]; !ok {
			return fmt.Errorf("config: orchestrator.user_memory_hook.provider %q is not defined under providers", h.Provider)
		}
		if h.Model == "" {
			return fmt.Errorf("config: orchestrator.user_memory_hook.model is empty")
		}
	}
	// Agents: memory.bucket names a SHARED role bucket, so a typo would silently hand
	// an agent a private silo of its own — exactly what the bucket model replaced.
	// Fail loudly instead.
	// (The role names are memory.RoleCoding / memory.RoleResearch; spelled out here
	// because internal/memory depends on this package, not the other way round.)
	for name, a := range c.Agents {
		switch a.Memory.Bucket {
		case "", "coding", "research":
		default:
			return fmt.Errorf("config: agent %q has unknown memory.bucket %q (known: coding, research)", name, a.Memory.Bucket)
		}
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
			if g.Judge.Skeptics < 0 {
				return fmt.Errorf("config: gates.judge.skeptics must be >= 0")
			}
		}
	}
	if c.Session.Compaction.Enabled {
		cc := c.Session.Compaction
		// model is now an optional fallback: compaction normally rides the active
		// run/node's own worker model. Only validate provider/model together when
		// a fallback is actually configured.
		if cc.Model != "" {
			if _, ok := c.Providers[cc.Provider]; !ok {
				return fmt.Errorf("config: session.compaction.provider %q is not defined under providers", cc.Provider)
			}
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
	if err := c.Otel.applyDefaults(); err != nil {
		return err
	}
	if err := c.Auth.validate(); err != nil {
		return err
	}
	return nil
}

// validate checks an auth: section for internal consistency. A nil receiver
// (section absent) is a no-op — auth stays disabled.
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

// applyDefaults validates and defaults the GitHub extension config. A nil
// receiver (no extensions.github section) is a no-op — the extension is off.
func (g *GitHubExtensionConfig) applyDefaults() error {
	if g == nil {
		return nil
	}
	// Exactly one issuer. Requiring exactly one (rather than letting one silently
	// win) surfaces a misconfiguration instead of hiding it.
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
			return fmt.Errorf("config: extensions.github.triggers has unknown entry %q (want mention, pr_opened, label, issue_plan, issue_implement, merge, ci_fix, or pr_fix)", t)
		}
	}
	// labels.review supersedes auto_review_label; the old key wins only when the
	// new one is unset, and both fall back to the historical default.
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
	if g.Labels.Monitor == "" {
		g.Labels.Monitor = defaultMonitorLabel
	}
	if g.Labels.Fix == "" {
		g.Labels.Fix = defaultFixLabel
	}
	if g.FixAttempts <= 0 {
		g.FixAttempts = defaultFixAttempts
	}
	if len(g.AllowedUsers) == 0 {
		slog.Warn("config: extensions.github.allowed_users is empty; DENYING every human-invoked trigger " +
			"(mention comments, quack:plan/implement/merge labels) until it is set — auto-review is unaffected")
	}
	return nil
}

// Issuer returns the value for the App JWT's `iss` claim: the Client ID when set
// (GitHub's recommended issuer), otherwise the stringified App ID. applyDefaults
// guarantees exactly one of the two is set.
func (g *GitHubExtensionConfig) Issuer() string {
	if g.ClientID != "" {
		return g.ClientID
	}
	return fmt.Sprintf("%d", g.AppID)
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
	if w.Sandbox == "" {
		w.Sandbox = defaultWorkspaceSandbox
	}
	// Default the check allowlist ON when the section is absent: derived checks
	// are additionally gated on the toolchain actually existing on the host
	// (vetting.toolchainPresent), so this can't fail nodes on a host without
	// go/npm. An EXPLICIT `check_commands: []` still disables checks.
	if w.CheckCommands == nil {
		w.CheckCommands = append([]string{}, defaultCheckCommands...)
	}
	if w.Sandbox != "bwrap" && w.Sandbox != "none" {
		return fmt.Errorf("config: workspace.sandbox is %q (want bwrap or none)", w.Sandbox)
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
	// PATH and HOME are owned by exec_path and the jail's isolated per-user
	// home respectively; letting workspace.env silently clobber either would
	// undo the hermetic-child guarantees those two already document.
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

// Provider returns the named provider config.
func (c *Config) Provider(name string) (ProviderConfig, bool) {
	p, ok := c.Providers[name]
	return p, ok
}
