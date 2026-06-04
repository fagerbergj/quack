package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func (m *mockChatService) CreateChat(ctx context.Context, systemPrompt *string) (*ChatSession, error) {
	session := &ChatSession{
		ID:        "test-id",
		CreatedAt: "2024-01-01T00:00:00Z",
		UpdatedAt: "2024-01-01T00:00:00Z",
	}
	if systemPrompt != nil {
		session.SystemPrompt = *systemPrompt
	}
	return session, nil
}

func (m *mockChatService) GetChat(ctx context.Context, id string) (*ChatSession, error) {
	return &ChatSession{
		ID:        id,
		CreatedAt: "2024-01-01T00:00:00Z",
		UpdatedAt: "2024-01-01T00:00:00Z",
	}, nil
}

func (m *mockChatService) ListChats(ctx context.Context) ([]ChatSession, error) {
	return []ChatSession{}, nil
}

func (m *mockChatService) DeleteChat(ctx context.Context, id string) error {
	return nil
}

func (m *mockChatService) AddMessage(ctx context.Context, chatID string, role string, content string) (*Message, error) {
	return &Message{
		ID:        "msg-id",
		ChatID:    chatID,
		Role:      role,
		Content:   content,
		CreatedAt: "2024-01-01T00:00:00Z",
	}, nil
}

func (m *mockChatService) GetMessages(ctx context.Context, chatID string) ([]Message, error) {
	return []Message{}, nil
}

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

	if w.Body.String() != "Test response" {
		t.Errorf("Expected 'Test response', got '%s'", w.Body.String())
	}
}

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

	if w.Body.String() == "" {
		t.Fatal("Expected JSON response")
	}
}

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

	if w.Body.String() == "" {
		t.Fatal("Expected JSON response")
	}
}

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
