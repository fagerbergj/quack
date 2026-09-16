package serve

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
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

// TestSeedRegistryWarnsOnIdentityCollisionKeepsDiskRow: a seed entry sharing
// a name with an on-disk row under a DIFFERENT identity is warned about, not
// fatal, and the on-disk row is left untouched (#1430 carry-over).
func TestSeedRegistryWarnsOnIdentityCollisionKeepsDiskRow(t *testing.T) {
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	if err := reg.Put(context.Background(), pluginreg.Plugin{
		Name: "widgets", Source: pluginreg.SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets",
	}); err != nil {
		t.Fatal(err)
	}
	if err := seedRegistry(context.Background(), reg, []string{"github:other/widgets"}); err != nil {
		t.Fatalf("seedRegistry should warn, not fail, on an identity collision: %v", err)
	}
	rows, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Owner != "acme" {
		t.Fatalf("rows = %+v, want the on-disk row (acme/widgets) untouched", rows)
	}
}

// TestSeedRegistryPropagatesPutFailureOnNewInsert: a new seed name whose
// Put fails (here: blocked by a same-path file, a real on-disk collision)
// surfaces the error rather than being swallowed.
func TestSeedRegistryPropagatesPutFailureOnNewInsert(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "widgets"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := pluginreg.NewFSRegistry(root)
	if err := seedRegistry(context.Background(), reg, []string{"github:acme/widgets"}); err == nil {
		t.Fatal("seedRegistry = nil, want the blocked Put's error to propagate")
	}
}

// TestPersistPluginRefusalLogsOnWriteFailure: a Put failure while
// persisting a boot refusal is logged, not returned or panicked on -
// there is nothing left for the caller to do about it.
func TestPersistPluginRefusalLogsOnWriteFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bad"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := pluginreg.NewFSRegistry(root)
	rows := []pluginreg.Plugin{{Name: "bad", Source: pluginreg.SourceLocal, Entry: "bad"}}
	persistPluginRefusal(context.Background(), reg, rows, "bad", errors.New("boom"))

	if !strings.Contains(buf.String(), "failed to persist plugin refusal") {
		t.Fatalf("log output = %q, want the persist-failure warning", buf.String())
	}
}

// TestAdmitPlugins_PersistRefusalsGatesTheRegistryWrite: a replay boot passes
// persistRefusals=false, so a refused row's error is never written to the registry.
func TestAdmitPlugins_PersistRefusalsGatesTheRegistryWrite(t *testing.T) {
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	row := pluginreg.Plugin{Name: "ghost", Source: pluginreg.SourceLocal, Entry: "ghost"}
	if err := reg.Put(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	rows := []pluginreg.Plugin{row}
	ghost := plugin.Plugin{Name: "ghost", Modules: []plugin.Module{{Name: "ghost", Path: "github.com/fagerbergj/quack-extensions/ghost"}}}

	admitted, refusals, err := admitPlugins(context.Background(), reg, rows, []plugin.Plugin{ghost}, nil, nil, false)
	if err != nil {
		t.Fatalf("admitPlugins: %v", err)
	}
	if len(admitted) != 0 || refusals["ghost"] == nil {
		t.Fatalf("admitted=%v refusals=%v, want ghost dropped and refused either way", admitted, refusals)
	}
	got, err := reg.List(context.Background())
	if err != nil || got[0].Error != "" {
		t.Fatalf("row after persistRefusals=false: %+v, err %v, want Error unwritten", got, err)
	}

	if _, _, err := admitPlugins(context.Background(), reg, rows, []plugin.Plugin{ghost}, nil, nil, true); err != nil {
		t.Fatalf("admitPlugins: %v", err)
	}
	got, err = reg.List(context.Background())
	if err != nil || got[0].Error == "" {
		t.Fatalf("row after persistRefusals=true: %+v, err %v, want the refusal persisted", got, err)
	}
}

// mcpJSONBody is a minimal, schema-valid mcp.json declaring one stdio server.
const mcpJSONBody = `{"$schema":"https://agent-plugins.org/schemas/1.1.0/mcp.schema.json","mcpServers":{"foo":{"type":"stdio","command":"echo"}}}`

// TestResolveRegistryPlugins_GithubRowMCPIsClearedWithWarning is the epic's
// documented scope (#1427: "mcp.json in a fetched plugin is ignored with a
// warning, follow-up #1434") - a github: row's mcp.json must not spawn
// stdio servers, unlike a local root's.
func TestResolveRegistryPlugins_GithubRowMCPIsClearedWithWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	registryRoot := t.TempDir()
	clone := pluginreg.CloneDir(registryRoot, "ghrepo")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	writePluginManifest(t, clone, "ghrepo")
	if err := os.WriteFile(filepath.Join(clone, "mcp.json"), []byte(mcpJSONBody), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := []pluginreg.Plugin{{Name: "ghrepo", Source: pluginreg.SourceGitHub, Entry: "github:acme/ghrepo", Owner: "acme", Repo: "ghrepo"}}

	plugins, err := resolveRegistryPlugins(registryRoot, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 {
		t.Fatalf("resolveRegistryPlugins = %d plugins, want 1", len(plugins))
	}
	if len(plugins[0].MCPServers) != 0 {
		t.Fatalf("github row MCPServers = %v, want cleared (epic scope: fetched plugins ignore mcp.json)", plugins[0].MCPServers)
	}
	if !strings.Contains(buf.String(), "mcp.json ignored") || !strings.Contains(buf.String(), "ghrepo") || !strings.Contains(buf.String(), "#1434") {
		t.Fatalf("log output = %q, want a warning naming ghrepo and #1434", buf.String())
	}
}

// TestResolveRegistryPlugins_LocalRowKeepsMCPServers: a local root (config-
// only, plugins.seed) is unaffected - only github: rows are in #1434's scope.
func TestResolveRegistryPlugins_LocalRowKeepsMCPServers(t *testing.T) {
	root := t.TempDir()
	writePluginManifest(t, root, "local-plugin")
	if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte(mcpJSONBody), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := []pluginreg.Plugin{{Name: "local-plugin", Source: pluginreg.SourceLocal, Entry: root}}

	plugins, err := resolveRegistryPlugins(t.TempDir(), rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 {
		t.Fatalf("resolveRegistryPlugins = %d plugins, want 1", len(plugins))
	}
	if len(plugins[0].MCPServers) != 1 {
		t.Fatalf("local row MCPServers = %v, want the one declared server kept", plugins[0].MCPServers)
	}
}
