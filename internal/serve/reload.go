package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
)

// reloader re-runs boot's plugin pipeline into a new roster generation. Its
// mutex serializes reloads; Roster.OnDead never takes it.
type reloader struct {
	mu       sync.Mutex
	cfg      *config.Config // plugins root/seed and extension modules; never seeded by a reload
	reg      pluginreg.FetchRegistry
	skills   *swappableSkillSource
	declared atomic.Pointer[map[string]bool]
	// roster is nil until boot has built the agents; a reload before then
	// (or in a skills-only test) swaps skills alone.
	roster *rosterReload
}

// rosterReload is the agent half of a reload: what boot built the first
// generation from, and the generation currently serving new runs.
type rosterReload struct {
	pristine  *config.Config // cfg before any plugin seeded into it
	res       *artifactsrc.Resolver
	build     func(cfg *config.Config, tools []extTool) (builtAgents, error)
	sdkTools  []extTool
	mcpOn     bool // false when boot had no sandbox caps to spawn servers with
	exec      *dag.Executor
	shapesRef *atomic.Pointer[[]workflowcatalog.Shape]
	cur       *generation
}

// builtAgents is one buildAgents pass; dropped holds the optional agents it left out.
type builtAgents struct {
	clients  map[string]adkagent.Agent
	models   map[string]model.LLM
	gateCfgs *gateConfigs
	dropped  map[string]error
}

// generation is one roster plus what a reload diffs against.
type generation struct {
	roster  *dag.Roster
	mcp     *mcpSet
	bundles map[string]string // agent -> bundle hash
	configs map[string]string // agent -> resolved config digest
	shapes  []workflowcatalog.Shape
}

// attach hands the reloader boot's first generation, turning on agent reloads.
func (r *reloader) attach(rr *rosterReload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roster = rr
}

// shutdown releases whichever MCP set is current; retired sets close as their last run ends.
func (r *reloader) shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.roster != nil {
		r.roster.cur.mcp.Release()
	}
}

func (r *reloader) mcpDeclared() map[string]bool { return *r.declared.Load() }

// reload swaps in a new generation, or returns an error with nothing swapped
// and the old roster still serving. Per-member failures only drop that member.
func (r *reloader) reload(ctx context.Context) (schema.PluginReloadReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A client hanging up must not leave half-spawned servers behind.
	ctx = context.WithoutCancel(ctx)
	rep := newReloadReport(r.generation())
	admitted, err := r.admit(ctx, &rep)
	if err != nil {
		return rep, err
	}
	if r.roster == nil {
		r.swapSkills(admitted)
		return rep, nil
	}
	next, admitted, err := r.roster.next(ctx, admitted, &rep)
	if err != nil {
		return rep, err
	}
	r.swapSkills(admitted)
	r.roster.swap(next, &rep)
	return rep, nil
}

func (r *reloader) generation() uint64 {
	if r.roster == nil {
		return 0
	}
	return r.roster.cur.roster.Gen
}

// admit is boot's list -> resolve -> admit; a refused REST row is reported and dropped.
func (r *reloader) admit(ctx context.Context, rep *schema.PluginReloadReport) ([]plugin.Plugin, error) {
	rows, err := r.reg.List(ctx)
	if err != nil {
		return nil, abortReload(rep, schema.Registry, err)
	}
	rows = pluginreg.OrderBySeed(r.cfg.Plugins.Seed, rows)
	rows = append(rows, pluginreg.EmbeddedQuackPlugin())
	plugins, err := resolveRegistryPlugins(r.cfg.Plugins.Root, rows)
	if err != nil {
		return nil, abortReload(rep, schema.Resolve, err)
	}
	admitted, refusals, err := admitPlugins(ctx, r.reg, rows, plugins, r.cfg.Plugins.Seed, r.cfg.Extensions.Modules)
	if err != nil {
		return nil, abortReload(rep, schema.Admission, err)
	}
	for _, name := range slices.Sorted(maps.Keys(refusals)) {
		reloadFailure(rep, name, "", schema.Admission, refusals[name])
	}
	return admitted, nil
}

func (r *reloader) swapSkills(admitted []plugin.Plugin) {
	r.skills.Swap(newSkillSource(admitted))
	r.declared.Store(mcpDeclaredNames(admitted))
}

// next builds the candidate generation; admitted comes back without the
// plugins whose seeding failed.
func (g *rosterReload) next(ctx context.Context, admitted []plugin.Plugin, rep *schema.PluginReloadReport) (*generation, []plugin.Plugin, error) {
	cand, admitted, owners := g.seed(admitted, rep)
	if err := cand.RequireAgentBundlesAndModels(); err != nil {
		return nil, nil, abortReload(rep, schema.Config, err)
	}
	set := g.nextMCP(ctx, admitted, rep)
	built, err := g.build(cand, append(slices.Clone(g.sdkTools), set.tools()...))
	if err != nil {
		if set != g.cur.mcp {
			go set.Release()
		}
		return nil, nil, abortReload(rep, schema.Agent, err)
	}
	for _, name := range slices.Sorted(maps.Keys(built.dropped)) {
		reloadFailure(rep, owners[name], name, schema.Agent, built.dropped[name])
	}
	return g.generation(ctx, cand, built, set, g.cur.roster.Gen+1), admitted, nil
}

// seed merges each plugin into its own copy of the candidate, so one plugin's
// failed seed is dropped without leaving half its agents behind.
func (g *rosterReload) seed(admitted []plugin.Plugin, rep *schema.PluginReloadReport) (*config.Config, []plugin.Plugin, map[string]string) {
	cand := cloneForSeeding(g.pristine)
	kept := make([]plugin.Plugin, 0, len(admitted))
	owners := map[string]string{}
	for _, p := range admitted {
		try := cloneForSeeding(cand)
		res, err := seedPlugin(try, p)
		if err != nil {
			slog.Warn("plugin seed failed at reload; plugin dropped", "component", "reload", "plugin", p.Name, "err", err)
			reloadFailure(rep, p.Name, "", schema.Seed, err)
			continue
		}
		cand = try
		kept = append(kept, p)
		for _, a := range res.Agents {
			owners[a] = p.Name
		}
	}
	dropUnclaimedOptionalAgents(cand)
	return cand, kept, owners
}

func (g *rosterReload) nextMCP(ctx context.Context, admitted []plugin.Plugin, rep *schema.PluginReloadReport) *mcpSet {
	if !g.mcpOn {
		return g.cur.mcp
	}
	set, m := g.cur.mcp.next(ctx, admitted)
	rep.McpServers = schema.PluginReloadServers{
		Started: append([]string{}, m.Started...), Reused: append([]string{}, m.Reused...), Stopped: append([]string{}, m.Stopped...),
	}
	for _, f := range m.Failures {
		reloadFailure(rep, f.Plugin, f.Server, schema.Mcp, f.Err)
	}
	return set
}

// generation wraps one build as a roster whose death releases set.
func (g *rosterReload) generation(ctx context.Context, cfg *config.Config, built builtAgents, set *mcpSet, gen uint64) *generation {
	infos, media, text, bundles := buildAgentInfos(ctx, cfg, g.res, built.clients)
	roster := &dag.Roster{
		Gen: gen, Agents: built.clients, Models: built.models, Media: media, Infos: infos, Text: text,
		CfgFor: built.gateCfgs.For, SpecFor: admissionSpecFor(cfg),
		// A retired server still runs from the plugin's one clone dir, which Fetch
		// checks out in place, so it may read the new generation's files.
		OnDead: func() { go set.Release() },
	}
	configs := make(map[string]string, len(built.clients))
	for name := range built.clients {
		configs[name] = digest(cfg.Agents[name])
	}
	shapes := catalogShapes(cfg, workflowcatalog.FromConfig(cfg.Workflows, cfg.Revision), built.clients)
	return &generation{roster: roster, mcp: set, bundles: bundles, configs: configs, shapes: shapes}
}

// swap installs next for unpinned runs; a run pinned to the old roster keeps it.
func (g *rosterReload) swap(next *generation, rep *schema.PluginReloadReport) {
	g.exec.SetRoster(next.roster)
	dag.SetAgentRoster(next.roster.Infos)
	g.shapesRef.Store(&next.shapes)
	diffGenerations(g.cur, next, rep)
	g.cur = next
	rep.Generation = int64(next.roster.Gen)
}

// catalogShapes drops shapes naming an optional agent the build left out.
func catalogShapes(cfg *config.Config, raw []workflowcatalog.Shape, clients map[string]adkagent.Agent) []workflowcatalog.Shape {
	dropped := map[string]bool{}
	for name, ac := range cfg.Agents {
		if _, ok := clients[name]; ac.Optional && !ok {
			dropped[name] = true
		}
	}
	return workflowcatalog.DropAgents(raw, dropped)
}

// cloneForSeeding copies the two fields plugin seeding writes (Agents,
// Workflows), so seeding the copy leaves c untouched.
func cloneForSeeding(c *config.Config) *config.Config {
	cp := *c
	cp.Agents = maps.Clone(c.Agents)
	cp.Workflows = slices.Clone(c.Workflows)
	return &cp
}

func diffGenerations(old, next *generation, rep *schema.PluginReloadReport) {
	agents := diffNamed(agentDigests(old), agentDigests(next))
	rep.Agents.Added, rep.Agents.Removed = agents.Added, agents.Removed
	for _, name := range agents.Updated {
		rep.Agents.Updated = append(rep.Agents.Updated, schema.PluginReloadAgentUpdate{Name: name, BundleHash: next.bundles[name]})
	}
	rep.Workflows = diffNamed(shapeDigests(old.shapes), shapeDigests(next.shapes))
}

// agentDigests keys by configs, which lists every built agent even when its
// bundle hash could not be re-read.
func agentDigests(g *generation) map[string]string {
	out := make(map[string]string, len(g.configs))
	for name, c := range g.configs {
		out[name] = g.bundles[name] + "|" + c
	}
	return out
}

// diffNamed compares name -> digest maps.
func diffNamed(old, next map[string]string) schema.PluginReloadNames {
	out := schema.PluginReloadNames{Added: []string{}, Updated: []string{}, Removed: []string{}}
	for _, name := range slices.Sorted(maps.Keys(next)) {
		if prev, ok := old[name]; !ok {
			out.Added = append(out.Added, name)
		} else if prev != next[name] {
			out.Updated = append(out.Updated, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(old)) {
		if _, ok := next[name]; !ok {
			out.Removed = append(out.Removed, name)
		}
	}
	return out
}

func shapeDigests(shapes []workflowcatalog.Shape) map[string]string {
	out := make(map[string]string, len(shapes))
	for _, s := range shapes {
		out[s.Name] = digest(s)
	}
	return out
}

func digest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// newReloadReport starts every list empty, never null, on the wire.
func newReloadReport(gen uint64) schema.PluginReloadReport {
	return schema.PluginReloadReport{
		Generation: int64(gen),
		Agents:     schema.PluginReloadAgents{Added: []string{}, Updated: []schema.PluginReloadAgentUpdate{}, Removed: []string{}},
		Workflows:  schema.PluginReloadNames{Added: []string{}, Updated: []string{}, Removed: []string{}},
		McpServers: schema.PluginReloadServers{Started: []string{}, Reused: []string{}, Stopped: []string{}},
		Failures:   []schema.PluginReloadFailure{},
	}
}

func reloadFailure(rep *schema.PluginReloadReport, pluginName, member string, stage schema.PluginReloadFailureStage, err error) {
	f := schema.PluginReloadFailure{Stage: stage, Error: err.Error()}
	if pluginName != "" {
		f.Plugin = &pluginName
	}
	if member != "" {
		f.Member = &member
	}
	rep.Failures = append(rep.Failures, f)
}

// abortReload records a whole-reload failure and returns it as the reload's error.
func abortReload(rep *schema.PluginReloadReport, stage schema.PluginReloadFailureStage, err error) error {
	reloadFailure(rep, "", "", stage, err)
	return err
}
