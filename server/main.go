package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/jasonfagerberg/agent-researcher/server/api/mcp"
	"github.com/jasonfagerberg/agent-researcher/server/api/rest"
	"github.com/jasonfagerberg/agent-researcher/server/core"
)

func main() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// TODO: Initialize repositories (SQLite for now)
	chatRepo := &mockChatRepository{}
	messageRepo := &mockMessageRepository{}

	// TODO: Initialize LLM service
	llmService := &mockLLMService{}

	// Initialize services with hexagonal architecture
	researcherService := core.NewResearchService(llmService)
	chatService := core.NewChatService(chatRepo, messageRepo)

	// Initialize API handlers
	restHandler := rest.NewHandler(researcherService, chatService)
	mcpHandler := mcp.NewHandler(chatService)

	// Register REST routes
	restHandler.RegisterRoutes(r)

	// Register MCP routes
	mux := http.NewServeMux()
	mux.Handle("/", r)
	mcpHandler.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		log.Println("Starting server on :8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	log.Println("Server stopped")
}

// Mock implementations for now
type mockChatRepository struct{}

func (r *mockChatRepository) Create(ctx context.Context, session *core.ChatSession) error {
	return nil
}

func (r *mockChatRepository) Get(ctx context.Context, id string) (*core.ChatSession, error) {
	return &core.ChatSession{
		ID:           id,
		SystemPrompt: "",
		CreatedAt:    "2024-01-01T00:00:00Z",
		UpdatedAt:    "2024-01-01T00:00:00Z",
	}, nil
}

func (r *mockChatRepository) List(ctx context.Context) ([]core.ChatSession, error) {
	return []core.ChatSession{}, nil
}

func (r *mockChatRepository) Delete(ctx context.Context, id string) error {
	return nil
}

type mockMessageRepository struct{}

func (r *mockMessageRepository) Create(ctx context.Context, message *core.Message) error {
	return nil
}

func (r *mockMessageRepository) ListByChat(ctx context.Context, chatID string) ([]core.Message, error) {
	return []core.Message{}, nil
}

type mockLLMService struct{}

func (s *mockLLMService) Generate(ctx context.Context, prompt string) (string, error) {
	return "This is a mock response. Replace with actual LLM integration.", nil
}
