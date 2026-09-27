package serve

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/workspace"
)

func stubMCPPlugin(t *testing.T, name string) plugin.Plugin {
	t.Helper()
	return plugin.Plugin{Name: name, Root: t.TempDir(), MCPServers: map[string]plugin.MCPServer{
		"probe": {Command: "quack-mcpstub", Env: map[string]string{runAsMCPStub: "1"}},
	}}
}

func procPID(p *mcpProc) int { return p.transport.cmd.Command.Process.Pid }

// procExited reads ProcessState, which the SDK's Close sets via cmd.Wait before returning.
func procExited(p *mcpProc) bool { return p.transport.cmd.Command.ProcessState != nil }

func procFor(t *testing.T, s *mcpSet, name string) *mcpProc {
	t.Helper()
	for _, p := range s.procs {
		if p.key.String() == name {
			return p
		}
	}
	t.Fatalf("set has no server %q", name)
	return nil
}

func TestClosableTransportStopsChildAndRefusesReconnect(t *testing.T) {
	installMCPStub(t)
	cmd, err := mcpCommand(stubMCPPlugin(t, "a"), stubMCPPlugin(t, "a").MCPServers["probe"], t.TempDir(), workspace.Caps{Sandbox: workspace.SandboxNone})
	if err != nil {
		t.Fatal(err)
	}
	tr := &closableTransport{cmd: &mcp.CommandTransport{Command: cmd}}
	ts, err := mcptoolset.New(mcptoolset.Config{Transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	ctx := bootToolCtx{context.Background()}
	if _, err := ts.Tools(ctx); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("child still running after Close")
	}
	// ADK's connectionRefresher reconnects on a closed connection; the retry must hit our refusal.
	if _, err := ts.Tools(ctx); err == nil || !strings.Contains(err.Error(), errTransportClosed.Error()) {
		t.Fatalf("Tools after Close err = %v, want the reconnect refused with %q", err, errTransportClosed)
	}
	if _, err := tr.Connect(context.Background()); !errors.Is(err, errTransportClosed) {
		t.Fatalf("Connect after Close = %v, want errTransportClosed", err)
	}
}

func TestMCPSetNextReusesUnchangedRestartsChanged(t *testing.T) {
	installMCPStub(t)
	a, b := stubMCPPlugin(t, "a"), stubMCPPlugin(t, "b")
	gen0 := newMCPSet(t.TempDir(), workspace.Caps{Sandbox: workspace.SandboxNone})
	gen1, rep1 := gen0.next(context.Background(), []plugin.Plugin{a, b})
	if len(rep1.Started) != 2 || len(rep1.Failures) != 0 || len(gen1.tools()) != 2 {
		t.Fatalf("gen1 report = %+v, tools = %d; want both started", rep1, len(gen1.tools()))
	}
	a1, b1 := procFor(t, gen1, "a/probe"), procFor(t, gen1, "b/probe")

	b.MCPServers = map[string]plugin.MCPServer{"probe": {Command: "quack-mcpstub", Env: map[string]string{runAsMCPStub: "1", "X": "2"}}}
	gen2, rep2 := gen1.next(context.Background(), []plugin.Plugin{a, b})
	if !slices.Equal(rep2.Reused, []string{"a/probe"}) || !slices.Equal(rep2.Started, []string{"b/probe"}) || !slices.Equal(rep2.Stopped, []string{"b/probe"}) {
		t.Fatalf("gen2 report = %+v, want a reused, b restarted", rep2)
	}
	a2, b2 := procFor(t, gen2, "a/probe"), procFor(t, gen2, "b/probe")
	if procPID(a2) != procPID(a1) {
		t.Fatalf("unchanged server pid %d -> %d, want reused", procPID(a1), procPID(a2))
	}
	if procPID(b2) == procPID(b1) {
		t.Fatal("changed server kept its pid, want a restart")
	}

	gen1.Release()
	if !procExited(b1) {
		t.Fatal("old b still running after its only set was released")
	}
	if procExited(a1) {
		t.Fatal("a closed while gen2 still lists it")
	}
	gen2.Acquire()
	gen2.Release()
	if procExited(a2) || procExited(b2) {
		t.Fatal("gen2 servers closed while a hold remains")
	}
	gen2.Release()
	if !procExited(a2) || !procExited(b2) {
		t.Fatal("gen2 servers still running after the last release")
	}
}

func TestMCPSetNextReportsSpawnFailurePerServer(t *testing.T) {
	installMCPStub(t)
	bad := plugin.Plugin{Name: "bad", Root: t.TempDir(), MCPServers: map[string]plugin.MCPServer{"probe": {Command: "quack-no-such-binary"}}}
	set, rep := newMCPSet(t.TempDir(), workspace.Caps{Sandbox: workspace.SandboxNone}).next(context.Background(), []plugin.Plugin{bad, stubMCPPlugin(t, "good")})
	defer set.Release()
	if len(rep.Failures) != 1 || rep.Failures[0].Plugin != "bad" || rep.Failures[0].Err == nil {
		t.Fatalf("failures = %+v, want only bad/probe", rep.Failures)
	}
	if !slices.Equal(rep.Started, []string{"good/probe"}) || len(set.tools()) != 1 {
		t.Fatalf("started = %v tools = %d, want good/probe with its 1 tool", rep.Started, len(set.tools()))
	}
}

func TestBootPluginMCPClosesServersAtShutdown(t *testing.T) {
	installMCPStub(t)
	b := &boot{cfg: &config.Config{Workspace: config.WorkspaceConfig{Root: t.TempDir()}}}
	set := b.bootPluginMCP(context.Background(), []plugin.Plugin{stubMCPPlugin(t, "a")}, workspace.Caps{Sandbox: workspace.SandboxNone})
	p := procFor(t, set, "a/probe")
	if procExited(p) {
		t.Fatal("boot server exited before shutdown")
	}
	b.runCleanups()
	if !procExited(p) {
		t.Fatal("boot server still running after shutdown cleanups")
	}
}
