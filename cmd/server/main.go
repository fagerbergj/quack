// Command server is Quack's entrypoint: it loads config, builds the inference
// model, orchestrator, and stores, and serves the REST + MCP API plus the
// embedded SPA.
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/server"
	mcpserver "github.com/fagerbergj/quack/internal/server/mcp"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/store"
)

//go:embed all:web/dist
var webDist embed.FS

const systemPrompt = "You are Quack, a helpful local assistant. Answer the user concisely and helpfully."

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

	orch, err := orchestrator.New(llm, st.Sessions, systemPrompt)
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
