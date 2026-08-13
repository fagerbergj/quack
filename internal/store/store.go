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
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/artifact"
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
	GithubRepo  string `json:"github_repo,omitempty"`
	GithubURL   string `json:"github_url,omitempty"`
	GithubState string `json:"github_state,omitempty"`
	// ADK session identity (GitHub commenter's login for dispatched chats).
	// Column is adk_session_user: session_user collides with Postgres' SESSION_USER.
	SessionUser string `gorm:"column:adk_session_user" json:"session_user,omitempty"`
	// RunStatus/PendingQuestion are the last run's terminal outcome (RunStatus* consts),
	// stamped by StampRunOutcome so ListChats reads it instead of recomputing per chat (#738).
	// Never "queued"/"running" - those stay live, in-memory-only signals.
	RunStatus       string `json:"-"`
	PendingQuestion string `json:"-"`
	// ActiveTurnID is set by MarkRunActive when a run starts and cleared by StampRunOutcome
	// when it ends cleanly. Left over (non-empty) with no live hub/queue signal means the run
	// died before it could stamp an outcome - the read path's crash fallback (#738).
	ActiveTurnID string `json:"-"`
	// Archived hides the chat from the main list by default (toggled by the user or auto-archived
	// on PR merge when config AutoArchiveOnMerge is enabled; untouched by archive ops).
	Archived bool `gorm:"column:archived;default:false" json:"archived"`
	// Origin is an extension-set sdk.ChatOrigin, marshaled opaquely (#275). Not yet
	// API-exposed - ChatSummary.origin is a follow-up once the SPA renders it generically.
	Origin string `json:"-"`
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
	// Orchestrator's own token usage, stamped alongside Model (same reason:
	// SQL-summable for the chat-wide aggregate without walking ADK session
	// events). Empty for DAG turns (per-node on DagNode).
	PromptTokens     int32 `json:"prompt_tokens,omitempty"`
	CompletionTokens int32 `json:"completion_tokens,omitempty"`
	ReasoningTokens  int32 `json:"reasoning_tokens,omitempty"`
	TotalTokens      int32 `json:"total_tokens,omitempty"`
	CachedTokens     int32 `json:"cached_tokens,omitempty"`
	// Input is the turn's trigger text, stamped at dispatch time by SetTurnInput -
	// before the run's first session event exists, so a run still queued on the
	// worker semaphore has something to show. GetTurnsWithContent falls back to
	// the session walk for turns that predate this column.
	Input string `json:"-"`
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
	CachedTokens     int32      `json:"cached_tokens"`
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
	PromptTokens, CompletionTokens, ReasoningTokens, TotalTokens, CachedTokens int32
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
	userText, asstText, asstThink                                              string
	toolCalls                                                                  []ToolCallRecord
	promptTokens, completionTokens, reasoningTokens, cachedTokens, totalTokens int32
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
			cur.cachedTokens += ev.UsageMetadata.CachedContentTokenCount
			cur.totalTokens += ev.UsageMetadata.TotalTokenCount
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
	// Counts SELECT queries issued on db - test instrumentation for N+1 regressions (#738).
	queryCount atomic.Int64
	// artifacts: nil unless SetArtifactService was called - DeleteChat cascades into it when set.
	artifacts artifact.Service
}

// QueryCount returns the number of SELECT queries issued so far. Test-only instrumentation.
func (s *Store) QueryCount() int64 { return s.queryCount.Load() }

// SetArtifactService wires the artifact service DeleteChat cascades chat
// deletion into. Not part of New - the artifact service is built separately
// (see internal/serve) and may be a different store than this one's.
func (s *Store) SetArtifactService(svc artifact.Service) { s.artifacts = svc }

// New opens the persistence store, runs migrations, and returns it.
func New(kind, url string) (*Store, error) {
	dialector, err := dialectorFor(kind, url)
	if err != nil {
		return nil, err
	}
	gormCfg := &gorm.Config{Logger: slogGormLogger()}
	db, err := gorm.Open(dialector(), gormCfg)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := db.Callback().Query().After("gorm:query").Register("quack:count_queries", func(*gorm.DB) {
		s.queryCount.Add(1)
	}); err != nil {
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
	s.Sessions = sessions
	s.instanceID = uuid.NewString()
	return s, nil
}

// InstanceID identifies this Store for node-ownership tracking.
func (s *Store) InstanceID() string { return s.instanceID }

// SetInstanceID overrides the random default. Call once with a persisted identity
// (LoadOrCreateInstanceID) before any node writes; ephemeral CLIs keep the default.
func (s *Store) SetInstanceID(id string) { s.instanceID = id }

// slogGormLogger routes GORM's slow-query warnings through slog. Shared by
// New and the standalone artifact-service opener (artifact.go), which don't
// otherwise share a construction path.
func slogGormLogger() logger.Interface {
	return logger.New(
		slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
		},
	)
}

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

// ErrInvalidPageToken is returned by ListChats when the page token doesn't
// decode, or was issued under a different ordering or scope than it's being
// replayed against (never silently honored under the wrong one).
var ErrInvalidPageToken = errors.New("invalid page token")

// chatsSort names the ordering a page token was issued under. ListChats
// supports exactly one ordering today; this exists so a future second
// ordering gets its own value here instead of a token silently being
// replayed against an ordering it wasn't issued for.
type chatsSort string

const chatsSortUpdatedAtDesc chatsSort = "updated_at_desc"

// ChatsScope selects which chats ListChats considers, pushed into the SQL
// predicate so a page never fetches rows the caller can't see. Two
// independent flags, not a 3-valued enum - "all" was never a chat status,
// just "don't filter". The zero value (both false) defaults to Active-only.
// Both true means no archived predicate at all (not archived IN (true,false)),
// so the planner sees a plain scan.
type ChatsScope struct {
	Active   bool `json:"a"`
	Archived bool `json:"r"`
}

// chatsPageToken is ListChats' opaque continuation token. The caller-visible
// contract is just "an anchor under a named ordering and scope": ID anchors
// it because ID is immutable, unlike UpdatedAt, which churns under an active
// run. UpdatedAt still rides along inside the token - resolving an ID anchor
// back to its position in an updated_at-sorted list needs the value it was
// last seen at, so the token carries it as its own implementation detail,
// not as part of the contract a caller is meant to understand. Scope is a
// struct of independent flags rather than an ordered list, so encoding it is
// inherently canonical - {active,archived} and {archived,active} collapse to
// the identical Go value (and therefore identical JSON) with no sort step.
type chatsPageToken struct {
	Sort      chatsSort  `json:"s"`
	Scope     ChatsScope `json:"sc"`
	ID        string     `json:"i"`
	UpdatedAt time.Time  `json:"u"`
}

func encodeChatsPageToken(t chatsPageToken) string {
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeChatsPageToken validates the token was issued for the sort and scope
// it's being replayed against. A token minted before scoping existed carries
// a zero-value Scope ({false false}) - treated as {Active: true}, the
// pre-existing default behavior, rather than rejected outright.
func decodeChatsPageToken(s string, scope ChatsScope) (chatsPageToken, error) {
	var t chatsPageToken
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return t, fmt.Errorf("%w: %v", ErrInvalidPageToken, err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, fmt.Errorf("%w: %v", ErrInvalidPageToken, err)
	}
	if t.Sort != chatsSortUpdatedAtDesc {
		return t, fmt.Errorf("%w: issued for sort %q, not %q", ErrInvalidPageToken, t.Sort, chatsSortUpdatedAtDesc)
	}
	tokenScope := t.Scope
	if !tokenScope.Active && !tokenScope.Archived {
		tokenScope = ChatsScope{Active: true}
	}
	if tokenScope != scope {
		return t, fmt.Errorf("%w: issued for scope %+v, not %+v", ErrInvalidPageToken, tokenScope, scope)
	}
	return t, nil
}

// ListChats returns up to limit chats within scope, most-recently-updated
// first, starting after pageToken ("" for the first page). It returns the
// opaque token for the next page, or "" if this page was the last. limit <= 0
// becomes ChatsPageDefaultLimit; limit above ChatsPageMaxLimit is capped.
// The zero-value scope (both flags false) defaults to {Active: true}.
//
// This is keyset (not offset) pagination: see chatsPageToken. The scope
// predicate is applied in SQL, not filtered from an already-fetched page, so
// a page always returns exactly limit rows (or fewer only at the true end of
// that scope) and the cursor never advances past a row the caller never saw.
func (s *Store) ListChats(ctx context.Context, limit int, pageToken string, scope ChatsScope) ([]Chat, string, error) {
	if limit <= 0 {
		limit = ChatsPageDefaultLimit
	} else if limit > ChatsPageMaxLimit {
		limit = ChatsPageMaxLimit
	}
	if !scope.Active && !scope.Archived {
		scope.Active = true
	}

	q := s.db.WithContext(ctx).Order("updated_at desc, id desc").Limit(limit + 1)
	switch {
	case scope.Active && scope.Archived:
		// No predicate - both archived and active rows.
	case scope.Active:
		q = q.Where("archived = ?", false)
	case scope.Archived:
		q = q.Where("archived = ?", true)
	}
	if pageToken != "" {
		t, err := decodeChatsPageToken(pageToken, scope)
		if err != nil {
			return nil, "", err
		}
		q = q.Where("updated_at < ? OR (updated_at = ? AND id < ?)", t.UpdatedAt, t.UpdatedAt, t.ID)
	}

	var chats []Chat
	if err := q.Find(&chats).Error; err != nil {
		return nil, "", err
	}

	next := ""
	if len(chats) > limit {
		chats = chats[:limit]
		last := chats[limit-1]
		next = encodeChatsPageToken(chatsPageToken{Sort: chatsSortUpdatedAtDesc, Scope: scope, ID: last.ID, UpdatedAt: last.UpdatedAt})
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
	s.deleteChatArtifacts(ctx, id, sessionUser)
	return nil
}

// deleteChatArtifacts best-effort cascades chat deletion into the artifact
// service (attachment bytes for this chat's session) - same failure posture
// as the ADK session reap above: log and move on, never block the delete.
func (s *Store) deleteChatArtifacts(ctx context.Context, chatID, sessionUser string) {
	if s.artifacts == nil {
		return
	}
	lr, err := s.artifacts.List(ctx, &artifact.ListRequest{AppName: chatAppName, UserID: sessionUser, SessionID: chatID})
	if err != nil {
		slog.Warn("chat deleted but its artifacts could not be listed for cleanup",
			"component", "store", "chat", chatID, "err", err)
		return
	}
	for _, name := range lr.FileNames {
		// List also surfaces "user:"-prefixed names (visible cross-session by
		// design) - this chat doesn't own those, so it must not delete them.
		if strings.HasPrefix(name, "user:") {
			continue
		}
		if err := s.artifacts.Delete(ctx, &artifact.DeleteRequest{AppName: chatAppName, UserID: sessionUser, SessionID: chatID, FileName: name}); err != nil {
			slog.Warn("chat deleted but one of its artifacts could not be reaped",
				"component", "store", "chat", chatID, "name", name, "err", err)
		}
	}
}

// SetChatGitHub upserts the originating GitHub repo/URL/state. Creates the row if missing
// (webhook may fire before chat exists). SessionUser fixed at creation.
func (s *Store) SetChatGitHub(ctx context.Context, id, repo, url, state, sessionUser string) error {
	now := time.Now().UTC()
	c := &Chat{ID: id, CreatedAt: now, UpdatedAt: now, GithubRepo: repo, GithubURL: url, GithubState: state, SessionUser: sessionUser}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"github_repo", "github_url", "github_state", "updated_at"}),
	}).Create(c).Error
}

// SetChatOrigin upserts an extension-dispatched chat's SessionUser and Origin
// JSON. Creates the row if missing (dispatch may arrive before any chat
// exists) - SessionUser is fixed at creation, same rule as SetChatGitHub.
func (s *Store) SetChatOrigin(ctx context.Context, id, sessionUser, originJSON string) error {
	now := time.Now().UTC()
	c := &Chat{ID: id, CreatedAt: now, UpdatedAt: now, SessionUser: sessionUser, Origin: originJSON}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"origin", "updated_at"}),
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

// ArchiveChat toggles the archived flag on a chat.
// Archiving never touches UpdatedAt so that archive/unarchive doesn't reorder
// the recency-sorted chat list - UpdateColumn (not Update) is required for that:
// GORM auto-stamps UpdatedAt on any plain Update/Updates call by field-name
// convention, and only UpdateColumn/UpdateColumns skip that.
func (s *Store) ArchiveChat(ctx context.Context, id string, archived bool) error {
	return s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", id).UpdateColumn("archived", archived).Error
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

// SetTurnInput persists the turn's trigger text at dispatch time (both
// rest.SendChatMessage and an extension's Dispatch call this right after
// SaveTurn), before the run's first session event exists.
func (s *Store) SetTurnInput(ctx context.Context, chatID, turnID, input string) error {
	return s.db.WithContext(ctx).Model(&ChatTurn{}).
		Where("id = ? AND chat_id = ?", turnID, chatID).
		Update("input", input).Error
}

// TurnUsage is the orchestrator's own per-turn token usage (DAG turns credit
// tokens per-node on DagNode instead).
type TurnUsage struct {
	PromptTokens, CompletionTokens, ReasoningTokens, TotalTokens, CachedTokens int32
}

// SetTurnUsage stamps the model + token usage on the turn row in one write
// (ADK drops both ModelVersion and a chat-wide-summable usage shape on
// read - see GetChatUsage). Called once at run end for a plain-reply turn.
func (s *Store) SetTurnUsage(ctx context.Context, chatID, turnID, model string, u TurnUsage) error {
	return s.db.WithContext(ctx).Model(&ChatTurn{}).
		Where("id = ? AND chat_id = ?", turnID, chatID).
		Updates(map[string]any{
			"model":             model,
			"prompt_tokens":     u.PromptTokens,
			"completion_tokens": u.CompletionTokens,
			"reasoning_tokens":  u.ReasoningTokens,
			"total_tokens":      u.TotalTokens,
			"cached_tokens":     u.CachedTokens,
		}).Error
}

// ListTurns returns all turns for a chat ordered by sequence.
func (s *Store) ListTurns(ctx context.Context, chatID string) ([]ChatTurn, error) {
	var turns []ChatTurn
	err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Order("seq asc").Find(&turns).Error
	return turns, err
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
		tc := TurnContent{
			ID: t.ID, CreatedAt: t.CreatedAt, Model: t.Model,
			// Stamped by SetTurnUsage at run end - the SQL-summable source of
			// truth. Turns that predate that stamp fall back to the session
			// walk below.
			PromptTokens: t.PromptTokens, CompletionTokens: t.CompletionTokens,
			ReasoningTokens: t.ReasoningTokens, TotalTokens: t.TotalTokens, CachedTokens: t.CachedTokens,
			// Stamped by SetTurnInput at dispatch time - present even for a turn
			// that's still queued, before any session event exists. Turns that
			// predate that stamp fall back to the session walk below.
			UserText: t.Input,
		}
		if i < len(groups) {
			if tc.UserText == "" {
				tc.UserText = groups[i].userText
			}
			tc.AsstText = groups[i].asstText
			tc.AsstThink = groups[i].asstThink
			tc.ToolCalls = groups[i].toolCalls
			if tc.PromptTokens == 0 && tc.CompletionTokens == 0 {
				tc.PromptTokens = groups[i].promptTokens
				tc.CompletionTokens = groups[i].completionTokens
				tc.ReasoningTokens = groups[i].reasoningTokens
				tc.CachedTokens = groups[i].cachedTokens
				tc.TotalTokens = groups[i].totalTokens
			}
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

// UsageAggregate sums token usage across the two places a chat spends
// tokens: ChatTurn (plain-reply turns) and DagNode (per-node DAG spend).
type UsageAggregate struct {
	InputTokens, OutputTokens, ReasoningTokens, CachedTokens, TotalTokens int64
}

func (a *UsageAggregate) add(prompt, completion, reasoning, cached, total int64) {
	a.InputTokens += prompt
	a.OutputTokens += completion
	a.ReasoningTokens += reasoning
	a.CachedTokens += cached
	a.TotalTokens += total
}

// tokenSums is the shared Scan target for a SUM(...) row over either table's
// token columns.
type tokenSums struct {
	Prompt, Completion, Reasoning, Cached, Total int64
}

const sumTokenCols = "COALESCE(SUM(prompt_tokens),0) AS prompt, COALESCE(SUM(completion_tokens),0) AS completion, " +
	"COALESCE(SUM(reasoning_tokens),0) AS reasoning, COALESCE(SUM(cached_tokens),0) AS cached, COALESCE(SUM(total_tokens),0) AS total"

// GetChatUsage returns one chat's token aggregate via two SQL SUMs (turns,
// then DAG nodes joined through their plan) - never loads a row into memory.
func (s *Store) GetChatUsage(ctx context.Context, chatID string) (UsageAggregate, error) {
	var agg UsageAggregate

	var t tokenSums
	if err := s.db.WithContext(ctx).Model(&ChatTurn{}).Where("chat_id = ?", chatID).
		Select(sumTokenCols).Scan(&t).Error; err != nil {
		return UsageAggregate{}, err
	}
	agg.add(t.Prompt, t.Completion, t.Reasoning, t.Cached, t.Total)

	var n tokenSums
	if err := s.db.WithContext(ctx).Table("dag_nodes").
		Joins("JOIN dag_plans ON dag_plans.id = dag_nodes.plan_id").
		Where("dag_plans.chat_id = ?", chatID).
		Select(sumTokenCols).
		Scan(&n).Error; err != nil {
		return UsageAggregate{}, err
	}
	agg.add(n.Prompt, n.Completion, n.Reasoning, n.Cached, n.Total)
	return agg, nil
}

// ChatsUsageTotals sums each chat's total tokens (turns + DAG nodes) for a
// batch of chat ids in two GROUP BY queries - the sidebar's one extra
// round-trip per page, not one per chat.
func (s *Store) ChatsUsageTotals(ctx context.Context, chatIDs []string) (map[string]int64, error) {
	totals := make(map[string]int64, len(chatIDs))
	if len(chatIDs) == 0 {
		return totals, nil
	}

	var turnRows []struct {
		ChatID string
		Total  int64
	}
	if err := s.db.WithContext(ctx).Model(&ChatTurn{}).
		Select("chat_id, COALESCE(SUM(total_tokens),0) AS total").
		Where("chat_id IN ?", chatIDs).Group("chat_id").
		Scan(&turnRows).Error; err != nil {
		return nil, err
	}
	for _, r := range turnRows {
		totals[r.ChatID] += r.Total
	}

	var nodeRows []struct {
		ChatID string
		Total  int64
	}
	if err := s.db.WithContext(ctx).Table("dag_nodes").
		Select("dag_plans.chat_id AS chat_id, COALESCE(SUM(dag_nodes.total_tokens),0) AS total").
		Joins("JOIN dag_plans ON dag_plans.id = dag_nodes.plan_id").
		Where("dag_plans.chat_id IN ?", chatIDs).Group("dag_plans.chat_id").
		Scan(&nodeRows).Error; err != nil {
		return nil, err
	}
	for _, r := range nodeRows {
		totals[r.ChatID] += r.Total
	}
	return totals, nil
}
