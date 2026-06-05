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
