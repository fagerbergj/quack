package serve

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/mcptoolset"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/workspace"
)

func stubMCPPlugin(t *testing.T, name string, modes ...string) plugin.Plugin {
	t.Helper()
	env := map[string]string{runAsMCPStub: "1"}
	for _, m := range modes {
		env[m] = "1"
	}
	return plugin.Plugin{Name: name, Root: t.TempDir(), MCPServers: map[string]plugin.MCPServer{
		"probe": {Command: "quack-mcpstub", Env: env},
	}}
}

func noSandbox() workspace.Caps { return workspace.Caps{Sandbox: workspace.SandboxNone} }

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
	p := stubMCPPlugin(t, "a")
	cmd, err := mcpCommand(p, p.MCPServers["probe"], t.TempDir(), noSandbox())
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

func TestCloseKillsServerGrandchildren(t *testing.T) {
	installMCPStub(t)
	marker := filepath.Join(t.TempDir(), "grandchild.pid")
	stub, err := exec.LookPath("quack-mcpstub")
	if err != nil {
		t.Fatal(err)
	}
	p := plugin.Plugin{Name: "g", Root: t.TempDir(), MCPServers: map[string]plugin.MCPServer{
		"probe": {Command: "sh", Args: []string{"-c", "sleep 777 & echo $! > " + marker + "; " + stub}, Env: map[string]string{runAsMCPStub: "1"}},
	}}
	set, rep := newMCPSet(t.TempDir(), noSandbox()).next(context.Background(), []plugin.Plugin{p})
	if len(rep.Started) != 1 {
		t.Fatalf("report = %+v, want the wrapped server started", rep)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	set.Release()
	// The killed sleep is reparented and reaped by init, so poll for ESRCH.
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived Close", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMCPSetNextReusesUnchangedRestartsChanged(t *testing.T) {
	installMCPStub(t)
	a, b := stubMCPPlugin(t, "a"), stubMCPPlugin(t, "b")
	gen1, rep1 := newMCPSet(t.TempDir(), noSandbox()).next(context.Background(), []plugin.Plugin{a, b})
	t.Cleanup(gen1.Release)
	if len(rep1.Started) != 2 || len(rep1.Failures) != 0 || len(gen1.tools()) != 2 {
		t.Fatalf("gen1 report = %+v, tools = %d; want both started", rep1, len(gen1.tools()))
	}
	a1, b1 := procFor(t, gen1, "a/probe"), procFor(t, gen1, "b/probe")

	b.MCPServers["probe"].Env["X"] = "2"
	gen2, rep2 := gen1.next(context.Background(), []plugin.Plugin{a, b})
	t.Cleanup(gen2.Release)
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
	gen1.Release() // idempotent: a second call must not drop gen2's hold on a
	if !procExited(b1) {
		t.Fatal("old b still running after its only set was released")
	}
	if procExited(a1) {
		t.Fatal("a closed while gen2 still lists it")
	}
	gen2.Release()
	if !procExited(a2) || !procExited(b2) {
		t.Fatal("gen2 servers still running after the last release")
	}
}

func TestMCPSetConcurrentNextSharesProcesses(t *testing.T) {
	installMCPStub(t)
	a := stubMCPPlugin(t, "a")
	gen1, _ := newMCPSet(t.TempDir(), noSandbox()).next(context.Background(), []plugin.Plugin{a})
	proc := procFor(t, gen1, "a/probe")
	gens := make([]*mcpSet, 4)
	var wg sync.WaitGroup
	for i := range gens {
		wg.Go(func() {
			var rep mcpReport
			gens[i], rep = gen1.next(context.Background(), []plugin.Plugin{a})
			if !slices.Equal(rep.Reused, []string{"a/probe"}) {
				t.Errorf("concurrent next report = %+v, want a reused", rep)
			}
		})
	}
	wg.Wait()
	gen1.Release()
	for _, g := range gens[1:] {
		g.Release()
	}
	if procExited(proc) {
		t.Fatal("a closed while one generation still lists it")
	}
	gens[0].Release()
	if !procExited(proc) {
		t.Fatal("a still running after every generation was released")
	}
}

func TestMCPSetNextReportsSpawnFailurePerServer(t *testing.T) {
	installMCPStub(t)
	bad := plugin.Plugin{Name: "bad", Root: t.TempDir(), MCPServers: map[string]plugin.MCPServer{"probe": {Command: "quack-no-such-binary"}}}
	set, rep := newMCPSet(t.TempDir(), noSandbox()).next(context.Background(), []plugin.Plugin{bad, stubMCPPlugin(t, "good")})
	defer set.Release()
	if len(rep.Failures) != 1 || rep.Failures[0].Plugin != "bad" || rep.Failures[0].Err == nil {
		t.Fatalf("failures = %+v, want only bad/probe", rep.Failures)
	}
	if !slices.Equal(rep.Started, []string{"good/probe"}) || len(set.tools()) != 1 {
		t.Fatalf("started = %v tools = %d, want good/probe with its 1 tool", rep.Started, len(set.tools()))
	}
}

func TestMCPSetReleaseClosesStubbornServersInParallel(t *testing.T) {
	installMCPStub(t)
	old := mcpTerminateGrace
	mcpTerminateGrace = time.Second
	t.Cleanup(func() { mcpTerminateGrace = old })
	set, _ := newMCPSet(t.TempDir(), noSandbox()).next(context.Background(), []plugin.Plugin{
		stubMCPPlugin(t, "a", stubStubborn), stubMCPPlugin(t, "b", stubStubborn),
	})
	start := time.Now()
	set.Release()
	// Each stubborn server costs two graces (EOF, then SIGTERM) before SIGKILL; serial would be 4s.
	if e := time.Since(start); e > 3500*time.Millisecond {
		t.Fatalf("Release took %v, want about one server's 2s escalation", e)
	}
	if !procExited(set.procs[0]) || !procExited(set.procs[1]) {
		t.Fatal("stubborn servers still running after Release")
	}
}

// callCtx is the agent.Context an MCP tool's Run needs; only the context and confirmation are read.
type callCtx struct {
	agent.Context
	ctx context.Context
}

func (c callCtx) Deadline() (time.Time, bool)                          { return c.ctx.Deadline() }
func (c callCtx) Done() <-chan struct{}                                { return c.ctx.Done() }
func (c callCtx) Err() error                                           { return c.ctx.Err() }
func (c callCtx) Value(k any) any                                      { return c.ctx.Value(k) }
func (c callCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

func TestMCPSetReleaseFailsInFlightCall(t *testing.T) {
	installMCPStub(t)
	set, _ := newMCPSet(t.TempDir(), noSandbox()).next(context.Background(), []plugin.Plugin{stubMCPPlugin(t, "s", stubSlow)})
	run, ok := set.tools()[0].tool.(interface {
		Run(agent.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("tool %T has no Run", set.tools()[0].tool)
	}
	done := make(chan error, 1)
	go func() {
		_, err := run.Run(callCtx{ctx: context.Background()}, map[string]any{})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	set.Release()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("in-flight call succeeded after its server was released")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("in-flight call hung after Release")
	}
}

func TestBootPluginMCPClosesServersAtShutdown(t *testing.T) {
	installMCPStub(t)
	b := &boot{cfg: &config.Config{Workspace: config.WorkspaceConfig{Root: t.TempDir()}}}
	set := b.bootPluginMCP(context.Background(), []plugin.Plugin{stubMCPPlugin(t, "a")}, noSandbox())
	p := procFor(t, set, "a/probe")
	if procExited(p) {
		t.Fatal("boot server exited before shutdown")
	}
	b.runCleanups()
	if !procExited(p) {
		t.Fatal("boot server still running after shutdown cleanups")
	}
}
