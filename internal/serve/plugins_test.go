package serve

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/plugin"
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
	if err := checkPluginConfig([]plugin.Plugin{required}, map[string]yaml.Node{"usage": {}}); err == nil {
		t.Error("empty block for config:required = nil, want a boot error")
	}
	// A skill-only or optional-config plugin keeps warn-and-skip semantics.
	if err := checkPluginConfig([]plugin.Plugin{optional}, map[string]yaml.Node{"noop": {}}); err != nil {
		t.Errorf("empty block for optional config: %v", err)
	}
}
