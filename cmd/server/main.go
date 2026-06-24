// Command server is Quack's entrypoint: it loads config, builds the inference
// model, orchestrator, and stores, and serves the REST + MCP API plus the
// embedded SPA.
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	adkagent "google.golang.org/adk/agent"
	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/loadmemorytool"
	"google.golang.org/adk/tool/skilltoolset"
	"google.golang.org/adk/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/docstore"
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

func main() {
	setupLogging()

	cfgPath := os.Getenv("QUACK_CONFIG")
	if cfgPath == "" {
		cfgPath = "config/quack.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fatal("config load failed", "err", err)
	}

	sessionStore, ok := cfg.Store(cfg.Session.Store)
	if !ok {
		fatal("session store not found in stores registry", "store", cfg.Session.Store)
	}
	st, err := store.Open(sessionStore.URL)
	if err != nil {
		fatal("store open failed", "err", err)
	}
	if n, err := st.FailStaleDagNodes(context.Background()); err != nil {
		slog.Error("fail stale dag nodes", "component", "store", "err", err)
	} else if n > 0 {
		slog.Info("marked orphaned dag nodes failed (previous process killed mid-run)", "component", "store", "count", n)
	}

	prov, _ := cfg.Provider(cfg.Orchestrator.Provider)
	llm, err := inference.NewModel(prov, cfg.Orchestrator.Model)
	if err != nil {
		fatal("inference model init failed", "err", err)
	}

	// Load skills once at startup; pass the toolset to every specialist agent so
	// all agents can call load_skill / list_skills / load_skill_resource.
	skillSrc := skill.NewFileSystemSource(os.DirFS("skills/"))
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		fatal("skills toolset init failed", "err", err)
	}

	// Semantic memory (M6): a memory tool bound to a vector store (with QDRANT_URL
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
			fatal("task memory init failed", "err", err)
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
				fatal("user memory init failed", "err", err)
			}
			userStore = s
			slog.Info("user memory enabled", "component", "startup", "collection", rm.Collection)
		}
	}

	// Document record store: opened only when an agent binds a doc tool, so an
	// unconfigured deployment never holds an idle connection. A bound doc tool
	// without a configured store is a startup error (misconfiguration).
	var docStore docstore.DocStore
	if cfg.UsesDocStore() {
		sc, ok := cfg.DocRecordStore()
		if !ok || sc.URL == "" {
			fatal("a document tool is bound but its store is not configured", "tool", "create_document")
		}
		if docStore, err = docstore.New(sc.Kind, sc.URL); err != nil {
			fatal("document store init failed", "err", err)
		}
		slog.Info("document store enabled", "component", "startup", "kind", sc.Kind)
	}

	// Full-text index: opened when search_document or create_document is bound and
	// an FTS store is configured (empty url ⇒ skip, like memory's self-disable).
	var docFTS docstore.FTSIndex
	if cfg.UsesFTS() {
		if sc, ok := cfg.FTSStore(); ok && sc.URL != "" {
			if docFTS, err = docstore.NewFTS(sc.Kind, sc.URL, sc.Collection); err != nil {
				fatal("document full-text index init failed", "err", err)
			}
			slog.Info("document full-text index enabled", "component", "startup", "kind", sc.Kind)
		}
	}

	// Vector index for documents: opened when semantic_search_document is bound and
	// its vector store (carrying the embedder) is configured.
	var docVector docstore.VectorIndex
	if cfg.UsesVector() {
		if rv, ok := cfg.DocVectorStore(); ok {
			eprov, ok := cfg.Provider(rv.Embedder.Provider)
			if !ok {
				fatal("document vector embedder provider not found", "provider", rv.Embedder.Provider)
			}
			embedder, eerr := inference.NewEmbedder(eprov, rv.Embedder.Model)
			if eerr != nil {
				fatal("document vector embedder init failed", "err", eerr)
			}
			if docVector, err = docstore.NewVector(rv.Kind, rv.URL, rv.Collection, embedder); err != nil {
				fatal("document vector index init failed", "err", err)
			}
			slog.Info("document vector index enabled", "component", "startup", "collection", rv.Collection)
		}
	}

	// Build each declarative agent, expose it over A2A, and collect a client the
	// DAG executor can dispatch to. Servers run for the process lifetime.
	clientMap, servers, err := buildAgents(cfg, st.Sessions, skillTS, taskStore, docStore, docFTS, docVector)
	if err != nil {
		fatal("agent build failed", "err", err)
	}
	defer func() {
		for _, s := range servers {
			_ = s.Close()
		}
	}()

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
		fatal("orchestrator bundle load failed", "err", err)
	}
	// Load the format-markdown skill frontmatter so the orchestrator's prompt
	// lists it and the model knows to call load_skill("format-markdown") for
	// direct-answer responses.
	fmFm, err := skillSrc.LoadFrontmatter(context.Background(), "format-markdown")
	if err != nil {
		fatal("format-markdown skill load failed", "err", err)
	}
	// plan-work is the DAG-authoring playbook the orchestrator loads before planning.
	planWorkFm, err := skillSrc.LoadFrontmatter(context.Background(), "plan-work")
	if err != nil {
		fatal("plan-work skill load failed", "err", err)
	}
	// When user memory is on, append the orchestrator's memory.md guidance (its
	// "what to remember about the user" section) to its behaviour — gated the same
	// way agent bundles are.
	orchBehaviour := orchBundle.Prompt
	if userStore != nil {
		mem, err := agent.LoadBundleMemory("agents/orchestrator")
		if err != nil {
			fatal("orchestrator memory.md load failed", "err", err)
		}
		if mem != "" {
			orchBehaviour += "\n\n" + mem
		}
	}
	orchSysPrompt := promptbuilder.Orchestrator(rosterSB.String(), []*skill.Frontmatter{fmFm, planWorkFm}, orchBehaviour)

	planner := dag.NewPlanner(agentInfos)
	executor := dag.NewExecutor(st.Sessions, clientMap, mediaAgents, cfg.Dag.MaxActiveNodes)
	orch := orchestrator.New(st.Sessions, llm, orchSysPrompt, planner, executor, skillTS, userStore)

	spa, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		fatal("embed SPA fs failed", "err", err)
	}

	handler := server.New(server.Options{
		REST: rest.NewHandler(st, orch, llm),
		MCP:  mcpserver.Handler(orch),
		SPA:  spa,
	})

	srv := &http.Server{Addr: cfg.Server.Addr, Handler: handler}
	go func() {
		slog.Info("quack listening", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("http serve failed", "err", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	slog.Info("stopped")
}

// setupLogging installs the process-wide slog handler from LOG_LEVEL
// (debug|info|warn|error, default info) and LOG_FORMAT (text|json, default
// text). SetDefault also reroutes any stray stdlib log.* through this handler.
// ponytail: env-driven; add a LevelVar when runtime re-leveling is actually needed.
func setupLogging() {
	// slog.Level implements TextUnmarshaler: "" and unknown values error out,
	// leaving the zero value LevelInfo — our intended default.
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(os.Getenv("LOG_LEVEL")))
	opts := &slog.HandlerOptions{Level: lvl}
	// stdout (not stderr): logs are the program's output for a server; let the
	// container/orchestration layer collect and ship them.
	var h slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}

// fatal logs at Error and exits — slog has no Fatal of its own.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

// buildAgents loads each configured agent bundle, builds its model and built-in
// tools, exposes it over a co-located A2A server, and returns:
//   - clientMap: agent name → A2A client (for the DAG executor)
//   - servers: A2A server handles (to close on shutdown)
func buildAgents(cfg *config.Config, sessions session.Service, skillTS *skilltoolset.SkillToolset, taskStore *memory.Store, docStore docstore.DocStore, docFTS docstore.FTSIndex, docVector docstore.VectorIndex) (map[string]adkagent.Agent, []*agent.A2AServer, error) {
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
			return nil, nil, err
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
				return nil, nil, fmt.Errorf("gates.judge: provider %q not found", cfg.Gates.Judge.Provider)
			}
			judge, err := inference.NewModel(jprov, cfg.Gates.Judge.Model)
			if err != nil {
				return nil, nil, fmt.Errorf("gates.judge: model: %w", err)
			}
			judgeFactory = vetting.NewJudgeFactory(judge, nil)
		}
		slog.Info("trust gate enabled", "component", "startup",
			"deterministic_rounds", gateCfg.DeterministicRounds, "self_critique_rounds", gateCfg.SelfCritiqueRounds,
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
			return nil, nil, fmt.Errorf("compaction: provider %q not found", compCfg.Provider)
		}
		var err error
		if summarizer, err = inference.NewModel(cprov, compCfg.Model); err != nil {
			return nil, nil, fmt.Errorf("compaction: model: %w", err)
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
	var servers []*agent.A2AServer

	for _, name := range names {
		ac := cfg.Agents[name]

		prov, ok := cfg.Provider(ac.Provider)
		if !ok {
			return nil, servers, fmtErr(name, "provider %q not found", ac.Provider)
		}
		m, err := inference.NewModel(prov, ac.Model)
		if err != nil {
			return nil, servers, fmtErr(name, "model: %v", err)
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
				WebSearch:  tools.Backend{Kind: cfg.Tools["web_search"].Kind, URL: cfg.Tools["web_search"].URL},
				Fetch:      tools.Backend{Kind: cfg.Tools["web_fetch"].Kind, URL: cfg.Tools["web_fetch"].URL},
				Summarizer: m,
				Cache:      urlCache,
				DocStore:   docStore,
				FTS:        docFTS,
				Vector:     docVector,
			})
			if err != nil {
				return nil, servers, fmtErr(name, "tools: %v", err)
			}
		}

		bundle, err := agent.LoadBundle(ac.Bundle)
		if err != nil {
			return nil, servers, fmtErr(name, "bundle: %v", err)
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
				return nil, servers, fmtErr(name, "memory.md: %v", err)
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
			return nil, servers, fmtErr(name, "build: %v", err)
		}

		served := ag
		if cfg.Gates.Enabled() && !ac.IsGated() {
			slog.Info("trust gate skipped for agent (gated: false)", "component", "startup", "agent", name)
		}
		if cfg.Gates.Enabled() && ac.IsGated() {
			agentGateCfg := gateCfg
			// An agent participates in task memory iff it has a memory.md (loaded as
			// memGuidance above). Such agents commit on a judge pass even when they
			// staged nothing, so Commit's answer-extraction still runs.
			agentGateCfg.CommitMemory = taskStore != nil && memGuidance != ""
			if override, err := vetting.LoadBundleRubric(ac.Bundle); err != nil {
				return nil, servers, fmtErr(name, "rubric: %v", err)
			} else if override != "" {
				agentGateCfg.Rubric = override
				slog.Info("using per-agent rubric from bundle", "component", "startup", "agent", name)
			}
			// A tool-less twin of the worker (same model + prompt, no tools) for the
			// finalize write-up: when the worker keeps researching instead of writing,
			// a tool-having re-invoke ignores "stop and write" — a tool-less one can't,
			// so it produces the answer from context in one pass.
			writer, err := agent.Build(bundle, m, nil, nil, comp, "")
			if err != nil {
				return nil, servers, fmtErr(name, "writer: %v", err)
			}
			if served, err = vetting.NewGatedAgent(ag, writer, judgeFactory, agentGateCfg); err != nil {
				return nil, servers, fmtErr(name, "gate: %v", err)
			}
		}

		srv, err := agent.Serve(served, sessions, taskMem)
		if err != nil {
			return nil, servers, fmtErr(name, "a2a serve: %v", err)
		}
		servers = append(servers, srv)

		client, err := srv.Client()
		if err != nil {
			return nil, servers, fmtErr(name, "a2a client: %v", err)
		}
		clientMap[name] = client
		slog.Info("agent serving over A2A", "component", "startup", "agent", name, "url", srv.Card.SupportedInterfaces[0].URL)
	}
	return clientMap, servers, nil
}

func fmtErr(agentName, format string, args ...any) error {
	return fmt.Errorf("agent %q: "+format, append([]any{agentName}, args...)...)
}
