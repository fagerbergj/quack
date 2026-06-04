package port

// LLMService defines the interface for LLM operations
type LLMService interface {
	Generate(prompt string) (string, error)
}

// ResearcherService defines the interface for research operations
type ResearcherService interface {
	// Research performs research on a given prompt
	Research(prompt string) (string, error)
	
	// StreamResearch performs research and streams responses
	StreamResearch(prompt string) (<-chan string, <-chan error)
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
	Create(session *ChatSession) error
	Get(id string) (*ChatSession, error)
	List() ([]ChatSession, error)
	Delete(id string) error
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
	Create(message *Message) error
	ListByChat(chatID string) ([]Message, error)
}

// ChatService defines the interface for chat operations
type ChatService interface {
	// CreateChat creates a new chat session
	CreateChat(systemPrompt string) (*ChatSession, error)
	
	// GetChat retrieves a chat session by ID
	GetChat(id string) (*ChatSession, error)
	
	// ListChats retrieves all chat sessions
	ListChats() ([]ChatSession, error)
	
	// DeleteChat deletes a chat session
	DeleteChat(id string) error
	
	// AddMessage adds a message to a chat
	AddMessage(chatID string, role string, content string) (*Message, error)
	
	// GetMessages retrieves messages for a chat
	GetMessages(chatID string) ([]Message, error)
}
