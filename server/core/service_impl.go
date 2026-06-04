package core

import (
	"github.com/jasonfagerberg/agent-researcher/server/core/port"
)

// ResearchServiceImpl is the core business logic for research
type ResearchServiceImpl struct {
	llm port.LLMService
}

// NewResearchService creates a new research service
func NewResearchService(llm port.LLMService) port.ResearcherService {
	return &ResearchServiceImpl{
		llm: llm,
	}
}

// Research performs research on a given prompt
func (s *ResearchServiceImpl) Research(prompt string) (string, error) {
	return s.llm.Generate(prompt)
}

// StreamResearch streams research responses
func (s *ResearchServiceImpl) StreamResearch(prompt string) (<-chan string, <-chan error) {
	out := make(chan string)
	errs := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errs)

		result, err := s.llm.Generate(prompt)
		if err != nil {
			errs <- err
			return
		}

		out <- result
	}()

	return out, errs
}

// ChatServiceImpl is the core business logic for chat operations
type ChatServiceImpl struct {
	chatRepo    port.ChatRepository
	messageRepo port.MessageRepository
}

// NewChatService creates a new chat service
func NewChatService(chatRepo port.ChatRepository, messageRepo port.MessageRepository) port.ChatService {
	return &ChatServiceImpl{
		chatRepo:    chatRepo,
		messageRepo: messageRepo,
	}
}

// CreateChat creates a new chat session
func (s *ChatServiceImpl) CreateChat(systemPrompt string) (*port.ChatSession, error) {
	session := &port.ChatSession{
		ID:           generateID(),
		SystemPrompt: systemPrompt,
		CreatedAt:    "2024-01-01T00:00:00Z",
		UpdatedAt:    "2024-01-01T00:00:00Z",
	}

	if err := s.chatRepo.Create(session); err != nil {
		return nil, err
	}

	return session, nil
}

// GetChat retrieves a chat session by ID
func (s *ChatServiceImpl) GetChat(id string) (*port.ChatSession, error) {
	return s.chatRepo.Get(id)
}

// ListChats retrieves all chat sessions
func (s *ChatServiceImpl) ListChats() ([]port.ChatSession, error) {
	return s.chatRepo.List()
}

// DeleteChat deletes a chat session
func (s *ChatServiceImpl) DeleteChat(id string) error {
	return s.chatRepo.Delete(id)
}

// AddMessage adds a message to a chat
func (s *ChatServiceImpl) AddMessage(chatID string, role string, content string) (*port.Message, error) {
	message := &port.Message{
		ID:        generateID(),
		ChatID:    chatID,
		Role:      role,
		Content:   content,
		CreatedAt: "2024-01-01T00:00:00Z",
	}

	if err := s.messageRepo.Create(message); err != nil {
		return nil, err
	}

	// Update chat timestamp
	session, err := s.chatRepo.Get(chatID)
	if err != nil {
		return nil, err
	}
	session.UpdatedAt = "2024-01-01T00:00:00Z"
	if err := s.chatRepo.Create(session); err != nil {
		return nil, err
	}

	return message, nil
}

// GetMessages retrieves messages for a chat
func (s *ChatServiceImpl) GetMessages(chatID string) ([]port.Message, error) {
	return s.messageRepo.ListByChat(chatID)
}

// generateID generates a UUID-like identifier
func generateID() string {
	return "temp-id-placeholder"
}
