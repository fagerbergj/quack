package serve

import (
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
)

// TestBindJudgeRefresher proves system/judge's resolved Config rebinds the
// shared judge model and maps effort onto thinking_level, and that an
// invalid override falls back to gates.judge's static binding (#1421 P2).
func TestBindJudgeRefresher(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"judge-prov": {Kind: "replay", Bundle: writeCurrentDateReplayFixture(t)},
			"other-prov": {Kind: "replay", Bundle: writeCurrentDateReplayFixture(t)},
		},
		Models: map[string]config.ModelConfig{
			"judge-model": {Provider: "judge-prov"},
			"bound-judge": {Provider: "other-prov"},
		},
		Gates: config.GatesConfig{
			Judge: config.JudgeConfig{Provider: "judge-prov", Model: "judge-model", MaxRounds: 1, ThinkingLevel: "low"},
		},
	}
	jprov := cfg.Providers["judge-prov"]
	static, err := inference.NewModelWithEffort(jprov, cfg.Gates.Judge.Model, artifact.InMemoryService(), nil, "")
	if err != nil {
		t.Fatalf("static judge model: %v", err)
	}
	overridable := inference.NewOverridable(static)
	refresh := bindJudgeRefresher(cfg, jprov, artifact.InMemoryService(), overridable)

	// No override: static binding, static thinking_level.
	if got := refresh(artifactsrc.Artifact{Name: "system/judge"}); got != "low" {
		t.Errorf("thinking_level = %q, want the static low", got)
	}
	if got := overridable.Name(); got != "judge-model" {
		t.Errorf("Name() = %q, want the static judge-model", got)
	}

	// Valid override: model, provider and effort all rebind.
	got := refresh(artifactsrc.Artifact{Name: "system/judge", Config: map[string]any{"model": "bound-judge", "effort": "high"}})
	if got != "high" {
		t.Errorf("thinking_level = %q, want high", got)
	}
	if got := overridable.Name(); got != "bound-judge" {
		t.Errorf("Name() = %q, want bound-judge after a valid override", got)
	}

	// Invalid override: falls back to the static binding, not a broken round.
	got = refresh(artifactsrc.Artifact{Name: "system/judge", Config: map[string]any{"model": "no-such-model"}})
	if got != "low" {
		t.Errorf("thinking_level = %q, want the static low after an invalid override", got)
	}
	if got := overridable.Name(); got != "judge-model" {
		t.Errorf("Name() = %q, want the static judge-model after an invalid override", got)
	}
}
