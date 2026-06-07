// Package store is Quack's persistence layer. The ADK database SessionService
// (Postgres) is the source of truth for conversation events; a thin `chats`
// table holds the REST resource surface. A chat's ID is also its ADK session ID,
// so chat history is derived from the session's events (no duplicate table).
package store

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/session"
	"google.golang.org/adk/session/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Chat is the app-level chat record. Its ID doubles as the ADK session ID.
type Chat struct {
	ID           string    `gorm:"primaryKey" json:"id"`
	Title        string    `json:"title"`
	SystemPrompt string    `json:"system_prompt"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Message is a chat message projected from an ADK session event.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Store wraps the relational DB (chat metadata) and the ADK session service.
type Store struct {
	db       *gorm.DB
	Sessions session.Service
}

// Open connects to Postgres, runs migrations for both the app table and the ADK
// session/event tables, and returns the store.
func Open(dsn string) (*Store, error) {
	// Keep real errors and slow-query warnings, but drop the "record not found"
	// spam: ADK probes app_states/user_states (which Quack never writes) and looks
	// up sessions before creating them, so those misses are normal, not problems.
	gormCfg := &gorm.Config{Logger: logger.New(
		log.New(os.Stdout, "", log.LstdFlags),
		logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
		},
	)}
	db, err := gorm.Open(postgres.Open(dsn), gormCfg)
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&Chat{}); err != nil {
		return nil, err
	}
	sessions, err := database.NewSessionService(postgres.Open(dsn), gormCfg)
	if err != nil {
		return nil, err
	}
	if err := database.AutoMigrate(sessions); err != nil {
		return nil, err
	}
	return &Store{db: db, Sessions: sessions}, nil
}

// CreateChat inserts a new chat and returns it.
func (s *Store) CreateChat(ctx context.Context, systemPrompt string) (*Chat, error) {
	now := time.Now().UTC()
	c := &Chat{ID: uuid.NewString(), SystemPrompt: systemPrompt, CreatedAt: now, UpdatedAt: now}
	if err := s.db.WithContext(ctx).Create(c).Error; err != nil {
		return nil, err
	}
	return c, nil
}

// ListChats returns all chats, most-recently-updated first.
func (s *Store) ListChats(ctx context.Context) ([]Chat, error) {
	var chats []Chat
	if err := s.db.WithContext(ctx).Order("updated_at desc").Find(&chats).Error; err != nil {
		return nil, err
	}
	return chats, nil
}

// GetChat returns one chat, or (nil, nil) if it does not exist.
func (s *Store) GetChat(ctx context.Context, id string) (*Chat, error) {
	var c Chat
	err := s.db.WithContext(ctx).First(&c, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// DeleteChat removes a chat row. (The ADK session events are left to ADK.)
func (s *Store) DeleteChat(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Delete(&Chat{}, "id = ?", id).Error
}

// Touch bumps a chat's updated_at to now.
func (s *Store) Touch(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", id).Update("updated_at", time.Now().UTC()).Error
}

// UpdateTitle sets the human-readable title for a chat.
func (s *Store) UpdateTitle(ctx context.Context, id, title string) error {
	return s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", id).Update("title", title).Error
}

// Messages projects a chat's history from its ADK session events. Role is
// derived from the event author ("user" vs the agent). If the session does not
// exist yet (chat created but no message sent), it returns no messages.
func (s *Store) Messages(ctx context.Context, appName, userID, chatID string) ([]Message, error) {
	resp, err := s.Sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: chatID})
	if err != nil || resp == nil {
		// Session not created yet (no turns) — treat as empty history.
		return nil, nil
	}
	var msgs []Message
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		var text string
		for _, p := range ev.Content.Parts {
			// Project only the answer: skip reasoning (Thought) and tool
			// call/response parts. Those stream live but aren't persisted into
			// chat history.
			if p == nil || p.Thought || p.FunctionCall != nil || p.FunctionResponse != nil {
				continue
			}
			text += p.Text
		}
		if text == "" {
			continue
		}
		role := "assistant"
		if ev.Author == "user" {
			role = "user"
		}
		msgs = append(msgs, Message{Role: role, Content: text})
	}
	return msgs, nil
}
