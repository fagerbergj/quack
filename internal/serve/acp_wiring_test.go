package serve

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestResolveGateCfg_SetsMemoryArtifactAndPlugins: a gated agent's MemoryArtifact and Plugins must land
// on the boot-resolved vetting.Config, not just its scalar grading facts.
func TestResolveGateCfg_SetsMemoryArtifactAndPlugins(t *testing.T) {
	ctx := context.Background()
	bundle, err := agent.LoadBundle(ctx, nil, "../../.agents/plugins/github/agents/code-reviewer")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	memArt := artifactsrc.Artifact{Name: "memory/code-reviewer", Source: "static", VersionID: "m1"}
	cfg := &config.Config{Gates: config.GatesConfig{DeterministicChecks: config.StageConfig{MaxRounds: 1}}}
	reg := pluginreg.NewFSRegistry(t.TempDir())
	gateCfgs := newGateConfigs(1)

	if _, err := resolveGateCfg(cfg, nil, vetting.Config{}, "code-reviewer", config.AgentConfig{Bundle: "../../.agents/plugins/github/agents/code-reviewer"},
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
	if len(c.Plugins) != 1 || c.Plugins[0].Name != "quack" || c.Plugins[0].SHA != "" {
		t.Errorf("Plugins = %+v, want exactly [{quack \"\"}] (an empty registry - only the embedded fallback)", c.Plugins)
	}
}
