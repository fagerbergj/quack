package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jasonfagerberg/agent-researcher/server/core"
)

// TestMCPConfig tests the MCP configuration endpoint
func TestMCPConfig(t *testing.T) {
	chatService := &mockChatService{}
	handler := NewHandler(chatService)

	req := httptest.NewRequest("GET", "/api/v1/research/mcp", nil)
	w := httptest.NewRecorder()

	handler.handleMCPGet(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("Expected JSON response, got %s", w.Body.String())
	}

	if result["name"] != "agent-researcher" {
		t.Errorf("Expected name 'agent-researcher', got '%v'", result["name"])
	}

	if result["version"] != "0.1.0" {
		t.Errorf("Expected version '0.1.0', got '%v'", result["version"])
	}

	if tools, ok := result["tools"].([]interface{}); ok {
		if len(tools) != 3 {
			t.Errorf("Expected 3 tools, got %d", len(tools))
		}
	} else {
		t.Fatal("Expected tools array")
	}
}

// TestMCPHandler tests the MCP handler with wrong method
func TestMCPHandler(t *testing.T) {
	chatService := &mockChatService{}
	handler := NewHandler(chatService)

	req := httptest.NewRequest("POST", "/api/v1/research/mcp", nil)
	w := httptest.NewRecorder()

	handler.handleMCP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

// Mock implementations
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
