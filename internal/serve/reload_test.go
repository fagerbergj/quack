package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// reloadPlugin is one local plugin root's contents for writeReloadPlugin.
type reloadPlugin struct {
	agents    map[string]string // bundle name -> agent.yaml
	workflows map[string]string // shape name -> workflows/<name>.yaml
	servers   string            // mcp.json's mcpServers members, "" for no mcp.json
}

func writeReloadPlugin(t *testing.T, root string, p reloadPlugin) {
	t.Helper()
	agents := slices.Sorted(mapsKeys(p.agents))
	workflows := slices.Sorted(mapsKeys(p.workflows))
	ns, _ := json.Marshal(map[string]any{"schemaVersion": 1, "agents": agents, "workflows": workflows})
	manifest := `{"$schema":"https://agent-plugins.org/schemas/1.1.0/plugin.schema.json","name":"p","extensions":{"io.github.fagerbergj.quack":` + string(ns) + `}}`
	write := func(rel, body string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("plugin.json", manifest)
	for name, yml := range p.agents {
		write(filepath.Join("agents", name, "agent-card.json"), `{"name":"`+name+`","description":"the `+name+` agent"}`)
		write(filepath.Join("agents", name, "prompt.md"), "You are "+name+".")
		write(filepath.Join("agents", name, "agent.yaml"), yml)
	}
	for name, yml := range p.workflows {
		write(filepath.Join("workflows", name+".yaml"), yml)
	}
	_ = os.Remove(filepath.Join(root, "mcp.json"))
	if p.servers != "" {
		write("mcp.json", `{"$schema":"https://agent-plugins.org/schemas/1.1.0/mcp.schema.json","mcpServers":{`+p.servers+`}}`)
	}
}

func mapsKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// stubServer is one mcp.json member running the self-exec stub; extraEnv
// changes its launch spec (so its reuse key) without changing its behaviour.
func stubServer(name, extraEnv string) string {
	return `"` + name + `":{"type":"stdio","command":"quack-mcpstub","env":{"_QUACK_MCP_STUB_SERVER":"1"` + extraEnv + `}}`
}

type reloadRig struct {
	r      *reloader
	reg    pluginreg.FetchRegistry
	exec   *dag.Executor
	shapes atomic.Pointer[[]workflowcatalog.Shape]
}

// newReloadRig wires a reloader the way boot does, from an empty generation 1.
func newReloadRig(t *testing.T, cfg *config.Config, build func(*config.Config, []extTool) (builtAgents, error)) *reloadRig {
	t.Helper()
	cfg.Plugins = &config.PluginsConfig{Root: t.TempDir()}
	cfg.Workspace.Root = t.TempDir()
	rig := &reloadRig{reg: pluginreg.NewFSRegistry(cfg.Plugins.Root)}
	rig.exec = dag.NewExecutor(session.InMemoryService(), nil, nil, nil, nil, nil)
	rig.r = &reloader{cfg: cfg, reg: rig.reg, skills: newSwappableSkillSource(newSkillSource(nil))}
	rig.r.declared.Store(&map[string]bool{})
	rr := &rosterReload{pristine: cloneForSeeding(cfg), build: build, mcpOn: true, exec: rig.exec, shapesRef: &rig.shapes}
	rr.cur = rr.generation(context.Background(), cfg, builtAgents{gateCfgs: newGateConfigs(0)}, newMCPSet(cfg.Workspace.Root, noSandbox()), 1)
	rig.exec.SetRoster(rr.cur.roster)
	rig.r.attach(rr)
	t.Cleanup(rig.r.shutdown)
	return rig
}

// addLocal registers root as a REST-style (non-seed) local row.
func (rig *reloadRig) addLocal(t *testing.T, root string) {
	t.Helper()
	entry, err := pluginreg.ParseEntry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.reg.Put(context.Background(), pluginreg.FromEntry(entry)); err != nil {
		t.Fatal(err)
	}
}

func (rig *reloadRig) reload(t *testing.T) schema.PluginReloadReport {
	t.Helper()
	rep, err := rig.r.reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v (report %+v)", err, rep)
	}
	return rep
}

func (rig *reloadRig) cur() *generation { return rig.r.roster.cur }

// fakeBuild makes one llmagent per configured agent, on the model llm returns for that build.
func fakeBuild(llm func() model.LLM) func(*config.Config, []extTool) (builtAgents, error) {
	return func(cfg *config.Config, _ []extTool) (builtAgents, error) {
		m := llm()
		b := builtAgents{clients: map[string]adkagent.Agent{}, models: map[string]model.LLM{}, gateCfgs: newGateConfigs(0), dropped: map[string]error{}}
		for name := range cfg.Agents {
			a, err := llmagent.New(llmagent.Config{Name: name, Model: m, Description: name, Instruction: "ROLE:" + name})
			if err != nil {
				return builtAgents{}, err
			}
			b.clients[name], b.models[name] = a, m
		}
		return b, nil
	}
}

// realBuild runs buildAgents over minimal deps, as the buildAgents tests do.
func realBuild(t *testing.T) func(*config.Config, []extTool) (builtAgents, error) {
	t.Helper()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := skill.Source(newSwappableSkillSource(newSkillSource(nil)))
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillsource.New(src, jail, localUserID)})
	if err != nil {
		t.Fatal(err)
	}
	scoped := func(names []string) (*skilltoolset.SkillToolset, error) {
		return skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillsource.New(skillsource.Scoped(src, names), jail, localUserID)})
	}
	nodeServers := newPerNodeServers()
	t.Cleanup(nodeServers.closeAll)
	return func(cfg *config.Config, tools []extTool) (builtAgents, error) {
		b := builtAgents{dropped: map[string]error{}}
		var err error
		b.clients, b.models, _, _, _, b.gateCfgs, _, err = buildAgents(cfg, nil, session.InMemoryService(), skillTS, src, scoped,
			nil, jail, nil, tools, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nodeServers, b.dropped)
		return b, err
	}
}

func reloadTestConfig() *config.Config {
	cfg := minimalPluginTestConfig()
	cfg.Workspace.Sandbox = "none"
	return cfg
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitPIDExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// heldLLM answers FROM-OLD once release closes; entered closes on the first call.
type heldLLM struct {
	entered, release chan struct{}
	once             sync.Once
}

func (*heldLLM) Name() string { return "held" }

func (h *heldLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		h.once.Do(func() { close(h.entered) })
		<-h.release
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "FROM-OLD"}}}}, nil)
	}
}

// A node pinned to the old generation finishes on its agent and keeps its
// old server until done(); an unchanged server is shared, not respawned.
func TestReloadDuringRunKeepsPinnedAgentAndServers(t *testing.T) {
	installMCPStub(t)
	t.Setenv("QUACK_RESEARCHER_MODEL", "m")
	blocking := &heldLLM{entered: make(chan struct{}), release: make(chan struct{})}
	var builds atomic.Int32
	rig := newReloadRig(t, reloadTestConfig(), fakeBuild(func() model.LLM {
		if builds.Add(1) == 1 {
			return blocking
		}
		return stubLLM{}
	}))
	root := filepath.Join(t.TempDir(), "p")
	writeReloadPlugin(t, root, reloadPlugin{
		agents:  map[string]string{"scout": "model_role: researcher\n"},
		servers: stubServer("keep", "") + "," + stubServer("change", ""),
	})
	rig.addLocal(t, root)
	rep := rig.reload(t)
	if rep.Generation != 2 || !slices.Equal(rep.Agents.Added, []string{"scout"}) || len(rep.McpServers.Started) != 2 {
		t.Fatalf("first reload = %+v, want gen 2 adding scout and starting both servers", rep)
	}
	oldSet := rig.cur().mcp
	keepPID, changePID := procPID(procFor(t, oldSet, "p/keep")), procPID(procFor(t, oldSet, "p/change"))

	ctx, done := rig.exec.Pin(context.Background())
	type result struct {
		out map[string]string
		err error
	}
	ran := make(chan result, 1)
	go func() {
		plan := dag.Plan{ID: "plan", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "scout", Task: "T"}}}
		out, _, _, err := rig.exec.RunPlanStep(ctx, plan, "quack", "u", "chat", nil, map[string]bool{"n1": true})
		ran <- result{out, err}
	}()
	<-blocking.entered

	writeReloadPlugin(t, root, reloadPlugin{servers: stubServer("keep", "") + "," + stubServer("change", `,"GEN":"2"`)})
	rep = rig.reload(t)
	if rep.Generation != 3 || !slices.Equal(rep.Agents.Removed, []string{"scout"}) {
		t.Fatalf("second reload = %+v, want gen 3 removing scout", rep)
	}
	if !slices.Equal(rep.McpServers.Reused, []string{"p/keep"}) || !slices.Equal(rep.McpServers.Started, []string{"p/change"}) || !slices.Equal(rep.McpServers.Stopped, []string{"p/change"}) {
		t.Fatalf("mcp_servers = %+v, want keep reused and change restarted", rep.McpServers)
	}
	if got := procPID(procFor(t, rig.cur().mcp, "p/keep")); got != keepPID {
		t.Fatalf("unchanged server pid %d -> %d, want the same process", keepPID, got)
	}
	if !pidAlive(changePID) {
		t.Fatal("old change server stopped while a run was still pinned to its roster")
	}

	close(blocking.release)
	res := <-ran
	if res.err != nil || res.out["n1"] != "FROM-OLD" {
		t.Fatalf("pinned node = (%v, %v), want FROM-OLD from the removed agent", res.out, res.err)
	}
	if !pidAlive(changePID) {
		t.Fatal("old change server stopped before the run's done()")
	}
	done()
	waitPIDExit(t, changePID)
	if !pidAlive(keepPID) {
		t.Fatal("shared keep server stopped with the old roster")
	}
}

func failureFor(rep schema.PluginReloadReport, stage schema.PluginReloadFailureStage, member string) (schema.PluginReloadFailure, bool) {
	for _, f := range rep.Failures {
		if f.Stage == stage && (member == "" || (f.Member != nil && *f.Member == member)) {
			return f, true
		}
	}
	return schema.PluginReloadFailure{}, false
}

func TestReloadDropsAndReportsFailingMembers(t *testing.T) {
	installMCPStub(t)
	t.Setenv("QUACK_RESEARCHER_MODEL", "m")
	rig := newReloadRig(t, reloadTestConfig(), realBuild(t))
	good := filepath.Join(t.TempDir(), "good")
	writeReloadPlugin(t, good, reloadPlugin{
		agents: map[string]string{
			"scout": "model_role: researcher\ntools: [probe]\n",
			"lost":  "model_role: researcher\ntools: [no_such_tool]\n",
		},
		servers: stubServer("probe", "") + `,"broken":{"type":"stdio","command":"quack-no-such-binary"}`,
	})
	bad := filepath.Join(t.TempDir(), "badyaml")
	writeReloadPlugin(t, bad, reloadPlugin{agents: map[string]string{"typo": "tools: [unterminated"}})
	rig.addLocal(t, good)
	rig.addLocal(t, bad)

	rep := rig.reload(t)
	if rep.Generation != 2 || !slices.Equal(rep.Agents.Added, []string{"scout"}) {
		t.Fatalf("reload = %+v, want gen 2 with only scout added", rep)
	}
	if f, ok := failureFor(rep, schema.Agent, "lost"); !ok || f.Plugin == nil || *f.Plugin != "good" {
		t.Errorf("failures = %+v, want lost dropped at stage agent, owned by good", rep.Failures)
	}
	if f, ok := failureFor(rep, schema.Mcp, "broken"); !ok || *f.Plugin != "good" {
		t.Errorf("failures = %+v, want good/broken at stage mcp", rep.Failures)
	}
	if f, ok := failureFor(rep, schema.Seed, ""); !ok || *f.Plugin != "badyaml" {
		t.Errorf("failures = %+v, want badyaml dropped at stage seed", rep.Failures)
	}
	if _, ok := rig.cur().roster.Agents["scout"]; !ok {
		t.Fatal("scout missing from the swapped roster")
	}
	if rig.r.mcpDeclared()["badyaml"] {
		t.Error("a plugin dropped at seed still counts as serving")
	}
}

// failingList is a registry whose List fails.
type failingList struct{ pluginreg.FetchRegistry }

func (failingList) List(context.Context) ([]pluginreg.Plugin, error) {
	return nil, errors.New("registry down")
}

// Every abort leaves the previous roster serving and releases what it spawned.
func TestReloadAbortKeepsPreviousRoster(t *testing.T) {
	installMCPStub(t)
	t.Setenv("QUACK_RESEARCHER_MODEL", "m")
	cases := []struct {
		name  string
		stage schema.PluginReloadFailureStage
		setup func(t *testing.T, rig *reloadRig, root string)
	}{
		{"registry read", schema.Registry, func(t *testing.T, rig *reloadRig, _ string) {
			rig.r.reg = failingList{rig.reg}
		}},
		{"incomplete config agent", schema.Config, func(t *testing.T, rig *reloadRig, _ string) {
			rig.r.roster.pristine.Agents = map[string]config.AgentConfig{"ghost": {}}
		}},
		{"agent build", schema.Agent, func(t *testing.T, rig *reloadRig, _ string) {
			rig.r.roster.build = func(*config.Config, []extTool) (builtAgents, error) {
				return builtAgents{}, errors.New("config agent broke")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReloadRig(t, reloadTestConfig(), fakeBuild(func() model.LLM { return stubLLM{} }))
			root := filepath.Join(t.TempDir(), "p")
			writeReloadPlugin(t, root, reloadPlugin{agents: map[string]string{"scout": "model_role: researcher\n"}})
			rig.addLocal(t, root)
			rig.reload(t)
			before := rig.cur()

			writeReloadPlugin(t, root, reloadPlugin{agents: map[string]string{"scout": "model_role: researcher\n"}, servers: stubServer("fresh", "")})
			tc.setup(t, rig, root)
			rep, err := rig.r.reload(context.Background())
			if err == nil {
				t.Fatal("reload = nil error, want an abort")
			}
			if _, ok := failureFor(rep, tc.stage, ""); !ok || rep.Generation != 2 {
				t.Fatalf("report = %+v, want a %s failure at the unchanged gen 2", rep, tc.stage)
			}
			if rig.cur() != before || rig.exec.RosterFor(context.Background()) != before.roster {
				t.Fatal("an aborted reload swapped the roster")
			}
		})
	}
}

// pidsWithEnv lists live processes whose environment holds kv.
func pidsWithEnv(kv string) []int {
	var pids []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if raw, err := os.ReadFile("/proc/" + e.Name() + "/environ"); err == nil && bytes.Contains(raw, []byte(kv+"\x00")) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// An abort after new servers spawned releases them.
func TestReloadAbortReleasesSpawnedServers(t *testing.T) {
	installMCPStub(t)
	rig := newReloadRig(t, reloadTestConfig(), fakeBuild(func() model.LLM { return stubLLM{} }))
	mark := strconv.FormatInt(time.Now().UnixNano(), 10)
	root := filepath.Join(t.TempDir(), "p")
	writeReloadPlugin(t, root, reloadPlugin{servers: stubServer("fresh", `,"_QUACK_RELOAD_MARK":"`+mark+`"`)})
	rig.addLocal(t, root)
	var spawned []int
	rig.r.roster.build = func(*config.Config, []extTool) (builtAgents, error) {
		spawned = pidsWithEnv("_QUACK_RELOAD_MARK=" + mark)
		return builtAgents{}, errors.New("config agent broke")
	}
	if _, err := rig.r.reload(context.Background()); err == nil {
		t.Fatal("reload = nil error, want the build abort")
	}
	if len(spawned) != 1 {
		t.Fatalf("servers running during build = %v, want the one fresh server", spawned)
	}
	waitPIDExit(t, spawned[0])
	if len(rig.r.mcpDeclared()) != 0 {
		t.Fatal("an aborted reload published its plugins' servers")
	}
}

// A reload re-seeds from the pristine config: a deployment override keeps
// winning, and an edited plugin shape replaces the old one instead of stacking.
func TestReloadReseedsFromPristineConfig(t *testing.T) {
	t.Setenv("QUACK_RESEARCHER_MODEL", "m")
	cfg := reloadTestConfig()
	cfg.Agents = map[string]config.AgentConfig{"scout": {ContextWindow: 1234}}
	rig := newReloadRig(t, cfg, fakeBuild(func() model.LLM { return stubLLM{} }))
	root := filepath.Join(t.TempDir(), "p")
	shape := func(trigger string) string {
		return "name: flow\ntrigger: " + trigger + "\nshape: s\nagents: [scout]\n"
	}
	writeReloadPlugin(t, root, reloadPlugin{
		agents:    map[string]string{"scout": "model_role: researcher\ncontext_window: 99\n"},
		workflows: map[string]string{"flow": shape("first trigger")},
	})
	rig.addLocal(t, root)
	rig.reload(t)
	writeReloadPlugin(t, root, reloadPlugin{
		agents:    map[string]string{"scout": "model_role: researcher\ncontext_window: 99\n"},
		workflows: map[string]string{"flow": shape("second trigger")},
	})
	rep := rig.reload(t)

	if !slices.Equal(rep.Workflows.Updated, []string{"flow"}) {
		t.Fatalf("workflows = %+v, want flow updated", rep.Workflows)
	}
	shapes := *rig.shapes.Load()
	if len(shapes) != 1 || shapes[0].Trigger != "second trigger" {
		t.Fatalf("catalog = %+v, want the one edited flow", shapes)
	}
	infos := rig.cur().roster.Infos
	if len(infos) != 1 || infos[0].ContextWindow != 1234 {
		t.Fatalf("roster infos = %+v, want scout with the config override's context_window", infos)
	}
	if rig.r.roster.pristine.Agents["scout"].Bundle != "" || len(rig.r.roster.pristine.Workflows) != 0 {
		t.Fatal("a reload seeded into the pristine config")
	}
}

func TestDiffGenerations(t *testing.T) {
	old := &generation{
		bundles: map[string]string{"same": "h1", "rebundled": "h1", "reconfigured": "h1", "gone": "h1"},
		configs: map[string]string{"same": "c", "rebundled": "c", "reconfigured": "c", "gone": "c"},
		shapes:  []workflowcatalog.Shape{{Name: "kept"}, {Name: "edited", Trigger: "a"}, {Name: "dropped"}},
	}
	next := &generation{
		bundles: map[string]string{"same": "h1", "rebundled": "h2", "reconfigured": "h1", "new": "h1"},
		configs: map[string]string{"same": "c", "rebundled": "c", "reconfigured": "c2", "new": "c"},
		shapes:  []workflowcatalog.Shape{{Name: "kept"}, {Name: "edited", Trigger: "b"}, {Name: "added"}},
	}
	rep := newReloadReport(1)
	diffGenerations(old, next, &rep)
	want := schema.PluginReloadAgents{
		Added:   []string{"new"},
		Updated: []schema.PluginReloadAgentUpdate{{Name: "rebundled", BundleHash: "h2"}, {Name: "reconfigured", BundleHash: "h1"}},
		Removed: []string{"gone"},
	}
	gotA, _ := json.Marshal(rep.Agents)
	wantA, _ := json.Marshal(want)
	if string(gotA) != string(wantA) {
		t.Errorf("agents = %s, want %s", gotA, wantA)
	}
	gotW, _ := json.Marshal(rep.Workflows)
	if w := `{"added":["added"],"removed":["dropped"],"updated":["edited"]}`; string(gotW) != w {
		t.Errorf("workflows = %s, want %s", gotW, w)
	}
}

// The wire report never carries null lists.
func TestNewReloadReportListsAreEmptyNotNull(t *testing.T) {
	raw, _ := json.Marshal(newReloadReport(7))
	if strings.Contains(string(raw), "null") {
		t.Fatalf("report = %s, want [] for every list", raw)
	}
}

// A booted server serves POST /api/v1/plugins/reload off the wired reloader.
func TestInProcessBootServesPluginReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUACK_LLM_API_KEY", "x")
	cfgPath := filepath.Join(dir, "quack.yaml")
	rendered := cli.EmitServerConfig(cli.InitAnswers{
		Endpoint: "http://127.0.0.1:1/v1", APIKey: "x", MainModel: "m",
		SessionKind: "sqlite", SessionURL: filepath.Join(dir, "store.db"), Coding: true, Sandbox: "none",
	})
	if err := os.WriteFile(cfgPath, []byte(rendered), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadDeferringAgentCompleteness(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspace.Root = filepath.Join(dir, "ws")
	cfg.Plugins.Root = filepath.Join(dir, "plugins")
	base, stop, err := InProcessFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer func() { _ = stop() }()

	resp, err := http.Post(base+"/api/v1/plugins/reload", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rep schema.PluginReloadReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || rep.Generation != 2 {
		t.Fatalf("reload = %d %+v, want 200 at generation 2", resp.StatusCode, rep)
	}
}
