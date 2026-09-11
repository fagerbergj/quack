package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// piACPEnv must carry context_window and max_output_tokens together, and omit
// both when context_window is unset - unset falls back to pi's 128000 default instead of quack's configured one.
func TestPiACPEnvContextWindow(t *testing.T) {
	fields := func(ac config.AgentConfig) map[string]any {
		env := piACPEnv(config.ProviderConfig{}, ac, nil)
		raw := strings.TrimPrefix(env[0], "PI_ACP_CONFIG=")
		var cfg map[string]any
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatalf("PI_ACP_CONFIG: %v:\n%s", err, raw)
		}
		return cfg
	}

	cfg := fields(config.AgentConfig{Model: "m", ContextWindow: 65536, Acp: &config.AcpAgentConfig{}})
	if got, _ := cfg["context_window"].(float64); int(got) != 65536 {
		t.Fatalf("expected context_window 65536, got %+v", cfg)
	}
	if _, ok := cfg["max_output_tokens"]; !ok {
		t.Fatalf("expected max_output_tokens alongside context_window, got %+v", cfg)
	}

	cfg = fields(config.AgentConfig{Model: "m", Acp: &config.AcpAgentConfig{}})
	if _, ok := cfg["context_window"]; ok {
		t.Fatalf("expected no context_window when context_window unset, got %+v", cfg)
	}
	if _, ok := cfg["max_output_tokens"]; ok {
		t.Fatalf("expected no max_output_tokens when context_window unset, got %+v", cfg)
	}
}

// TestPiACPEnvFlatShape pins the payload's field set to exactly what the
// pi-acp shim reads - a stray extra key here is one pi silently ignores.
func TestPiACPEnvFlatShape(t *testing.T) {
	env := piACPEnv(config.ProviderConfig{Endpoint: "http://x/v1", APIKey: "k"},
		config.AgentConfig{Model: "m", ContextWindow: 65536, Acp: &config.AcpAgentConfig{}},
		[]string{"/skills"})
	if len(env) != 1 || !strings.HasPrefix(env[0], "PI_ACP_CONFIG=") {
		t.Fatalf("unexpected env: %v", env)
	}
	raw := strings.TrimPrefix(env[0], "PI_ACP_CONFIG=")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("PI_ACP_CONFIG: %v:\n%s", err, raw)
	}
	want := map[string]bool{
		"endpoint": true, "api_key": true, "model": true,
		"context_window": true, "max_output_tokens": true, "skill_paths": true,
	}
	for k := range cfg {
		if !want[k] {
			t.Errorf("unexpected field %q in PI_ACP_CONFIG: %v", k, cfg)
		}
	}
	if cfg["endpoint"] != "http://x/v1" || cfg["api_key"] != "k" || cfg["model"] != "m" {
		t.Errorf("unexpected values: %+v", cfg)
	}
}

// TestPiACPEnvDefaultsAPIKey: an unset provider key becomes "unused" - pi's
// OpenAI-compatible client still requires a non-empty Authorization value.
func TestPiACPEnvDefaultsAPIKey(t *testing.T) {
	env := piACPEnv(config.ProviderConfig{Endpoint: "http://x/v1"}, config.AgentConfig{Model: "m", Acp: &config.AcpAgentConfig{}}, nil)
	raw := strings.TrimPrefix(env[0], "PI_ACP_CONFIG=")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("PI_ACP_CONFIG: %v:\n%s", err, raw)
	}
	if cfg["api_key"] != "unused" {
		t.Errorf("api_key = %v, want %q", cfg["api_key"], "unused")
	}
}
