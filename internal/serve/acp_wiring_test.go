package serve

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/acp"
	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestResolveGateCfg_SetsMemoryArtifactAndPlugins pins #1455 B1/B2 at the
// point they broke: a gated agent's MemoryArtifact and Plugins must land on
// the boot-resolved vetting.Config, not just its scalar grading facts.
func TestResolveGateCfg_SetsMemoryArtifactAndPlugins(t *testing.T) {
	ctx := context.Background()
	bundle, err := agent.LoadBundle(ctx, nil, "agents/code-reviewer")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	memArt := artifactsrc.Artifact{Name: "memory/code-reviewer", Source: "static", VersionID: "m1"}
	cfg := &config.Config{Gates: config.GatesConfig{DeterministicChecks: config.StageConfig{MaxRounds: 1}}}
	reg := pluginreg.NewFSRegistry(t.TempDir())
	gateCfgs := newGateConfigs(1)

	if _, err := resolveGateCfg(cfg, nil, vetting.Config{}, "code-reviewer", config.AgentConfig{Bundle: "agents/code-reviewer"},
		true, "## remember", memArt, bundle, gateCfgs, reg); err != nil {
		t.Fatalf("resolveGateCfg: %v", err)
	}

	c, ok := gateCfgs.boot["code-reviewer"]
	if !ok {
		t.Fatal("resolveGateCfg did not register a boot config")
	}
	if c.MemoryArtifact.Name != memArt.Name || c.MemoryArtifact.Source != memArt.Source || c.MemoryArtifact.VersionID != memArt.VersionID {
		t.Errorf("MemoryArtifact = %+v, want %+v", c.MemoryArtifact, memArt)
	}
	if len(c.Plugins) == 0 {
		t.Error("Plugins is empty, want at least the always-in-scope embedded quack ref")
	}
}

// TestBuildACPNode_WiresPreambleAndMemoryArtifact pins #1455 B1 directly at
// the regression site: buildACPNode resolves memArt for resolveGateCfg and
// must hand the SAME artifact to acp.Options, and PreambleArtifact must read
// back the exact artifact the preamble body was built from.
func TestBuildACPNode_WiresPreambleAndMemoryArtifact(t *testing.T) {
	ctx := context.Background()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	builtinSkillSrc := newSkillSource(nil)
	cfg := &config.Config{Gates: config.GatesConfig{DeterministicChecks: config.StageConfig{MaxRounds: 1}}}
	ac := config.AgentConfig{Bundle: "agents/code-reviewer", Acp: &config.AcpAgentConfig{Command: []string{"/bin/true"}}}
	reg := pluginreg.NewFSRegistry(t.TempDir())
	gateCfgs := newGateConfigs(1)
	taskStore := &memory.Store{} // never dereferenced by buildACPNode - only nil-checked

	ag, err := buildACPNode("code-reviewer", ac, config.ProviderConfig{}, cfg, nil, workspace.Caps{}, jail, taskStore,
		builtinSkillSrc, vetting.Config{}, gateCfgs, nil, nil, nil, nil, nil, nil, reg)
	if err != nil {
		t.Fatalf("buildACPNode: %v", err)
	}
	a, ok := ag.(*acp.Agent)
	if !ok {
		t.Fatalf("buildACPNode returned %T, want *acp.Agent", ag)
	}
	opts := acp.OptionsForTesting(a)

	bundle, err := agent.LoadBundle(ctx, nil, "agents/code-reviewer")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	wantMem, _, err := agent.LoadBundleMemory(ctx, nil, "agents/code-reviewer")
	if err != nil {
		t.Fatalf("LoadBundleMemory: %v", err)
	}
	if wantMem == "" {
		t.Fatal("fixture bundle agents/code-reviewer has no memory.md - test proves nothing")
	}
	if opts.MemoryArtifact.Name != "memory/code-reviewer" || opts.MemoryArtifact.VersionID == "" {
		t.Errorf("Options.MemoryArtifact = %+v, want a resolved memory/code-reviewer artifact", opts.MemoryArtifact)
	}

	if opts.PreambleArtifact == nil {
		t.Fatal("Options.PreambleArtifact is nil")
	}
	// Building the preamble once (as steerHooks does) must stash the exact
	// artifact PreambleArtifact then reads back.
	_ = opts.Preamble(ctx)
	wantPrompt := bundle.ResolvePrompt(ctx, nil)
	got := opts.PreambleArtifact(ctx)
	if got.Name != wantPrompt.Name || got.VersionID != wantPrompt.VersionID {
		t.Errorf("PreambleArtifact() = %+v, want %+v (the version the preamble body was built from)", got, wantPrompt)
	}
}
