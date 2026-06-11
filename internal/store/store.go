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

// ChatTurn is one user→assistant exchange. Its ID is the response_id exposed
// in the REST API. Sequence is 0-based insertion order within the chat.
type ChatTurn struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	ChatID    string    `gorm:"index" json:"chat_id"`
	Seq       int       `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
}

// DagPlan stores the JSON-encoded plan for a chat turn so the DAG can be
// re-displayed on page reload. TurnID links it to the ChatTurn that produced it.
type DagPlan struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	ChatID    string    `gorm:"index" json:"chat_id"`
	TurnID    string    `gorm:"index" json:"turn_id"`
	PlanJSON  string    `json:"plan_json"`
	CreatedAt time.Time `json:"created_at"`
}

// DagNode stores the execution state of one DAG node.
type DagNode struct {
	NodeID           string     `gorm:"primaryKey;column:node_id" json:"node_id"`
	PlanID           string     `gorm:"primaryKey;column:plan_id" json:"plan_id"`
	Status           string     `json:"status"`
	OutputPreview    string     `json:"output_preview"`
	Error            string     `json:"error"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	Model            string     `json:"model"`
	PromptTokens     int32      `json:"prompt_tokens"`
	CompletionTokens int32      `json:"completion_tokens"`
	TotalTokens      int32      `json:"total_tokens"`
	FinishReason     string     `json:"finish_reason"`
	DurationMs       int64      `json:"duration_ms"`
}

// TurnContent is the fully-joined view of one turn used to build API responses.
type TurnContent struct {
	ID        string
	CreatedAt time.Time
	UserText  string
	AsstText  string
	AsstThink string
	Plan      *DagPlan
	Nodes     []DagNode
}

// Store wraps the relational DB (chat metadata) and the ADK session service.
type Store struct {
	db       *gorm.DB
	Sessions session.Service
}

// Open connects to Postgres, runs migrations for both the app table and the ADK
// session/event tables, and returns the store.
func Open(dsn string) (*Store, error) {
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
	if err := db.AutoMigrate(&Chat{}, &ChatTurn{}, &DagPlan{}, &DagNode{}); err != nil {
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

// SaveTurn persists a new turn at the next available sequence position.
func (s *Store) SaveTurn(ctx context.Context, chatID, turnID string) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&ChatTurn{}).Where("chat_id = ?", chatID).Count(&count).Error; err != nil {
		return err
	}
	t := &ChatTurn{ID: turnID, ChatID: chatID, Seq: int(count), CreatedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Create(t).Error
}

// ListTurns returns all turns for a chat ordered by sequence.
func (s *Store) ListTurns(ctx context.Context, chatID string) ([]ChatTurn, error) {
	var turns []ChatTurn
	err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Order("seq asc").Find(&turns).Error
	return turns, err
}

// GetTurn returns one turn by ID, or (nil, nil) if not found.
func (s *Store) GetTurn(ctx context.Context, chatID, turnID string) (*ChatTurn, error) {
	var t ChatTurn
	err := s.db.WithContext(ctx).Where("id = ? AND chat_id = ?", turnID, chatID).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &t, err
}

// SaveDagPlan persists a DAG plan linked to the given turn.
func (s *Store) SaveDagPlan(ctx context.Context, chatID, planID, turnID, planJSON string) error {
	now := time.Now().UTC()
	p := &DagPlan{ID: planID, ChatID: chatID, TurnID: turnID, PlanJSON: planJSON, CreatedAt: now}
	return s.db.WithContext(ctx).Create(p).Error
}

// UpsertDagNode creates or updates a DAG node's execution state.
func (s *Store) UpsertDagNode(ctx context.Context, node DagNode) error {
	return s.db.WithContext(ctx).Save(&node).Error
}

// GetDagNodes returns all nodes for a plan.
func (s *Store) GetDagNodes(ctx context.Context, planID string) ([]DagNode, error) {
	var nodes []DagNode
	err := s.db.WithContext(ctx).Where("plan_id = ?", planID).Find(&nodes).Error
	return nodes, err
}

// GetTurnsWithContent returns fully-joined turn data for a chat: ADK event
// text grouped by turn, with the associated DAG plan and nodes when present.
// Turns are matched to ADK event groups by sequence order.
func (s *Store) GetTurnsWithContent(ctx context.Context, appName, userID, chatID string) ([]TurnContent, error) {
	turns, err := s.ListTurns(ctx, chatID)
	if err != nil {
		return nil, err
	}

	// Group ADK events into per-turn buckets separated by user-authored events.
	type group struct{ userText, asstText, asstThink string }
	var groups []group
	var cur *group

	resp, err := s.Sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: chatID})
	if err == nil && resp != nil {
		for ev := range resp.Session.Events().All() {
			if ev == nil || ev.Content == nil {
				continue
			}
			if ev.Author == "user" {
				groups = append(groups, group{})
				cur = &groups[len(groups)-1]
				for _, p := range ev.Content.Parts {
					if p != nil && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil {
						cur.userText += p.Text
					}
				}
			} else if cur != nil {
				for _, p := range ev.Content.Parts {
					if p == nil || p.FunctionCall != nil || p.FunctionResponse != nil {
						continue
					}
					if p.Thought {
						cur.asstThink += p.Text
					} else {
						cur.asstText += p.Text
					}
				}
			}
		}
	}

	// Index DAG plans by turn ID.
	var plans []DagPlan
	_ = s.db.WithContext(ctx).Where("chat_id = ?", chatID).Find(&plans).Error
	planByTurn := make(map[string]*DagPlan, len(plans))
	for i := range plans {
		planByTurn[plans[i].TurnID] = &plans[i]
	}

	result := make([]TurnContent, len(turns))
	for i, t := range turns {
		tc := TurnContent{ID: t.ID, CreatedAt: t.CreatedAt}
		if i < len(groups) {
			tc.UserText = groups[i].userText
			tc.AsstText = groups[i].asstText
			tc.AsstThink = groups[i].asstThink
		}
		if plan := planByTurn[t.ID]; plan != nil {
			tc.Plan = plan
			tc.Nodes, _ = s.GetDagNodes(ctx, plan.ID)
		}
		result[i] = tc
	}
	return result, nil
}

// GetTurnWithContent returns the fully-joined content for a single turn.
func (s *Store) GetTurnWithContent(ctx context.Context, appName, userID, chatID, turnID string) (*TurnContent, error) {
	turns, err := s.GetTurnsWithContent(ctx, appName, userID, chatID)
	if err != nil {
		return nil, err
	}
	for i := range turns {
		if turns[i].ID == turnID {
			return &turns[i], nil
		}
	}
	return nil, nil
}
