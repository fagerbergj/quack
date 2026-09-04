package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// TestLoadForSandbox_SkipsRuntimeValidation: a config with every runtime
// value (endpoint/model/db) left as an unset ${VAR} - e.g. a CI image with
// no QUACK_*_MODEL/QUACK_DATABASE_URL - loads under LoadForSandbox (the
// path `quack sandbox` uses) but still fails Load, so nothing that DOES need
// live inference plumbing accidentally starts on an incomplete config.
func TestLoadForSandbox_SkipsRuntimeValidation(t *testing.T) {
	path := writeTemp(t, `
providers:
  default:
    kind: openai
    endpoint: ${QUACK_LLM_ENDPOINT}
stores:
  main: { kind: sqlite, url: ${QUACK_DATABASE_URL} }
session: { store: main }
orchestrator:
  provider: default
  model: ${QUACK_ORCH_MODEL}
agents:
  code-reviewer:
    bundle: agents/code-reviewer
    provider: default
    model: ${QUACK_CODER_MODEL}
    acp: { command: ["opencode", "acp"], read_only: true }
  image-reader:
    bundle: agents/image-reader
    provider: default
    model: ${QUACK_IMAGE_MODEL}
workspace:
  root: /tmp/quack-sandbox-test-workspace
`)

	if _, err := Load(path); err == nil {
		t.Fatal("Load: expected an error on empty models/endpoint/db url, got nil")
	}

	c, err := LoadForSandbox(path)
	if err != nil {
		t.Fatalf("LoadForSandbox: %v", err)
	}
	if c.Agents["code-reviewer"].Acp == nil || !c.Agents["code-reviewer"].Acp.ReadOnly {
		t.Errorf("expected code-reviewer's acp block to survive: %+v", c.Agents["code-reviewer"])
	}
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
models:
  m: { provider: default, role: worker }
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
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
`))
	if err == nil {
		t.Fatal("expected error for unknown provider kind")
	}
}

// TestLoadAcceptsReplayProviderForkMode pins #605's fork-replay config shape:
// kind: replay + fork_mode: fork + a live provider config all round-trip
// through Load cleanly.
func TestLoadAcceptsReplayProviderForkMode(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default:
    kind: replay
    bundle: /tmp/bundle.zip
    fork_mode: fork
    fork_from: node-a
    live: { kind: openai, endpoint: http://x, api_key: k }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := c.Providers["default"]
	if p.ForkMode != "fork" || p.ForkFrom != "node-a" {
		t.Errorf("provider = %+v, want fork_mode=fork fork_from=node-a", p)
	}
	if p.Live == nil || p.Live.Kind != "openai" || p.Live.Endpoint != "http://x" {
		t.Errorf("Live = %+v, want the live openai config", p.Live)
	}
}

func TestLoadRejectsReplayForkModeWithoutLive(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: replay, bundle: /tmp/bundle.zip, fork_mode: fork }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
`))
	if err == nil {
		t.Fatal("expected error for fork_mode: fork with no live provider config")
	}
}

func TestLoadRejectsReplayForkModeUnknownValue(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: replay, bundle: /tmp/bundle.zip, fork_mode: bogus }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
`))
	if err == nil {
		t.Fatal("expected error for an unsupported fork_mode value")
	}
}

func TestLoadRejectsMissingOrchestratorProvider(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: nope, model: m }
`))
	if err == nil {
		t.Fatal("expected error for missing orchestrator provider")
	}
}

func TestLoadRejectsUserMemoryHookMissingModel(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator:
  provider: default
  model: m
  user_memory_hook: { enabled: true, provider: default }
`))
	if err == nil {
		t.Fatal("expected error for enabled user_memory_hook with no model")
	}
}

func TestLoadRejectsUserMemoryHookUnknownProvider(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator:
  provider: default
  model: m
  user_memory_hook: { enabled: true, provider: nope, model: m2 }
`))
	if err == nil {
		t.Fatal("expected error for user_memory_hook with unknown provider")
	}
}

func TestLoadAllowsUserMemoryHookDisabledWithoutModel(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Orchestrator.UserMemoryHook.Enabled {
		t.Error("user_memory_hook should default to disabled")
	}
}

func TestLoadRejectsUnknownStoreKind(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
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
models:
  m: { provider: default, role: worker }
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
models:
  m: { provider: default, role: worker }
  r-model: { provider: default, role: worker }
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

// TestConsolidationSchedule checks schedule is honored when set explicitly
// (including the "" opt-out), defaults when absent, and rejects a bad cron.
func TestConsolidationSchedule(t *testing.T) {
	const cfg = `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
  e: { provider: default, role: embed }
  c: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
  vec:
    kind: qdrant
    url: qdrant:6334
    embedder: { provider: default, model: e }
    consolidation: { provider: default, model: c, schedule: %s }
session: { store: main }
orchestrator: { provider: default, model: m }
tools:
  stage_memory: { store: vec, collection: task_memory }
`
	c, err := Load(writeTemp(t, fmt.Sprintf(cfg, `"30 3 * * *"`)))
	if err != nil {
		t.Fatal(err)
	}
	rm, ok := c.MemoryStore("stage_memory")
	if !ok || rm.Consolidation.Schedule == nil || *rm.Consolidation.Schedule != "30 3 * * *" {
		t.Errorf("explicit schedule should be honored, got %+v", rm.Consolidation)
	}

	c, err = Load(writeTemp(t, fmt.Sprintf(cfg, `""`)))
	if err != nil {
		t.Fatal(err)
	}
	rm, ok = c.MemoryStore("stage_memory")
	if !ok || rm.Consolidation.Schedule == nil || *rm.Consolidation.Schedule != "" {
		t.Errorf(`explicit schedule: "" should be honored as opt-out, got %+v`, rm.Consolidation)
	}

	_, err = Load(writeTemp(t, fmt.Sprintf(cfg, `"not a cron"`)))
	if err == nil {
		t.Fatal("malformed schedule should fail validation")
	}
}

// TestStoreExtends checks a child store inherits the parent's connection and
// overrides only the fields it sets.
func TestStoreExtends(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  base: { kind: postgres, url: pg }
  doc:  { extends: base, collection: documents }
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
	if doc.Collection != "documents" {
		t.Errorf("doc.Collection = %q, want override", doc.Collection)
	}
}

// TestMemoryStore checks task/user memory resolution from tool bindings and the
// QDRANT-unset self-disable.
func TestMemoryStore(t *testing.T) {
	const cfg = `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
  e: { provider: default, role: embed }
  c: { provider: default, role: worker }
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
	if rm.Consolidation.Schedule == nil || *rm.Consolidation.Schedule != defaultConsolidationSchedule {
		t.Errorf("consolidation.schedule = %v, want default %q", rm.Consolidation.Schedule, defaultConsolidationSchedule)
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
models:
  m: { provider: default, role: worker }
  c-model: { provider: default, role: worker }
  s-model: { provider: default, role: worker }
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

func TestLoadAuthDisabledWhenSectionAbsent(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth != nil {
		t.Errorf("Auth = %+v, want nil (section absent)", c.Auth)
	}
}

func TestLoadAuthOIDC(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
auth:
  oidc:
    issuer: https://idp.example.com
    audience: quack
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth == nil || c.Auth.OIDC == nil {
		t.Fatal("expected Auth.OIDC to be set")
	}
	if c.Auth.OIDC.Issuer != "https://idp.example.com" || c.Auth.OIDC.Audience != "quack" {
		t.Errorf("OIDC = %+v", c.Auth.OIDC)
	}
}

func TestLoadAuthTrustedHeaders(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
auth:
  trusted_headers:
    user: X-authentik-username
    groups: X-authentik-groups
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth == nil || c.Auth.TrustedHeaders == nil {
		t.Fatal("expected Auth.TrustedHeaders to be set")
	}
	if c.Auth.TrustedHeaders.User != "X-authentik-username" || c.Auth.TrustedHeaders.Groups != "X-authentik-groups" {
		t.Errorf("TrustedHeaders = %+v", c.Auth.TrustedHeaders)
	}
}

func TestLoadRejectsAuthWithNeitherSubBlock(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
auth: {}
`))
	if err == nil {
		t.Fatal("expected error for auth section with neither oidc nor trusted_headers")
	}
}

func TestLoadRejectsAuthOIDCMissingIssuer(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
auth:
  oidc:
    audience: quack
`))
	if err == nil {
		t.Fatal("expected error for oidc block missing issuer")
	}
}

func TestLoadRejectsAuthOIDCMissingAudience(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
auth:
  oidc:
    issuer: https://idp.example.com
`))
	if err == nil {
		t.Fatal("expected error for oidc block missing audience")
	}
}

func TestLoadRejectsAuthTrustedHeadersMissingUser(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
auth:
  trusted_headers:
    groups: X-authentik-groups
`))
	if err == nil {
		t.Fatal("expected error for trusted_headers block missing user")
	}
}

// TestLoadOldConfigWithoutPostgresLedgerStillParses guards #1100: a config
// with no recording block at all (every field pre-dating the Postgres
// ledger option) must keep parsing - prod's quack.yaml is bind-mounted and
// STRICT, so an old config can never start failing after this change.
func TestLoadOldConfigWithoutPostgresLedgerStillParses(t *testing.T) {
	if _, err := Load(writeTemp(t, baseConfig)); err != nil {
		t.Fatalf("old config without a recording block should still load: %v", err)
	}
}

// TestLoadAcceptsPostgresRecordingStore is the new capability #1100 adds:
// recording.store may now name a postgres store, not just filesystem.
func TestLoadAcceptsPostgresRecordingStore(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
observability:
  recording:
    enabled: true
    store: main
`))
	if err != nil {
		t.Fatalf("recording.store pointing at a postgres store should load: %v", err)
	}
	s, ok := c.Store(c.Observability.Recording.Store)
	if !ok || s.Kind != "postgres" {
		t.Errorf("recording store %q did not resolve to postgres: %+v ok=%v", c.Observability.Recording.Store, s, ok)
	}
}

// baseConfig is a minimal valid config that adversarial tests append to.
const baseConfig = `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
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
		{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"}, {"QUACK_MEDIA_MODEL", "qwen3-omni-30b"}, {"QUACK_IMAGE_MODEL", "qwen3-vl-32b"},
		{"QUACK_JUDGE_MODEL", "gemma4-26b-a4b"}, {"QUACK_EMBED_MODEL", "qwen3-embed"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
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
	if c.Gates.Judge.MaxOutputTokens != 8192 {
		t.Errorf("gates.judge.max_output_tokens = %d, want the shipped default 8192 (#889)", c.Gates.Judge.MaxOutputTokens)
	}
}

// TestRealConfigWorkersHaveNoDirectGitHubMutation pins the staged-delivery
// spine's core safety property: git_push and github_pull_request/
// github_submit_review (the latter isn't even a registered tool anymore - see
// internal/github/tools.go) must never appear in a worker's tools: list. A
// worker commits locally and stages; only the trust gate, post judge-pass,
// pushes and posts.
func TestRealConfigWorkersHaveNoDirectGitHubMutation(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"}, {"QUACK_DATABASE_URL", "postgres://localhost/db"},
		{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"}, {"QUACK_MEDIA_MODEL", "qwen3-omni-30b"}, {"QUACK_IMAGE_MODEL", "qwen3-vl-32b"},
		{"QUACK_JUDGE_MODEL", "gemma4-26b-a4b"}, {"QUACK_EMBED_MODEL", "qwen3-embed"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
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
				t.Errorf("agent %q lists forbidden delivery tool %q - commit locally and stage_pr/stage_review instead", name, tl)
			}
		}
	}
	// The code agents are external ACP subprocesses (0.6.0): no quack tools at
	// all, and the reviewer/explorer are read_only - delivery is entirely
	// gate-owned (disk probe + answer probe).
	for _, name := range []string{"code-implementer", "code-reviewer", "code-explorer"} {
		ac, ok := c.Agents[name]
		if !ok {
			t.Fatalf("agent %q not found in shipped config", name)
		}
		if ac.Acp == nil || len(ac.Acp.Command) == 0 {
			t.Errorf("agent %q must be ACP-backed in the shipped config", name)
			continue
		}
		if len(ac.Tools) != 0 {
			t.Errorf("agent %q is ACP-backed but still lists quack tools %v", name, ac.Tools)
		}
		wantRO := name != "code-implementer"
		if ac.Acp.ReadOnly != wantRO {
			t.Errorf("agent %q read_only = %v, want %v", name, ac.Acp.ReadOnly, wantRO)
		}
		// Only the explorer clones - it reads third-party repos the gate never
		// provisions. An agent whose work gets delivered must not gain clone.
		wantClone := name == "code-explorer"
		if ac.Acp.AllowClone != wantClone {
			t.Errorf("agent %q allow_clone = %v, want %v", name, ac.Acp.AllowClone, wantClone)
		}
	}
}

// TestManagedConfigLoads guards config/managed.yaml drift: it must parse,
// validate, and actually be the managed topology (the thing `quack server`
// reads to decide whether to bring up the stores stack).
func TestManagedConfigLoads(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"},
		{"QUACK_ORCH_MODEL", "r"}, {"QUACK_RESEARCHER_MODEL", "r"}, // managed.yaml's own doc: reuse the worker model
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
	if w.CheckTimeoutSeconds != 600 {
		t.Errorf("CheckTimeoutSeconds = %d, want 600", w.CheckTimeoutSeconds)
	}
	if len(w.CheckCommands) == 0 {
		t.Errorf("CheckCommands = %v, want the default allowlist (checks ON by default; derived checks are toolchain-gated)", w.CheckCommands)
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

// TestWorkspaceGCDefaults: an absent workspace.gc: section still gets a
// running reaper at its documented defaults.
func TestWorkspaceGCDefaults(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatal(err)
	}
	gc := c.Workspace.GC
	if !gc.IsEnabled() {
		t.Error("IsEnabled() = false, want true (default)")
	}
	if gc.ChatTTLHours != 168 {
		t.Errorf("ChatTTLHours = %d, want 168", gc.ChatTTLHours)
	}
	if gc.ScratchTTLHours != 6 {
		t.Errorf("ScratchTTLHours = %d, want 6", gc.ScratchTTLHours)
	}
	if gc.IntervalHours != 1 {
		t.Errorf("IntervalHours = %d, want 1", gc.IntervalHours)
	}
	if gc.HomeMaxMB != 500 {
		t.Errorf("HomeMaxMB = %d, want 500", gc.HomeMaxMB)
	}
}

// TestWorkspaceGCOverrides proves every gc: field round-trips, and that
// enabled: false is honored (not overwritten by the true default).
func TestWorkspaceGCOverrides(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
workspace:
  gc:
    enabled: false
    chat_ttl_hours: 24
    scratch_ttl_hours: 2
    interval_hours: 4
    home_max_mb: 100
`))
	if err != nil {
		t.Fatal(err)
	}
	gc := c.Workspace.GC
	if gc.IsEnabled() {
		t.Error("IsEnabled() = true, want false (explicitly disabled)")
	}
	if gc.ChatTTLHours != 24 {
		t.Errorf("ChatTTLHours = %d, want 24", gc.ChatTTLHours)
	}
	if gc.ScratchTTLHours != 2 {
		t.Errorf("ScratchTTLHours = %d, want 2", gc.ScratchTTLHours)
	}
	if gc.IntervalHours != 4 {
		t.Errorf("IntervalHours = %d, want 4", gc.IntervalHours)
	}
	if gc.HomeMaxMB != 100 {
		t.Errorf("HomeMaxMB = %d, want 100", gc.HomeMaxMB)
	}
}

// TestWorkspaceGCRejectsNegativeHours: a negative TTL/interval is a config
// error, not a silent default (0 alone means "use the default" - see
// WorkspaceGCConfig's doc).
func TestWorkspaceGCRejectsNegativeHours(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
workspace:
  gc:
    chat_ttl_hours: -1
`))
	if err == nil {
		t.Fatal("expected error for a negative workspace.gc TTL")
	}
}

// TestWorkspaceGCRejectsNegativeHomeMaxMB: same "0 means default, negative
// means error" contract as the TTL fields, applied to the quota field.
func TestWorkspaceGCRejectsNegativeHomeMaxMB(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
workspace:
  gc:
    home_max_mb: -1
`))
	if err == nil {
		t.Fatal("expected error for a negative workspace.gc.home_max_mb")
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
// the default in place - this test would catch that).
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
  check_timeout_seconds: 300
  check_commands: ["go build", "go test"]
  check_setup: ["make plugins"]
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
	if w.CheckTimeoutSeconds != 300 {
		t.Errorf("CheckTimeoutSeconds = %d, want 300", w.CheckTimeoutSeconds)
	}
	if len(w.CheckCommands) != 2 || w.CheckCommands[0] != "go build" || w.CheckCommands[1] != "go test" {
		t.Errorf("CheckCommands = %v, want [go build, go test]", w.CheckCommands)
	}
	if len(w.CheckSetup) != 1 || w.CheckSetup[0] != "make plugins" {
		t.Errorf("CheckSetup = %v, want [make plugins]", w.CheckSetup)
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

// TestWorkspaceEnvDefaultsEmpty: no workspace.env section ⇒ just the
// GOTOOLCHAIN/GOMODCACHE defaults the sandbox needs to build offline (#936).
func TestWorkspaceEnvDefaultsEmpty(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"GOTOOLCHAIN": "local", "GOMODCACHE": "/usr/local/go/pkg/mod"}
	if len(c.Workspace.Env) != len(want) || c.Workspace.Env["GOTOOLCHAIN"] != want["GOTOOLCHAIN"] || c.Workspace.Env["GOMODCACHE"] != want["GOMODCACHE"] {
		t.Errorf("Env = %v, want %v", c.Workspace.Env, want)
	}
}

// TestWorkspaceEnvGoDefaultsOverridable proves explicit workspace.env entries
// win over the GOTOOLCHAIN/GOMODCACHE defaults.
func TestWorkspaceEnvGoDefaultsOverridable(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
workspace:
  env:
    GOTOOLCHAIN: auto
    GOMODCACHE: /custom/mod/cache
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace.Env["GOTOOLCHAIN"] != "auto" {
		t.Errorf("Env[GOTOOLCHAIN] = %q, want auto", c.Workspace.Env["GOTOOLCHAIN"])
	}
	if c.Workspace.Env["GOMODCACHE"] != "/custom/mod/cache" {
		t.Errorf("Env[GOMODCACHE] = %q, want /custom/mod/cache", c.Workspace.Env["GOMODCACHE"])
	}
}

// TestWorkspaceEnvParsesAndInterpolates proves workspace.env round-trips and
// its values interpolate ${VAR} exactly like every other config field.
func TestWorkspaceEnvParsesAndInterpolates(t *testing.T) {
	t.Setenv("QUACK_TEST_JAVA_HOME", "/opt/jdk-21")
	c, err := Load(writeTemp(t, baseConfig+`
workspace:
  env:
    JAVA_HOME: ${QUACK_TEST_JAVA_HOME}
    ANDROID_HOME: /opt/android-sdk
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace.Env["JAVA_HOME"] != "/opt/jdk-21" {
		t.Errorf("Env[JAVA_HOME] = %q, want interpolated /opt/jdk-21", c.Workspace.Env["JAVA_HOME"])
	}
	if c.Workspace.Env["ANDROID_HOME"] != "/opt/android-sdk" {
		t.Errorf("Env[ANDROID_HOME] = %q, want /opt/android-sdk", c.Workspace.Env["ANDROID_HOME"])
	}
}

// TestWorkspaceEnvRejectsEmptyKey guards the yaml shape `env: {"": "x"}`.
func TestWorkspaceEnvRejectsEmptyKey(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
workspace:
  env:
    "": "x"
`))
	if err == nil {
		t.Fatal("expected error for an empty workspace.env key")
	}
}

// TestWorkspaceEnvRejectsPathAndHome: PATH/HOME already have dedicated knobs
// (exec_path, the jail's isolated per-user home) - silently letting env
// override either would undo the hermetic-child guarantees those document.
func TestWorkspaceEnvRejectsPathAndHome(t *testing.T) {
	for _, key := range []string{"PATH", "HOME"} {
		_, err := Load(writeTemp(t, baseConfig+"\nworkspace:\n  env:\n    "+key+": /tmp/x\n"))
		if err == nil {
			t.Errorf("workspace.env[%s]: expected a startup error, got none", key)
		}
	}
}

// TestGitCredentialsParsesAndDefaultsUsername proves git_credentials round-trips
// (the ${VAR} value interpolates, an omitted username defaults to
// x-access-token) and guards parse.
func TestGitCredentialsParsesAndDefaultsUsername(t *testing.T) {
	t.Setenv("QUACK_GITHUB_TOKEN", "ghp_secret123")
	c, err := Load(writeTemp(t, baseConfig+`
workspace:
  git_credentials:
    - host: github.com
      token: ${QUACK_GITHUB_TOKEN}
    - host: gitlab.example.com
      username: custom-user
      token: ${QUACK_GITHUB_TOKEN}
  guards:
    web_fetch: judge
`))
	if err != nil {
		t.Fatal(err)
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
	if c.Workspace.Guards["web_fetch"] != "judge" {
		t.Errorf("Guards[web_fetch] = %q, want judge", c.Workspace.Guards["web_fetch"])
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
// silent leak - checked BEFORE ${VAR} expansion (see validateNoLiteralTokens).
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

// An unrecognized top-level key under extensions: must pass strict parsing
// as a raw node (issue #275) - internal/serve resolves it against
// sdk.Registered() later, config never interprets it.
func TestExtensionsModulesPassThroughOpaquely(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
extensions:
  noop:
    greeting: hi there
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	node, ok := c.Extensions.Modules["noop"]
	if !ok {
		t.Fatal("extensions.noop not captured in Modules")
	}
	var got struct {
		Greeting string `yaml:"greeting"`
	}
	if err := node.Decode(&got); err != nil {
		t.Fatalf("decode captured node: %v", err)
	}
	if got.Greeting != "hi there" {
		t.Errorf("greeting = %q, want %q", got.Greeting, "hi there")
	}
}

// TestExtensionsGitHubPassesThroughOpaquely pins the GitHub-extension
// migration's config-surface consequence directly: extensions.github is no
// longer typed/strict in quack itself (config.GitHubExtensionConfig is
// gone) - it passes through Modules exactly like any other module, unknown
// fields included. quack-extensions/github's own Factory validates it now.
func TestExtensionsGitHubPassesThroughOpaquely(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
extensions:
  github:
    app_id: 1
    private_key: ${QUACK_GH_KEY}
    webhook_secret: ${QUACK_GH_SECRET}
    bogus_field: true
`))
	if err != nil {
		t.Fatalf("unexpected error: %v (extensions.github should be opaque to quack now)", err)
	}
	node, ok := c.Extensions.Modules["github"]
	if !ok {
		t.Fatal("extensions.github not captured in Modules")
	}
	var got struct {
		AppID int64 `yaml:"app_id"`
	}
	if err := node.Decode(&got); err != nil {
		t.Fatalf("decode captured node: %v", err)
	}
	if got.AppID != 1 {
		t.Errorf("app_id = %d, want 1", got.AppID)
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
    model: m
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
	if c.Gates.Judge.MaxOutputTokens != 0 {
		t.Errorf("max_output_tokens = %d, want 0 (uncapped) when omitted - unlike threshold/max_iterations this field is never Go-side defaulted, only quack.yaml's own text sets 8192", c.Gates.Judge.MaxOutputTokens)
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

// TestLoadGatesJudgeMaxOutputTokensRoundTrips proves an explicit
// max_output_tokens value survives loading unchanged - the field is additive
// and never Go-side defaulted, so whatever the operator writes is what ships.
func TestLoadGatesJudgeMaxOutputTokensRoundTrips(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
gates:
  rubric: "be good"
  judge: { provider: default, model: m, max_rounds: 1, max_output_tokens: 4096 }
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Gates.Judge.MaxOutputTokens != 4096 {
		t.Errorf("max_output_tokens = %d, want 4096", c.Gates.Judge.MaxOutputTokens)
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
		"negative max_output_tokens": `
gates: { rubric: r, judge: { provider: default, model: j, max_rounds: 1, max_output_tokens: -1 } }`,
	}
	for name, block := range cases {
		if _, err := Load(writeTemp(t, baseConfig+block)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestCoderModelFallsBackToResearcherModel proves agents.code-implementer's
// model (${QUACK_CODER_MODEL}) resolves to QUACK_RESEARCHER_MODEL's value
// when QUACK_CODER_MODEL is unset - the documented (config/quack.yaml) but,
// before this, unenforced fallback; see expandEnv.
func TestCoderModelFallsBackToResearcherModel(t *testing.T) {
	t.Setenv("QUACK_RESEARCHER_MODEL", "researcher-model")
	// Deliberately NOT setting QUACK_CODER_MODEL.
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
  researcher-model: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
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
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
  coder-model: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
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
// error, not a panic or a silent pass - expandEnv has no third fallback.
func TestCoderModelEmptyWithNoResearcherModelEither(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
agents:
  code-implementer: { bundle: agents/code-implementer, provider: default, model: ${QUACK_CODER_MODEL}, tools: [] }
`))
	if err == nil {
		t.Fatal("Load: want error (empty model) when neither QUACK_CODER_MODEL nor QUACK_RESEARCHER_MODEL is set")
	}
}

// An UNSET check_commands defaults to the shipped allowlist (derived checks
// are further gated on the toolchain existing - vetting.toolchainPresent);
// an EXPLICIT empty list still means "checks disabled".
func TestWorkspaceDefaults_CheckCommands(t *testing.T) {
	unset := WorkspaceConfig{}
	if err := unset.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if len(unset.CheckCommands) == 0 {
		t.Fatal("unset check_commands must default to the shipped allowlist")
	}

	disabled := WorkspaceConfig{CheckCommands: []string{}}
	if err := disabled.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if len(disabled.CheckCommands) != 0 {
		t.Fatalf("explicit [] must stay disabled, got %v", disabled.CheckCommands)
	}
}

// Unlike check_commands, check_setup has no shipped default list - an unset
// check_setup must stay empty (no bootstrap runs) rather than auto-enable one.
func TestWorkspaceDefaults_CheckSetupNotEnabledByDefault(t *testing.T) {
	unset := WorkspaceConfig{}
	if err := unset.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if len(unset.CheckSetup) != 0 {
		t.Fatalf("unset check_setup = %v, want empty - it must never auto-enable a bootstrap", unset.CheckSetup)
	}
}

// TestKnownFieldsRejectsUnknownTopLevel proves a completely unknown top-level
// key is caught by KnownFields(true) and named in the error.
func TestKnownFieldsRejectsUnknownTopLevel(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
foobar: true
`))
	if err == nil {
		t.Fatal("expected error for unknown top-level key 'foobar'")
	}
	if !strings.Contains(err.Error(), "foobar") {
		t.Fatalf("error should name the unknown field: %v", err)
	}
}

// TestKnownFieldsRejectsUnknownNestedKey proves an unknown key inside any nested
// block (server, workspace, gates.judge, etc.) is caught.
func TestKnownFieldsRejectsUnknownNestedKey(t *testing.T) {
	for _, desc := range []struct {
		yaml  string
		field string
	}{
		{`
server:
  foobar: true`,
			"foobar"}, // server block
		{`
workspace:
  max_read_kb: 128
  foobar: x`,
			"foobar"}, // workspace block
		{`
gates:
  rubric: r
  judge:
    provider: default
    model: j
    foobar: y
    max_rounds: 1`,
			"foobar"}, // gate/judge
	} {
		_, err := Load(writeTemp(t, baseConfig+desc.yaml))
		if err == nil {
			t.Errorf("%s: expected error for unknown nested field %q", desc.field, desc.field)
			continue
		}
		if !strings.Contains(err.Error(), desc.field) {
			t.Errorf("%s: error should name the unknown field: %v", desc.field, err)
		}
	}
}

// TestKnownFieldsRejectsMemoryRoleRename proves the deprecated memory_role key
// is rejected with a migration hint naming the replacement.
func TestKnownFieldsRejectsMemoryRoleRename(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
agents:
  code-reviewer:
    bundle: agents/code-reviewer
    provider: default
    model: c-model
    memory_role: coding
`))
	if err == nil {
		t.Fatal("expected error for deprecated memory_role key")
	}
	if !strings.Contains(err.Error(), "memory_role") {
		t.Errorf("error should mention the old key 'memory_role': %v", err)
	}
	if !strings.Contains(err.Error(), "memory.bucket") {
		t.Errorf("error should name the replacement 'memory.bucket': %v", err)
	}
}

// TestAllowCloneRequiresReadOnly: clone is only safe for an agent that cannot
// write its worktree, so allow_clone without read_only is a config error, not a
// silently-granted capability.
func TestAllowCloneRequiresReadOnly(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
agents:
  code-implementer:
    bundle: agents/code-implementer
    provider: default
    model: c-model
    acp:
      command: ["opencode", "acp"]
      allow_clone: true
`))
	if err == nil {
		t.Fatal("expected an error for acp.allow_clone without acp.read_only")
	}
	if !strings.Contains(err.Error(), "allow_clone") || !strings.Contains(err.Error(), "read_only") {
		t.Errorf("error should name both keys: %v", err)
	}
}

// TestValidConfigStillLoads ensures KnownFields(true) doesn't regress on a config that
// contains every valid known field (the exact set the struct defines).
func TestValidConfigStillLoads(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  main: { kind: postgres, url: u }
session: { store: main }
orchestrator: { provider: default, model: m }
server:
  addr: ":9999"
models:
  m: { provider: default, role: worker }
  j-model: { provider: default, role: worker }
  r-model:
    provider: default
    role: worker
    context_window: 65536
    limits:
      sessions: 4
      kv_tokens: 65536
    cost:
      input_per_mtok: 0.6
      output_per_mtok: 3.6
agents:
  code-reviewer:
    bundle: agents/code-reviewer
    provider: default
    model: r-model
    memory:
      bucket: coding
tools:
  stage_memory: { store: main }
gates:
  rubric: "be good"
  deterministic_checks: { max_rounds: 2 }
  judge:
    provider: default
    model: j-model
    max_rounds: 1
`))
	if err != nil {
		t.Fatalf("valid config should load without error: %v", err)
	}
	mem := c.Agents["code-reviewer"].Memory.Bucket
	if mem != "coding" {
		t.Errorf("memory.bucket = %q, want 'coding'", mem)
	}
	if c.Server.Addr != ":9999" {
		t.Errorf("addr = %q, want :9999", c.Server.Addr)
	}
}

// TestKnownRenamesMapIsPopulated ensures the map has at least one entry so nobody
// deletes it accidentally and the pre-scan path is nontrivial.
func TestKnownRenamesMapHasEntries(t *testing.T) {
	if len(knownRenames) == 0 {
		t.Fatal("knownRenames map should not be empty")
	}
	if _, ok := knownRenames["memory_role"]; !ok {
		t.Error("expected 'memory_role' in knownRenames")
	}
}

// TestScanForKnownRenames tests the scan-for-renames helper directly.
func TestScanForKnownRenames(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"memory_role present", "  memory_role: coding", true},
		{"memory_role with indent", "\nagents:\n  reviewer:\n    memory_role: x\n", true},
		{"no renamed keys", "  bucket: coding\n  provider: default\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := scanForKnownRenames(tc.yaml)
			if tc.wantErr && err == nil {
				t.Error("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// baseObservabilityYAML is the smallest valid config, reused by the
// recording-inheritance tests below with an `observability:` block appended.
func baseObservabilityYAML(t *testing.T, observability string) string {
	t.Helper()
	t.Setenv("QUACK_LLM_ENDPOINT", "http://x/v1")
	t.Setenv("QUACK_LLM_API_KEY", "secret")
	t.Setenv("QUACK_DATABASE_URL", "postgres://localhost/db")
	t.Setenv("QUACK_ORCH_MODEL", "m")
	return writeTemp(t, `
providers:
  default: { kind: openai, endpoint: ${QUACK_LLM_ENDPOINT}, api_key: ${QUACK_LLM_API_KEY} }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: ${QUACK_DATABASE_URL} }
  ledger: { kind: filesystem, root: /tmp/quack-recordings }
session: { store: main }
orchestrator: { provider: default, model: ${QUACK_ORCH_MODEL} }
`+observability)
}

func TestRecordingUnsetFollowsOtelEnabled(t *testing.T) {
	c, err := Load(baseObservabilityYAML(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Observability.Otel.IsEnabled() {
		t.Fatal("otel should default enabled")
	}
	if !c.Observability.Recording.IsEnabled(c.Observability.Otel.IsEnabled()) {
		t.Error("recording.enabled unset should inherit otel.enabled (true)")
	}
}

func TestRecordingCannotBeOnWhenOtelDisabled(t *testing.T) {
	c, err := Load(baseObservabilityYAML(t, `
observability:
  otel: { enabled: false }
  recording: { enabled: true, store: ledger }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Observability.Recording.IsEnabled(c.Observability.Otel.IsEnabled()) {
		t.Error("recording must never be enabled when otel itself is disabled")
	}
}

func TestRecordingExplicitFalseOverridesOtelEnabled(t *testing.T) {
	c, err := Load(baseObservabilityYAML(t, `
observability:
  recording: { enabled: false }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Observability.Recording.IsEnabled(c.Observability.Otel.IsEnabled()) {
		t.Error("explicit recording.enabled: false should stay off even though otel defaults on")
	}
}

func TestRecordingUnconfiguredStoreDoesNotFailLoad(t *testing.T) {
	// No observability: section at all - otel and recording both default
	// enabled, but no store is named. Per the ledger's "off or store error ⇒
	// zero behavior change" rule, this must degrade at wiring time, not fail
	// config load.
	if _, err := Load(baseObservabilityYAML(t, "")); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestRecordingNamedStoreMustResolveAndBeFilesystemOrPostgres(t *testing.T) {
	if _, err := Load(baseObservabilityYAML(t, `
observability:
  recording: { enabled: true, store: does-not-exist }
`)); err == nil {
		t.Error("expected an error for a recording.store that isn't defined under stores")
	}
	// "main" is kind: postgres in baseObservabilityYAML - allowed since #1100.
	if _, err := Load(baseObservabilityYAML(t, `
observability:
  recording: { enabled: true, store: main }
`)); err != nil {
		t.Errorf("recording.store on a postgres store should be allowed: %v", err)
	}
	if _, err := Load(baseObservabilityYAML(t, `
stores:
  vec: { kind: qdrant, url: http://x }
observability:
  recording: { enabled: true, store: vec }
`)); err == nil {
		t.Error("expected an error for a recording.store that isn't filesystem or postgres")
	}
}

func TestFilesystemStoreDefaultsRoot(t *testing.T) {
	t.Setenv("QUACK_LLM_ENDPOINT", "http://x/v1")
	t.Setenv("QUACK_LLM_API_KEY", "secret")
	t.Setenv("QUACK_DATABASE_URL", "postgres://localhost/db")
	t.Setenv("QUACK_ORCH_MODEL", "m")
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: ${QUACK_LLM_ENDPOINT}, api_key: ${QUACK_LLM_API_KEY} }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: ${QUACK_DATABASE_URL} }
  ledger2: { kind: filesystem }
session: { store: main }
orchestrator: { provider: default, model: ${QUACK_ORCH_MODEL} }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, ok := c.Store("ledger2")
	if !ok || s.Root != defaultLedgerRoot {
		t.Errorf("Store(ledger2).Root = %q, want %q", s.Root, defaultLedgerRoot)
	}
}

// workflowAgentConfig is a minimal agents: block a workflow shape's agents
// list can reference.
const workflowAgentConfig = `
agents:
  document-classifier:
    bundle: agents/document-classifier
    provider: default
    model: m
`

// TestWorkflowShapeValidIsComposable pins issue #805 test case 1's config
// side: a well-formed shape naming a configured agent survives validation,
// carrying provenance (operator source, config revision, approved).
func TestWorkflowShapeValidIsComposable(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+workflowAgentConfig+`
workflows:
  - name: document-ingest
    trigger: "Ingest a new document into the knowledge base"
    agents: [document-classifier]
    shape: "ONE `+"`document-classifier`"+` node (terminal - classifies and files the document)"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Workflows) != 1 {
		t.Fatalf("Workflows = %+v, want 1 entry", c.Workflows)
	}
	w := c.Workflows[0]
	if w.Name != "document-ingest" || len(w.Agents) != 1 || w.Agents[0] != "document-classifier" {
		t.Errorf("workflow shape = %+v", w)
	}
	if c.Revision == "" {
		t.Error("Revision is empty; workflow shape provenance needs a config revision")
	}
}

// TestWorkflowShapeMissingAgentFailsStartup is issue #805 test case 3: a
// shape naming an agent that isn't configured must fail loudly at startup,
// naming both the shape and the missing agent - never silently produce a
// plan the executor can't run.
func TestWorkflowShapeMissingAgentFailsStartup(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
workflows:
  - name: document-ingest
    trigger: "Ingest a new document into the knowledge base"
    agents: [document-classifier]
    shape: "ONE `+"`document-classifier`"+` node (terminal)"
`))
	if err == nil {
		t.Fatal("expected an error for a workflow shape naming an unconfigured agent")
	}
	if !strings.Contains(err.Error(), "document-ingest") || !strings.Contains(err.Error(), "document-classifier") {
		t.Errorf("error = %q, want it to name both the shape and the missing agent", err)
	}
}

// TestWorkflowShapeMalformedIsSkipped is issue #805 test case 4: a malformed
// shape (missing a required field) is dropped with a warning, never fails
// startup, and a well-formed shape alongside it still loads.
func TestWorkflowShapeMalformedIsSkipped(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+workflowAgentConfig+`
workflows:
  - name: broken
    trigger: "Missing a shape"
    agents: [document-classifier]
  - name: document-ingest
    trigger: "Ingest a new document into the knowledge base"
    agents: [document-classifier]
    shape: "ONE `+"`document-classifier`"+` node (terminal)"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Workflows) != 1 || c.Workflows[0].Name != "document-ingest" {
		t.Errorf("Workflows = %+v, want only the well-formed shape", c.Workflows)
	}
}

// TestWorkflowShapesDefaultEmpty pins the regression guard's config side: no
// workflows: key at all parses to an empty slice, not an error.
func TestWorkflowShapesDefaultEmpty(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Workflows) != 0 {
		t.Errorf("Workflows = %+v, want empty", c.Workflows)
	}
}

// TestWorkflowShapeBoundNodesValid pins workflow binding: a
// well-formed nodes: list on a shape survives validation and round-trips
// verbatim (id/agent/task/depends_on/rubric), so workflowcatalog.Bind has
// exactly what it needs.
func TestWorkflowShapeBoundNodesValid(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+workflowAgentConfig+`
workflows:
  - name: document-ingest
    trigger: "Ingest a new document into the knowledge base"
    agents: [document-classifier]
    shape: "ONE `+"`document-classifier`"+` node (terminal - classifies and files the document)"
    nodes:
      - id: classify
        agent: document-classifier
        task: "Classify and file this document.\n\n{{ask}}"
        rubric: "Output names the chosen folder and a one-line reason."
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Workflows) != 1 {
		t.Fatalf("Workflows = %+v, want 1 entry", c.Workflows)
	}
	nodes := c.Workflows[0].Nodes
	if len(nodes) != 1 {
		t.Fatalf("Nodes = %+v, want 1 node", nodes)
	}
	n := nodes[0]
	if n.ID != "classify" || n.Agent != "document-classifier" || !strings.Contains(n.Task, "{{ask}}") || n.Rubric == "" {
		t.Errorf("bound node = %+v, want fields preserved verbatim", n)
	}
}

// TestWorkflowShapeBoundNodeUnregisteredArtifactFailsStartup: a bound node's
// artifact must be a registered recordstore kind (#1128) - a typo here
// previously reached SaveBlob at run time and was silently warn-logged away.
func TestWorkflowShapeBoundNodeUnregisteredArtifactFailsStartup(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+workflowAgentConfig+`
workflows:
  - name: document-ingest
    trigger: "Ingest a new document into the knowledge base"
    agents: [document-classifier]
    shape: "ONE `+"`document-classifier`"+` node (terminal)"
    nodes:
      - id: classify
        agent: document-classifier
        task: "classify it"
        artifact: not-a-real-kind
`))
	if err == nil {
		t.Fatal("expected an error for a bound node with an unregistered artifact kind")
	}
	if !strings.Contains(err.Error(), "not-a-real-kind") || !strings.Contains(err.Error(), "valid kinds:") {
		t.Errorf("error = %q, want it to name the bad kind and list valid kinds", err)
	}
}

// TestWorkflowShapeBoundNodeUnknownAgentFailsStartup: a bound node naming an
// agent absent from `agents:` must fail loud at config load, not surface
// only when a dispatch tries to bind it later.
func TestWorkflowShapeBoundNodeUnknownAgentFailsStartup(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+workflowAgentConfig+`
workflows:
  - name: document-ingest
    trigger: "Ingest a new document into the knowledge base"
    agents: [document-classifier]
    shape: "ONE `+"`document-classifier`"+` node (terminal)"
    nodes:
      - id: classify
        agent: nope
        task: "classify it"
`))
	if err == nil {
		t.Fatal("expected an error for a bound node naming an unconfigured agent")
	}
	if !strings.Contains(err.Error(), "document-ingest") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %q, want it to name both the shape and the missing agent", err)
	}
}

// TestWorkflowShapeBoundNodeCycleFailsStartup: a bound shape whose nodes
// depend on each other in a cycle must fail loud at config load - the whole
// point of validating structure once, so a dispatch never rediscovers it.
func TestWorkflowShapeBoundNodeCycleFailsStartup(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+workflowAgentConfig+`
workflows:
  - name: document-ingest
    trigger: "Ingest a new document into the knowledge base"
    agents: [document-classifier]
    shape: "two `+"`document-classifier`"+` nodes"
    nodes:
      - id: a
        agent: document-classifier
        task: "step a"
        depends_on: [b]
      - id: b
        agent: document-classifier
        task: "step b"
        depends_on: [a]
`))
	if err == nil {
		t.Fatal("expected an error for a bound node cycle")
	}
	if !strings.Contains(err.Error(), "document-ingest") || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, want it to name the shape and mention the cycle", err)
	}
}

// TestWorkflowShapeBoundNodeDuplicateIDFailsStartup: duplicate node ids
// within one bound shape would make depends_on ambiguous - fail loud.
func TestWorkflowShapeBoundNodeDuplicateIDFailsStartup(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+workflowAgentConfig+`
workflows:
  - name: document-ingest
    trigger: "Ingest a new document into the knowledge base"
    agents: [document-classifier]
    shape: "two `+"`document-classifier`"+` nodes"
    nodes:
      - id: a
        agent: document-classifier
        task: "step one"
      - id: a
        agent: document-classifier
        task: "step two"
`))
	if err == nil {
		t.Fatal("expected an error for duplicate bound node ids")
	}
	if !strings.Contains(err.Error(), "document-ingest") || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want it to name the shape and mention the duplicate", err)
	}
}

// TestWorkflowShapeBoundNodeMissingFieldFailsStartup: a bound node missing
// id/agent/task is a malformed BINDING, not a malformed hint - it fails
// startup loudly rather than silently dropping (unlike a malformed
// trigger/shape/agents shape, which is only ever a hint and gets skipped).
func TestWorkflowShapeBoundNodeMissingFieldFailsStartup(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+workflowAgentConfig+`
workflows:
  - name: document-ingest
    trigger: "Ingest a new document into the knowledge base"
    agents: [document-classifier]
    shape: "ONE `+"`document-classifier`"+` node (terminal)"
    nodes:
      - id: classify
        agent: document-classifier
`))
	if err == nil {
		t.Fatal("expected an error for a bound node missing task")
	}
	if !strings.Contains(err.Error(), "document-ingest") {
		t.Errorf("error = %q, want it to name the shape", err)
	}
}

// TestRealConfigDocumentIngestWorkflowExampleLoads loads the shipped
// config/quack.yaml (real agents:, not a throwaway fixture) with an
// uncommented copy of its own extensions.remarkable + document-ingest
// workflows: examples appended, proving the example this repo ships actually
// parses and binds against the real image-reader/synthesizer agents.
func TestRealConfigDocumentIngestWorkflowExampleLoads(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"}, {"QUACK_DATABASE_URL", "postgres://localhost/db"},
		{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"}, {"QUACK_MEDIA_MODEL", "qwen3-omni-30b"}, {"QUACK_IMAGE_MODEL", "qwen3-vl-32b"},
		{"QUACK_JUDGE_MODEL", "gemma4-26b-a4b"}, {"QUACK_EMBED_MODEL", "qwen3-embed"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
		{"RMFAKECLOUD_URL", "https://rm.example.com"}, {"RMFAKECLOUD_EMAIL", "a@example.com"}, {"RMFAKECLOUD_PASSWORD", "pw"},
	} {
		t.Setenv(kv[0], kv[1])
	}

	raw, err := os.ReadFile("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("read shipped config: %v", err)
	}
	c, err := Load(writeTemp(t, string(raw)+`
extensions:
  remarkable:
    base_url: ${RMFAKECLOUD_URL}
    email: ${RMFAKECLOUD_EMAIL}
    password: ${RMFAKECLOUD_PASSWORD}
workflows:
  - name: document-ingest
    trigger: "Ingest a new document (e.g. a reMarkable export) into the knowledge base"
    agents: [image-reader, synthesizer]
    shape: "ONE `+"`image-reader`"+` node (transcribes the attached document) -> ONE `+"`synthesizer`"+` node (terminal - writes the structured summary)"
    nodes:
      - id: transcribe
        agent: image-reader
        task: "Transcribe this document's full content, preserving structure (headings, lists, sketches described in words).\n\n{{ask}}"
      - id: summarize
        agent: synthesizer
        depends_on: [transcribe]
        task: "Write a structured Markdown summary of the transcribed document above: key facts, decisions, and action items.\n\n{{ask}}"
`))
	if err != nil {
		t.Fatalf("Load with document-ingest example: %v", err)
	}

	node, ok := c.Extensions.Modules["remarkable"]
	if !ok {
		t.Fatal("extensions.remarkable not captured in Modules")
	}
	var rmCfg struct {
		BaseURL  string `yaml:"base_url"`
		Email    string `yaml:"email"`
		Password string `yaml:"password"`
	}
	if err := node.Decode(&rmCfg); err != nil {
		t.Fatalf("decode remarkable config: %v", err)
	}
	if rmCfg.BaseURL != "https://rm.example.com" || rmCfg.Email != "a@example.com" || rmCfg.Password != "pw" {
		t.Errorf("remarkable config = %+v, want env-interpolated fields", rmCfg)
	}

	var shape *WorkflowShape
	for i := range c.Workflows {
		if c.Workflows[i].Name == "document-ingest" {
			shape = &c.Workflows[i]
		}
	}
	if shape == nil {
		t.Fatalf("Workflows = %+v, want a document-ingest shape", c.Workflows)
	}
	if len(shape.Nodes) != 2 {
		t.Fatalf("document-ingest nodes = %+v, want 2", shape.Nodes)
	}
	if shape.Nodes[0].Agent != "image-reader" || !strings.Contains(shape.Nodes[0].Task, "{{ask}}") {
		t.Errorf("transcribe node = %+v", shape.Nodes[0])
	}
	if shape.Nodes[1].Agent != "synthesizer" || len(shape.Nodes[1].DependsOn) != 1 || shape.Nodes[1].DependsOn[0] != "transcribe" {
		t.Errorf("summarize node = %+v", shape.Nodes[1])
	}
}

// TestAcpAgentSkillsWarns: skills: on an ACP-harness agent is silently
// ignored at runtime (workers get the full library), so loading must warn.
func TestAcpAgentSkillsWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	_, err := LoadForSandbox(writeTemp(t, `
providers:
  default:
    kind: openai
    endpoint: ${QUACK_LLM_ENDPOINT}
stores:
  main: { kind: sqlite, url: ${QUACK_DATABASE_URL} }
session: { store: main }
orchestrator:
  provider: default
  model: ${QUACK_ORCH_MODEL}
agents:
  code-reviewer:
    bundle: agents/code-reviewer
    provider: default
    model: ${QUACK_CODER_MODEL}
    skills: [contribute]
    acp: { command: ["opencode", "acp"], read_only: true }
workspace:
  root: /tmp/quack-acp-skills-warn-test
`))
	if err != nil {
		t.Fatalf("LoadForSandbox: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "no effect on an ACP-harness agent") || !strings.Contains(out, "agent=code-reviewer") {
		t.Errorf("expected a warning naming code-reviewer, got: %q", out)
	}
}

// TestModelsRegistry pins the #1007 config-layer prerequisite: the models:
// registry, its validation rules, and provider derivation. Each case is a
// full config; failure cases assert both an error and that it names the
// offender.
func TestModelsRegistry(t *testing.T) {
	const providers = `
providers:
  default: { kind: openai, endpoint: http://x }
`
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr string // substring; "" ⇒ expect success
	}{
		{
			name: "valid: provider derived from model, limits parsed",
			yaml: providers + `
models:
  w1:
    provider: default
    role: worker
    context_window: 131072
    limits: { sessions: 4, kv_tokens: 131072 }
    cost: { input_per_mtok: 0.6, output_per_mtok: 3.6 }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: w1 }
agents:
  worker: { bundle: agents/worker, model: w1, context_window: 65536 }
`,
		},
		{
			name: "model references unknown provider",
			yaml: providers + `
models:
  w1: { provider: nope, role: worker }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: m }
`,
			wantErr: `model "w1" provider "nope" is not defined under providers`,
		},
		{
			name: "agent model not in registry",
			yaml: providers + `
models:
  m: { provider: default, role: worker }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: m }
agents:
  worker: { bundle: agents/worker, provider: default, model: ghost }
`,
			wantErr: `agent "worker" model "ghost" is not defined under models`,
		},
		{
			name: "agent context_window exceeds model context_window",
			yaml: providers + `
models:
  w1: { provider: default, role: worker, context_window: 32768 }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: w1 }
agents:
  worker: { bundle: agents/worker, model: w1, context_window: 65536 }
`,
			wantErr: `agent "worker" context_window 65536 exceeds model "w1" context_window 32768`,
		},
		{
			name: "agent context_window exceeds model kv_tokens (deadlock guard)",
			yaml: providers + `
models:
  w1: { provider: default, role: worker, context_window: 131072, limits: { kv_tokens: 32768 } }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: w1 }
agents:
  worker: { bundle: agents/worker, model: w1, context_window: 65536 }
`,
			wantErr: `agent "worker" context_window 65536 exceeds model "w1" limits.kv_tokens 32768`,
		},
		{
			name: `model role not a key of provider limits.active`,
			yaml: `
providers:
  default: { kind: openai, endpoint: http://x, limits: { active: { judge: 1 } } }
models:
  w1: { provider: default, role: worker }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: w1 }
`,
			wantErr: `model "w1" role "worker" is not a key of provider "default" limits.active`,
		},
		{
			name: "agent provider disagrees with model's provider",
			yaml: `
providers:
  default: { kind: openai, endpoint: http://x }
  other: { kind: openai, endpoint: http://y }
models:
  w1: { provider: default, role: worker }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: w1 }
agents:
  worker: { bundle: agents/worker, provider: other, model: w1 }
`,
			wantErr: `agent "worker" provider "other" disagrees with model "w1"'s provider "default"`,
		},
		{
			name: "old-shape provider-nested pricing is a migration error, not silently dropped",
			yaml: `
providers:
  default:
    kind: openai
    endpoint: http://x
    models:
      w1: { input_per_mtok: 0.6, output_per_mtok: 3.6 }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: m }
`,
			wantErr: `providers.default.models is no longer supported`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.yaml))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Load: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestAgentProviderDerivedFromModel proves an agent that omits provider:
// resolves it from its model's registry entry.
func TestAgentProviderDerivedFromModel(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  w1: { provider: default, role: worker }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: w1 }
agents:
  worker: { bundle: agents/worker, model: w1 }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Agents["worker"].Provider; got != "default" {
		t.Errorf("agent provider = %q, want derived %q", got, "default")
	}
}

// TestModelCostResolvesThroughNewPath proves cost lookup now reads
// models.<name>.cost - the location gen_ai.client.cost/Langfuse readers must
// use post-#1007-config-layer - not the old providers.<p>.models path.
func TestModelCostResolvesThroughNewPath(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  priced: { provider: default, role: worker, cost: { input_per_mtok: 0.6, output_per_mtok: 3.6 } }
  unpriced: { provider: default, role: worker }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: priced }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cost := c.ModelCost("priced")
	if cost == nil || cost.InputPerMTok != 0.6 || cost.OutputPerMTok != 3.6 {
		t.Errorf("ModelCost(priced) = %+v, want {0.6 3.6}", cost)
	}
	if got := c.ModelCost("unpriced"); got != nil {
		t.Errorf("ModelCost(unpriced) = %+v, want nil (no cost: block ⇒ no guessed price)", got)
	}
	if got := c.ModelCost("nonexistent"); got != nil {
		t.Errorf("ModelCost(nonexistent) = %+v, want nil", got)
	}
}

// TestDuplicateModelKeyGetsHelpfulHint: two roles resolving to the same
// model name (common with an env-var-keyed models: registry) hits a raw
// YAML duplicate-key error; it should still name the model and hint why.
func TestDuplicateModelKeyGetsHelpfulHint(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  shared: { provider: default, role: worker }
  shared: { provider: default, role: judge }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: shared }
`))
	if err == nil {
		t.Fatal("expected an error for a duplicate models: key")
	}
	if !strings.Contains(err.Error(), "shared") || !strings.Contains(err.Error(), "resolved to the same model name") {
		t.Errorf("error should name the key and hint at the ${ENV} collision: %v", err)
	}
}

// TestModelRegistrationCoversNonAgentRefs pins each of the six non-agent
// Provider+Model fields (orchestrator, user_memory_hook, gates.judge,
// session.compaction, store embedder/consolidation) against an unregistered
// model, one per checkModelRegistered call site - each error must name that
// field.
func TestModelRegistrationCoversNonAgentRefs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "orchestrator.model",
			yaml: `
providers:
  default: { kind: openai, endpoint: http://x }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: ghost }
`,
			wantErr: `orchestrator.model "ghost" is not defined under models`,
		},
		{
			name: "orchestrator.user_memory_hook.model",
			yaml: `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator:
  provider: default
  model: m
  user_memory_hook: { enabled: true, provider: default, model: ghost }
`,
			wantErr: `orchestrator.user_memory_hook.model "ghost" is not defined under models`,
		},
		{
			name: "gates.judge.model",
			yaml: `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores: { main: { kind: postgres, url: u } }
session: { store: main }
orchestrator: { provider: default, model: m }
gates:
  rubric: "be good"
  judge: { provider: default, model: ghost, max_rounds: 1 }
`,
			wantErr: `gates.judge.model "ghost" is not defined under models`,
		},
		{
			name: "session.compaction.model",
			yaml: `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores: { main: { kind: postgres, url: u } }
session:
  store: main
  compaction: { enabled: true, provider: default, model: ghost }
orchestrator: { provider: default, model: m }
`,
			wantErr: `session.compaction.model "ghost" is not defined under models`,
		},
		{
			name: "store embedder.model",
			yaml: `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
  vec: { kind: qdrant, url: qdrant:6334, embedder: { provider: default, model: ghost } }
session: { store: main }
orchestrator: { provider: default, model: m }
`,
			wantErr: `store "vec" embedder.model "ghost" is not defined under models`,
		},
		{
			name: "store consolidation.model",
			yaml: `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
  e: { provider: default, role: embed }
stores:
  main: { kind: postgres, url: u }
  vec:
    kind: qdrant
    url: qdrant:6334
    embedder: { provider: default, model: e }
    consolidation: { provider: default, model: ghost }
session: { store: main }
orchestrator: { provider: default, model: m }
`,
			wantErr: `store "vec" consolidation.model "ghost" is not defined under models`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.yaml))
			if err == nil {
				t.Fatal("Load: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
