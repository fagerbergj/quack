package rest

import (
	"context"
	"testing"

	"github.com/jasonfagerberg/agent-researcher/server/core"
)

// TestHealthCheck tests the health check endpoint
func TestHealthCheck(t *testing.T) {
	// TODO: Implement proper test with httptest
}

// TestResearchEndpoint tests the research endpoint
func TestResearchEndpoint(t *testing.T) {
	// TODO: Implement proper test with httptest
}

// TestChatEndpoints tests the chat endpoints
func TestChatEndpoints(t *testing.T) {
	// TODO: Implement proper test with httptest
}

// TestSendMessageEndpoint tests the send message endpoint
func TestSendMessageEndpoint(t *testing.T) {
	// TODO: Implement proper test with httptest
}

// Mock interfaces for testing
type mockResearchService struct{}

func (m *mockResearchService) Research(ctx context.Context, prompt string) (string, error) {
	return "Test response", nil
}

func (m *mockResearchService) StreamResearch(ctx context.Context, prompt string) (<-chan string, <-chan error) {
	out := make(chan string)
	errs := make(chan error, 1)
	go func() {
		out <- "Test response"
		close(out)
		close(errs)
	}()
	return out, errs
}

type mockChatService struct{}

func (m *mockChatService) CreateChat(ctx context.Context, systemPrompt string) (*core.ChatSession, error) {
	return &core.ChatSession{
		ID:           "test-id",
		SystemPrompt: systemPrompt,
		CreatedAt:    "2024-01-01T00:00:00Z",
		UpdatedAt:    "2024-01-01T00:00:00Z",
	}, nil
}

func (m *mockChatService) GetChat(ctx context.Context, id string) (*core.ChatSession, error) {
	return &core.ChatSession{
		ID:           id,
		SystemPrompt: "",
		CreatedAt:    "2024-01-01T00:00:00Z",
		UpdatedAt:    "2024-01-01T00:00:00Z",
	}, nil
}

func (m *mockChatService) ListChats(ctx context.Context) ([]core.ChatSession, error) {
	return []core.ChatSession{}, nil
}

func (m *mockChatService) DeleteChat(ctx context.Context, id string) error {
	return nil
}

func (m *mockChatService) AddMessage(ctx context.Context, chatID string, role string, content string) (*core.Message, error) {
	return &core.Message{
		ID:        "msg-id",
		ChatID:    chatID,
		Role:      role,
		Content:   content,
		CreatedAt: "2024-01-01T00:00:00Z",
	}, nil
}

func (m *mockChatService) GetMessages(ctx context.Context, chatID string) ([]core.Message, error) {
	return []core.Message{}, nil
}
