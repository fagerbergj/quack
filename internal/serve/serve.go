// Package serve is Quack's server bootstrap: it loads config, builds the
// inference model, orchestrator, and stores, and serves the REST + MCP API plus
// the embedded SPA. The `quack server run` command calls Run.
package serve

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/bundledir"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/promptbuilder"
	"github.com/fagerbergj/quack/internal/server"
	mcpserver "github.com/fagerbergj/quack/internal/server/mcp"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

//go:embed all:web/dist
var webDist embed.FS

// Run builds the server and serves it on cfg.Server.Addr (or :port) until ctx is
// cancelled (the caller wires SIGINT/SIGTERM). The standalone `quack server run`
// path; logs to stdout so a container/supervisor collects them.
func Run(ctx context.Context, configPath string, port int) error {
	setupLoggingTo(os.Stdout, slog.LevelInfo)
	handler, cleanup, addr, err := build(ctx, configPath, port)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := &http.Server{Addr: addr, Handler: handler}
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("quack listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()
	select {
	case err := <-serveErr:
		return fmt.Errorf("http serve failed: %w", err)
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	slog.Info("stopped")
	return nil
}

// InProcess builds the server and serves it on an ephemeral loopback port,
// returning its base URL and a stop func. This is how `quack` runs the duck
// locally — co-hosted in the CLI process, no separate `quack server run`. Logs go
// to stderr (default warn) so the client's stdout stays clean (e.g. `quack -p`).
func InProcess(ctx context.Context, configPath string) (baseURL string, stop func() error, err error) {
	setupLoggingTo(os.Stderr, slog.LevelWarn)
	handler, cleanup, _, err := build(ctx, configPath, 0)
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("listen loopback: %w", err)
	}
	srv := &http.Server{Handler: handler}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("in-process serve failed", "component", "serve", "err", err)
		}
	}()
	stop = func() error {
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		serr := srv.Shutdown(sc)
		cleanup()
		return serr
	}
	return "http://" + ln.Addr().String(), stop, nil
}

// build loads config and constructs the HTTP handler (orchestrator + stores +
// agents + SPA), shared by Run and InProcess. It returns the handler, a cleanup
// func (close A2A servers; note managed stores), and the listen addr. On any
// error it runs the cleanups registered so far and returns the error.
func build(ctx context.Context, configPath string, port int) (handler http.Handler, cleanup func(), addr string, err error) {
	var cleanups []func()
	runCleanups := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	defer func() {
		if err != nil {
			runCleanups()
			handler = nil
		}
	}()

	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("config load failed: %w", err)
	}
	addr = cfg.Server.Addr
	if port != 0 {
		addr = fmt.Sprintf(":%d", port)
	}

	// Managed topology: bring up the Postgres + Qdrant stores via docker compose
	// before opening them. embedded/external just run against pre-configured
	// stores. Stores are left running on exit (persistent infra — restart the app
	// freely); tear them down with `docker compose -p quack-stores down`.
	if cfg.Server.Managed() {
		if err = upStores(ctx); err != nil {
			return nil, nil, "", err
		}
		cleanups = append(cleanups, func() {
			slog.Info("managed stores left running; tear down with `docker compose -p quack-stores down`", "component", "serve")
		})
	}

	sessionStore, ok := cfg.Store(cfg.Session.Store)
	if !ok {
		return nil, nil, "", fmt.Errorf("session store %q not found in stores registry", cfg.Session.Store)
	}
	st, err := store.New(sessionStore.Kind, sessionStore.URL)
	if err != nil {
		return nil, nil, "", fmt.Errorf("store open failed: %w", err)
	}
	if n, err := st.FailStaleDagNodes(context.Background()); err != nil {
		slog.Error("fail stale dag nodes", "component", "store", "err", err)
	} else if n > 0 {
		slog.Info("marked orphaned dag nodes failed (previous process killed mid-run)", "component", "store", "count", n)
	}

	prov, _ := cfg.Provider(cfg.Orchestrator.Provider)
	llm, err := inference.NewModel(prov, cfg.Orchestrator.Model)
	if err != nil {
		return nil, nil, "", fmt.Errorf("inference model init failed: %w", err)
	}

	// Load skills once at startup; pass the toolset to every specialist agent so
	// all agents can call load_skill / list_skills / load_skill_resource. Skills
	// resolve from disk in cwd first (live repo edits) then the embedded copy,
	// so an installed binary works from any directory.
	skillSrc := skill.NewFileSystemSource(bundledir.SubFS("skills"))
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		return nil, nil, "", fmt.Errorf("skills toolset init failed: %w", err)
	}

	// Semantic memory (M6): a memory tool bound to a vector store (with QUACK_QDRANT_URL
	// set) turns it on — config composes it, no dedicated block. Task memory
	// follows `stage_memory` (researchers' recall + the trust gate's vetted
	// commit); user memory follows `commit_memory` bound to the orchestrator. A
	// store with no URL self-disables, so qdrant-less runs keep working.
	openMemory := func(rm config.ResolvedMemory, domain string) (*memory.Store, error) {
		eprov, ok := cfg.Provider(rm.Embedder.Provider)
		if !ok {
			return nil, fmt.Errorf("embedder provider %q not found", rm.Embedder.Provider)
		}
		embedder, err := inference.NewEmbedder(eprov, rm.Embedder.Model)
		if err != nil {
			return nil, fmt.Errorf("embedder: %w", err)
		}
		cprov, ok := cfg.Provider(rm.Consolidation.Provider)
		if !ok {
			return nil, fmt.Errorf("consolidation provider %q not found", rm.Consolidation.Provider)
		}
		consolidator, err := inference.NewModel(cprov, rm.Consolidation.Model)
		if err != nil {
			return nil, fmt.Errorf("consolidation model: %w", err)
		}
		return memory.New(context.Background(), rm.Kind, rm.URL, embedder, consolidator, rm.Collection, domain, rm.TopK, rm.MinScore)
	}
	var taskStore, userStore *memory.Store
	if rm, ok := cfg.MemoryStore("stage_memory"); ok {
		s, err := openMemory(rm, "task")
		if err != nil {
			return nil, nil, "", fmt.Errorf("task memory init failed: %w", err)
		}
		taskStore = s
		slog.Info("semantic memory enabled", "component", "startup", "collection", rm.Collection,
			"embedder", rm.Embedder.Model, "consolidation", rm.Consolidation.Model)
	}
	// User memory: presence of commit_memory on the orchestrator is the switch.
	if slices.Contains(cfg.Orchestrator.Tools, "commit_memory") {
		if rm, ok := cfg.MemoryStore("commit_memory"); ok {
			s, err := openMemory(rm, "user")
			if err != nil {
				return nil, nil, "", fmt.Errorf("user memory init failed: %w", err)
			}
			userStore = s
			slog.Info("user memory enabled", "component", "startup", "collection", rm.Collection)
		}
	}

	// Build each declarative agent, expose it over A2A, and collect a client the
	// DAG executor can dispatch to. Servers run for the process lifetime.
	clientMap, modelMap, servers, judgeFactory, gateCfgs, err := buildAgents(cfg, st.Sessions, skillTS, taskStore)
	if err != nil {
		return nil, nil, "", fmt.Errorf("agent build failed: %w", err)
	}
	cleanups = append(cleanups, func() {
		for _, s := range servers {
			_ = s.Close()
		}
	})

	// Build agent info list for the planner (name + description) and a set of
	// media-capable agents (those with "image" or "audio" in their Inputs config)
	// so the executor knows which nodes should receive attachment parts.
	agentInfos := make([]dag.AgentInfo, 0, len(clientMap))
	mediaAgents := make(map[string]bool)
	for name, c := range clientMap {
		agentInfos = append(agentInfos, dag.AgentInfo{Name: name, Description: c.Description()})
		ac := cfg.Agents[name]
		for _, inp := range ac.Inputs {
			if inp == "image" || inp == "audio" {
				mediaAgents[name] = true
				break
			}
		}
	}
	// Sort for a stable prompt (map iteration order is random) and render the
	// roster the orchestrator authors its DAG over.
	sort.Slice(agentInfos, func(i, j int) bool { return agentInfos[i].Name < agentInfos[j].Name })
	var rosterSB strings.Builder
	for _, a := range agentInfos {
		fmt.Fprintf(&rosterSB, "- `%s` — %s\n", a.Name, a.Description)
	}

	// Load orchestrator bundle for its system prompt.
	orchBundle, err := agent.LoadBundle("agents/orchestrator")
	if err != nil {
		return nil, nil, "", fmt.Errorf("orchestrator bundle load failed: %w", err)
	}
	// Load the format-markdown skill frontmatter so the orchestrator's prompt
	// lists it and the model knows to call load_skill("format-markdown") for
	// direct-answer responses.
	fmFm, err := skillSrc.LoadFrontmatter(context.Background(), "format-markdown")
	if err != nil {
		return nil, nil, "", fmt.Errorf("format-markdown skill load failed: %w", err)
	}
	// plan-work is the DAG-authoring playbook the orchestrator loads before planning.
	planWorkFm, err := skillSrc.LoadFrontmatter(context.Background(), "plan-work")
	if err != nil {
		return nil, nil, "", fmt.Errorf("plan-work skill load failed: %w", err)
	}
	// When user memory is on, append the orchestrator's memory.md guidance (its
	// "what to remember about the user" section) to its behaviour — gated the same
	// way agent bundles are.
	orchBehaviour := orchBundle.Prompt
	if userStore != nil {
		mem, err := agent.LoadBundleMemory("agents/orchestrator")
		if err != nil {
			return nil, nil, "", fmt.Errorf("orchestrator memory.md load failed: %w", err)
		}
		if mem != "" {
			orchBehaviour += "\n\n" + mem
		}
	}
	orchSysPrompt := promptbuilder.Orchestrator(rosterSB.String(), []*skill.Frontmatter{fmFm, planWorkFm}, orchBehaviour)

	planner := dag.NewPlanner(agentInfos)
	// cfgFor supplies the per-agent trust-gate config to the graph executor; a
	// non-gated (or gates-disabled) agent gets the zero Config (JudgeRounds=0), so
	// RunGatedRefine runs the worker once and returns it ungated.
	cfgFor := func(name string) vetting.Config { return gateCfgs[name] }
	// Advisor: a formative consult run inside the gate before each worker draft
	// (replaces the dropped self-critique). Built from the judge's model + the
	// agents/advisor bundle; nil (skip the consult) when gating is off or it fails
	// to build. Not added to the plannable roster — it's a gate helper, not a node.
	var advisorAgent adkagent.Agent
	if cfg.Gates.JudgeEnabled() {
		if aprov, ok := cfg.Provider(cfg.Gates.Judge.Provider); ok {
			if am, merr := inference.NewModel(aprov, cfg.Gates.Judge.Model); merr != nil {
				slog.Warn("advisor model build failed; consult disabled", "component", "startup", "err", merr)
			} else if ab, berr := agent.LoadBundle("agents/advisor"); berr != nil {
				slog.Warn("advisor bundle load failed; consult disabled", "component", "startup", "err", berr)
			} else if built, aerr := agent.Build(ab, am, nil, nil, agent.Compaction{}, ""); aerr != nil {
				slog.Warn("advisor build failed; consult disabled", "component", "startup", "err", aerr)
			} else {
				advisorAgent = built
				slog.Info("advisor enabled", "component", "startup", "model", cfg.Gates.Judge.Model)
			}
		}
	}
	executor := dag.NewExecutor(st.Sessions, clientMap, modelMap, advisorAgent, judgeFactory, cfgFor, mediaAgents)
	executor.SetMaxActive(cfg.Dag.MaxActiveNodes)
	orch := orchestrator.New(st.Sessions, llm, orchSysPrompt, planner, executor, skillTS, userStore)

	spa, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return nil, nil, "", fmt.Errorf("embed SPA fs failed: %w", err)
	}

	handler = server.New(server.Options{
		REST: rest.NewHandler(st, orch, llm),
		MCP:  mcpserver.Handler(orch),
		SPA:  spa,
	})
	return handler, runCleanups, addr, nil
}

// setupLoggingTo installs the process-wide slog handler writing to w, at
// QUACK_LOG_LEVEL (debug|info|warn|error) or `fallback` when that env is unset,
// in text or QUACK_LOG_FORMAT=json. SetDefault also reroutes stray stdlib log.*.
// `quack server run` logs to stdout (a supervisor collects them); the in-process
// duck logs to stderr at warn so the client's stdout stays clean.
func setupLoggingTo(w io.Writer, fallback slog.Level) {
	lvl := fallback
	if s := os.Getenv("QUACK_LOG_LEVEL"); s != "" {
		_ = lvl.UnmarshalText([]byte(s)) // unknown values leave fallback untouched
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(w, opts)
	if strings.EqualFold(os.Getenv("QUACK_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(w, opts)
	}
	slog.SetDefault(slog.New(h))
}

// buildAgents loads each configured agent bundle, builds its model and built-in
// tools, exposes it over a co-located A2A server, and returns:
//   - clientMap: agent name → A2A client (for the DAG executor)
//   - servers: A2A server handles (to close on shutdown)
func buildAgents(cfg *config.Config, sessions session.Service, skillTS *skilltoolset.SkillToolset, taskStore *memory.Store) (map[string]adkagent.Agent, map[string]model.LLM, []*agent.A2AServer, vetting.JudgeFactory, map[string]vetting.Config, error) {
	// Derive the recall service, leaving it nil (not a non-nil interface wrapping
	// a nil pointer) when memory is off. The gate takes the concrete *memory.Store
	// directly (taskStore), so it has no such typed-nil hazard.
	var taskMem adkmemory.Service
	if taskStore != nil {
		taskMem = taskStore
	}
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	urlCache := tools.NewURLCache()

	var judgeFactory vetting.JudgeFactory
	var gateCfg vetting.Config
	if cfg.Gates.Enabled() {
		var err error
		if gateCfg, err = vetting.FromConfig(cfg.Gates); err != nil {
			return nil, nil, nil, nil, nil, err
		}
		// The trust gate commits vetted tradecraft on a judge pass (M6). nil
		// *memory.Store when memory is off — the gate's nil check handles it.
		gateCfg.Memory = taskStore
		// The judge model is only built when the judge stage is active; the
		// deterministic + self-critique stages run without it. One-shot judge (no
		// web tools): citation backing is now checked deterministically in code, so
		// the judge scores in a single pass instead of an agentic re-fetch loop
		// (a multi-step re-fetch loop is wasted work for ~no gain).
		if cfg.Gates.JudgeEnabled() {
			jprov, ok := cfg.Provider(cfg.Gates.Judge.Provider)
			if !ok {
				return nil, nil, nil, nil, nil, fmt.Errorf("gates.judge: provider %q not found", cfg.Gates.Judge.Provider)
			}
			judge, err := inference.NewModel(jprov, cfg.Gates.Judge.Model)
			if err != nil {
				return nil, nil, nil, nil, nil, fmt.Errorf("gates.judge: model: %w", err)
			}
			judgeFactory = vetting.NewJudgeFactory(judge, nil)
		}
		slog.Info("trust gate enabled", "component", "startup",
			"deterministic_rounds", gateCfg.DeterministicRounds,
			"judge", cfg.Gates.Judge.Model, "judge_rounds", gateCfg.JudgeRounds, "threshold", gateCfg.Threshold)
	}

	// Build the compaction summariser once and share it across every gated agent.
	// An agent with no context_window configured is left uncompacted; see
	// compactionFor below.
	var summarizer model.LLM
	compCfg := cfg.Session.Compaction
	if compCfg.Enabled {
		cprov, ok := cfg.Provider(compCfg.Provider)
		if !ok {
			return nil, nil, nil, nil, nil, fmt.Errorf("compaction: provider %q not found", compCfg.Provider)
		}
		var err error
		if summarizer, err = inference.NewModel(cprov, compCfg.Model); err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("compaction: model: %w", err)
		}
		slog.Info("context compaction enabled", "component", "startup", "summariser", compCfg.Model, "prune", compCfg.PruneEnabled())
	}
	compactionFor := func(ac config.AgentConfig) agent.Compaction {
		if !compCfg.Enabled {
			return agent.Compaction{}
		}
		if ac.ContextWindow == 0 {
			slog.Warn("context compaction: agent has no context_window configured; not compacting it", "component", "startup", "model", ac.Model)
			return agent.Compaction{}
		}
		return agent.Compaction{
			Summarizer:    summarizer,
			ContextWindow: ac.ContextWindow,
			Prune:         compCfg.PruneEnabled(),
			Enabled:       true,
		}
	}

	clientMap := make(map[string]adkagent.Agent, len(cfg.Agents))
	modelMap := make(map[string]model.LLM, len(cfg.Agents))
	gateCfgs := make(map[string]vetting.Config, len(cfg.Agents)) // agent name → per-agent gate cfg (gated agents only)
	var servers []*agent.A2AServer

	for _, name := range names {
		ac := cfg.Agents[name]

		prov, ok := cfg.Provider(ac.Provider)
		if !ok {
			return nil, nil, servers, nil, nil, fmtErr(name, "provider %q not found", ac.Provider)
		}
		m, err := inference.NewModel(prov, ac.Model)
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "model: %v", err)
		}

		// Memory tools (M6) are ADK-native and route through the runner's
		// MemoryService (set in agent.Serve). preload_memory is ambient recall
		// (added to every agent when memory is on); load_memory is deliberate recall
		// (opt-in via the agent's tools list). Strip load_memory from the builtin
		// names regardless, so tools.Build never sees an unknown tool.
		toolNames := make([]string, 0, len(ac.Tools))
		wantLoadMemory := false
		for _, t := range ac.Tools {
			switch t {
			case "load_memory":
				wantLoadMemory = true // ADK-native; added below when memory is on
				continue
			case "stage_memory":
				if taskMem == nil {
					continue // memory off: don't build a sink that never commits
				}
			}
			toolNames = append(toolNames, t)
		}
		var builtins []tool.Tool
		if len(toolNames) > 0 {
			builtins, err = tools.Build(toolNames, tools.Deps{
				WebSearch:  tools.Backend{Kind: cfg.Tools["web_search"].Kind, URL: cfg.Tools["web_search"].URL, Key: cfg.Tools["web_search"].APIKey()},
				Fetch:      tools.Backend{Kind: cfg.Tools["web_fetch"].Kind, URL: cfg.Tools["web_fetch"].URL},
				Summarizer: m,
				Cache:      urlCache,
			})
			if err != nil {
				return nil, nil, servers, nil, nil, fmtErr(name, "tools: %v", err)
			}
		}

		bundle, err := agent.LoadBundle(ac.Bundle)
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "bundle: %v", err)
		}
		// Memory guidance (M6): the bundle's optional memory.md. Its presence marks the
		// agent as a memory participant — ONLY such agents get recall (preload/load_memory).
		// Tool-less combiners (synthesizer) and media/image readers have no memory.md, so
		// they never touch the embedder. That matters: the synthesizer's input is all
		// upstream findings concatenated (tens of KB), and embedding that on the CPU
		// embedder is a large, useless job that head-of-line-blocks the queue and stalls
		// the whole DAG. Recall belongs only to agents that research.
		var memGuidance string
		if taskStore != nil {
			if memGuidance, err = agent.LoadBundleMemory(ac.Bundle); err != nil {
				return nil, nil, servers, nil, nil, fmtErr(name, "memory.md: %v", err)
			}
		}
		if taskMem != nil && memGuidance != "" {
			builtins = append(builtins, memory.NewPreload())
			if wantLoadMemory {
				builtins = append(builtins, loadmemorytool.New())
			}
		}
		comp := compactionFor(ac)
		ag, err := agent.Build(bundle, m, builtins, []tool.Toolset{skillTS}, comp, memGuidance)
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "build: %v", err)
		}

		// The agent is served PLAIN — the trust gate is applied per-node by the graph
		// (dag.BuildWorkflow → vetting.RunGatedRefine), not wrapped around the agent.
		// Here we only collect the per-agent gate config (base + this bundle's rubric
		// override + memory participation) for the executor's cfgFor.
		if cfg.Gates.Enabled() && !ac.IsGated() {
			slog.Info("trust gate skipped for agent (gated: false)", "component", "startup", "agent", name)
		}
		if cfg.Gates.Enabled() && ac.IsGated() {
			agentGateCfg := gateCfg
			// An agent participates in task memory iff it has a memory.md (loaded as
			// memGuidance above). Such agents commit on a judge pass even when they
			// staged nothing, so Commit's answer-extraction still runs.
			agentGateCfg.CommitMemory = taskStore != nil && memGuidance != ""
			// A retrieval agent (web tools in its list) must actually retrieve —
			// a zero-activity answer hard-fails the deterministic fold instead of
			// sailing to the judge ungraded (see vetting.Config.RequireRetrieval).
			for _, tn := range ac.Tools {
				if tn == "web_search" || tn == "web_fetch" {
					agentGateCfg.RequireRetrieval = true
					break
				}
			}
			if override, err := vetting.LoadBundleRubric(ac.Bundle); err != nil {
				return nil, nil, servers, nil, nil, fmtErr(name, "rubric: %v", err)
			} else if override != "" {
				agentGateCfg.Rubric = override
				slog.Info("using per-agent rubric from bundle", "component", "startup", "agent", name)
			}
			gateCfgs[name] = agentGateCfg
		}

		srv, err := agent.Serve(ag, sessions, taskMem)
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "a2a serve: %v", err)
		}
		servers = append(servers, srv)

		client, err := srv.Client()
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "a2a client: %v", err)
		}
		clientMap[name] = client
		modelMap[name] = m
		// tools listed here is the definitive record of what the agent can call —
		// when a worker "didn't use its tool", check this line first.
		slog.Info("agent serving over A2A", "component", "startup", "agent", name, "url", srv.Card.SupportedInterfaces[0].URL, "tools", ac.Tools)
	}
	return clientMap, modelMap, servers, judgeFactory, gateCfgs, nil
}

func fmtErr(agentName, format string, args ...any) error {
	return fmt.Errorf("agent %q: "+format, append([]any{agentName}, args...)...)
}
