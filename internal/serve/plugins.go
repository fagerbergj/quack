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
	"gorm.io/gorm"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/workspace"
)

// seedRegistry inserts each seed entry into reg if its name is absent - Put
// only when List lacks it, so the UI/REST (P2) own the list after boot. A
// stale or identity-colliding on-disk row is warned about, not silent.
func seedRegistry(ctx context.Context, reg pluginreg.FetchRegistry, seed []string) error {
	existing, err := reg.List(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]pluginreg.Plugin, len(existing))
	stale := make(map[string]bool, len(existing))
	for _, p := range existing {
		byName[p.Name] = p
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
		row := pluginreg.FromEntry(e)
		if existingRow, ok := byName[name]; ok {
			// Put would only ever error here (SameIdentity is exactly its
			// own collision check) - warn directly instead of re-deriving it.
			if !pluginreg.SameIdentity(existingRow, row) {
				slog.Warn("plugin seed entry collides with a different plugin already registered under this name; keeping the on-disk row",
					"component", "startup", "name", name, "entry", e.Raw)
			}
			continue
		}
		if err := reg.Put(ctx, row); err != nil {
			return err
		}
		byName[name] = row
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
func fetchRegistryPlugins(ctx context.Context, reg pluginreg.FetchRegistry, rows []pluginreg.Plugin) []pluginreg.Plugin {
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

// openPluginRegistry picks the backend plugins.store names ("" filesystem,
// else a sqlite/postgres stores[] entry), reusing st's connection when it's
// the SAME store session.store uses instead of opening a second pool.
func (b *boot) openPluginRegistry(st *store.Store) (pluginreg.FetchRegistry, error) {
	cfg := b.cfg
	if cfg.Plugins.Store == "" {
		return pluginreg.NewFSRegistry(cfg.Plugins.Root), nil
	}
	sc, ok := cfg.Store(cfg.Plugins.Store)
	if !ok {
		return nil, fmt.Errorf("plugins.store %q not found in stores registry", cfg.Plugins.Store)
	}
	var db *gorm.DB
	if st != nil && cfg.Plugins.Store == cfg.Session.Store {
		db = st.DB()
	} else {
		var err error
		db, err = pluginreg.OpenDB(sc.Kind, sc.URL)
		if err != nil {
			return nil, fmt.Errorf("plugins.store %q: %w", cfg.Plugins.Store, err)
		}
	}
	return pluginreg.NewDBRegistry(db, cfg.Plugins.Root)
}

// bootPluginRegistry seeds and fetches the plugin registry.
func (b *boot) bootPluginRegistry(ctx context.Context, st *store.Store) (pluginreg.FetchRegistry, []pluginreg.Plugin, error) {
	reg, err := b.openPluginRegistry(st)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin registry open: %w", err)
	}
	if err := seedRegistry(ctx, reg, b.cfg.Plugins.Seed); err != nil {
		return nil, nil, fmt.Errorf("plugin registry seed: %w", err)
	}
	rows, err := reg.List(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin registry list: %w", err)
	}
	rows = fetchRegistryPlugins(ctx, reg, rows)
	rows = pluginreg.OrderBySeed(b.cfg.Plugins.Seed, rows)
	rows = append(rows, pluginreg.EmbeddedQuackPlugin())
	return reg, rows, nil
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

// mcpDeclaredNames names every plugin whose mcp.json declared a server -
// REST's live note; the agents' actual tool wiring stays boot-fixed.
func mcpDeclaredNames(plugins []plugin.Plugin) *map[string]bool {
	m := make(map[string]bool, len(plugins))
	for _, p := range plugins {
		if len(p.MCPServers) > 0 {
			m[p.Name] = true
		}
	}
	return &m
}

// resolveRegistryPlugins resolves each row's root and stamps the registry row
// name onto each result, never plugin.json's own name.
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

// checkModuleLinked matches p's declared modules under quack's namespace
// against the modules actually linked into this binary. Go has no safe
// dynamic loading, so a declared-but-unlinked module names the import to add.
func checkModuleLinked(p plugin.Plugin) error {
	linked := extsdk.Registered()
	for _, m := range p.Modules {
		if _, ok := linked[m.Name]; !ok {
			return fmt.Errorf("plugin %q declares module %q (%s), which is not linked into this binary; add its blank import to internal/serve/extensions_registry.go", p.Name, m.Name, m.Path)
		}
	}
	return nil
}

// checkPluginModules runs checkModuleLinked over every plugin, fatal on the
// first failure - boot's original whole-list gate.
func checkPluginModules(plugins []plugin.Plugin) error {
	for _, p := range plugins {
		if err := checkModuleLinked(p); err != nil {
			return err
		}
	}
	return nil
}

// checkConfigRequired enforces the namespace block's config: "required" for
// p. A module not configured at all stays dormant; one whose extensions:
// block is present but empty fails, named, rather than deeper in its factory.
func checkConfigRequired(p plugin.Plugin, modules map[string]yaml.Node) error {
	if !p.ConfigRequired {
		return nil
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
	return nil
}

// checkPluginConfig runs checkConfigRequired over every plugin, fatal on the
// first failure - boot's original whole-list gate.
func checkPluginConfig(plugins []plugin.Plugin, modules map[string]yaml.Node) error {
	for _, p := range plugins {
		if err := checkConfigRequired(p, modules); err != nil {
			return err
		}
	}
	return nil
}

// checkPlugin runs both refusal checks against ONE plugin - the shared unit
// initSkills' boot admission and rebuildSkills' whole-list gate both check
// against (#1430 severe).
func checkPlugin(p plugin.Plugin, modules map[string]yaml.Node) error {
	if err := checkModuleLinked(p); err != nil {
		return err
	}
	return checkConfigRequired(p, modules)
}

// manifestClaims tracks, across one admission pass, which plugin already
// claimed each manifest-listed agent/workflow name - the manifest-list era's
// replacement for the old silent config merge.
type manifestClaims struct {
	agents    map[string]string
	workflows map[string]string
}

func newManifestClaims() manifestClaims {
	return manifestClaims{agents: map[string]string{}, workflows: map[string]string{}}
}

// claim refuses (NamespaceError-class) when another admitted plugin already
// claimed one of p's listed names - p.Name is already the registry row name here (resolveRegistryPlugins stamps it).
func (c manifestClaims) claim(p plugin.Plugin) error {
	if err := claimNames(p, "agents", p.Agents, c.agents); err != nil {
		return err
	}
	return claimNames(p, "workflows", p.Workflows, c.workflows)
}

func claimNames(p plugin.Plugin, kind string, names []string, seen map[string]string) error {
	// A name listed twice within this plugin is plugin.CheckManifestLists'
	// to refuse, before and regardless of the module gate.
	for _, name := range names {
		if owner, ok := seen[name]; ok {
			return &plugin.NamespaceError{Root: p.Root, Err: fmt.Errorf("%s entry %q: plugin %q and plugin %q both list it", kind, name, owner, p.Name)}
		}
		seen[name] = p.Name
	}
	return nil
}

// admitOnePlugin adds the manifest-list checks to checkPlugin's refusals: a
// listed-but-missing entry always refuses; a name collision only when p's module gate is enabled.
func admitOnePlugin(p plugin.Plugin, modules map[string]yaml.Node, claims manifestClaims) error {
	if err := checkPlugin(p, modules); err != nil {
		return err
	}
	if err := plugin.CheckManifestLists(p); err != nil {
		return err
	}
	gated, err := pluginModuleGateEnabled(modules, p)
	if err != nil {
		return err
	}
	if !gated {
		return nil
	}
	return claims.claim(p)
}

// seedPluginNames is the set of registry-row names plugins.seed configures -
// admitPlugins' fatal-vs-drop boundary.
func seedPluginNames(seed []string) map[string]bool {
	names := make(map[string]bool, len(seed))
	for _, s := range seed {
		if e, err := pluginreg.ParseEntry(s); err == nil {
			names[e.Name()] = true
		}
	}
	return names
}

// admitPlugins: a plugins.seed (config) plugin's refusal is fatal, named; a
// REST-added row's refusal just drops that plugin, warned and named in refusals.
func admitPlugins(ctx context.Context, reg pluginreg.FetchRegistry, rows []pluginreg.Plugin, plugins []plugin.Plugin, seed []string, modules map[string]yaml.Node) ([]plugin.Plugin, map[string]error, error) {
	seedNames := seedPluginNames(seed)
	refusals := make(map[string]error)
	out := make([]plugin.Plugin, 0, len(plugins))
	claims := newManifestClaims()
	// claims.claim resolves a name collision to whichever plugin reaches it
	// first in plugins' order; every caller relies on pluginreg.OrderBySeed
	// having already put every seed row ahead of REST-added ones.
	for _, p := range plugins {
		plugin.WarnUnlistedManifestEntries(p)
		if err := admitOnePlugin(p, modules, claims); err != nil {
			if seedNames[p.Name] {
				return nil, nil, err
			}
			slog.Warn("plugin refused; dropped from the roster, other plugins still load",
				"component", "startup", "plugin", p.Name, "err", err)
			persistPluginRefusal(ctx, reg, rows, p.Name, err)
			refusals[p.Name] = err
			continue
		}
		out = append(out, p)
	}
	return out, refusals, nil
}

// persistPluginRefusal stores cause on name's registry row so GET /plugins
// shows why boot dropped it - best-effort; a write failure only logs.
func persistPluginRefusal(ctx context.Context, reg pluginreg.FetchRegistry, rows []pluginreg.Plugin, name string, cause error) {
	for _, row := range rows {
		if row.Name == name {
			row.Error = cause.Error()
			if err := reg.Put(ctx, row); err != nil {
				slog.Warn("failed to persist plugin refusal on its row", "component", "startup", "plugin", name, "err", err)
			}
			return
		}
	}
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
