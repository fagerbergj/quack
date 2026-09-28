package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/workspace"
)

// piACPEnv must carry context_window and max_output_tokens together, and omit
// both when context_window is unset - unset falls back to pi's 128000 default instead of quack's configured one.
func TestPiACPEnvContextWindow(t *testing.T) {
	fields := func(ac config.AgentConfig) map[string]any {
		env := piACPEnv(config.ProviderConfig{}, ac, nil, workspace.SandboxBwrap)
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
		config.AgentConfig{Model: "m", ContextWindow: 65536, Acp: &config.AcpAgentConfig{ReadOnly: true, AllowClone: true}},
		[]string{"/skills"}, workspace.SandboxBwrap)
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
		"context_window": true, "max_output_tokens": true, "skill_paths": true, "allow_clone": true,
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
	env := piACPEnv(config.ProviderConfig{Endpoint: "http://x/v1"}, config.AgentConfig{Model: "m", Acp: &config.AcpAgentConfig{}}, nil, workspace.SandboxBwrap)
	raw := strings.TrimPrefix(env[0], "PI_ACP_CONFIG=")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("PI_ACP_CONFIG: %v:\n%s", err, raw)
	}
	if cfg["api_key"] != "unused" {
		t.Errorf("api_key = %v, want %q", cfg["api_key"], "unused")
	}
}

// TestPiACPEnvAllowClone: acp.allow_clone reaches the shim only under a sandbox
// that enforces the read-only work tree the clone deny's lift rests on.
func TestPiACPEnvAllowClone(t *testing.T) {
	cases := []struct {
		name    string
		acp     config.AcpAgentConfig
		sandbox workspace.SandboxMode
		want    bool
	}{
		{"bwrap", config.AcpAgentConfig{ReadOnly: true, AllowClone: true}, workspace.SandboxBwrap, true},
		{"landlock", config.AcpAgentConfig{ReadOnly: true, AllowClone: true}, workspace.SandboxLandlock, true},
		{"none drops it", config.AcpAgentConfig{ReadOnly: true, AllowClone: true}, workspace.SandboxNone, false},
		{"unset", config.AcpAgentConfig{ReadOnly: true}, workspace.SandboxBwrap, false},
		{"without read_only", config.AcpAgentConfig{AllowClone: true}, workspace.SandboxBwrap, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := piACPEnv(config.ProviderConfig{}, config.AgentConfig{Model: "m", Acp: &tc.acp}, nil, tc.sandbox)
			var cfg map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(env[0], "PI_ACP_CONFIG=")), &cfg); err != nil {
				t.Fatal(err)
			}
			if got := cfg["allow_clone"] == true; got != tc.want {
				t.Errorf("allow_clone = %v, want %v (cfg %v)", cfg["allow_clone"], tc.want, cfg)
			}
		})
	}
}
