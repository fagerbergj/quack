package serve

import (
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// An orchestrator with no declared context_window must not make kv a
// scheduling dimension: reserving the model's whole budget (#1067) would let
// one turn block every worker node on that model.
func TestOrchestratorSpecOmitsKVWhenNoContextWindow(t *testing.T) {
	cfg := &config.Config{
		Orchestrator: config.OrchestratorConfig{Model: "m"},
		Models: map[string]config.ModelConfig{
			"m": {Provider: "p", Role: "worker", ContextWindow: 262144, Limits: &config.ModelLimits{Sessions: 4, KVTokens: 262144}},
		},
	}
	spec := orchestratorSpec(cfg)
	if spec.KVTokens != 0 {
		t.Fatalf("KVTokens = %d, want 0 - an undeclared window must not reserve the model's whole kv budget", spec.KVTokens)
	}
	if spec.Model != "m" || spec.Provider != "p" || spec.Role != "worker" {
		t.Fatalf("spec lost its identity dimensions: %+v", spec)
	}
}

// A declared window is honoured, so an operator can opt the orchestrator into
// kv accounting at a size that actually fits alongside its workers.
func TestOrchestratorSpecUsesDeclaredContextWindow(t *testing.T) {
	cfg := &config.Config{
		Orchestrator: config.OrchestratorConfig{Model: "m", ContextWindow: 65536},
		Models: map[string]config.ModelConfig{
			"m": {Provider: "p", Role: "worker", ContextWindow: 262144, Limits: &config.ModelLimits{Sessions: 4, KVTokens: 262144}},
		},
	}
	if got := orchestratorSpec(cfg).KVTokens; got != 65536 {
		t.Fatalf("KVTokens = %d, want 65536", got)
	}
}

// Without limits.kv_tokens the model has no kv dimension at all, declared
// window or not - the same rule admissionSpecFor applies to agents.
func TestOrchestratorSpecOmitsKVWhenModelHasNoKVLimit(t *testing.T) {
	cfg := &config.Config{
		Orchestrator: config.OrchestratorConfig{Model: "m", ContextWindow: 65536},
		Models:       map[string]config.ModelConfig{"m": {Provider: "p", Role: "worker", ContextWindow: 262144}},
	}
	if got := orchestratorSpec(cfg).KVTokens; got != 0 {
		t.Fatalf("KVTokens = %d, want 0", got)
	}
}
