package eval

import (
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Orchestrator: config.OrchestratorConfig{Model: "orch-old"},
		Agents: map[string]config.AgentConfig{
			"code-implementer": {Model: "coder-old", Acp: &config.AcpAgentConfig{Command: []string{"opencode", "acp"}}},
			"code-reviewer":    {Model: "coder-old", Acp: &config.AcpAgentConfig{Command: []string{"opencode", "acp"}}},
			"web-researcher":   {Model: "researcher-old"},
			"synthesizer":      {Model: "researcher-old"},
			"image-reader":     {Model: "image-old", Inputs: []string{"text", "image"}},
		},
		Gates: config.GatesConfig{Judge: config.JudgeConfig{Model: "judge-fixed", MaxRounds: 1}},
	}
}

func TestOverrideModel_Coder(t *testing.T) {
	cfg := testConfig()
	changed, err := OverrideModel(cfg, RoleCoder, "new-model")
	if err != nil {
		t.Fatalf("OverrideModel: %v", err)
	}
	want := []string{"code-implementer", "code-reviewer"}
	if len(changed) != len(want) || changed[0] != want[0] || changed[1] != want[1] {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if cfg.Agents["code-implementer"].Model != "new-model" || cfg.Agents["code-reviewer"].Model != "new-model" {
		t.Errorf("coder agents not overridden: %+v", cfg.Agents)
	}
	if cfg.Agents["web-researcher"].Model != "researcher-old" {
		t.Errorf("researcher agent should be untouched by --role coder, got %q", cfg.Agents["web-researcher"].Model)
	}
	if cfg.Orchestrator.Model != "orch-old" {
		t.Errorf("orchestrator should be untouched by --role coder, got %q", cfg.Orchestrator.Model)
	}
	if cfg.Gates.Judge.Model != "judge-fixed" {
		t.Errorf("judge model must NEVER be overridden, got %q", cfg.Gates.Judge.Model)
	}
}

func TestOverrideModel_Researcher(t *testing.T) {
	cfg := testConfig()
	changed, err := OverrideModel(cfg, RoleResearcher, "new-model")
	if err != nil {
		t.Fatalf("OverrideModel: %v", err)
	}
	want := []string{"synthesizer", "web-researcher"}
	if len(changed) != len(want) || changed[0] != want[0] || changed[1] != want[1] {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if cfg.Agents["code-implementer"].Model != "coder-old" {
		t.Errorf("coder agent should be untouched by --role researcher, got %q", cfg.Agents["code-implementer"].Model)
	}
	// A media/image agent takes non-text input, so it's neither coder nor
	// researcher - it must stay untouched even though it has no acp: block.
	if cfg.Agents["image-reader"].Model != "image-old" {
		t.Errorf("image-reader must be untouched by --role researcher, got %q", cfg.Agents["image-reader"].Model)
	}
}

func TestOverrideModel_Orch(t *testing.T) {
	cfg := testConfig()
	changed, err := OverrideModel(cfg, RoleOrch, "new-model")
	if err != nil {
		t.Fatalf("OverrideModel: %v", err)
	}
	if len(changed) != 1 || changed[0] != "orchestrator" {
		t.Errorf("changed = %v, want [orchestrator]", changed)
	}
	if cfg.Orchestrator.Model != "new-model" {
		t.Errorf("orchestrator.Model = %q, want new-model", cfg.Orchestrator.Model)
	}
	if cfg.Agents["code-implementer"].Model != "coder-old" {
		t.Errorf("agents must be untouched by --role orch")
	}
}

func TestOverrideModel_All(t *testing.T) {
	cfg := testConfig()
	changed, err := OverrideModel(cfg, RoleAll, "new-model")
	if err != nil {
		t.Fatalf("OverrideModel: %v", err)
	}
	// orchestrator + 4 agents (image-reader excluded), sorted.
	want := []string{"code-implementer", "code-reviewer", "orchestrator", "synthesizer", "web-researcher"}
	if len(changed) != len(want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	for i, w := range want {
		if changed[i] != w {
			t.Errorf("changed[%d] = %q, want %q", i, changed[i], w)
		}
	}
	if cfg.Agents["image-reader"].Model != "image-old" {
		t.Errorf("image-reader must never be touched, even by --role all")
	}
	if cfg.Gates.Judge.Model != "judge-fixed" {
		t.Errorf("judge model must never be touched, even by --role all")
	}
}

func TestOverrideModel_UnknownRole(t *testing.T) {
	cfg := testConfig()
	if _, err := OverrideModel(cfg, "bogus", "new-model"); err == nil {
		t.Error("OverrideModel with an unknown role: want error, got nil")
	}
}

func TestOverrideModel_EmptyModel(t *testing.T) {
	cfg := testConfig()
	if _, err := OverrideModel(cfg, RoleAll, ""); err == nil {
		t.Error("OverrideModel with an empty model: want error, got nil")
	}
}
