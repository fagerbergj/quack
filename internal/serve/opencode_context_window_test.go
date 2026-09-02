package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/workspace"
)

// opencodeEnv must carry the agent's configured context_window through as
// the model's limit.context, and omit it entirely when unset - the pi-acp
// shim reads this field to set pi's contextWindow instead of pi's 128000 default.
func TestOpencodeEnvContextWindow(t *testing.T) {
	modelLimit := func(ac config.AgentConfig) map[string]any {
		env := opencodeEnv(config.ProviderConfig{}, ac, nil, workspace.Caps{Sandbox: workspace.SandboxLandlock})
		raw := strings.TrimPrefix(env[0], "OPENCODE_CONFIG_CONTENT=")
		var cfg struct {
			Provider struct {
				Quack struct {
					Models map[string]struct {
						Limit map[string]any `json:"limit"`
					} `json:"models"`
				} `json:"quack"`
			} `json:"provider"`
		}
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatalf("OPENCODE_CONFIG_CONTENT: %v:\n%s", err, raw)
		}
		return cfg.Provider.Quack.Models[ac.Model].Limit
	}

	limit := modelLimit(config.AgentConfig{Model: "m", ContextWindow: 65536, Acp: &config.AcpAgentConfig{}})
	if got, _ := limit["context"].(float64); int(got) != 65536 {
		t.Fatalf("expected limit.context 65536, got %+v", limit)
	}

	if limit := modelLimit(config.AgentConfig{Model: "m", Acp: &config.AcpAgentConfig{}}); limit != nil {
		t.Fatalf("expected no limit block when context_window unset, got %+v", limit)
	}
}
