package serve

import (
	"testing"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
)

// TestAgentBundlesLoad guards that every agent bundle referenced by the shipped
// config (plus the orchestrator) loads — a malformed agent-card.json or missing
// prompt.md fails here instead of at startup.
func TestAgentBundlesLoad(t *testing.T) {
	for _, kv := range [][2]string{
		{"LLM_ENDPOINT", "http://x/v1"}, {"LLM_API_KEY", "k"}, {"DATABASE_URL", "postgres://localhost/db"},
		{"ORCH_MODEL", "m"}, {"RESEARCHER_MODEL", "r"}, {"MEDIA_MODEL", "md"}, {"IMAGE_MODEL", "im"},
		{"JUDGE_MODEL", "j"}, {"EMBED_MODEL", "e"}, {"SEARXNG_URL", "http://s"}, {"CRAWL4AI_URL", "http://c"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c, err := config.Load("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	bundles := []string{"agents/orchestrator"}
	for _, a := range c.Agents {
		bundles = append(bundles, a.Bundle)
	}
	for _, b := range bundles {
		if _, err := agent.LoadBundle("../../" + b); err != nil {
			t.Errorf("bundle %q failed to load: %v", b, err)
		}
	}
}
