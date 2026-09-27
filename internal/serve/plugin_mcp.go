package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/workspace"
)

// errTransportClosed is what ADK's reconnect-on-EOF gets after Close, so a
// retired server's tool call fails instead of respawning the process.
var errTransportClosed = errors.New("plugin MCP server stopped")

// closableTransport records the connection mcptoolset opens, since the toolset
// itself has no Close and would otherwise leak the child.
type closableTransport struct {
	cmd *mcp.CommandTransport

	mu     sync.Mutex
	conn   mcp.Connection
	closed bool
}

func (t *closableTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errTransportClosed
	}
	conn, err := t.cmd.Connect(ctx)
	if err != nil {
		return nil, err
	}
	t.conn = conn
	return conn, nil
}

// Close blocks until the child exits: the SDK closes stdin, then SIGTERMs and
// SIGKILLs after its TerminateDuration grace each.
func (t *closableTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

// mcpKey identifies one server process; a change in any field is a restart.
// spec hashes the wrapped argv, env, and cwd, so a sandbox change counts too.
type mcpKey struct{ plugin, server, rev, spec string }

func (k mcpKey) String() string { return k.plugin + "/" + k.server }

func mcpKeyFor(p plugin.Plugin, server string, cmd *exec.Cmd) mcpKey {
	spec := sha256.Sum256(fmt.Appendf(nil, "%q\n%q\n%q", cmd.Args, slices.Sorted(slices.Values(cmd.Env)), cmd.Dir))
	return mcpKey{plugin: p.Name, server: server, rev: p.Root + "@" + p.SHA, spec: hex.EncodeToString(spec[:])}
}

// mcpProc is one live server, shared by every mcpSet that lists it.
type mcpProc struct {
	key       mcpKey
	transport *closableTransport
	tools     []tool.Tool
	refs      atomic.Int32 // live sets listing this proc
}

func (p *mcpProc) release() {
	if p.refs.Add(-1) != 0 {
		return
	}
	if err := p.transport.Close(); err != nil {
		slog.Debug("plugin MCP server exited uncleanly", "component", "plugin", "server", p.key, "err", err)
	}
}

// mcpSet is one generation of plugin MCP servers. next's caller owns one hold;
// Acquire adds more, and each server closes once no live set lists it.
type mcpSet struct {
	dataRoot string
	caps     workspace.Caps
	procs    []*mcpProc
	refs     atomic.Int32
}

// mcpReport names servers as "plugin/server". A changed server is both
// Stopped (its old process) and Started.
type mcpReport struct {
	Started, Reused, Stopped []string
	Failures                 []mcpFailure
}

type mcpFailure struct {
	Plugin, Server string
	Err            error
}

// newMCPSet is the empty generation boot advances from.
func newMCPSet(dataRoot string, caps workspace.Caps) *mcpSet {
	s := &mcpSet{dataRoot: dataRoot, caps: caps}
	s.refs.Store(1)
	return s
}

// next builds admitted's generation: an unchanged server keeps its process and
// tools, a new or changed one is spawned and enumerated. s must still be held.
func (s *mcpSet) next(ctx context.Context, admitted []plugin.Plugin) (*mcpSet, mcpReport) {
	live := make(map[mcpKey]*mcpProc, len(s.procs))
	for _, p := range s.procs {
		live[p.key] = p
	}
	out := newMCPSet(s.dataRoot, s.caps)
	var rep mcpReport
	for _, p := range admitted {
		for _, name := range slices.Sorted(maps.Keys(p.MCPServers)) {
			proc, reused, err := out.acquireOrSpawn(ctx, p, name, live)
			if err != nil {
				slog.Warn("plugin MCP server unavailable; its tools are not loaded", "component", "plugin", "plugin", p.Name, "server", name, "err", err)
				rep.Failures = append(rep.Failures, mcpFailure{Plugin: p.Name, Server: name, Err: err})
				continue
			}
			if reused {
				rep.Reused = append(rep.Reused, proc.key.String())
			} else {
				slog.Info("plugin MCP server loaded", "component", "plugin", "plugin", p.Name, "server", name, "tools", len(proc.tools))
				rep.Started = append(rep.Started, proc.key.String())
			}
			delete(live, proc.key)
			out.procs = append(out.procs, proc)
		}
	}
	for _, p := range s.procs {
		if _, gone := live[p.key]; gone {
			rep.Stopped = append(rep.Stopped, p.key.String())
		}
	}
	return out, rep
}

func (s *mcpSet) acquireOrSpawn(ctx context.Context, p plugin.Plugin, name string, live map[mcpKey]*mcpProc) (*mcpProc, bool, error) {
	cmd, err := mcpCommand(p, p.MCPServers[name], s.dataRoot, s.caps)
	if err != nil {
		return nil, false, err
	}
	key := mcpKeyFor(p, name, cmd)
	if proc, ok := live[key]; ok {
		proc.refs.Add(1)
		return proc, true, nil
	}
	proc, err := spawnMCP(ctx, key, cmd)
	return proc, false, err
}

// spawnMCP starts one server and enumerates its tools: quack binds tools per
// node BY NAME (extToolsByName), so they must be known before any invocation.
func spawnMCP(ctx context.Context, key mcpKey, cmd *exec.Cmd) (*mcpProc, error) {
	tr := &closableTransport{cmd: &mcp.CommandTransport{Command: cmd}}
	ts, err := mcptoolset.New(mcptoolset.Config{
		Client:    mcp.NewClient(&mcp.Implementation{Name: "quack", Version: "1"}, nil),
		Transport: tr,
	})
	if err != nil {
		return nil, err
	}
	// §7.2.2 rule 5: a server that hangs on spawn, handshake, or listing must
	// cost only its own tools. The caller's context has no deadline of its own.
	enumCtx, cancel := context.WithTimeout(ctx, mcpEnumerateTimeout)
	defer cancel()
	tools, err := ts.Tools(bootToolCtx{enumCtx})
	if err != nil {
		_ = tr.Close()
		return nil, err
	}
	proc := &mcpProc{key: key, transport: tr, tools: tools}
	proc.refs.Store(1)
	return proc, nil
}

// tools is the set's servers' tools for the agents' shared tool set.
func (s *mcpSet) tools() []extTool {
	var out []extTool
	for _, p := range s.procs {
		for _, t := range p.tools {
			out = append(out, extTool{provider: p.key.plugin, tool: t})
		}
	}
	return out
}

// Acquire adds a hold on s; pair each with one Release.
func (s *mcpSet) Acquire() { s.refs.Add(1) }

// Release drops one hold; the last one drops s's servers. It blocks while a
// server closes, so a caller on a run's exit path should call it in a goroutine.
func (s *mcpSet) Release() {
	if s.refs.Add(-1) != 0 {
		return
	}
	for _, p := range s.procs {
		p.release()
	}
}

// bootPluginMCP starts boot's generation of plugin MCP servers and releases it
// at shutdown.
func (b *boot) bootPluginMCP(ctx context.Context, plugins []plugin.Plugin, caps workspace.Caps) *mcpSet {
	set, _ := newMCPSet(b.cfg.Workspace.Root, caps).next(ctx, plugins)
	b.cleanups = append(b.cleanups, set.Release)
	return set
}
