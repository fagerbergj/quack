package core

import (
	"context"
	"time"

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
func (s *ResearchServiceImpl) Research(ctx context.Context, prompt string) (string, error) {
	return s.llm.Generate(ctx, prompt)
}

// StreamResearch streams research responses
func (s *ResearchServiceImpl) StreamResearch(ctx context.Context, prompt string) (<-chan string, <-chan error) {
	out := make(chan string)
	errs := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errs)

		result, err := s.llm.Generate(ctx, prompt)
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
func (s *ChatServiceImpl) CreateChat(ctx context.Context, systemPrompt string) (*port.ChatSession, error) {
	session := &port.ChatSession{
		ID:           generateID(),
		SystemPrompt: systemPrompt,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.chatRepo.Create(ctx, session); err != nil {
		return nil, err
	}

	return session, nil
}

// GetChat retrieves a chat session by ID
func (s *ChatServiceImpl) GetChat(ctx context.Context, id string) (*port.ChatSession, error) {
	return s.chatRepo.Get(ctx, id)
}

// ListChats retrieves all chat sessions
func (s *ChatServiceImpl) ListChats(ctx context.Context) ([]port.ChatSession, error) {
	return s.chatRepo.List(ctx)
}

// DeleteChat deletes a chat session
func (s *ChatServiceImpl) DeleteChat(ctx context.Context, id string) error {
	return s.chatRepo.Delete(ctx, id)
}

// AddMessage adds a message to a chat
func (s *ChatServiceImpl) AddMessage(ctx context.Context, chatID string, role string, content string) (*port.Message, error) {
	message := &port.Message{
		ID:        generateID(),
		ChatID:    chatID,
		Role:      role,
		Content:   content,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.messageRepo.Create(ctx, message); err != nil {
		return nil, err
	}

	// Update chat timestamp
	session, err := s.chatRepo.Get(ctx, chatID)
	if err != nil {
		return nil, err
	}
	session.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.chatRepo.Create(ctx, session); err != nil {
		return nil, err
	}

	return message, nil
}

// GetMessages retrieves messages for a chat
func (s *ChatServiceImpl) GetMessages(ctx context.Context, chatID string) ([]port.Message, error) {
	return s.messageRepo.ListByChat(ctx, chatID)
}

// generateID generates a UUID-like identifier
func generateID() string {
	return "temp-id-placeholder"
}
