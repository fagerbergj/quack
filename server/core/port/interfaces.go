package port

import "context"

// LLMService defines the interface for LLM operations
type LLMService interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// ResearchService defines the interface for research operations
type ResearchService interface {
	Research(ctx context.Context, prompt string) (string, error)
}

// ChatSession represents a chat session
type ChatSession struct {
	ID           string
	SystemPrompt string
	CreatedAt    string
	UpdatedAt    string
}

// ChatRepository defines the interface for chat persistence
type ChatRepository interface {
	Create(ctx context.Context, session *ChatSession) error
	Get(ctx context.Context, id string) (*ChatSession, error)
	List(ctx context.Context) ([]ChatSession, error)
	Delete(ctx context.Context, id string) error
}

// Message represents a chat message
type Message struct {
	ID        string
	ChatID    string
	Role      string // user or assistant
	Content   string
	CreatedAt string
}

// MessageRepository defines the interface for message persistence
type MessageRepository interface {
	Create(ctx context.Context, message *Message) error
	ListByChat(ctx context.Context, chatID string) ([]Message, error)
}
