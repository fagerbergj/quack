// Command server is Quack's entrypoint: it loads config, builds the inference
// model, orchestrator, and stores, and serves the REST + MCP API plus the
// embedded SPA.
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/skilltoolset"
	"google.golang.org/adk/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/orchestrator"
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
	cfgPath := os.Getenv("QUACK_CONFIG")
	if cfgPath == "" {
		cfgPath = "config/quack.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.Stores.Relational.URL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	if n, err := st.FailStaleDagNodes(context.Background()); err != nil {
		log.Printf("store: fail stale dag nodes: %v", err)
	} else if n > 0 {
		log.Printf("store: marked %d orphaned dag node(s) failed (previous process killed mid-run)", n)
	}

	prov, _ := cfg.Provider(cfg.Orchestrator.Provider)
	llm, err := inference.NewModel(prov, cfg.Orchestrator.Model)
	if err != nil {
		log.Fatalf("inference: %v", err)
	}

	// Load skills once at startup; pass the toolset to every specialist agent so
	// all agents can call load_skill / list_skills / load_skill_resource.
	skillSrc := skill.NewFileSystemSource(os.DirFS("skills/"))
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		log.Fatalf("skills: %v", err)
	}

	// Build each declarative agent, expose it over A2A, and collect a client the
	// DAG executor can dispatch to. Servers run for the process lifetime.
	clientMap, servers, err := buildAgents(cfg, st.Sessions, skillTS)
	if err != nil {
		log.Fatalf("agents: %v", err)
	}
	defer func() {
		for _, s := range servers {
			_ = s.Close()
		}
	}()

	// Build agent info list for the planner (name + description).
	agentInfos := make([]dag.AgentInfo, 0, len(clientMap))
	for name, c := range clientMap {
		agentInfos = append(agentInfos, dag.AgentInfo{Name: name, Description: c.Description()})
	}

	planner := dag.NewPlanner(llm, agentInfos)
	executor := dag.NewExecutor(st.Sessions, clientMap, cfg.Dag.MaxActiveNodes)
	orch := orchestrator.New(st.Sessions, planner, executor)

	spa, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	handler := server.New(server.Options{
		REST: rest.NewHandler(st, orch, llm),
		MCP:  mcpserver.Handler(orch),
		SPA:  spa,
	})

	srv := &http.Server{Addr: cfg.Server.Addr, Handler: handler}
	go func() {
		log.Printf("quack listening on %s", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Println("stopped")
}

// buildAgents loads each configured agent bundle, builds its model and built-in
// tools, exposes it over a co-located A2A server, and returns:
//   - clientMap: agent name → A2A client (for the DAG executor)
//   - servers: A2A server handles (to close on shutdown)
func buildAgents(cfg *config.Config, sessions session.Service, skillTS *skilltoolset.SkillToolset) (map[string]adkagent.Agent, []*agent.A2AServer, error) {
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	urlCache := tools.NewInMemoryURLCache()

	var judgeFactory vetting.JudgeFactory
	var gateCfg vetting.Config
	if cfg.Gates.Enabled() {
		var err error
		if gateCfg, err = vetting.FromConfig(cfg.Gates); err != nil {
			return nil, nil, err
		}
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
		log.Printf("trust gate enabled: deterministic_rounds=%d self_critique_rounds=%d judge=%q judge_rounds=%d threshold=%.2f",
			gateCfg.DeterministicRounds, gateCfg.SelfCritiqueRounds, cfg.Gates.Judge.Model, gateCfg.JudgeRounds, gateCfg.Threshold)
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

		var builtins []tool.Tool
		if len(ac.Tools) > 0 {
			builtins, err = tools.Build(ac.Tools, tools.Deps{
				SearXNG:    cfg.Tools.WebSearch.Backend,
				Crawl4AI:   cfg.Tools.Fetch.RenderBackend,
				Summarizer: m,
				Cache:      urlCache,
			})
			if err != nil {
				return nil, servers, fmtErr(name, "tools: %v", err)
			}
		}

		bundle, err := agent.LoadBundle(ac.Bundle)
		if err != nil {
			return nil, servers, fmtErr(name, "bundle: %v", err)
		}
		ag, err := agent.Build(bundle, m, builtins, []tool.Toolset{skillTS})
		if err != nil {
			return nil, servers, fmtErr(name, "build: %v", err)
		}

		served := ag
		if cfg.Gates.Enabled() {
			agentGateCfg := gateCfg
			if override, err := vetting.LoadBundleRubric(ac.Bundle); err != nil {
				return nil, servers, fmtErr(name, "rubric: %v", err)
			} else if override != "" {
				agentGateCfg.Rubric = override
				log.Printf("agent %q: using per-agent rubric from bundle", name)
			}
			// A tool-less twin of the worker (same model + prompt, no tools) for the
			// finalize write-up: when the worker keeps researching instead of writing,
			// a tool-having re-invoke ignores "stop and write" — a tool-less one can't,
			// so it produces the answer from context in one pass.
			writer, err := agent.Build(bundle, m, nil, nil)
			if err != nil {
				return nil, servers, fmtErr(name, "writer: %v", err)
			}
			if served, err = vetting.NewGatedAgent(ag, writer, judgeFactory, agentGateCfg); err != nil {
				return nil, servers, fmtErr(name, "gate: %v", err)
			}
		}

		srv, err := agent.Serve(served, sessions)
		if err != nil {
			return nil, servers, fmtErr(name, "a2a serve: %v", err)
		}
		servers = append(servers, srv)

		client, err := srv.Client()
		if err != nil {
			return nil, servers, fmtErr(name, "a2a client: %v", err)
		}
		clientMap[name] = client
		log.Printf("agent %q serving over A2A at %s", name, srv.Card.SupportedInterfaces[0].URL)
	}
	return clientMap, servers, nil
}

func fmtErr(agentName, format string, args ...any) error {
	return fmt.Errorf("agent %q: "+format, append([]any{agentName}, args...)...)
}
