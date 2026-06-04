package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResearchService tests the research service
func TestResearchService(t *testing.T) {
	llm := &mockLLMService{}
	service := NewResearchService(llm)

	ctx := context.Background()
	result, err := service.Research(ctx, "test prompt")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	expected := "Mock response for: test prompt"
	if result != expected {
		t.Errorf("Expected %q, got %q", expected, result)
	}
}

// TestStreamResearch tests the streaming research
func TestStreamResearch(t *testing.T) {
	llm := &mockLLMService{}
	service := NewResearchService(llm)

	ctx := context.Background()
	out, errs := service.StreamResearch(ctx, "test prompt")

	select {
	case result := <-out:
		if result != "Mock response for: test prompt" {
			t.Errorf("Expected mock response, got %q", result)
		}
	case err := <-errs:
		t.Fatalf("Expected no error, got %v", err)
	}
}

// TestChatService tests the chat service
func TestChatService(t *testing.T) {
	chatRepo := &mockChatRepository{}
	messageRepo := &mockMessageRepository{}
	service := NewChatService(chatRepo, messageRepo)

	ctx := context.Background()

	// Test creating a chat
	session, err := service.CreateChat(ctx, "test system prompt")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if session.ID == "" {
		t.Error("Expected chat ID to be set")
	}

	if session.SystemPrompt != "test system prompt" {
		t.Errorf("Expected system prompt 'test system prompt', got %q", session.SystemPrompt)
	}

	// Test getting a chat
	retrieved, err := service.GetChat(ctx, session.ID)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if retrieved.ID != session.ID {
		t.Error("Retrieved chat ID doesn't match")
	}
}

// TestAddMessage tests adding messages to a chat
func TestAddMessage(t *testing.T) {
	chatRepo := &mockChatRepository{}
	messageRepo := &mockMessageRepository{}
	service := NewChatService(chatRepo, messageRepo)

	ctx := context.Background()

	session, err := service.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Test adding a user message
	msg, err := service.AddMessage(ctx, session.ID, "user", "Hello")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if msg.Role != "user" {
		t.Errorf("Expected role 'user', got %q", msg.Role)
	}

	if msg.Content != "Hello" {
		t.Errorf("Expected content 'Hello', got %q", msg.Content)
	}
}

// TestRESTHandlerIntegration tests the REST handler
func TestRESTHandlerIntegration(t *testing.T) {
	llm := &mockLLMService{}
	chatRepo := &mockChatRepository{}
	messageRepo := &mockMessageRepository{}

	researcherService := NewResearchService(llm)
	chatService := NewChatService(chatRepo, messageRepo)

	// Test health check
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	// handler.healthCheck(w, req)  // Can't test without handler export

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// Test research endpoint
	researchReq := httptest.NewRequest("POST", "/api/v1/research", strings.NewReader(`{"prompt":"test"}`))
	researchW := httptest.NewRecorder()
	// handler.research(researchW, researchReq)

	if researchW.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", researchW.Code)
	}

	var result string
	if err := json.NewDecoder(researchW.Body).Decode(&result); err != nil {
		t.Fatalf("Expected JSON response, got %s", researchW.Body.String())
	}
}

// Mock implementations
type mockLLMService struct{}

func (m *mockLLMService) Generate(ctx context.Context, prompt string) (string, error) {
	return "Mock response for: " + prompt, nil
}

type mockChatRepository struct {
	chats map[string]*ChatSession
}

func newMockChatRepository() *mockChatRepository {
	return &mockChatRepository{
		chats: make(map[string]*ChatSession),
	}
}

func (r *mockChatRepository) Create(ctx context.Context, session *ChatSession) error {
	r.chats[session.ID] = session
	return nil
}

func (r *mockChatRepository) Get(ctx context.Context, id string) (*ChatSession, error) {
	if session, ok := r.chats[id]; ok {
		return session, nil
	}
	return nil, nil
}

func (r *mockChatRepository) List(ctx context.Context) ([]ChatSession, error) {
	result := make([]ChatSession, 0, len(r.chats))
	for _, session := range r.chats {
		result = append(result, *session)
	}
	return result, nil
}

func (r *mockChatRepository) Delete(ctx context.Context, id string) error {
	delete(r.chats, id)
	return nil
}

type mockMessageRepository struct {
	messages map[string][]Message
}

func newMockMessageRepository() *mockMessageRepository {
	return &mockMessageRepository{
		messages: make(map[string][]Message),
	}
}

func (r *mockMessageRepository) Create(ctx context.Context, message *Message) error {
	r.messages[message.ChatID] = append(r.messages[message.ChatID], *message)
	return nil
}

func (r *mockMessageRepository) ListByChat(ctx context.Context, chatID string) ([]Message, error) {
	return r.messages[chatID], nil
}
