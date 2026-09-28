package serve

import (
	"context"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
	"github.com/fagerbergj/quack/internal/workspace"
)

// runAsMCPStub: set, the test binary re-execs itself as a stdio MCP server
// instead of running go test (the mcp go-sdk's own fork-and-exec trick).
const runAsMCPStub = "_QUACK_MCP_STUB_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(runAsMCPStub) != "" {
		runMCPStubServer()
		return
	}
	os.Exit(m.Run())
}

// runMCPStubServer records the expanded ${PLUGIN_ROOT} arg it launched with,
// then serves one tool over stdio for mcpSet.next to enumerate.
func runMCPStubServer() {
	if args := os.Args[1:]; len(args) >= 2 {
		_ = os.WriteFile(args[1], []byte(args[0]), 0o644)
	}
	// Stubborn mode ignores both stdin EOF and SIGTERM, so only SIGKILL stops it.
	stubborn := os.Getenv(stubStubborn) != ""
	if stubborn {
		signal.Ignore(syscall.SIGTERM)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "quack-mcp-stub", Version: "0.0.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "probe", Description: "test probe tool"}, probeHandler)
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})
	if stubborn {
		select {}
	}
}

// stubSlow makes the probe tool block, for a call still in flight at Close.
const (
	stubSlow     = "_QUACK_MCP_STUB_SLOW"
	stubStubborn = "_QUACK_MCP_STUB_STUBBORN"
)

type probeArgs struct{}

func probeHandler(ctx context.Context, _ *mcp.CallToolRequest, _ probeArgs) (*mcp.CallToolResult, any, error) {
	if os.Getenv(stubSlow) != "" {
		<-ctx.Done()
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
}

// installMCPStub puts a copy of the test binary on PATH: exec.Command's
// bare-name lookup uses the real process PATH, not the spawned child's cmd.Env.
func installMCPStub(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatalf("open test binary: %v", err)
	}
	defer src.Close()

	dir := t.TempDir()
	dst, err := os.OpenFile(filepath.Join(dir, "quack-mcpstub"), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatalf("create stub binary: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		t.Fatalf("copy test binary: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fetchedMCPJSONBody round-trips ${PLUGIN_ROOT} to a marker file under
// ${PLUGIN_DATA} and opts its child into the self-exec trigger above.
const fetchedMCPJSONBody = `{"$schema":"https://agent-plugins.org/schemas/1.1.0/mcp.schema.json","mcpServers":{"probe":{"type":"stdio","command":"quack-mcpstub","args":["${PLUGIN_ROOT}","${PLUGIN_DATA}/root-seen"],"env":{"_QUACK_MCP_STUB_SERVER":"1"}}}}`

// TestFetchedPluginMCPServerSpawnsAndEnumeratesTools runs the real
// Fetch -> resolveRegistryPlugins -> mcpSet.next pipeline end to end.
func TestFetchedPluginMCPServerSpawnsAndEnumeratesTools(t *testing.T) {
	installMCPStub(t)

	bare, work := pluginregtest.NewFixtureRepo(t)
	if err := os.WriteFile(filepath.Join(work, "plugin.json"), []byte(`{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"mcptest"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "mcp.json"), []byte(fetchedMCPJSONBody), 0o644); err != nil {
		t.Fatal(err)
	}
	pluginregtest.RunGit(t, work, "add", ".")
	pluginregtest.RunGit(t, work, "commit", "--quiet", "-m", "add mcp.json")
	pluginregtest.RunGit(t, work, "push", "--quiet", "origin", "main")

	prevRemote := pluginreg.RemoteURL
	pluginreg.RemoteURL = func(owner, repo string) string { return bare }
	t.Cleanup(func() { pluginreg.RemoteURL = prevRemote })

	registryRoot := t.TempDir()
	reg := pluginreg.NewFSRegistry(registryRoot)
	entry, err := pluginreg.ParseEntry("github:acme/mcptest")
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := reg.Fetch(context.Background(), pluginreg.FromEntry(entry))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fetched.Error != "" {
		t.Fatalf("Fetch recorded an error: %s", fetched.Error)
	}

	plugins, err := resolveRegistryPlugins(registryRoot, []pluginreg.Plugin{fetched})
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 || len(plugins[0].MCPServers) != 1 {
		t.Fatalf("resolveRegistryPlugins plugins = %+v, want 1 plugin with 1 declared server", plugins)
	}
	if plugins[0].SHA == "" || plugins[0].SHA != fetched.SHA {
		t.Fatalf("plugin SHA = %q, want the fetched row's %q (the MCP reuse key's revision)", plugins[0].SHA, fetched.SHA)
	}
	cloneRoot := pluginreg.CloneDir(registryRoot, "mcptest")
	if plugins[0].Root != cloneRoot {
		t.Fatalf("plugin root = %q, want the clone root %q", plugins[0].Root, cloneRoot)
	}

	dataRoot := t.TempDir()
	caps := workspace.Caps{Sandbox: workspace.SandboxNone}
	set, _ := newMCPSet(dataRoot, caps).next(context.Background(), plugins)
	t.Cleanup(set.Release)
	tools := set.tools()
	if len(tools) != 1 {
		t.Fatalf("next = %d tools, want 1 (the server enumerated and admitted)", len(tools))
	}
	if tools[0].provider != "mcptest" || tools[0].tool.Name() != "probe" {
		t.Fatalf("tool = provider %q name %q, want mcptest/probe", tools[0].provider, tools[0].tool.Name())
	}

	marker, err := os.ReadFile(filepath.Join(dataRoot, "plugins", "mcptest", "root-seen"))
	if err != nil {
		t.Fatalf("read PLUGIN_ROOT marker: %v", err)
	}
	if string(marker) != cloneRoot {
		t.Fatalf("${PLUGIN_ROOT} seen by the server = %q, want the clone root %q", marker, cloneRoot)
	}
}

// TestMCPDeclaredReflectsRosterAfterUpdate: mcpDeclared, unlike the agents'
// baked-in tool wiring, follows the roster live through rebuildSkills.
func TestMCPDeclaredReflectsRosterAfterUpdate(t *testing.T) {
	registryRoot := t.TempDir()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{Root: registryRoot}}}
	skills, err := b.initSkills(context.Background(), jail, nil, nil)
	if err != nil {
		t.Fatalf("initSkills: %v", err)
	}
	if skills.mcpDeclared()["extra"] {
		t.Fatal("mcpDeclared()[extra] = true before the row exists")
	}

	pluginRoot := t.TempDir()
	writePluginManifest(t, pluginRoot, "extra")
	if err := os.WriteFile(filepath.Join(pluginRoot, "mcp.json"), []byte(mcpJSONBody), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := pluginreg.ParseEntry(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	reg := pluginreg.NewFSRegistry(registryRoot)
	if err := reg.Put(context.Background(), pluginreg.FromEntry(entry)); err != nil {
		t.Fatal(err)
	}

	if _, err := skills.rebuildSkills(); err != nil {
		t.Fatalf("rebuildSkills: %v", err)
	}
	if !skills.mcpDeclared()[entry.Name()] {
		t.Fatalf("mcpDeclared() = %v, want %q flagged after rebuild", skills.mcpDeclared(), entry.Name())
	}

	// A refused row starts no server on restart either, so it must not
	// flip the flag true and tell an operator to restart for nothing.
	refusedRoot := t.TempDir()
	writeGhostPluginManifest(t, refusedRoot, "refused")
	if err := os.WriteFile(filepath.Join(refusedRoot, "mcp.json"), []byte(mcpJSONBody), 0o644); err != nil {
		t.Fatal(err)
	}
	refusedEntry, err := pluginreg.ParseEntry(refusedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(context.Background(), pluginreg.FromEntry(refusedEntry)); err != nil {
		t.Fatal(err)
	}
	refusals, err := skills.rebuildSkills()
	if err != nil {
		t.Fatalf("rebuildSkills: %v", err)
	}
	if refusals[refusedEntry.Name()] == nil {
		t.Fatalf("refusals = %v, want %q refused (unlinked module)", refusals, refusedEntry.Name())
	}
	if skills.mcpDeclared()[refusedEntry.Name()] {
		t.Fatalf("mcpDeclared()[%q] = true for a refused row, want false", refusedEntry.Name())
	}
}
