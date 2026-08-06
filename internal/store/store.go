// Package store is Quack's persistence layer. A chat's ID is also its ADK session ID,
// so chat history is derived from the session's events (no duplicate table).
package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/fagerbergj/quack/internal/dag"
)

// Chat is the app-level chat record. Its ID doubles as the ADK session ID.
type Chat struct {
	ID           string    `gorm:"primaryKey" json:"id"`
	Title        string    `json:"title"`
	SystemPrompt string    `json:"system_prompt"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// Set only for GitHub-originated chats (id github-<owner>-<repo>-<number>).
	GithubRepo string `json:"github_repo,omitempty"`
	GithubURL  string `json:"github_url,omitempty"`
	// ADK session identity (GitHub commenter's login for dispatched chats).
	// Column is adk_session_user: session_user collides with Postgres' SESSION_USER.
	SessionUser string `gorm:"column:adk_session_user" json:"session_user,omitempty"`
}

// ChatTurn is one user→assistant exchange. Its ID is the response_id in the REST API.
type ChatTurn struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	ChatID    string    `gorm:"index" json:"chat_id"`
	Seq       int       `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
	// Model that produced the orchestrator's reply; ADK drops ModelVersion on read.
	// Empty for DAG turns (per-node on DagNode).
	Model string `json:"model,omitempty"`
}

// GithubSnapshot stores the full GitHub state fetched at a github-origin
// chat's last dispatch. JSON is opaque to this package (mirrors DagPlan.PlanJSON).
type GithubSnapshot struct {
	ChatID    string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	JSON      string    `json:"json"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GithubReviewBaseline stores the patch-ids of PR commits quack has actually
// reviewed. Separate from GithubSnapshot: only a review-delivering dispatch advances this.
type GithubReviewBaseline struct {
	ChatID    string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	PatchIDs  string    `json:"patch_ids"` // JSON array of strings; row absent = never reviewed
	UpdatedAt time.Time `json:"updated_at"`
}

// GithubFixState tracks the CI auto-heal loop bound for one PR chat.
// Durable so process restart doesn't reset state and thrash forever.
type GithubFixState struct {
	ChatID    string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	LastSHA   string    `gorm:"column:last_sha" json:"last_sha"`
	Stopped   bool      `json:"stopped"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GithubMergeIntent records a standing merge authorization for a PR chat,
// durable across restarts so quack:merge applied before a review still works.
type GithubMergeIntent struct {
	ChatID      string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	RequestedBy string    `json:"requested_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// DagPlan stores the JSON-encoded DAG plan for a chat turn (re-display on reload).
type DagPlan struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	ChatID    string    `gorm:"index" json:"chat_id"`
	TurnID    string    `gorm:"index" json:"turn_id"`
	PlanJSON  string    `json:"plan_json"`
	CreatedAt time.Time `json:"created_at"`
}

// ChatEvent backs the hub's durable replay after restart. Cleared on new run per chat.
type ChatEvent struct {
	ChatID    string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	Seq       int64     `gorm:"primaryKey;autoIncrement:false" json:"seq"`
	Event     string    `json:"event"`
	CreatedAt time.Time `json:"created_at"`
}

// DagNode stores the execution state of one DAG node.
type DagNode struct {
	NodeID        string `gorm:"primaryKey;column:node_id" json:"node_id"`
	PlanID        string `gorm:"primaryKey;column:plan_id" json:"plan_id"`
	Status        string `json:"status"` // dag.NodeStatus value
	OutputPreview string `json:"output_preview"`
	// Full vetted text (OutputPreview truncated to 250 chars for display).
	Output           string     `json:"output,omitempty"`
	Error            string     `json:"error"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	Model            string     `json:"model"`
	PromptTokens     int32      `json:"prompt_tokens"`
	CompletionTokens int32      `json:"completion_tokens"`
	ReasoningTokens  int32      `json:"reasoning_tokens"`
	TotalTokens      int32      `json:"total_tokens"`
	FinishReason     string     `json:"finish_reason"`
	DurationMs       int64      `json:"duration_ms"`
	JudgeRounds      int32      `json:"judge_rounds"`
	JudgeFinalScore  float64    `json:"judge_final_score"`
	JudgePassed      bool       `json:"judge_passed"`
	// Stamped when a node is queued/running for ownership tracking.
	InstanceID string `gorm:"column:instance_id" json:"-"`
	// Bumped on every write; fallback for FailStaleDagNodes when no InstanceID matches.
	UpdatedAt *time.Time `json:"-"`
}

// TurnContent is the fully-joined view of one turn used to build API responses.
type TurnContent struct {
	ID        string
	CreatedAt time.Time
	UserText  string
	AsstText  string
	AsstThink string
	ToolCalls []ToolCallRecord // orchestrator-level tool calls, in event order
	Plan      *DagPlan
	Nodes     []DagNode
	// Orchestrator's own token usage (DAG turn per-node tokens are on Nodes).
	PromptTokens, CompletionTokens, ReasoningTokens int32
	// Orchestrator's model for plain-reply turns; empty for DAG turns.
	Model string
}

// ToolCallRecord is one orchestrator tool call with its result paired by call ID.
type ToolCallRecord struct {
	CallID string
	Name   string
	Args   map[string]any
	Result map[string]any
}

// ADK's internal agent-transfer tool; excluded as activity-log noise.
const transferTool = "transfer_to_agent"

// Mirror tools.ChoiceToolName/ChoiceAnswerKey; surface the choice as user text.
const (
	choiceToolName  = "get_user_choice"
	choiceAnswerKey = "choice"
)

// Mirror ADK's adk_request_input resume shape; surface the HITL answer as user text.
const (
	nodeInputCallName   = "adk_request_input"
	nodeInputPayloadKey = "payload"
)

// Mirrors orchestrator.orchestratorName; gate-internal activity is never the user-facing message.
const orchestratorAuthor = "orchestrator"

// Per-turn content extracted from a session's events.
type turnGroup struct {
	userText, asstText, asstThink                   string
	toolCalls                                       []ToolCallRecord
	promptTokens, completionTokens, reasoningTokens int32
}

// groupSessionEvents buckets session events into per-turn groups, split on user events.
// Pure (no DB) so extraction is unit-testable.
func groupSessionEvents(events iter.Seq[*session.Event]) []turnGroup {
	var groups []turnGroup
	var cur *turnGroup
	for ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		if ev.Author == "user" {
			groups = append(groups, turnGroup{})
			cur = &groups[len(groups)-1]
			for _, p := range ev.Content.Parts {
				if p == nil {
					continue
				}
				// Clarification or HITL answer arrives as FunctionResponse (Role:user).
				if p.FunctionResponse != nil {
					switch p.FunctionResponse.Name {
					case choiceToolName:
						if c, ok := p.FunctionResponse.Response[choiceAnswerKey].(string); ok {
							cur.userText += c
						}
					case nodeInputCallName:
						if c, ok := p.FunctionResponse.Response[nodeInputPayloadKey].(string); ok {
							cur.userText += c
						}
					}
					continue
				}
				if !p.Thought && p.FunctionCall == nil {
					cur.userText += p.Text
				}
			}
			continue
		}
		if cur == nil {
			continue
		}
		// Gate-internal activity is never the user-facing message; skip it.
		if ev.Author != orchestratorAuthor {
			continue
		}
		if ev.UsageMetadata != nil {
			cur.promptTokens += ev.UsageMetadata.PromptTokenCount
			cur.completionTokens += ev.UsageMetadata.CandidatesTokenCount
			cur.reasoningTokens += ev.UsageMetadata.ThoughtsTokenCount
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			switch {
			case p.FunctionCall != nil:
				if p.FunctionCall.Name == transferTool {
					continue
				}
				cur.toolCalls = append(cur.toolCalls, ToolCallRecord{
					CallID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Args: p.FunctionCall.Args,
				})
			case p.FunctionResponse != nil:
				if p.FunctionResponse.Name == transferTool {
					continue
				}
				// Pair to the earlier call by ID (a call always precedes its response).
				for i := range cur.toolCalls {
					if cur.toolCalls[i].CallID == p.FunctionResponse.ID {
						cur.toolCalls[i].Result = p.FunctionResponse.Response
						break
					}
				}
			case p.Thought:
				cur.asstThink += p.Text
			default:
				cur.asstText += p.Text
			}
		}
	}
	return groups
}

// Store wraps the relational DB and ADK session service.
type Store struct {
	db       *gorm.DB
	Sessions session.Service
	// Identifies this store for node-ownership tracking (random per New, or overridden).
	instanceID string
}

// New opens the persistence store, runs migrations, and returns it.
func New(kind, url string) (*Store, error) {
	dialector, err := dialectorFor(kind, url)
	if err != nil {
		return nil, err
	}
	// Route GORM's slow-query warnings through slog.
	gormCfg := &gorm.Config{Logger: logger.New(
		slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
		},
	)}
	db, err := gorm.Open(dialector(), gormCfg)
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&Chat{}, &ChatTurn{}, &DagPlan{}, &DagNode{}, &ChatEvent{}, &GithubSnapshot{}, &GithubReviewBaseline{}, &GithubFixState{}, &GithubMergeIntent{}); err != nil {
		return nil, err
	}
	sessions, err := database.NewSessionService(dialector(), gormCfg)
	if err != nil {
		return nil, err
	}
	if err := database.AutoMigrate(sessions); err != nil {
		return nil, err
	}
	return &Store{db: db, Sessions: sessions, instanceID: uuid.NewString()}, nil
}

// InstanceID identifies this Store for node-ownership tracking.
func (s *Store) InstanceID() string { return s.instanceID }

// SetInstanceID overrides the random default. Call once with a persisted identity
// (LoadOrCreateInstanceID) before any node writes; ephemeral CLIs keep the default.
func (s *Store) SetInstanceID(id string) { s.instanceID = id }

// dialectorFor returns a factory that yields a GORM dialector for kind+url.
// SQLite shares one *sql.DB with max 1 conn to prevent SQLITE_BUSY.
func dialectorFor(kind, url string) (func() gorm.Dialector, error) {
	switch kind {
	case "", "postgres":
		return func() gorm.Dialector { return postgres.Open(url) }, nil
	case "sqlite":
		sqlDB, err := sql.Open(sqlite.DriverName, sqliteDSN(url))
		if err != nil {
			return nil, fmt.Errorf("store: open sqlite: %w", err)
		}
		sqlDB.SetMaxOpenConns(1) // one writer; serialize the two pools onto it
		return func() gorm.Dialector { return &sqlite.Dialector{Conn: sqlDB} }, nil
	default:
		return nil, fmt.Errorf("store: unsupported kind %q (postgres or sqlite)", kind)
	}
}

// sqliteDSN enables WAL + busy timeout. Existing query params are left untouched.
func sqliteDSN(url string) string {
	if strings.Contains(url, "?") {
		return url
	}
	return url + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
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

// ChatsPageDefaultLimit is used when a caller passes limit <= 0.
const ChatsPageDefaultLimit = 20

// ChatsPageMaxLimit bounds limit regardless of what a caller requests.
const ChatsPageMaxLimit = 100

// ErrInvalidCursor is returned by ListChats when cursor doesn't decode.
var ErrInvalidCursor = errors.New("invalid cursor")

// chatCursor is a keyset position: the (updated_at, id) of the last chat on
// the previous page. id breaks ties on updated_at and gives every chat a
// stable position, which an offset can't: updated_at changes whenever a run
// is active on that chat, so an offset would skip or repeat a row across two
// requests straddling that change.
type chatCursor struct {
	UpdatedAt time.Time `json:"u"`
	ID        string    `json:"i"`
}

func encodeChatCursor(c chatCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeChatCursor(s string) (chatCursor, error) {
	var c chatCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return c, nil
}

// ListChats returns up to limit chats, most-recently-updated first, starting
// after cursor ("" for the first page). It returns the cursor for the next
// page, or "" if this page was the last. limit <= 0 becomes
// ChatsPageDefaultLimit; limit above ChatsPageMaxLimit is capped.
//
// This is keyset (not offset) pagination: see chatCursor.
func (s *Store) ListChats(ctx context.Context, limit int, cursor string) ([]Chat, string, error) {
	if limit <= 0 {
		limit = ChatsPageDefaultLimit
	} else if limit > ChatsPageMaxLimit {
		limit = ChatsPageMaxLimit
	}

	q := s.db.WithContext(ctx).Order("updated_at desc, id desc").Limit(limit + 1)
	if cursor != "" {
		c, err := decodeChatCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		q = q.Where("updated_at < ? OR (updated_at = ? AND id < ?)", c.UpdatedAt, c.UpdatedAt, c.ID)
	}

	var chats []Chat
	if err := q.Find(&chats).Error; err != nil {
		return nil, "", err
	}

	next := ""
	if len(chats) > limit {
		chats = chats[:limit]
		last := chats[limit-1]
		next = encodeChatCursor(chatCursor{UpdatedAt: last.UpdatedAt, ID: last.ID})
	}
	return chats, next, nil
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

// Mirrors orchestrator.AppName (store can't import it).
const chatAppName = "quack"

// SessionUserFor resolves the ADK session identity: per-chat SessionUser or id-shape default.
func SessionUserFor(c Chat) string {
	if c.SessionUser != "" {
		return c.SessionUser
	}
	if strings.HasPrefix(c.ID, "github-") {
		return "github"
	}
	return "local"
}

// SessionUserForChat is SessionUserFor for callers holding only a chat id.
func (s *Store) SessionUserForChat(ctx context.Context, id string) string {
	c, err := s.GetChat(ctx, id)
	if err != nil || c == nil {
		if strings.HasPrefix(id, "github-") {
			return "github"
		}
		return "local"
	}
	return SessionUserFor(*c)
}

// DeleteChat removes a chat and everything associated. Runs in one transaction;
// ADK session delete is best-effort after commit (separate service, can't join tx).
func (s *Store) DeleteChat(ctx context.Context, id string) error {
	// Resolve before the tx removes the chats row (SessionUserForChat would fall back to id-shape).
	sessionUser := s.SessionUserForChat(ctx, id)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var planIDs []string
		if err := tx.Model(&DagPlan{}).Where("chat_id = ?", id).Pluck("id", &planIDs).Error; err != nil {
			return err
		}
		if len(planIDs) > 0 {
			if err := tx.Where("plan_id IN ?", planIDs).Delete(&DagNode{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("chat_id = ?", id).Delete(&DagPlan{}).Error; err != nil {
			return err
		}
		if err := tx.Where("chat_id = ?", id).Delete(&ChatTurn{}).Error; err != nil {
			return err
		}
		if err := tx.Where("chat_id = ?", id).Delete(&ChatEvent{}).Error; err != nil {
			return err
		}
		return tx.Delete(&Chat{}, "id = ?", id).Error
	})
	if err != nil {
		return err
	}
	if err := s.Sessions.Delete(ctx, &session.DeleteRequest{AppName: chatAppName, UserID: sessionUser, SessionID: id}); err != nil {
		slog.Warn("chat deleted but its ADK session could not be reaped",
			"component", "store", "chat", id, "err", err)
	}
	return nil
}

// Touch bumps a chat's updated_at to now.
func (s *Store) Touch(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", id).Update("updated_at", time.Now().UTC()).Error
}

// SetChatGitHub upserts the originating GitHub repo/URL. Creates the row if missing
// (webhook may fire before chat exists). SessionUser fixed at creation.
func (s *Store) SetChatGitHub(ctx context.Context, id, repo, url, sessionUser string) error {
	now := time.Now().UTC()
	c := &Chat{ID: id, CreatedAt: now, UpdatedAt: now, GithubRepo: repo, GithubURL: url, SessionUser: sessionUser}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"github_repo", "github_url", "updated_at"}),
	}).Create(c).Error
}

// GetGithubSnapshot returns the stored snapshot JSON, or ("", false, nil) when none exists.
func (s *Store) GetGithubSnapshot(ctx context.Context, chatID string) (string, bool, error) {
	var row GithubSnapshot
	err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.JSON, true, nil
}

// SetGithubSnapshot upserts the snapshot JSON for the next resume's diff.
func (s *Store) SetGithubSnapshot(ctx context.Context, chatID, json string) error {
	row := &GithubSnapshot{ChatID: chatID, JSON: json, UpdatedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"json", "updated_at"}),
	}).Create(row).Error
}

// GetGithubReviewBaseline returns the patch-id list quack last delivered a review at.
func (s *Store) GetGithubReviewBaseline(ctx context.Context, chatID string) (string, bool, error) {
	var row GithubReviewBaseline
	err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.PatchIDs, true, nil
}

// SetGithubReviewBaseline upserts the patch-id list (only when a review is delivered).
func (s *Store) SetGithubReviewBaseline(ctx context.Context, chatID, patchIDsJSON string) error {
	row := &GithubReviewBaseline{ChatID: chatID, PatchIDs: patchIDsJSON, UpdatedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"patch_ids", "updated_at"}),
	}).Create(row).Error
}

// GetGithubFixState returns the auto-heal state, or (nil, nil) when none exists.
func (s *Store) GetGithubFixState(ctx context.Context, chatID string) (*GithubFixState, error) {
	var row GithubFixState
	err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// SetGithubFixState upserts the auto-heal state (persisted before fix run so crash doesn't refund).
func (s *Store) SetGithubFixState(ctx context.Context, st GithubFixState) error {
	st.UpdatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_sha", "stopped", "updated_at"}),
	}).Create(&st).Error
}

// DeleteGithubFixState re-arms auto-heal (human re-applied the fix label).
func (s *Store) DeleteGithubFixState(ctx context.Context, chatID string) error {
	return s.db.WithContext(ctx).Where("chat_id = ?", chatID).Delete(&GithubFixState{}).Error
}

// GetGithubMergeIntent returns the merge authorization, or (nil, nil) when none.
func (s *Store) GetGithubMergeIntent(ctx context.Context, chatID string) (*GithubMergeIntent, error) {
	var row GithubMergeIntent
	err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// SetGithubMergeIntent upserts the merge authorization (quack:merge label applied).
func (s *Store) SetGithubMergeIntent(ctx context.Context, chatID, requestedBy string) error {
	now := time.Now().UTC()
	row := &GithubMergeIntent{ChatID: chatID, RequestedBy: requestedBy, CreatedAt: now, UpdatedAt: now}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"requested_by", "updated_at"}),
	}).Create(row).Error
}

// DeleteGithubMergeIntent clears the merge authorization (consumed by merge).
func (s *Store) DeleteGithubMergeIntent(ctx context.Context, chatID string) error {
	return s.db.WithContext(ctx).Where("chat_id = ?", chatID).Delete(&GithubMergeIntent{}).Error
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

// SetTurnModel stamps the model on the turn row (ADK drops ModelVersion on read).
func (s *Store) SetTurnModel(ctx context.Context, chatID, turnID, model string) error {
	return s.db.WithContext(ctx).Model(&ChatTurn{}).
		Where("id = ? AND chat_id = ?", turnID, chatID).
		Update("model", model).Error
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

// SaveDagPlan persists a DAG plan linked to a turn.
func (s *Store) SaveDagPlan(ctx context.Context, chatID, planID, turnID, planJSON string) error {
	now := time.Now().UTC()
	p := &DagPlan{ID: planID, ChatID: chatID, TurnID: turnID, PlanJSON: planJSON, CreatedAt: now}
	return s.db.WithContext(ctx).Create(p).Error
}

// UpsertDagNode creates or updates a DAG node's execution state.
func (s *Store) UpsertDagNode(ctx context.Context, node DagNode) error {
	db := s.db.WithContext(ctx)
	// Never let a nil StartedAt/InstanceID erase a real one from an earlier write.
	if node.StartedAt == nil {
		db = db.Omit("started_at")
	}
	if node.InstanceID == "" {
		db = db.Omit("instance_id")
	}
	t := time.Now().UTC()
	node.UpdatedAt = &t
	return db.Save(&node).Error
}

// InsertChatEvent persists one run event. Caller assigns Seq and serializes inserts.
func (s *Store) InsertChatEvent(ctx context.Context, ev ChatEvent) error {
	return s.db.WithContext(ctx).Create(&ev).Error
}

// LoadChatEvents returns events with seq > afterSeq (afterSeq=0 for full run).
func (s *Store) LoadChatEvents(ctx context.Context, chatID string, afterSeq int64) ([]ChatEvent, error) {
	var evs []ChatEvent
	err := s.db.WithContext(ctx).
		Where("chat_id = ? AND seq > ?", chatID, afterSeq).
		Order("seq asc").Find(&evs).Error
	return evs, err
}

// DeleteChatEvents drops a chat's run events (fresh start for new run).
func (s *Store) DeleteChatEvents(ctx context.Context, chatID string) error {
	return s.db.WithContext(ctx).Where("chat_id = ?", chatID).Delete(&ChatEvent{}).Error
}

// TrimChatEvents drops events at or below upToSeq (window long runs to replay ceiling).
func (s *Store) TrimChatEvents(ctx context.Context, chatID string, upToSeq int64) error {
	return s.db.WithContext(ctx).Where("chat_id = ? AND seq <= ?", chatID, upToSeq).Delete(&ChatEvent{}).Error
}

// staleNodeCeiling: dead-man's-switch for orphaned nodes. Generous (runs finish in minutes).
const staleNodeCeiling = 12 * time.Hour

// FailStaleDagNodes marks orphaned queued/running nodes as failed. Uses dag constants
// (bulk SQL can't invoke CanTransition per row).
func (s *Store) FailStaleDagNodes(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().Add(-staleNodeCeiling)
	res := s.db.WithContext(ctx).Model(&DagNode{}).
		Where("status IN ?", []string{string(dag.StatusQueued), string(dag.StatusRunning)}).
		// IS NULL covers ALTER TABLE ADD COLUMN no-default rows.
		Where("instance_id IS NULL OR instance_id = ? OR instance_id = ? OR updated_at < ?", "", s.instanceID, cutoff).
		Updates(map[string]any{"status": string(dag.StatusFailed), "error": "server restarted mid-run"})
	return res.RowsAffected, res.Error
}

// GetDagNodes returns all nodes for a plan.
func (s *Store) GetDagNodes(ctx context.Context, planID string) ([]DagNode, error) {
	var nodes []DagNode
	err := s.db.WithContext(ctx).Where("plan_id = ?", planID).Find(&nodes).Error
	return nodes, err
}

// GetDagNode returns one node's persisted state, or (nil, nil) if it has no row yet.
func (s *Store) GetDagNode(ctx context.Context, planID, nodeID string) (*DagNode, error) {
	var n DagNode
	err := s.db.WithContext(ctx).Where("plan_id = ? AND node_id = ?", planID, nodeID).First(&n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// GetLatestDagPlan returns the most-recent DAG plan for a chat.
func (s *Store) GetLatestDagPlan(ctx context.Context, chatID string) (*DagPlan, error) {
	var p DagPlan
	err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Order("created_at DESC").First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetTurnsWithContent returns fully-joined turn data with DAG plan and nodes.
func (s *Store) GetTurnsWithContent(ctx context.Context, appName, userID, chatID string) ([]TurnContent, error) {
	turns, err := s.ListTurns(ctx, chatID)
	if err != nil {
		return nil, err
	}

	// Group ADK events into per-turn buckets separated by user-authored events.
	var groups []turnGroup
	resp, err := s.Sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: chatID})
	if err == nil && resp != nil {
		groups = groupSessionEvents(resp.Session.Events().All())
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
		tc := TurnContent{ID: t.ID, CreatedAt: t.CreatedAt, Model: t.Model}
		if i < len(groups) {
			tc.UserText = groups[i].userText
			tc.AsstText = groups[i].asstText
			tc.AsstThink = groups[i].asstThink
			tc.ToolCalls = groups[i].toolCalls
			tc.PromptTokens = groups[i].promptTokens
			tc.CompletionTokens = groups[i].completionTokens
			tc.ReasoningTokens = groups[i].reasoningTokens
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
