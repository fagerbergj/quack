package serve

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"
	"google.golang.org/genai"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/workspace"
)

// embeddedQuackPlugin is quack's go:embedded skill bundle's registry row
// (#1427 P1): in-memory only, never Put to disk - it has no clone.
func embeddedQuackPlugin() pluginreg.Plugin {
	return pluginreg.Plugin{Name: "quack", Source: pluginreg.SourceEmbedded}
}

// seedRegistry inserts each seed entry into reg if its name is absent - Put
// only when List lacks it, so the UI/REST (P2) own the list after boot. A
// stale on-disk row (no longer in seed) is left alone but named in a warning (#1427 F6).
func seedRegistry(ctx context.Context, reg *pluginreg.FSRegistry, seed []string) error {
	existing, err := reg.List(ctx)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(existing))
	stale := make(map[string]bool, len(existing))
	for _, p := range existing {
		have[p.Name] = true
		stale[p.Name] = true
	}
	seenInSeed := make(map[string]string, len(seed)) // name -> raw entry
	for _, s := range seed {
		e, err := pluginreg.ParseEntry(s) // config.validatePlugins already checked every entry parses
		if err != nil {
			return err
		}
		name := e.Name()
		// A name repeated under a different entry is a config error, not a
		// Put collision (#1427 S3) - the vendored copy and a github: copy of
		// the SAME repo are expected to coexist under DIFFERENT names.
		if prev, dup := seenInSeed[name]; dup && prev != e.Raw {
			return fmt.Errorf("config: plugins.seed: name %q is listed twice, as %q and %q", name, prev, e.Raw)
		}
		seenInSeed[name] = e.Raw
		delete(stale, name)
		if have[name] {
			continue
		}
		if err := reg.Put(ctx, pluginreg.FromEntry(e)); err != nil {
			return err
		}
		have[name] = true
	}
	if len(stale) > 0 {
		names := make([]string, 0, len(stale))
		for n := range stale {
			names = append(names, n)
		}
		slog.Warn("plugin registry rows are absent from plugins.seed; they still load", "component", "startup", "names", names)
	}
	return nil
}

// fetchRegistryPlugins fetches every non-local row against its pinned/tracked
// ref (P0's gitTimeout per call already bounds each one). A failure is logged
// and left on the row - Fetch persists it - so boot continues on the last good clone.
func fetchRegistryPlugins(ctx context.Context, reg *pluginreg.FSRegistry, rows []pluginreg.Plugin) []pluginreg.Plugin {
	out := make([]pluginreg.Plugin, 0, len(rows))
	for _, p := range rows {
		if p.Source != pluginreg.SourceGitHub {
			out = append(out, p)
			continue
		}
		fetched, err := reg.Fetch(ctx, p)
		if err != nil {
			slog.Warn("plugin fetch failed at boot; serving the last good clone", "component", "startup", "plugin", p.Name, "err", err)
		}
		out = append(out, fetched)
	}
	return out
}

// bootPluginRegistry seeds and fetches the plugin registry, returning every
// row (github fetched, local as-is), ordered per plugins.seed (#1427 F2),
// plus the in-memory embedded quack row appended last.
func (b *boot) bootPluginRegistry(ctx context.Context) (*pluginreg.FSRegistry, []pluginreg.Plugin, error) {
	reg := pluginreg.NewFSRegistry(b.cfg.Plugins.Root)
	if err := seedRegistry(ctx, reg, b.cfg.Plugins.Seed); err != nil {
		return nil, nil, fmt.Errorf("plugin registry seed: %w", err)
	}
	rows, err := reg.List(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin registry list: %w", err)
	}
	rows = fetchRegistryPlugins(ctx, reg, rows)
	rows = orderBySeed(b.cfg.Plugins.Seed, rows)
	rows = append(rows, embeddedQuackPlugin())
	return reg, rows, nil
}

// orderBySeed reorders rows to match plugins.seed's listed order (bare-name
// resolution is "first in merge order wins", #1427 F2) - a row not in seed
// (added via the UI/REST, P2) sorts after, in List's name order.
func orderBySeed(seed []string, rows []pluginreg.Plugin) []pluginreg.Plugin {
	byName := make(map[string]pluginreg.Plugin, len(rows))
	for _, p := range rows {
		byName[p.Name] = p
	}
	out := make([]pluginreg.Plugin, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, s := range seed {
		e, err := pluginreg.ParseEntry(s) // config.validatePlugins already checked every entry parses
		if err != nil {
			continue
		}
		if p, ok := byName[e.Name()]; ok && !seen[e.Name()] {
			out = append(out, p)
			seen[e.Name()] = true
		}
	}
	for _, p := range rows {
		if !seen[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

// registryPluginRoots is every non-embedded row's resolved Root() - what
// plugin.Resolve reads (plugin.json/skills/mcp.json), replacing
// cfg.PluginRoots() as of #1427 P1.
func registryPluginRoots(registryRoot string, rows []pluginreg.Plugin) []string {
	var out []string
	for _, p := range rows {
		if p.Source == pluginreg.SourceEmbedded {
			continue
		}
		out = append(out, p.Root(registryRoot))
	}
	return out
}

// resolveRegistryPlugins resolves each row's root, then stamps the REGISTRY
// ROW NAME onto each result (#1427 S3, never plugin.json's) - matched by
// absolute root path, since plugin.Resolve silently drops a failed root.
func resolveRegistryPlugins(registryRoot string, rows []pluginreg.Plugin) ([]plugin.Plugin, error) {
	nameByAbsRoot := make(map[string]string, len(rows))
	for _, p := range rows {
		if p.Source == pluginreg.SourceEmbedded {
			continue
		}
		if abs, err := filepath.Abs(p.Root(registryRoot)); err == nil {
			nameByAbsRoot[abs] = p.Name
		}
	}
	plugins, err := plugin.Resolve(registryPluginRoots(registryRoot, rows))
	if err != nil {
		return nil, err
	}
	for i := range plugins {
		if name, ok := nameByAbsRoot[plugins[i].Root]; ok {
			plugins[i].Name = name
		}
	}
	return plugins, nil
}

// mcpEnumerateTimeout bounds the one blocking connect+ListTools a declared
// server gets at boot. Per-call contexts govern everything after.
var mcpEnumerateTimeout = 20 * time.Second // var so tests can shrink it

// checkPluginModules matches every module a plugin declares under quack's
// namespace against the modules actually linked into this binary. Go has no
// safe dynamic loading, so the manifest is documentation the compiler is
// checked against: a declared-but-unlinked module is a boot error naming the
// import to add, never a silently missing capability.
func checkPluginModules(plugins []plugin.Plugin) error {
	linked := extsdk.Registered()
	for _, p := range plugins {
		for _, m := range p.Modules {
			if _, ok := linked[m.Name]; !ok {
				return fmt.Errorf("plugin %q declares module %q (%s), which is not linked into this binary; add its blank import to internal/serve/extensions_registry.go", p.Name, m.Name, m.Path)
			}
		}
	}
	return nil
}

// checkPluginConfig enforces the namespace block's config: "required". A module that is not
// configured at all stays dormant, exactly as before; one whose extensions: block is present but
// empty fails the boot here with the plugin named, rather than deeper inside its own factory.
func checkPluginConfig(plugins []plugin.Plugin, modules map[string]yaml.Node) error {
	for _, p := range plugins {
		if !p.ConfigRequired {
			continue
		}
		for _, m := range p.Modules {
			node, ok := modules[m.Name]
			if !ok {
				continue
			}
			if node.IsZero() || len(node.Content) == 0 {
				return fmt.Errorf("config: extensions.%s is empty, but plugin %q declares config: \"required\"", m.Name, p.Name)
			}
		}
	}
	return nil
}

// pluginSpawnCaps is the sandbox bound an MCP server subprocess runs under -
// the same mode and exec path every other quack child gets, with the jail's
// home so TMPDIR lands inside a granted directory.
func pluginSpawnCaps(cfg *config.Config, jail *workspace.Jail) (workspace.Caps, error) {
	sandbox, err := workspace.ResolveSandbox(workspace.SandboxMode(cfg.Workspace.Sandbox))
	if err != nil {
		return workspace.Caps{}, err
	}
	home, err := jail.HomeDir(localUserID)
	if err != nil {
		return workspace.Caps{}, err
	}
	return workspace.Caps{Sandbox: sandbox, ExtraPath: cfg.Workspace.ExecPath, HomeDir: home}, nil
}

// pluginMCPTools starts every stdio MCP server declared in a plugin's mcp.json and returns its
// tools for the agents' shared tool set, out of process through the SAME sandbox seam ACP workers
// use (workspace.WrapArgv); a failed server costs only its own tools (spec §7.2.2 rule 5) - never the boot.
func pluginMCPTools(ctx context.Context, plugins []plugin.Plugin, dataRoot string, caps workspace.Caps) []extTool {
	var out []extTool
	for _, p := range plugins {
		names := make([]string, 0, len(p.MCPServers))
		for name := range p.MCPServers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			tools, err := mcpServerTools(ctx, p, p.MCPServers[name], dataRoot, caps)
			if err != nil {
				slog.Warn("plugin MCP server unavailable; its tools are not loaded",
					"component", "startup", "plugin", p.Name, "server", name, "err", err)
				continue
			}
			slog.Info("plugin MCP server loaded", "component", "startup", "plugin", p.Name, "server", name, "tools", len(tools))
			for _, t := range tools {
				out = append(out, extTool{provider: p.Name, tool: t})
			}
		}
	}
	return out
}

func mcpServerTools(ctx context.Context, p plugin.Plugin, s plugin.MCPServer, dataRoot string, caps workspace.Caps) ([]tool.Tool, error) {
	// §9.1: PLUGIN_DATA is client-chosen, must exist before launch, and must
	// survive plugin updates - so it lives outside the vendored tree.
	data := filepath.Join(dataRoot, "plugins", p.Name)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return nil, fmt.Errorf("plugin data dir: %w", err)
	}
	argv, env, cwd, err := s.Launch(p.Root, data)
	if err != nil {
		return nil, err
	}

	// WorkRoot pins the writable grant to PLUGIN_DATA. Without it
	// landlockGrants falls back to cwd, and since landlock UNIONS per-path
	// rules a root appearing in both lists would stay writable - a server
	// able to rewrite the skills/ that reach agent prompts. cwd is inside
	// data (see MCPServer.Launch) so the root is never re-added as an
	// outside-cwd grant either.
	caps.WorkRoot = data
	wrapped := workspace.WrapArgv(cwd, argv, caps, []string{p.Root}, []string{data})
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	cmd.Dir = cwd
	cmd.Env = append([]string{
		"PATH=" + workspace.ChildPath(caps),
		"HOME=" + data,
		"TMPDIR=" + workspace.SandboxTmpDir(caps),
		"NO_COLOR=1",
	}, env...)

	ts, err := mcptoolset.New(mcptoolset.Config{
		Client:    mcp.NewClient(&mcp.Implementation{Name: "quack", Version: "1"}, nil),
		Transport: &mcp.CommandTransport{Command: cmd},
	})
	if err != nil {
		return nil, err
	}
	// §7.2.2 rule 5: a server that hangs on spawn, handshake, or listing must
	// cost only its own tools. The startup context has no deadline of its own.
	enumCtx, cancel := context.WithTimeout(ctx, mcpEnumerateTimeout)
	defer cancel()
	return ts.Tools(bootToolCtx{enumCtx})
}

// bootToolCtx satisfies agent.ReadonlyContext for the one call that needs it: quack selects tools
// per node BY NAME (extToolsByName), so an MCP server's tools have to be enumerated once at boot,
// before any invocation exists. Every accessor is zero-valued; the real agent.Context arrives at call time.
type bootToolCtx struct{ context.Context }

func (bootToolCtx) UserContent() *genai.Content          { return nil }
func (bootToolCtx) InvocationID() string                 { return "" }
func (bootToolCtx) AgentName() string                    { return "" }
func (bootToolCtx) ReadonlyState() session.ReadonlyState { return nil }
func (bootToolCtx) UserID() string                       { return "" }
func (bootToolCtx) AppName() string                      { return "" }
func (bootToolCtx) SessionID() string                    { return "" }
func (bootToolCtx) Branch() string                       { return "" }
