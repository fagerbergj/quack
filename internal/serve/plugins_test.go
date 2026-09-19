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

// TestAdmitPlugins_PersistsRefusalOnTheRegistryRow: a REST-added row's
// refusal is dropped from the roster AND written to its registry row, so
// GET /plugins shows why boot/rebuild dropped it.
func TestAdmitPlugins_PersistsRefusalOnTheRegistryRow(t *testing.T) {
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	row := pluginreg.Plugin{Name: "ghost", Source: pluginreg.SourceLocal, Entry: "ghost"}
	if err := reg.Put(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	rows := []pluginreg.Plugin{row}
	ghost := plugin.Plugin{Name: "ghost", Modules: []plugin.Module{{Name: "ghost", Path: "github.com/fagerbergj/quack-extensions/ghost"}}}

	admitted, refusals, err := admitPlugins(context.Background(), reg, rows, []plugin.Plugin{ghost}, nil, nil)
	if err != nil {
		t.Fatalf("admitPlugins: %v", err)
	}
	if len(admitted) != 0 || refusals["ghost"] == nil {
		t.Fatalf("admitted=%v refusals=%v, want ghost dropped and refused either way", admitted, refusals)
	}
	got, err := reg.List(context.Background())
	if err != nil || got[0].Error == "" {
		t.Fatalf("row after admitPlugins: %+v, err %v, want the refusal persisted", got, err)
	}
}

// pluginWithAgent builds a plugin.Plugin whose AgentsDir/Agents list exactly
// one real, valid bundle - the fixture admitPlugins' manifest checks need
// (CheckManifestLists must pass) to reach the cross-plugin collision check.
func pluginWithAgent(t *testing.T, pluginName, agentName string) plugin.Plugin {
	t.Helper()
	dir := t.TempDir()
	writeGenericAgentBundle(t, dir, agentName)
	return plugin.Plugin{Name: pluginName, Root: t.TempDir(), AgentsDir: dir, Agents: []string{agentName}}
}

// Two REST-added (non-seed) rows listing the same agent name: the second
// collides and is dropped, warned and recorded in refusals; the first stays
// admitted, and boot itself never fails for a non-seed row (#1430) - the
// exact scenario review finding 4 flagged as bricking boot before this fix.
func TestAdmitPlugins_CrossPluginCollisionDropsNonSeedRow(t *testing.T) {
	reg := pluginreg.NewFSRegistry(t.TempDir())
	a := pluginWithAgent(t, "a", "scout")
	b := pluginWithAgent(t, "b", "scout")
	rowA := pluginreg.Plugin{Name: "a", Source: pluginreg.SourceLocal, Entry: "a"}
	rowB := pluginreg.Plugin{Name: "b", Source: pluginreg.SourceLocal, Entry: "b"}
	if err := reg.Put(context.Background(), rowA); err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(context.Background(), rowB); err != nil {
		t.Fatal(err)
	}

	admitted, refusals, err := admitPlugins(context.Background(), reg, []pluginreg.Plugin{rowA, rowB}, []plugin.Plugin{a, b}, nil, nil)
	if err != nil {
		t.Fatalf("admitPlugins: %v", err)
	}
	if len(admitted) != 1 || admitted[0].Name != "a" {
		t.Fatalf("admitted = %+v, want only a", admitted)
	}
	if refusals["b"] == nil || !strings.Contains(refusals["b"].Error(), "scout") {
		t.Fatalf("refusals[b] = %v, want a collision error naming scout", refusals["b"])
	}
}

// Same collision, but b is a plugins.seed row: admitPlugins must fail boot
// outright, not drop it - #1430's seed-row treatment applies to a manifest
// collision exactly like any other refusal.
func TestAdmitPlugins_CrossPluginCollisionFatalForSeedRow(t *testing.T) {
	reg := pluginreg.NewFSRegistry(t.TempDir())
	a := pluginWithAgent(t, "a", "scout")
	b := pluginWithAgent(t, "b", "scout")
	rowA := pluginreg.Plugin{Name: "a", Source: pluginreg.SourceLocal, Entry: "a"}
	rowB := pluginreg.Plugin{Name: "b", Source: pluginreg.SourceLocal, Entry: "b"}

	_, _, err := admitPlugins(context.Background(), reg, []pluginreg.Plugin{rowA, rowB}, []plugin.Plugin{a, b}, []string{"b"}, nil)
	if err == nil || !strings.Contains(err.Error(), "scout") {
		t.Fatalf("admitPlugins = %v, want a fatal collision error naming scout for the seed row", err)
	}
}

// A plugin whose module is not configured (gated off) never seeds, so its
// listed names must not block a different, actually-enabled plugin from
// claiming the same name.
func TestAdmitPlugins_GatedOffPluginDoesNotClaimNames(t *testing.T) {
	reg := pluginreg.NewFSRegistry(t.TempDir())
	off := pluginWithAgent(t, "off", "scout")
	// "usage" is a linked module (checkModuleLinked passes) with no
	// extensions.usage: configured here, so the gate itself is what's off.
	off.Modules = []plugin.Module{{Name: "usage", Path: "github.com/fagerbergj/quack-extensions/usage"}}
	on := pluginWithAgent(t, "on", "scout")
	rowOff := pluginreg.Plugin{Name: "off", Source: pluginreg.SourceLocal, Entry: "off"}
	rowOn := pluginreg.Plugin{Name: "on", Source: pluginreg.SourceLocal, Entry: "on"}

	admitted, refusals, err := admitPlugins(context.Background(), reg, []pluginreg.Plugin{rowOff, rowOn}, []plugin.Plugin{off, on}, nil, nil)
	if err != nil {
		t.Fatalf("admitPlugins: %v", err)
	}
	if len(refusals) != 0 {
		t.Fatalf("refusals = %v, want none - a gated-off plugin's own listed names are never a collision", refusals)
	}
	names := make(map[string]bool, len(admitted))
	for _, p := range admitted {
		names[p.Name] = true
	}
	if !names["off"] || !names["on"] {
		t.Fatalf("admitted = %+v, want both off (dormant) and on", admitted)
	}
}

// A REST-added row listing an agent that doesn't exist on disk is dropped
// like any other refusal, not a fatal boot error - review finding 4's core
// scenario: a third-party plugin over POST /plugins must never brick boot.
func TestAdmitPlugins_ListedButMissingDropsNonSeedRow(t *testing.T) {
	reg := pluginreg.NewFSRegistry(t.TempDir())
	bad := plugin.Plugin{Name: "bad", Root: t.TempDir(), AgentsDir: t.TempDir(), Agents: []string{"ghost"}}
	row := pluginreg.Plugin{Name: "bad", Source: pluginreg.SourceLocal, Entry: "bad"}

	admitted, refusals, err := admitPlugins(context.Background(), reg, []pluginreg.Plugin{row}, []plugin.Plugin{bad}, nil, nil)
	if err != nil {
		t.Fatalf("admitPlugins: %v", err)
	}
	if len(admitted) != 0 || refusals["bad"] == nil || !strings.Contains(refusals["bad"].Error(), "ghost") {
		t.Fatalf("admitted=%v refusals=%v, want bad dropped with a ghost-naming refusal", admitted, refusals)
	}
}

// mcpJSONBody is a minimal, schema-valid mcp.json declaring one stdio server.
const mcpJSONBody = `{"$schema":"https://agent-plugins.org/schemas/1.1.0/mcp.schema.json","mcpServers":{"foo":{"type":"stdio","command":"echo"}}}`

// TestResolveRegistryPlugins_GithubRowKeepsMCPServers: a fetched plugin's
// mcp.json loads exactly like a local root's, same PLUGIN_ROOT.
func TestResolveRegistryPlugins_GithubRowKeepsMCPServers(t *testing.T) {
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
	if len(plugins[0].MCPServers) != 1 {
		t.Fatalf("github row MCPServers = %v, want the one declared server kept", plugins[0].MCPServers)
	}
	if plugins[0].Root != clone {
		t.Fatalf("github row Root = %q, want the clone dir %q (PLUGIN_ROOT must be the clone root)", plugins[0].Root, clone)
	}
}

// TestResolveRegistryPlugins_GithubRowBrokenMCPWarnsAndDropsOnlyItsTools: a
// broken mcp.json degrades only that plugin's MCP tools, warned.
func TestResolveRegistryPlugins_GithubRowBrokenMCPWarnsAndDropsOnlyItsTools(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	registryRoot := t.TempDir()
	broken := pluginreg.CloneDir(registryRoot, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	writePluginManifest(t, broken, "broken")
	if err := os.WriteFile(filepath.Join(broken, "mcp.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := pluginreg.CloneDir(registryRoot, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	writePluginManifest(t, other, "other")
	if err := os.WriteFile(filepath.Join(other, "mcp.json"), []byte(mcpJSONBody), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := []pluginreg.Plugin{
		{Name: "broken", Source: pluginreg.SourceGitHub, Entry: "github:acme/broken", Owner: "acme", Repo: "broken"},
		{Name: "other", Source: pluginreg.SourceGitHub, Entry: "github:acme/other", Owner: "acme", Repo: "other"},
	}

	plugins, err := resolveRegistryPlugins(registryRoot, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 2 {
		t.Fatalf("resolveRegistryPlugins = %d plugins, want 2 (broken mcp.json must not drop the plugin itself)", len(plugins))
	}
	byName := map[string]plugin.Plugin{}
	for _, p := range plugins {
		byName[p.Name] = p
	}
	if len(byName["broken"].MCPServers) != 0 {
		t.Fatalf("broken row MCPServers = %v, want none", byName["broken"].MCPServers)
	}
	if len(byName["other"].MCPServers) != 1 {
		t.Fatalf("other row MCPServers = %v, want the one declared server kept", byName["other"].MCPServers)
	}
	if !strings.Contains(buf.String(), "mcp.json invalid") || !strings.Contains(buf.String(), "broken") {
		t.Fatalf("log output = %q, want a warning naming broken", buf.String())
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
