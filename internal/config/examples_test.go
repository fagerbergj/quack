package config

import (
	"path/filepath"
	"testing"
)

// TestExampleConfigsLoad pins the shipped docs/configuration/examples/*.yaml
// against real config.Load — a docs-only edit that breaks one of them (a
// missing models: registry entry, an unregistered model name, a bad argv)
// fails here instead of at a user's server start. Each file runs with the
// env vars it references set to the model names its own registry registers.
func TestExampleConfigsLoad(t *testing.T) {
	for _, tc := range []struct {
		file string
		env  [][2]string
	}{
		{"local-cli.yaml", [][2]string{
			{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"},
			{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"},
			{"QUACK_JUDGE_MODEL", "gemma4-26b-a4b"},
		}},
		{"remote-full.yaml", [][2]string{
			{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"},
			{"QUACK_DATABASE_URL", "postgres://localhost/db"}, {"QUACK_QDRANT_URL", "http://qdrant:6334"},
			{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"}, {"QUACK_CODER_MODEL", "qwen3.8-27b"},
			{"QUACK_MEDIA_MODEL", "qwen3-omni-30b"}, {"QUACK_IMAGE_MODEL", "qwen3-vl-32b"},
			{"QUACK_EMBED_MODEL", "qwen3-embed"}, {"QUACK_JUDGE_MODEL", "gemma4-26b-a4b"},
			{"QUACK_COMPACTION_MODEL", "gemma4-26b-a4b"}, {"QUACK_COMPACTION_ENABLED", "true"},
			{"QUACK_SEARXNG_URL", "http://searxng:8080"}, {"QUACK_EXA_API_KEY", "sk-test"}, {"QUACK_CRAWL4AI_URL", "http://crawl4ai:11235"},
			{"QUACK_WORKSPACE_ROOT", "/tmp/ws"}, {"QUACK_OTEL_OTLP_ENDPOINT", "http://otel:4318"},
			{"QUACK_RECORDING_ENABLED", "false"}, {"QUACK_GITHUB_TOKEN", "t"},
		}},
		{"docker-compose.yaml", nil}, // fully resolved, no env refs
	} {
		t.Run(tc.file, func(t *testing.T) {
			for _, kv := range tc.env {
				t.Setenv(kv[0], kv[1])
			}
			c, err := Load(filepath.Join("..", "..", "docs", "configuration", "examples", tc.file))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			for name, a := range c.Agents {
				if a.Acp != nil && len(a.Acp.Command) > 0 {
					if got, want := a.Acp.Command[0], "node"; got != want {
						t.Errorf("agent %s acp.command[0] = %q, want %q (pi-acp shim; opencode left the image)", name, got, want)
					}
				}
			}
		})
	}
}
