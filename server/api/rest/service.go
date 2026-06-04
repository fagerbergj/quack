package rest

import "context"

type LLMService interface {
	Generate(ctx context.Context, prompt string) (string, error)
	StreamGenerate(ctx context.Context, prompt string) (<-chan string, <-chan error)
}

type ChatRepository interface {
	Create(ctx context.Context, session *ChatSession) error
	Get(ctx context.Context, id string) (*ChatSession, error)
	List(ctx context.Context) ([]ChatSession, error)
	Delete(ctx context.Context, id string) error
}

type MessageRepository interface {
	Create(ctx context.Context, message *Message) error
	ListByChat(ctx context.Context, chatID string) ([]Message, error)
}

type ResearcherService interface {
	Research(ctx context.Context, prompt string) (string, error)
	StreamResearch(ctx context.Context, prompt string) (<-chan string, <-chan error)
}

type ChatService interface {
	CreateChat(ctx context.Context, systemPrompt *string) (*ChatSession, error)
	GetChat(ctx context.Context, id string) (*ChatSession, error)
	ListChats(ctx context.Context) ([]ChatSession, error)
	DeleteChat(ctx context.Context, id string) error
	AddMessage(ctx context.Context, chatID string, role string, content string) (*Message, error)
	GetMessages(ctx context.Context, chatID string) ([]Message, error)
}

type ChatSession struct {
	ID           string
	SystemPrompt string
	CreatedAt    string
	UpdatedAt    string
}

type Message struct {
	ID        string
	ChatID    string
	Role      string
	Content   string
	CreatedAt string
}

func NewResearchService(llm LLMService) ResearcherService {
	return &researchServiceImpl{
		llm: llm,
	}
}

type researchServiceImpl struct {
	llm LLMService
}

func (s *researchServiceImpl) Research(ctx context.Context, prompt string) (string, error) {
	return s.llm.Generate(ctx, prompt)
}

func (s *researchServiceImpl) StreamResearch(ctx context.Context, prompt string) (<-chan string, <-chan error) {
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

func NewChatService(chatRepo ChatRepository, messageRepo MessageRepository) ChatService {
	return &chatServiceImpl{
		chatRepo:    chatRepo,
		messageRepo: messageRepo,
	}
}

type chatServiceImpl struct {
	chatRepo    ChatRepository
	messageRepo MessageRepository
}

func (s *chatServiceImpl) CreateChat(ctx context.Context, systemPrompt *string) (*ChatSession, error) {
	session := &ChatSession{
		ID:           "temp-id-placeholder",
		CreatedAt:    "2024-01-01T00:00:00Z",
		UpdatedAt:    "2024-01-01T00:00:00Z",
	}
	if systemPrompt != nil {
		session.SystemPrompt = *systemPrompt
	}

	if err := s.chatRepo.Create(ctx, session); err != nil {
		return nil, err
	}

	return session, nil
}

func (s *chatServiceImpl) GetChat(ctx context.Context, id string) (*ChatSession, error) {
	return s.chatRepo.Get(ctx, id)
}

func (s *chatServiceImpl) ListChats(ctx context.Context) ([]ChatSession, error) {
	return s.chatRepo.List(ctx)
}

func (s *chatServiceImpl) DeleteChat(ctx context.Context, id string) error {
	return s.chatRepo.Delete(ctx, id)
}

func (s *chatServiceImpl) AddMessage(ctx context.Context, chatID string, role string, content string) (*Message, error) {
	message := &Message{
		ID:        "temp-id-placeholder",
		ChatID:    chatID,
		Role:      role,
		Content:   content,
		CreatedAt: "2024-01-01T00:00:00Z",
	}

	if err := s.messageRepo.Create(ctx, message); err != nil {
		return nil, err
	}

	session, err := s.chatRepo.Get(ctx, chatID)
	if err != nil {
		return nil, err
	}
	session.UpdatedAt = "2024-01-01T00:00:00Z"
	if err := s.chatRepo.Create(ctx, session); err != nil {
		return nil, err
	}

	return message, nil
}

func (s *chatServiceImpl) GetMessages(ctx context.Context, chatID string) ([]Message, error) {
	return s.messageRepo.ListByChat(ctx, chatID)
}
