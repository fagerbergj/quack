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

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/server"
	mcpserver "github.com/fagerbergj/quack/internal/server/mcp"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/tools"
)

//go:embed all:web/dist
var webDist embed.FS

// systemPrompt steers the thin dispatcher: it should delegate to specialist
// agents rather than answer research questions itself.
const systemPrompt = "You are Quack's orchestrator. You coordinate specialist agents. " +
	"When a request needs information from the web — news, facts, current events, looking something up, or research — " +
	"transfer to the web-researcher agent to handle it. " +
	"Handle a message yourself only when it is purely conversational, such as a greeting or a question about Quack itself."

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

	prov, _ := cfg.Provider(cfg.Orchestrator.Provider)
	llm, err := inference.NewModel(prov, cfg.Orchestrator.Model)
	if err != nil {
		log.Fatalf("inference: %v", err)
	}

	// Build each declarative agent, expose it over A2A, and collect a client the
	// orchestrator can delegate to. Servers run for the process lifetime. Agents
	// share the durable session store (namespaced by their own app_id) so their
	// A2A sessions survive restarts.
	clients, servers, err := buildAgents(cfg, st.Sessions)
	if err != nil {
		log.Fatalf("agents: %v", err)
	}
	defer func() {
		for _, s := range servers {
			_ = s.Close()
		}
	}()

	orch, err := orchestrator.New(llm, st.Sessions, systemPrompt, clients)
	if err != nil {
		log.Fatalf("orchestrator: %v", err)
	}

	spa, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	handler := server.New(server.Options{
		REST: rest.NewHandler(st, orch),
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
// tools, exposes it over a co-located A2A server, and returns the A2A clients
// (for the orchestrator to delegate to) alongside the servers (to close on
// shutdown).
func buildAgents(cfg *config.Config, sessions session.Service) ([]adkagent.Agent, []*agent.A2AServer, error) {
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic startup order

	var clients []adkagent.Agent
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

		builtins, err := tools.Build(ac.Tools, tools.Deps{
			SearXNG:    cfg.Tools.WebSearch.Backend,
			Crawl4AI:   cfg.Tools.Fetch.RenderBackend,
			Summarizer: m,
		})
		if err != nil {
			return nil, servers, fmtErr(name, "tools: %v", err)
		}

		bundle, err := agent.LoadBundle(ac.Bundle)
		if err != nil {
			return nil, servers, fmtErr(name, "bundle: %v", err)
		}
		ag, err := agent.Build(bundle, m, builtins)
		if err != nil {
			return nil, servers, fmtErr(name, "build: %v", err)
		}

		srv, err := agent.Serve(ag, sessions)
		if err != nil {
			return nil, servers, fmtErr(name, "a2a serve: %v", err)
		}
		servers = append(servers, srv)

		client, err := srv.Client()
		if err != nil {
			return nil, servers, fmtErr(name, "a2a client: %v", err)
		}
		clients = append(clients, client)
		log.Printf("agent %q serving over A2A at %s", name, srv.Card.SupportedInterfaces[0].URL)
	}
	return clients, servers, nil
}

func fmtErr(agentName, format string, args ...any) error {
	return fmt.Errorf("agent %q: "+format, append([]any{agentName}, args...)...)
}
