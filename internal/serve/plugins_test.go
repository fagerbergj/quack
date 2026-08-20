package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/workspace"
	"gopkg.in/yaml.v3"
)

// The migration proof: the manifest quack ships for the usage extension
// declares a module that IS linked, so a stock boot passes this check.
func TestCheckPluginModules_LinkedModulePasses(t *testing.T) {
	p := plugin.Plugin{Name: "usage", Modules: []plugin.Module{{Name: "usage", Path: "github.com/fagerbergj/quack-extensions/usage"}}}
	if err := checkPluginModules([]plugin.Plugin{p}); err != nil {
		t.Fatalf("checkPluginModules: %v", err)
	}
}

// Go cannot load a Go module at runtime, so a manifest promising one this
// binary was not built with must fail the boot naming the import to add -
// never boot a server quietly missing what it advertised.
func TestCheckPluginModules_UnlinkedModuleIsABootError(t *testing.T) {
	p := plugin.Plugin{Name: "ghost", Modules: []plugin.Module{{Name: "ghost", Path: "github.com/fagerbergj/quack-extensions/ghost"}}}
	err := checkPluginModules([]plugin.Plugin{p})
	if err == nil {
		t.Fatal("checkPluginModules = nil, want an error")
	}
	if !strings.Contains(err.Error(), "github.com/fagerbergj/quack-extensions/ghost") {
		t.Errorf("error %q does not name the missing import path", err)
	}
}

func node(t *testing.T, body string) yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Content) == 0 {
		return yaml.Node{}
	}
	return *doc.Content[0]
}

func TestCheckPluginConfig(t *testing.T) {
	required := plugin.Plugin{Name: "usage", ConfigRequired: true, Modules: []plugin.Module{{Name: "usage", Path: "x"}}}
	optional := plugin.Plugin{Name: "noop", Modules: []plugin.Module{{Name: "noop", Path: "x"}}}

	// Unconfigured stays dormant, exactly as before - never a boot failure.
	if err := checkPluginConfig([]plugin.Plugin{required}, map[string]yaml.Node{}); err != nil {
		t.Errorf("unconfigured required plugin: %v", err)
	}
	if err := checkPluginConfig([]plugin.Plugin{required}, map[string]yaml.Node{"usage": node(t, "prometheus_url: http://p:9090")}); err != nil {
		t.Errorf("configured required plugin: %v", err)
	}
	// Present-but-empty is the failure the namespace block is declaring.
	// The shape an operator actually types is `extensions: {usage: {}}` - a
	// mapping node with no content, not a zero Node.
	if err := checkPluginConfig([]plugin.Plugin{required}, map[string]yaml.Node{"usage": node(t, "{}")}); err == nil {
		t.Error("empty mapping for config:required = nil, want a boot error")
	}
	if err := checkPluginConfig([]plugin.Plugin{required}, map[string]yaml.Node{"usage": {}}); err == nil {
		t.Error("zero node for config:required = nil, want a boot error")
	}
	// A skill-only or optional-config plugin keeps warn-and-skip semantics.
	if err := checkPluginConfig([]plugin.Plugin{optional}, map[string]yaml.Node{"noop": {}}); err != nil {
		t.Errorf("empty block for optional config: %v", err)
	}
}

// TestPluginMCPTools_HangingServerCostsOnlyItsTools pins the boot-enumeration
// deadline (spec §7.2.2 rule 5): a server that never handshakes must time out
// and cost only its own tools, not stall the boot.
func TestPluginMCPTools_HangingServerCostsOnlyItsTools(t *testing.T) {
	old := mcpEnumerateTimeout
	mcpEnumerateTimeout = 300 * time.Millisecond
	defer func() { mcpEnumerateTimeout = old }()

	p := plugin.Plugin{
		Name: "hang", Root: t.TempDir(),
		MCPServers: map[string]plugin.MCPServer{"sleeper": {Command: "sleep", Args: []string{"100"}}},
	}
	caps := workspace.Caps{Sandbox: workspace.SandboxNone}
	start := time.Now()
	tools := pluginMCPTools(context.Background(), []plugin.Plugin{p}, t.TempDir(), caps)
	if len(tools) != 0 {
		t.Errorf("tools = %d, want 0", len(tools))
	}
	// Transport teardown adds a few seconds of kill-grace on top of the deadline.
	if e := time.Since(start); e > 15*time.Second {
		t.Fatalf("enumeration took %v; the deadline did not fire", e)
	}
}
