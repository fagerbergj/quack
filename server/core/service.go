package port

import "context"

// ResearcherService defines the service layer interface for research operations
type ResearcherService interface {
	// Research performs research on a given prompt
	Research(ctx context.Context, prompt string) (string, error)
	
	// StreamResearch performs research and streams responses
	StreamResearch(ctx context.Context, prompt string) (<-chan string, <-chan error)
}

// ChatService defines the service layer interface for chat operations
type ChatService interface {
	// CreateChat creates a new chat session
	CreateChat(ctx context.Context, systemPrompt string) (*ChatSession, error)
	
	// GetChat retrieves a chat session by ID
	GetChat(ctx context.Context, id string) (*ChatSession, error)
	
	// ListChats retrieves all chat sessions
	ListChats(ctx context.Context) ([]ChatSession, error)
	
	// DeleteChat deletes a chat session
	DeleteChat(ctx context.Context, id string) error
	
	// AddMessage adds a message to a chat
	AddMessage(ctx context.Context, chatID string, role string, content string) (*Message, error)
	
	// GetMessages retrieves messages for a chat
	GetMessages(ctx context.Context, chatID string) ([]Message, error)
}
