package core

import (
	"testing"

	"github.com/jasonfagerberg/agent-researcher/server/core/port"
)

// TestResearchService tests the research service
func TestResearchService(t *testing.T) {
	llm := &mockLLMService{}
	service := NewResearchService(llm)

	result, err := service.Research("test prompt")
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

	out, errs := service.StreamResearch("test prompt")

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

	// Test creating a chat
	session, err := service.CreateChat("test system prompt")
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
	retrieved, err := service.GetChat(session.ID)
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

	session, err := service.CreateChat("")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Test adding a user message
	msg, err := service.AddMessage(session.ID, "user", "Hello")
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

// Mock implementations
type mockLLMService struct{}

func (m *mockLLMService) Generate(prompt string) (string, error) {
	return "Mock response for: " + prompt, nil
}

type mockChatRepository struct {
	chats map[string]*port.ChatSession
}

func (r *mockChatRepository) Create(session *port.ChatSession) error {
	if r.chats == nil {
		r.chats = make(map[string]*port.ChatSession)
	}
	r.chats[session.ID] = session
	return nil
}

func (r *mockChatRepository) Get(id string) (*port.ChatSession, error) {
	if r.chats == nil {
		return nil, nil
	}
	return r.chats[id], nil
}

func (r *mockChatRepository) List() ([]port.ChatSession, error) {
	if r.chats == nil {
		return []port.ChatSession{}, nil
	}
	result := make([]port.ChatSession, 0, len(r.chats))
	for _, session := range r.chats {
		result = append(result, *session)
	}
	return result, nil
}

func (r *mockChatRepository) Delete(id string) error {
	delete(r.chats, id)
	return nil
}

type mockMessageRepository struct {
	messages map[string][]port.Message
}

func (r *mockMessageRepository) Create(message *port.Message) error {
	if r.messages == nil {
		r.messages = make(map[string][]port.Message)
	}
	r.messages[message.ChatID] = append(r.messages[message.ChatID], *message)
	return nil
}

func (r *mockMessageRepository) ListByChat(chatID string) ([]port.Message, error) {
	if r.messages == nil {
		return []port.Message{}, nil
	}
	return r.messages[chatID], nil
}
