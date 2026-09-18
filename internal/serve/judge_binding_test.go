package serve

import (
	"sync"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/vetting"
)

func testJudgeBindCfg(t *testing.T) (*config.Config, config.ProviderConfig) {
	t.Helper()
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"judge-prov": {Kind: "openai", Endpoint: "http://fake-provider.invalid"},
			"other-prov": {Kind: "openai", Endpoint: "http://fake-provider.invalid"},
		},
		Models: map[string]config.ModelConfig{
			"judge-model": {Provider: "judge-prov"},
			"bound-judge": {Provider: "other-prov"},
		},
		Gates: config.GatesConfig{
			Judge: config.JudgeConfig{Provider: "judge-prov", Model: "judge-model", MaxRounds: 1, ThinkingLevel: "low"},
		},
	}
	return cfg, cfg.Providers["judge-prov"]
}

// TestBindJudgeRefresher proves system/judge's resolved Config picks this
// round's own JudgeFactory+model and maps effort onto thinking_level, and
// that an invalid override falls back to gates.judge's static pair (#1421 P2).
func TestBindJudgeRefresher(t *testing.T) {
	cfg, jprov := testJudgeBindCfg(t)
	static, err := inference.NewModelWithEffort(jprov, cfg.Gates.Judge.Model, artifact.InMemoryService(), nil, "")
	if err != nil {
		t.Fatalf("static judge model: %v", err)
	}
	staticFactory := vetting.NewJudgeFactory(static, nil, nil)
	refresh := bindJudgeRefresher(cfg, jprov, artifact.InMemoryService(), static, staticFactory, staticFactory, nil, nil)

	// No override: static binding, static thinking_level.
	_, m, effort := refresh(artifactsrc.Artifact{Name: "system/judge"}, true)
	if effort != "low" || m.Name() != "judge-model" {
		t.Errorf("m=%q effort=%q, want judge-model/low", m.Name(), effort)
	}

	// Valid override: model, provider and effort all rebind.
	_, m, effort = refresh(artifactsrc.Artifact{Name: "system/judge", Config: map[string]any{"model": "bound-judge", "effort": "high"}}, true)
	if effort != "high" || m.Name() != "bound-judge" {
		t.Errorf("m=%q effort=%q, want bound-judge/high", m.Name(), effort)
	}

	// Invalid override: falls back to the static binding, not a broken round.
	_, m, effort = refresh(artifactsrc.Artifact{Name: "system/judge", Config: map[string]any{"model": "no-such-model"}}, true)
	if effort != "low" || m.Name() != "judge-model" {
		t.Errorf("m=%q effort=%q, want the static judge-model/low after an invalid override", m.Name(), effort)
	}
}

// TestBindJudgeRefresherCacheKeyIncludesProviderName is M-suggestion's regression
// test: two providers sharing an Endpoint but registered under different names
// (two accounts on one host) must resolve to distinct cached models, keyed by
// provider name too - not just Endpoint+Model, which would let account B
// silently reuse account A's built model/credentials.
func TestBindJudgeRefresherCacheKeyIncludesProviderName(t *testing.T) {
	sharedEndpoint := "http://shared-host"
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"judge-prov": {Kind: "openai", Endpoint: sharedEndpoint},
			"acct-a":     {Kind: "openai", Endpoint: sharedEndpoint},
			"acct-b":     {Kind: "openai", Endpoint: sharedEndpoint},
		},
		Models: map[string]config.ModelConfig{
			"judge-model": {Provider: "judge-prov"},
			"model-a":     {Provider: "acct-a"},
			"model-b":     {Provider: "acct-b"},
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
	staticFactory := vetting.NewJudgeFactory(static, nil, nil)
	refresh := bindJudgeRefresher(cfg, jprov, artifact.InMemoryService(), static, staticFactory, staticFactory, nil, nil)

	_, modelA, _ := refresh(artifactsrc.Artifact{Name: "system/judge", Config: map[string]any{"model": "model-a"}}, true)
	_, modelB, _ := refresh(artifactsrc.Artifact{Name: "system/judge", Config: map[string]any{"model": "model-b"}}, true)
	if modelA == modelB {
		t.Fatalf("model-a and model-b (different providers, same endpoint) resolved to the same cached model")
	}
	// Re-requesting model-a must hit the cache and return the exact same instance.
	_, modelAAgain, _ := refresh(artifactsrc.Artifact{Name: "system/judge", Config: map[string]any{"model": "model-a"}}, true)
	if modelAAgain != modelA {
		t.Fatalf("model-a was rebuilt instead of served from cache")
	}
}

// TestBindJudgeRefresherCacheKeySeparatesReadToolsEligibility is #1485's
// regression test: two nodes bound to the SAME override model - one a code
// node (hasReadTools true), one a research node (hasReadTools false) - must
// not share a cached factory, or the research node would inherit the code
// node's repo read tools (or vice versa). Each hasReadTools value gets its
// own cache entry and its own built model; repeating a value hits that entry.
func TestBindJudgeRefresherCacheKeySeparatesReadToolsEligibility(t *testing.T) {
	cfg, jprov := testJudgeBindCfg(t)
	static, err := inference.NewModelWithEffort(jprov, cfg.Gates.Judge.Model, artifact.InMemoryService(), nil, "")
	if err != nil {
		t.Fatalf("static judge model: %v", err)
	}
	staticFactory := vetting.NewJudgeFactory(static, nil, nil)
	refresh := bindJudgeRefresher(cfg, jprov, artifact.InMemoryService(), static, staticFactory, staticFactory, nil, nil)
	art := artifactsrc.Artifact{Name: "system/judge", Config: map[string]any{"model": "bound-judge"}}

	_, withTools, _ := refresh(art, true)
	_, withoutTools, _ := refresh(art, false)
	if withTools == withoutTools {
		t.Fatalf("hasReadTools=true and hasReadTools=false shared a cached model for the same override")
	}

	_, withToolsAgain, _ := refresh(art, true)
	if withToolsAgain != withTools {
		t.Fatalf("hasReadTools=true was rebuilt instead of served from its own cache entry")
	}
	_, withoutToolsAgain, _ := refresh(art, false)
	if withoutToolsAgain != withoutTools {
		t.Fatalf("hasReadTools=false was rebuilt instead of served from its own cache entry")
	}
}

// TestBindJudgeRefresherConcurrentNoCrossTalk is H1's required regression test:
// two goroutines resolving two DIFFERENT bindings, 500 rounds each, must each
// only ever see their own model - never a shared mutable swapped mid-flight.
// Run with -race.
func TestBindJudgeRefresherConcurrentNoCrossTalk(t *testing.T) {
	cfg, jprov := testJudgeBindCfg(t)
	static, err := inference.NewModelWithEffort(jprov, cfg.Gates.Judge.Model, artifact.InMemoryService(), nil, "")
	if err != nil {
		t.Fatalf("static judge model: %v", err)
	}
	staticFactory := vetting.NewJudgeFactory(static, nil, nil)
	refresh := bindJudgeRefresher(cfg, jprov, artifact.InMemoryService(), static, staticFactory, staticFactory, nil, nil)

	const rounds = 500
	run := func(modelName string, mismatches *int) {
		for i := 0; i < rounds; i++ {
			_, m, _ := refresh(artifactsrc.Artifact{Name: "system/judge", Config: map[string]any{"model": modelName}}, true)
			if m.Name() != modelName {
				*mismatches++
			}
		}
	}
	var wg sync.WaitGroup
	var aMismatches, bMismatches int
	wg.Add(2)
	go func() { defer wg.Done(); run("judge-model", &aMismatches) }()
	go func() { defer wg.Done(); run("bound-judge", &bMismatches) }()
	wg.Wait()

	if aMismatches != 0 || bMismatches != 0 {
		t.Fatalf("cross-talk: judge-model mismatches=%d, bound-judge mismatches=%d, want 0/0", aMismatches, bMismatches)
	}
}
