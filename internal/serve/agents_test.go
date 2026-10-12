package serve

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
)

// TestAgentBundlesLoad: a malformed agent-card.json or missing prompt.md in any shipped bundle
// fails here instead of at startup.
func TestAgentBundlesLoad(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"}, {"QUACK_DATABASE_URL", "postgres://localhost/db"},
		{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"}, {"QUACK_MEDIA_MODEL", "qwen3-omni-30b"}, {"QUACK_IMAGE_MODEL", "qwen3-vl-32b"},
		{"QUACK_JUDGE_MODEL", "gemma4-26b-a4b"}, {"QUACK_EMBED_MODEL", "qwen3-embed"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c, err := config.LoadDeferringAgentCompleteness("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	bundles := []string{"agents/orchestrator"}
	for _, a := range c.Agents {
		if a.Bundle == "" {
			continue // plugin-override entry; the github plugin's own tests cover its bundle load
		}
		bundles = append(bundles, a.Bundle)
	}
	for _, b := range bundles {
		if _, err := agent.LoadBundle(context.Background(), nil, "../../"+b); err != nil {
			t.Errorf("bundle %q failed to load: %v", b, err)
		}
	}
}
