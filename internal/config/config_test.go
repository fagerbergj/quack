package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "quack.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadInterpolatesEnv(t *testing.T) {
	t.Setenv("QUACK_LLM_ENDPOINT", "http://x/v1")
	t.Setenv("QUACK_LLM_API_KEY", "secret")
	t.Setenv("QUACK_DATABASE_URL", "postgres://localhost/db")
	t.Setenv("QUACK_ORCH_MODEL", "m")
	c, err := Load(writeTemp(t, `
providers:
  default:
    kind: openai
    endpoint: ${QUACK_LLM_ENDPOINT}
    api_key: ${QUACK_LLM_API_KEY}
stores:
  main: { kind: postgres, url: ${QUACK_DATABASE_URL} }
session: { store: main }
orchestrator:
  provider: default
  model: ${QUACK_ORCH_MODEL}
server:
  addr: ":9000"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Providers["default"].Endpoint; got != "http://x/v1" {
		t.Errorf("endpoint = %q, want interpolated", got)
	}
	if got := c.Providers["default"].APIKey; got != "secret" {
		t.Errorf("api_key = %q, want interpolated", got)
	}
	if got, _ := c.Store("main"); got.URL != "postgres://localhost/db" {
		t.Errorf("store url = %q, want interpolated", got.URL)
	}
	if c.Orchestrator.Model != "m" {
		t.Errorf("model = %q", c.Orchestrator.Model)
	}
	if c.Server.Addr != ":9000" {
		t.Errorf("addr = %q", c.Server.Addr)
	}
}

func TestLoadDefaultsServerAddr(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Addr != ":8080" {
		t.Errorf("default addr = %q, want :8080", c.Server.Addr)
	}
}

func TestLoadRejectsUnknownProviderKind(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: anthropic, endpoint: http://x }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
`))
	if err == nil {
		t.Fatal("expected error for unknown provider kind")
	}
}

func TestLoadRejectsMissingOrchestratorProvider(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: nope, model: m }
`))
	if err == nil {
		t.Fatal("expected error for missing orchestrator provider")
	}
}

func TestLoadRejectsUnknownStoreKind(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  bad: { kind: mysql, url: u }
session: { store: bad }
orchestrator: { provider: default, model: m }
`))
	if err == nil {
		t.Fatal("expected error for unknown store kind")
	}
}

func TestLoadRejectsUnknownSessionStore(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  main: { kind: postgres, url: u }
session: { store: nope }
orchestrator: { provider: default, model: m }
`))
	if err == nil {
		t.Fatal("expected error for session referencing an unknown store")
	}
}

func TestLoadParsesAgentsAndTools(t *testing.T) {
	t.Setenv("QUACK_SEARXNG_URL", "http://searxng:8080")
	t.Setenv("QUACK_CRAWL4AI_URL", "http://crawl4ai:11235")
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
agents:
  web-researcher:
    bundle: agents/web-researcher
    provider: default
    model: r-model
    tools: [web_search, web_fetch, summarize]
tools:
  web_search: { kind: searxng, url: ${QUACK_SEARXNG_URL} }
  web_fetch: { kind: crawl4ai, url: ${QUACK_CRAWL4AI_URL} }
`))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := c.Agents["web-researcher"]
	if !ok {
		t.Fatal("web-researcher agent not parsed")
	}
	if a.Model != "r-model" || a.Provider != "default" || len(a.Tools) != 3 {
		t.Errorf("agent = %+v, want model/provider/3 tools", a)
	}
	if c.Tools["web_search"].URL != "http://searxng:8080" {
		t.Errorf("web_search url = %q, want interpolated", c.Tools["web_search"].URL)
	}
	if c.Tools["web_fetch"].URL != "http://crawl4ai:11235" {
		t.Errorf("web_fetch url = %q, want interpolated", c.Tools["web_fetch"].URL)
	}
}

func TestLoadRejectsToolWithUnknownStore(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
tools:
  stage_memory: { store: nope }
`))
	if err == nil {
		t.Fatal("expected error for tool referencing an unknown store")
	}
}

func TestToolAuthAPIKey(t *testing.T) {
	t.Setenv("QUACK_EXA_API_KEY", "sk-exa-123")
	c, err := Load(writeTemp(t, baseConfig+`
tools:
  web_search:
    kind: exa
    auth: { kind: api_key, api_key: ${QUACK_EXA_API_KEY} }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Tools["web_search"].APIKey(); got != "sk-exa-123" {
		t.Errorf("APIKey() = %q, want the interpolated key", got)
	}
	// A tool with no auth yields no key.
	if got := (ToolConfig{}).APIKey(); got != "" {
		t.Errorf("APIKey() with no auth = %q, want empty", got)
	}
}

func TestLoadRejectsUnknownAuthKind(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
tools:
  web_search: { kind: exa, auth: { kind: oauth } }
`))
	if err == nil {
		t.Fatal("expected error for an unsupported auth kind")
	}
}

func TestServerTopology(t *testing.T) {
	for _, tc := range []struct {
		yaml      string
		managed   bool
		wantError bool
	}{
		{`server: { addr: ":8080" }`, false, false}, // absent ⇒ external
		{`server: { topology: external }`, false, false},
		{`server: { topology: embedded }`, false, false}, // label only; serve just runs
		{`server: { topology: managed }`, true, false},   // the orchestration trigger
		{`server: { topology: docker }`, false, true},    // typo ⇒ fail fast
	} {
		c, err := Load(writeTemp(t, baseConfig+tc.yaml))
		if tc.wantError {
			if err == nil {
				t.Errorf("%q: expected error, got nil", tc.yaml)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: Load: %v", tc.yaml, err)
			continue
		}
		if c.Server.Managed() != tc.managed {
			t.Errorf("%q: Managed() = %v, want %v", tc.yaml, c.Server.Managed(), tc.managed)
		}
	}
}

// TestStoreExtends checks a child store inherits the parent's connection and
// overrides only the fields it sets.
func TestStoreExtends(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  base: { kind: postgres, url: pg }
  doc:  { extends: base, schema: documents }
session: { store: base }
orchestrator: { provider: default, model: m }
`))
	if err != nil {
		t.Fatal(err)
	}
	doc, ok := c.Store("doc")
	if !ok {
		t.Fatal("doc store did not resolve")
	}
	if doc.Kind != "postgres" || doc.URL != "pg" {
		t.Errorf("doc did not inherit connection: %+v", doc)
	}
	if doc.Schema != "documents" {
		t.Errorf("doc.Schema = %q, want override", doc.Schema)
	}
}

// TestMemoryStore checks task/user memory resolution from tool bindings and the
// QDRANT-unset self-disable.
func TestMemoryStore(t *testing.T) {
	const cfg = `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  main: { kind: postgres, url: u }
  vec:
    kind: qdrant
    url: %s
    embedder: { provider: default, model: e }
    consolidation: { provider: default, model: c }
session: { store: main }
orchestrator: { provider: default, model: m }
tools:
  stage_memory: { store: vec, collection: task_memory }
`
	// With a URL, the stage_memory binding resolves to a usable vector store.
	c, err := Load(writeTemp(t, fmt.Sprintf(cfg, "qdrant:6334")))
	if err != nil {
		t.Fatal(err)
	}
	rm, ok := c.MemoryStore("stage_memory")
	if !ok {
		t.Fatal("stage_memory store should resolve")
	}
	if rm.Collection != "task_memory" || rm.TopK != 5 || rm.MinScore != 0.5 {
		t.Errorf("resolved = %+v, want collection/top_k/min_score defaults", rm)
	}
	// Empty URL (QUACK_QDRANT_URL unset) ⇒ memory self-disables.
	c, err = Load(writeTemp(t, fmt.Sprintf(cfg, "")))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.MemoryStore("stage_memory"); ok {
		t.Error("empty store URL should self-disable memory")
	}
}

func TestAgentConfigIsGated(t *testing.T) {
	if !(AgentConfig{}).IsGated() {
		t.Error("agents are gated by default")
	}
	f := false
	if (AgentConfig{Gated: &f}).IsGated() {
		t.Error("gated: false should opt out of the trust gate")
	}
}

func TestLoadParsesPerAgentJudgeRounds(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
agents:
  code-implementer:
    bundle: agents/code-implementer
    provider: default
    model: c-model
    judge_rounds: 8
  synthesizer:
    bundle: agents/synthesizer
    provider: default
    model: s-model
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Agents["code-implementer"].JudgeRounds; got != 8 {
		t.Errorf("code-implementer judge_rounds = %d, want 8", got)
	}
	// Unset ⇒ 0, the sentinel that means "inherit the global default".
	if got := c.Agents["synthesizer"].JudgeRounds; got != 0 {
		t.Errorf("unset judge_rounds = %d, want 0 (inherit global)", got)
	}
}

func TestLoadRejectsAgentWithUnknownProvider(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
agents:
  bad: { bundle: agents/bad, provider: nope, model: m, tools: [web_fetch] }
`))
	if err == nil {
		t.Fatal("expected error for agent with unknown provider")
	}
}

// baseConfig is a minimal valid config that adversarial tests append to.
const baseConfig = `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
`

// TestRealConfigLoads is a smoke test that the shipped config/quack.yaml parses
// and validates against the current structs (guards against config drift). Env
// is set so required URLs are non-empty; QUACK_QDRANT_URL left unset exercises the
// memory self-disable path.
func TestRealConfigLoads(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"}, {"QUACK_DATABASE_URL", "postgres://localhost/db"},
		{"QUACK_ORCH_MODEL", "m"}, {"QUACK_RESEARCHER_MODEL", "r"}, {"QUACK_MEDIA_MODEL", "md"}, {"QUACK_IMAGE_MODEL", "im"},
		{"QUACK_JUDGE_MODEL", "j"}, {"QUACK_EMBED_MODEL", "e"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c, err := Load("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("shipped config/quack.yaml failed to load: %v", err)
	}
	if s, ok := c.Store(c.Session.Store); !ok || s.Kind != "postgres" {
		t.Errorf("session store %q did not resolve to postgres: %+v ok=%v", c.Session.Store, s, ok)
	}
}

// TestRealConfigWorkersHaveNoDirectGitHubMutation pins the staged-delivery
// spine's core safety property (0.5.0): git_push and github_pull_request/
// github_submit_review (the latter isn't even a registered tool anymore — see
// internal/github/tools.go) must never appear in a worker's tools: list. A
// worker commits locally and stages; only the trust gate, post judge-pass,
// pushes and posts.
func TestRealConfigWorkersHaveNoDirectGitHubMutation(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"}, {"QUACK_DATABASE_URL", "postgres://localhost/db"},
		{"QUACK_ORCH_MODEL", "m"}, {"QUACK_RESEARCHER_MODEL", "r"}, {"QUACK_MEDIA_MODEL", "md"}, {"QUACK_IMAGE_MODEL", "im"},
		{"QUACK_JUDGE_MODEL", "j"}, {"QUACK_EMBED_MODEL", "e"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c, err := Load("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("shipped config/quack.yaml failed to load: %v", err)
	}
	forbidden := map[string]bool{"git_push": true, "github_pull_request": true, "github_submit_review": true}
	for name, ac := range c.Agents {
		for _, tl := range ac.Tools {
			if forbidden[tl] {
				t.Errorf("agent %q lists forbidden delivery tool %q — commit locally and stage_pr/stage_review instead", name, tl)
			}
		}
	}
	for _, name := range []string{"code-implementer", "code-reviewer"} {
		ac, ok := c.Agents[name]
		if !ok {
			t.Fatalf("agent %q not found in shipped config", name)
		}
		want := map[string]bool{"stage_review": name == "code-reviewer", "stage_comment": true, "unstage": true}
		if name == "code-implementer" {
			want["stage_pr"] = true
		}
		have := map[string]bool{}
		for _, tl := range ac.Tools {
			have[tl] = true
		}
		for tl, must := range want {
			if must && !have[tl] {
				t.Errorf("agent %q must list %q to hand delivery off to the gate", name, tl)
			}
		}
	}
}

// TestManagedConfigLoads guards config/managed.yaml drift: it must parse,
// validate, and actually be the managed topology (the thing `quack server`
// reads to decide whether to bring up the stores stack).
func TestManagedConfigLoads(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"},
		{"QUACK_ORCH_MODEL", "m"}, {"QUACK_RESEARCHER_MODEL", "r"},
		{"QUACK_JUDGE_MODEL", "j"}, {"QUACK_EMBED_MODEL", "e"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c, err := Load("../../config/managed.yaml")
	if err != nil {
		t.Fatalf("shipped config/managed.yaml failed to load: %v", err)
	}
	if !c.Server.Managed() {
		t.Errorf("managed.yaml topology = %q, want managed", c.Server.Topology)
	}
	if _, ok := c.Store(c.Session.Store); !ok {
		t.Errorf("session store %q not defined", c.Session.Store)
	}
	// The preset uses the keyless/no-container tool backends.
	if got := c.Tools["web_search"].Kind; got != "exa" {
		t.Errorf("managed.yaml web_search.kind = %q, want exa", got)
	}
	if got := c.Tools["web_fetch"].Kind; got != "direct" {
		t.Errorf("managed.yaml web_fetch.kind = %q, want direct", got)
	}
}

// TestWorkspaceDefaults proves a config with no workspace: section at all
// still gets a fully-defaulted WorkspaceConfig (root + every cap).
func TestWorkspaceDefaults(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatal(err)
	}
	w := c.Workspace
	if w.Root != "./workspace" {
		t.Errorf("Root = %q, want ./workspace", w.Root)
	}
	if w.MaxReadKB != 256 {
		t.Errorf("MaxReadKB = %d, want 256", w.MaxReadKB)
	}
	if w.MaxWriteKB != 2048 {
		t.Errorf("MaxWriteKB = %d, want 2048", w.MaxWriteKB)
	}
	if w.MaxResults != 200 {
		t.Errorf("MaxResults = %d, want 200", w.MaxResults)
	}
	if w.MaxListEntries != 500 {
		t.Errorf("MaxListEntries = %d, want 500", w.MaxListEntries)
	}
	if w.TimeoutSeconds != 60 {
		t.Errorf("TimeoutSeconds = %d, want 60", w.TimeoutSeconds)
	}
	if len(w.CheckCommands) != 0 {
		t.Errorf("CheckCommands = %v, want empty (checks unavailable by default)", w.CheckCommands)
	}
	// The child-process sandbox is ON by default: a deployment that says nothing
	// must get the OS boundary, not the pre-sandbox behaviour where a child (any
	// `sh -c`, whose text trips no metachar wall) had the server user's whole
	// filesystem. An unusable bwrap then refuses to START (workspace.ResolveSandbox);
	// opting out is explicit.
	if w.Sandbox != "bwrap" {
		t.Errorf("Sandbox = %q, want bwrap (a config that says nothing must still confine child processes)", w.Sandbox)
	}
	if w.Limits.AddressSpaceMB != 8192 || w.Limits.MaxProcs != 512 || w.Limits.MaxFileSizeMB != 1024 {
		t.Errorf("Limits = %+v, want {8192 512 1024}", w.Limits)
	}
}

// TestWorkspaceSandboxOverrides: `none` is the explicit opt-out, the limits
// round-trip, and any other value is a startup error rather than a typo that
// silently degrades to "no sandbox".
func TestWorkspaceSandboxOverrides(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
workspace:
  sandbox: none
  limits:
    address_space_mb: 2048
    max_procs: 64
    max_file_size_mb: 256
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace.Sandbox != "none" {
		t.Errorf("Sandbox = %q, want none", c.Workspace.Sandbox)
	}
	if c.Workspace.Limits != (WorkspaceLimits{AddressSpaceMB: 2048, MaxProcs: 64, MaxFileSizeMB: 256}) {
		t.Errorf("Limits = %+v, want the configured values", c.Workspace.Limits)
	}

	if _, err := Load(writeTemp(t, baseConfig+"\nworkspace:\n  sandbox: docker\n")); err == nil {
		t.Fatal("an unknown workspace.sandbox value must be a startup error")
	}
}

// TestWorkspaceParsesOverrides proves every workspace: field round-trips
// (the yaml gotcha this guards against: yaml.Unmarshal silently ignores
// unknown/misspelled keys, so a wrong field name would parse clean but leave
// the default in place — this test would catch that).
func TestWorkspaceParsesOverrides(t *testing.T) {
	t.Setenv("QUACK_WORKSPACE_ROOT", "/data/workspace")
	c, err := Load(writeTemp(t, baseConfig+`
workspace:
  root: ${QUACK_WORKSPACE_ROOT}
  max_read_kb: 64
  max_write_kb: 512
  max_results: 50
  max_list_entries: 100
  timeout_seconds: 30
  check_commands: ["go build", "go test"]
`))
	if err != nil {
		t.Fatal(err)
	}
	w := c.Workspace
	if w.Root != "/data/workspace" {
		t.Errorf("Root = %q, want interpolated /data/workspace", w.Root)
	}
	if w.MaxReadKB != 64 {
		t.Errorf("MaxReadKB = %d, want 64", w.MaxReadKB)
	}
	if w.MaxWriteKB != 512 {
		t.Errorf("MaxWriteKB = %d, want 512", w.MaxWriteKB)
	}
	if w.MaxResults != 50 {
		t.Errorf("MaxResults = %d, want 50", w.MaxResults)
	}
	if w.MaxListEntries != 100 {
		t.Errorf("MaxListEntries = %d, want 100", w.MaxListEntries)
	}
	if w.TimeoutSeconds != 30 {
		t.Errorf("TimeoutSeconds = %d, want 30", w.TimeoutSeconds)
	}
	if len(w.CheckCommands) != 2 || w.CheckCommands[0] != "go build" || w.CheckCommands[1] != "go test" {
		t.Errorf("CheckCommands = %v, want [go build, go test]", w.CheckCommands)
	}
}

func TestWorkspaceRejectsNegativeCaps(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
workspace:
  max_read_kb: -1
`))
	if err == nil {
		t.Fatal("expected error for a negative workspace cap")
	}
}

// TestGitCredentialsParsesAndDefaultsUsername proves git_credentials round-trips
// (the ${VAR} value interpolates, an omitted username defaults to
// x-access-token) and git_push/guards parse.
func TestGitCredentialsParsesAndDefaultsUsername(t *testing.T) {
	t.Setenv("QUACK_GITHUB_TOKEN", "ghp_secret123")
	c, err := Load(writeTemp(t, baseConfig+`
workspace:
  git_push: true
  git_credentials:
    - host: github.com
      token: ${QUACK_GITHUB_TOKEN}
    - host: gitlab.example.com
      username: custom-user
      token: ${QUACK_GITHUB_TOKEN}
  guards:
    delete_path: judge
    git_push: judge+confirm
`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Workspace.GitPush {
		t.Error("GitPush = false, want true")
	}
	if len(c.Workspace.GitCredentials) != 2 {
		t.Fatalf("GitCredentials = %v, want 2 entries", c.Workspace.GitCredentials)
	}
	first := c.Workspace.GitCredentials[0]
	if first.Host != "github.com" || first.Token != "ghp_secret123" {
		t.Errorf("first credential = %+v, want host github.com token ghp_secret123", first)
	}
	if first.Username != "x-access-token" {
		t.Errorf("Username = %q, want default x-access-token", first.Username)
	}
	second := c.Workspace.GitCredentials[1]
	if second.Username != "custom-user" {
		t.Errorf("Username = %q, want custom-user (explicit, not defaulted)", second.Username)
	}
	if c.Workspace.Guards["delete_path"] != "judge" {
		t.Errorf("Guards[delete_path] = %q, want judge", c.Workspace.Guards["delete_path"])
	}
	if c.Workspace.Guards["git_push"] != "judge+confirm" {
		t.Errorf("Guards[git_push] = %q, want judge+confirm", c.Workspace.Guards["git_push"])
	}
}

func TestGitCredentialsRejectsEmptyHost(t *testing.T) {
	t.Setenv("QUACK_GITHUB_TOKEN", "ghp_secret123")
	_, err := Load(writeTemp(t, baseConfig+`
workspace:
  git_credentials:
    - token: ${QUACK_GITHUB_TOKEN}
`))
	if err == nil {
		t.Fatal("expected error for a git_credentials entry with no host")
	}
}

func TestGuardsRejectsUnknownTier(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
workspace:
  guards:
    delete_path: yolo
`))
	if err == nil {
		t.Fatal("expected error for an unknown guard tier")
	}
}

// TestGitCredentialTokenRejectsLiteralValue is the mechanical raw-YAML check:
// a token: value that isn't an ${VAR} reference is a startup error, not a
// silent leak — checked BEFORE ${VAR} expansion (see validateNoLiteralTokens).
func TestGitCredentialTokenRejectsLiteralValue(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
workspace:
  git_credentials:
    - host: github.com
      token: ghp_this_is_a_literal_secret
`))
	if err == nil {
		t.Fatal("expected error for a literal token value")
	}
}

func TestGitCredentialTokenAllowsEnvReference(t *testing.T) {
	t.Setenv("QUACK_GITHUB_TOKEN", "ghp_ok")
	_, err := Load(writeTemp(t, baseConfig+`
workspace:
  git_credentials:
    - host: github.com
      token: ${QUACK_GITHUB_TOKEN}
`))
	if err != nil {
		t.Fatalf("unexpected error for a proper ${VAR} token: %v", err)
	}
}

func TestGitHubExtensionValidatesAndDefaults(t *testing.T) {
	t.Setenv("QUACK_GH_KEY", "pem")
	t.Setenv("QUACK_GH_SECRET", "s3cret")
	c, err := Load(writeTemp(t, baseConfig+`
extensions:
  github:
    app_id: 12345
    private_key: ${QUACK_GH_KEY}
    webhook_secret: ${QUACK_GH_SECRET}
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Extensions.GitHub == nil || c.Extensions.GitHub.AppID != 12345 {
		t.Fatal("github extension not parsed")
	}
	if c.Extensions.GitHub.Mention != "@quack" {
		t.Errorf("mention default = %q; want @quack", c.Extensions.GitHub.Mention)
	}
	// app_id only ⇒ Issuer is the stringified App ID.
	if got := c.Extensions.GitHub.Issuer(); got != "12345" {
		t.Errorf("Issuer() = %q; want 12345", got)
	}
	if l := c.Extensions.GitHub.Labels; l.Plan != "quack:plan" || l.Review != "quack-auto-review" {
		t.Errorf("labels defaults = %+v; want plan quack:plan, review quack-auto-review", l)
	}
}

func TestGitHubExtensionAutoReviewLabelAliasesLabelsReview(t *testing.T) {
	t.Setenv("QUACK_GH_KEY", "pem")
	t.Setenv("QUACK_GH_SECRET", "s3cret")
	// Deprecated auto_review_label still works when labels.review is unset…
	c, err := Load(writeTemp(t, baseConfig+`
extensions:
  github:
    app_id: 1
    private_key: ${QUACK_GH_KEY}
    webhook_secret: ${QUACK_GH_SECRET}
    auto_review_label: legacy-label
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.Extensions.GitHub.Labels.Review; got != "legacy-label" {
		t.Errorf("labels.review = %q; want legacy-label (alias)", got)
	}
	// …but labels.review wins when both are set.
	c, err = Load(writeTemp(t, baseConfig+`
extensions:
  github:
    app_id: 1
    private_key: ${QUACK_GH_KEY}
    webhook_secret: ${QUACK_GH_SECRET}
    auto_review_label: legacy-label
    labels:
      review: new-label
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.Extensions.GitHub.Labels.Review; got != "new-label" {
		t.Errorf("labels.review = %q; want new-label", got)
	}
}

func TestGitHubExtensionClientIDIssuer(t *testing.T) {
	t.Setenv("QUACK_GH_KEY", "pem")
	t.Setenv("QUACK_GH_SECRET", "s3cret")
	// client_id is GitHub's recommended issuer and a valid alternative to app_id.
	c, err := Load(writeTemp(t, baseConfig+`
extensions:
  github:
    client_id: Iv23liExample
    private_key: ${QUACK_GH_KEY}
    webhook_secret: ${QUACK_GH_SECRET}
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.Extensions.GitHub.Issuer(); got != "Iv23liExample" {
		t.Errorf("Issuer() = %q; want Iv23liExample", got)
	}
}

func TestGitHubExtensionRejectsLiteralSecrets(t *testing.T) {
	// webhook_secret / private_key must be ${VAR}, never a literal (same rule as
	// git-credential tokens; checked before ${VAR} expansion).
	for _, field := range []string{
		"webhook_secret: hunter2",
		"private_key: -----BEGIN-----",
	} {
		_, err := Load(writeTemp(t, baseConfig+`
extensions:
  github:
    app_id: 1
    private_key: ${X}
    webhook_secret: ${Y}
    `+field+"\n"))
		if err == nil {
			t.Fatalf("expected error for literal secret %q", field)
		}
	}
}

func TestGitHubExtensionRequiresFields(t *testing.T) {
	t.Setenv("QUACK_GH_KEY", "pem")
	t.Setenv("QUACK_GH_SECRET", "s3cret")
	// Missing both app_id and client_id.
	_, err := Load(writeTemp(t, baseConfig+`
extensions:
  github:
    private_key: ${QUACK_GH_KEY}
    webhook_secret: ${QUACK_GH_SECRET}
`))
	if err == nil {
		t.Fatal("expected error for missing issuer (app_id/client_id)")
	}
	// Both app_id and client_id set ⇒ ambiguous, rejected.
	_, err = Load(writeTemp(t, baseConfig+`
extensions:
  github:
    app_id: 1
    client_id: Iv23liExample
    private_key: ${QUACK_GH_KEY}
    webhook_secret: ${QUACK_GH_SECRET}
`))
	if err == nil {
		t.Fatal("expected error when both app_id and client_id are set")
	}
	// Missing private key entirely.
	_, err = Load(writeTemp(t, baseConfig+`
extensions:
  github:
    app_id: 1
    webhook_secret: ${QUACK_GH_SECRET}
`))
	if err == nil {
		t.Fatal("expected error for missing private key")
	}
}

func TestLoadGatesDefaultsAndDisabled(t *testing.T) {
	// No gates block ⇒ vetting disabled, config still valid.
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Gates.Enabled() {
		t.Error("gates should be disabled when no stage is configured")
	}

	// Judge enabled with zero threshold/iterations ⇒ defaults applied.
	c, err = Load(writeTemp(t, baseConfig+`
gates:
  rubric: "be good"
  deterministic_checks: { max_rounds: 4 }
  judge:
    provider: default
    model: judge-m
    max_rounds: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Gates.Enabled() || !c.Gates.JudgeEnabled() {
		t.Fatal("gates/judge should be enabled")
	}
	if c.Gates.Judge.Threshold != 0.7 || c.Gates.Judge.MaxIterations != 6 {
		t.Errorf("judge defaults not applied: threshold=%v max_iterations=%d", c.Gates.Judge.Threshold, c.Gates.Judge.MaxIterations)
	}
	if c.Gates.DeterministicChecks.MaxRounds != 4 {
		t.Errorf("stage rounds wrong: det=%d", c.Gates.DeterministicChecks.MaxRounds)
	}

	// Deterministic-only gate (no judge model) is valid and enabled.
	c, err = Load(writeTemp(t, baseConfig+`
gates:
  deterministic_checks: { max_rounds: 3 }
`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Gates.Enabled() || c.Gates.JudgeEnabled() {
		t.Error("deterministic-only gate should be enabled with judge disabled")
	}
}

func TestLoadGatesRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"unknown provider": `
gates: { rubric: r, judge: { provider: nope, model: j, max_rounds: 1 } }`,
		"judge needs rubric": `
gates: { judge: { provider: default, model: j, max_rounds: 1 } }`,
		"both rubrics": `
gates: { rubric: r, rubric_path: p, judge: { provider: default, model: j, max_rounds: 1 } }`,
		"bad threshold": `
gates: { rubric: r, judge: { provider: default, model: j, max_rounds: 1, threshold: 1.5 } }`,
		"negative rounds": `
gates: { rubric: r, deterministic_checks: { max_rounds: -1 }, judge: { provider: default, model: j, max_rounds: 1 } }`,
	}
	for name, block := range cases {
		if _, err := Load(writeTemp(t, baseConfig+block)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestCoderModelFallsBackToResearcherModel proves agents.code-implementer's
// model (${QUACK_CODER_MODEL}) resolves to QUACK_RESEARCHER_MODEL's value
// when QUACK_CODER_MODEL is unset — the documented (config/quack.yaml) but,
// before this, unenforced fallback; see expandEnv.
func TestCoderModelFallsBackToResearcherModel(t *testing.T) {
	t.Setenv("QUACK_RESEARCHER_MODEL", "researcher-model")
	// Deliberately NOT setting QUACK_CODER_MODEL.
	c, err := Load(writeTemp(t, baseConfig+`
agents:
  code-implementer: { bundle: agents/code-implementer, provider: default, model: ${QUACK_CODER_MODEL}, tools: [] }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Agents["code-implementer"].Model; got != "researcher-model" {
		t.Errorf("code-implementer model = %q, want fallback to QUACK_RESEARCHER_MODEL %q", got, "researcher-model")
	}
}

// TestCoderModelExplicitOverridesFallback proves an explicitly-set
// QUACK_CODER_MODEL wins over the QUACK_RESEARCHER_MODEL fallback.
func TestCoderModelExplicitOverridesFallback(t *testing.T) {
	t.Setenv("QUACK_RESEARCHER_MODEL", "researcher-model")
	t.Setenv("QUACK_CODER_MODEL", "coder-model")
	c, err := Load(writeTemp(t, baseConfig+`
agents:
  code-implementer: { bundle: agents/code-implementer, provider: default, model: ${QUACK_CODER_MODEL}, tools: [] }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Agents["code-implementer"].Model; got != "coder-model" {
		t.Errorf("code-implementer model = %q, want explicit QUACK_CODER_MODEL %q", got, "coder-model")
	}
}

// TestCoderModelEmptyWithNoResearcherModelEither proves the fallback chain's
// end state (both env vars unset) is a normal "empty model" validation
// error, not a panic or a silent pass — expandEnv has no third fallback.
func TestCoderModelEmptyWithNoResearcherModelEither(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
agents:
  code-implementer: { bundle: agents/code-implementer, provider: default, model: ${QUACK_CODER_MODEL}, tools: [] }
`))
	if err == nil {
		t.Fatal("Load: want error (empty model) when neither QUACK_CODER_MODEL nor QUACK_RESEARCHER_MODEL is set")
	}
}
