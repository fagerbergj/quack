package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// opencodeEnv must emit an acp mcp_servers URL in opencode's KEYED mcp map shape
// — {name:{type:"remote",url,enabled}}, the form opencode.json uses — not
// {"servers":[...]}, which opencode silently ignores (the server would never
// load, so the tools never reach the agent). Regression for #250: the first
// implementation compiled and passed the gate but used the ignored shape.
func TestOpencodeEnvMcpServersShape(t *testing.T) {
	env := opencodeEnv(config.ProviderConfig{}, config.AgentConfig{
		Model: "m",
		Acp:   &config.AcpAgentConfig{Command: []string{"opencode", "acp"}, McpServers: []string{"https://mcp.context7.com/mcp"}},
	}, nil)
	if len(env) != 1 || !strings.HasPrefix(env[0], "OPENCODE_CONFIG_CONTENT=") {
		t.Fatalf("unexpected env: %v", env)
	}
	raw := strings.TrimPrefix(env[0], "OPENCODE_CONFIG_CONTENT=")

	var cfg struct {
		Mcp map[string]struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Enabled bool   `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT is not the keyed mcp shape (%v):\n%s", err, raw)
	}
	if _, wrong := cfg.Mcp["servers"]; wrong {
		t.Fatalf("mcp used the ignored {servers:[...]} shape:\n%s", raw)
	}
	c7, ok := cfg.Mcp["context7"]
	if !ok {
		t.Fatalf("context7 not keyed into the mcp map:\n%s", raw)
	}
	if c7.Type != "remote" || c7.URL != "https://mcp.context7.com/mcp" || !c7.Enabled {
		t.Fatalf("context7 entry wrong: %+v", c7)
	}
}
