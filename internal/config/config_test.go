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
