package config

import (
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
	t.Setenv("LLM_ENDPOINT", "http://x/v1")
	t.Setenv("LLM_API_KEY", "secret")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("ORCH_MODEL", "m")
	c, err := Load(writeTemp(t, `
providers:
  default:
    kind: openai
    endpoint: ${LLM_ENDPOINT}
    api_key: ${LLM_API_KEY}
stores:
  relational:
    kind: postgres
    url: ${DATABASE_URL}
orchestrator:
  provider: default
  model: ${ORCH_MODEL}
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
	if c.Orchestrator.Model != "m" {
		t.Errorf("model = %q", c.Orchestrator.Model)
	}
	if c.Server.Addr != ":9000" {
		t.Errorf("addr = %q", c.Server.Addr)
	}
}

func TestLoadDefaultsServerAddr(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  relational: { kind: postgres, url: u }
orchestrator: { provider: default, model: m }
`))
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
  relational: { kind: postgres, url: u }
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
  relational: { kind: postgres, url: u }
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
  relational: { kind: mysql, url: u }
orchestrator: { provider: default, model: m }
`))
	if err == nil {
		t.Fatal("expected error for unknown store kind")
	}
}

func TestLoadParsesAgentsAndTools(t *testing.T) {
	t.Setenv("SEARXNG_URL", "http://searxng:8080")
	t.Setenv("CRAWL4AI_URL", "http://crawl4ai:11235")
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  relational: { kind: postgres, url: u }
orchestrator: { provider: default, model: m }
agents:
  web-researcher:
    bundle: agents/web-researcher
    provider: default
    model: r-model
    tools: [web_search, web_fetch, summarize]
tools:
  web_search: { backend: ${SEARXNG_URL} }
  web_fetch: { render_backend: ${CRAWL4AI_URL} }
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
	if c.Tools.WebSearch.Backend != "http://searxng:8080" {
		t.Errorf("web_search backend = %q, want interpolated", c.Tools.WebSearch.Backend)
	}
	if c.Tools.Fetch.RenderBackend != "http://crawl4ai:11235" {
		t.Errorf("fetch render_backend = %q, want interpolated", c.Tools.Fetch.RenderBackend)
	}
}

func TestLoadRejectsAgentWithUnknownProvider(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
stores:
  relational: { kind: postgres, url: u }
orchestrator: { provider: default, model: m }
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
  relational: { kind: postgres, url: u }
orchestrator: { provider: default, model: m }
`

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
  self_critique: { max_rounds: 1 }
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
	if c.Gates.DeterministicChecks.MaxRounds != 4 || c.Gates.SelfCritique.MaxRounds != 1 {
		t.Errorf("stage rounds wrong: det=%d self=%d", c.Gates.DeterministicChecks.MaxRounds, c.Gates.SelfCritique.MaxRounds)
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
		"judge/self_critique need rubric": `
gates: { self_critique: { max_rounds: 1 } }`,
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
