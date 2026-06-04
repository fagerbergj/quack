package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jasonfagerberg/agent-researcher/server/core"
)

// TestResearchEndpoint tests the research endpoint
func TestResearchEndpoint(t *testing.T) {
	researcherService := &mockResearchService{}
	chatService := &mockChatService{}
	handler := NewHandler(researcherService, chatService)

	req := httptest.NewRequest("POST", "/api/v1/research", strings.NewReader(`{"prompt":"test"}`))
	w := httptest.NewRecorder()

	handler.research(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var result string
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("Expected JSON response, got %s", w.Body.String())
	}

	if result != "Test response" {
		t.Errorf("Expected 'Test response', got '%s'", result)
	}
}

// TestCreateChatEndpoint tests the create chat endpoint
func TestCreateChatEndpoint(t *testing.T) {
	researcherService := &mockResearchService{}
	chatService := &mockChatService{}
	handler := NewHandler(researcherService, chatService)

	req := httptest.NewRequest("POST", "/api/v1/chats", strings.NewReader(`{"system_prompt":"test"}`))
	w := httptest.NewRecorder()

	handler.createChat(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var result struct {
		ID           string  `json:"id"`
		SystemPrompt *string `json:"system_prompt"`
	}

	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("Expected JSON response, got %s", w.Body.String())
	}

	if result.ID != "test-id" {
		t.Errorf("Expected ID 'test-id', got '%s'", result.ID)
	}

	if result.SystemPrompt == nil || *result.SystemPrompt != "test" {
		t.Errorf("Expected system prompt 'test', got nil or wrong value")
	}
}

// TestSendMessageEndpoint tests the send message endpoint
func TestSendMessageEndpoint(t *testing.T) {
	researcherService := &mockResearchService{}
	chatService := &mockChatService{}
	handler := NewHandler(researcherService, chatService)

	req := httptest.NewRequest("POST", "/api/v1/chats/test-id/messages", strings.NewReader(`{"content":"Hello"}`))
	w := httptest.NewRecorder()

	handler.sendMessage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
}

// TestListChatsEndpoint tests the list chats endpoint
func TestListChatsEndpoint(t *testing.T) {
	researcherService := &mockResearchService{}
	chatService := &mockChatService{}
	handler := NewHandler(researcherService, chatService)

	req := httptest.NewRequest("GET", "/api/v1/chats", nil)
	w := httptest.NewRecorder()

	handler.listChats(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("Expected JSON response, got %s", w.Body.String())
	}
}

// TestHealthCheckEndpoint tests the health check endpoint
func TestHealthCheckEndpoint(t *testing.T) {
	researcherService := &mockResearchService{}
	chatService := &mockChatService{}
	handler := NewHandler(researcherService, chatService)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	handler.healthCheck(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	if w.Body.String() != "ok" {
		t.Errorf("Expected 'ok', got '%s'", w.Body.String())
	}
}

// Mock implementations
type mockResearchService struct{}

func (m *mockResearchService) Research(prompt string) (string, error) {
	return "Test response", nil
}

func (m *mockResearchService) StreamResearch(prompt string) (<-chan string, <-chan error) {
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

func (m *mockChatService) CreateChat(systemPrompt string) (*core.ChatSession, error) {
	return &core.ChatSession{
		ID:           "test-id",
		SystemPrompt: systemPrompt,
		CreatedAt:    "2024-01-01T00:00:00Z",
		UpdatedAt:    "2024-01-01T00:00:00Z",
	}, nil
}

func (m *mockChatService) GetChat(id string) (*core.ChatSession, error) {
	return &core.ChatSession{
		ID:           id,
		SystemPrompt: "",
		CreatedAt:    "2024-01-01T00:00:00Z",
		UpdatedAt:    "2024-01-01T00:00:00Z",
	}, nil
}

func (m *mockChatService) ListChats() ([]core.ChatSession, error) {
	return []core.ChatSession{}, nil
}

func (m *mockChatService) DeleteChat(id string) error {
	return nil
}

func (m *mockChatService) AddMessage(chatID string, role string, content string) (*core.Message, error) {
	return &core.Message{
		ID:        "msg-id",
		ChatID:    chatID,
		Role:      role,
		Content:   content,
		CreatedAt: "2024-01-01T00:00:00Z",
	}, nil
}

func (m *mockChatService) GetMessages(chatID string) ([]core.Message, error) {
	return []core.Message{}, nil
}
