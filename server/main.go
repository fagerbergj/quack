package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/jasonfagerberg/agent-researcher/server/api/rest"
)

type mockChatRepository struct{}

func (r *mockChatRepository) Create(ctx context.Context, session *rest.ChatSession) error {
	return nil
}

func (r *mockChatRepository) Get(ctx context.Context, id string) (*rest.ChatSession, error) {
	return &rest.ChatSession{
		ID:           id,
		SystemPrompt: "",
		CreatedAt:    "2024-01-01T00:00:00Z",
		UpdatedAt:    "2024-01-01T00:00:00Z",
	}, nil
}

func (r *mockChatRepository) List(ctx context.Context) ([]rest.ChatSession, error) {
	return []rest.ChatSession{}, nil
}

func (r *mockChatRepository) Delete(ctx context.Context, id string) error {
	return nil
}

type mockMessageRepository struct{}

func (r *mockMessageRepository) Create(ctx context.Context, message *rest.Message) error {
	return nil
}

func (r *mockMessageRepository) ListByChat(ctx context.Context, chatID string) ([]rest.Message, error) {
	return []rest.Message{}, nil
}

type mockLLMService struct{}

func (s *mockLLMService) Generate(ctx context.Context, prompt string) (string, error) {
	return "Mock response for: " + prompt, nil
}

func (s *mockLLMService) StreamGenerate(ctx context.Context, prompt string) (<-chan string, <-chan error) {
	out := make(chan string)
	errs := make(chan error, 1)
	go func() {
		out <- "Mock response for: " + prompt
		close(out)
		close(errs)
	}()
	return out, errs
}

func main() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	chatRepo := &mockChatRepository{}
	messageRepo := &mockMessageRepository{}
	llmService := &mockLLMService{}

	researcherService := rest.NewResearchService(llmService)
	chatService := rest.NewChatService(chatRepo, messageRepo)

	restHandler := rest.NewHandler(researcherService, chatService)

	restHandler.RegisterRoutes(r)

	r.HandleFunc("/api/v1/research/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mcpConfig := map[string]interface{}{
				"name":        "agent-researcher",
				"version":     "0.1.0",
				"instructions": "Research agent for deep web research using LLMs and search tools",
				"tools": []map[string]interface{}{
					{
						"name":        "web_search",
						"description": "Search the web for information on a topic",
						"inputSchema": map[string]interface{}{
							"type":       "object",
							"properties": map[string]interface{}{
								"query": map[string]interface{}{
									"type":        "string",
									"description": "Search query",
								},
								"k": map[string]interface{}{
									"type":        "integer",
									"default":     5,
									"description": "Number of results to return",
								},
							},
							"required": []string{"query"},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mcpConfig)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
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
