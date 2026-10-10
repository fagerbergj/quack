// Package store is Quack's persistence layer. A chat's ID is also its ADK session ID,
// so chat history is derived from the session's events (no duplicate table).
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/genai"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/fold"
	"github.com/fagerbergj/quack/internal/pgdial"
	"github.com/fagerbergj/quack/internal/sqlitedsn"
	"github.com/fagerbergj/quack/internal/stream"
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
	// RunStatus/PendingQuestion are the last run's terminal outcome (RunStatus* consts), stamped so ListChats
	// needn't recompute per chat. Never "queued"/"running": those stay live, in-memory-only signals.
	RunStatus       string `json:"-"`
	PendingQuestion string `json:"-"`
	// ActiveTurnID is set by MarkRunActive and cleared by StampRunOutcome on a clean end. Left set with no live
	// hub/queue signal, the run died before stamping an outcome: the read path's crash fallback.
	ActiveTurnID string `json:"-"`
	// Archived hides the chat from the main list by default (toggled by the user or auto-archived
	// on PR merge when config AutoArchiveOnMerge is enabled; untouched by archive ops).
	Archived bool `gorm:"column:archived;default:false" json:"archived"`
	// Origin is an extension-set sdk.ChatOrigin, marshaled opaquely; not API-exposed.
	Origin string `json:"-"`
}

// ChatTurn is one user→assistant exchange. Its ID is the response_id in the REST API.
type ChatTurn struct {
	ID     string `gorm:"primaryKey" json:"id"`
	ChatID string `gorm:"index" json:"chat_id"`
	// Chat is constraint-only: never populated (so GORM never saves it), it teaches AutoMigrate the
	// ON DELETE CASCADE FK to chats.id.
	Chat      Chat      `gorm:"foreignKey:ChatID;references:ID;constraint:OnDelete:CASCADE" json:"-"`
	Seq       int       `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
	// Model that produced the orchestrator's reply; ADK drops ModelVersion on read.
	// Empty for DAG turns (per-node on DagNode).
	Model string `json:"model,omitempty"`
	// Orchestrator's own token usage, SQL-summable for the chat-wide aggregate without walking session events.
	// Empty for DAG turns (per-node on DagNode).
	PromptTokens     int32 `json:"prompt_tokens,omitempty"`
	CompletionTokens int32 `json:"completion_tokens,omitempty"`
	ReasoningTokens  int32 `json:"reasoning_tokens,omitempty"`
	TotalTokens      int32 `json:"total_tokens,omitempty"`
	CachedTokens     int32 `json:"cached_tokens,omitempty"`
	// UserText is the turn's own copy of the user's message: a ResetHistory dispatch deletes the whole session,
	// which would otherwise strand every earlier turn with no content to render.
	UserText string `json:"-"`
}

// DagPlan stores the JSON-encoded DAG plan for a chat turn (re-display on reload).
type DagPlan struct {
	ID     string `gorm:"primaryKey" json:"id"`
	ChatID string `gorm:"index" json:"chat_id"`
	// Chat is constraint-only - see ChatTurn.Chat's doc.
	Chat      Chat      `gorm:"foreignKey:ChatID;references:ID;constraint:OnDelete:CASCADE" json:"-"`
	TurnID    string    `gorm:"index" json:"turn_id"`
	PlanJSON  string    `json:"plan_json"`
	CreatedAt time.Time `json:"created_at"`
	// LastTurnID: the latest turn that ran a step of this plan - an extension's turn, which a retry answers.
	LastTurnID string `json:"last_turn_id,omitempty"`
}

// RunTurnID is the turn a retry or resume of this plan runs under and delivers its answer to.
func (p DagPlan) RunTurnID() string {
	if p.LastTurnID != "" {
		return p.LastTurnID
	}
	return p.TurnID
}

// DagExecPlan is a chat's latest full dag.Plan, one row per chat, written as the plan starts so a run cut
// mid-execute can still resume; its own table keeps the blob off every dag_plans read.
type DagExecPlan struct {
	ChatID string `gorm:"primaryKey"`
	// Chat is constraint-only - see ChatTurn.Chat's doc.
	Chat     Chat `gorm:"foreignKey:ChatID;references:ID;constraint:OnDelete:CASCADE"`
	PlanID   string
	PlanJSON string
}

// ChatEvent backs the hub's durable replay after restart. Cleared on new run per chat.
type ChatEvent struct {
	ChatID string `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	// Chat is constraint-only - see ChatTurn.Chat's doc.
	Chat      Chat      `gorm:"foreignKey:ChatID;references:ID;constraint:OnDelete:CASCADE" json:"-"`
	Seq       int64     `gorm:"primaryKey;autoIncrement:false" json:"seq"`
	Event     string    `json:"event"`
	CreatedAt time.Time `json:"created_at"`
}

// ProjectionWatermark tracks how far one chat's projection (sse, artifact, node) has folded the WAL; the writer
// advances FoldedSeq in the same transaction as its own write, so a restart resumes the fold from here.
type ProjectionWatermark struct {
	ChatID string `gorm:"column:chat_id;primaryKey" json:"chat_id"`
	// Chat is constraint-only - see ChatTurn.Chat's doc.
	Chat       Chat      `gorm:"foreignKey:ChatID;references:ID;constraint:OnDelete:CASCADE" json:"-"`
	Projection string    `gorm:"column:projection;primaryKey" json:"projection"`
	FoldedSeq  int64     `gorm:"column:folded_seq" json:"folded_seq"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// DagNode stores the execution state of one DAG node.
type DagNode struct {
	NodeID string `gorm:"primaryKey;column:node_id" json:"node_id"`
	// PK leads with node_id, so plan_id lookups (chat-list usage totals) seq-scan without this index.
	PlanID        string `gorm:"primaryKey;column:plan_id;index:idx_dag_nodes_plan_id" json:"plan_id"`
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
	// OTel trace id for a deep link (see stream.NodeStartData); "" when otel is disabled.
	TraceID string `gorm:"column:trace_id" json:"trace_id,omitempty"`
	// node_start owns trace_id and must be able to clear a previous run's id;
	// every other upsert simply omits the column.
	TraceIDSet bool `gorm:"-" json:"-"`
	// Lifecycle columns below are written only by SetNodeStatus/SetNodeQueue, so a stream-driven upsert can never
	// blank a pause a resume depends on. PauseReason: user | shutdown | awaiting_input.
	PauseReason string `gorm:"column:pause_reason" json:"pause_reason,omitempty"`
	// HITL question this node is parked on (node-scoped; chats.pending_question is the legacy chat-scoped copy).
	PendingQuestion string `gorm:"column:pending_question" json:"pending_question,omitempty"`
	// JSON []dag.QueuedMessage - the steer queue, so a queued message survives a restart.
	QueuedMessages string `gorm:"column:queued_messages" json:"-"`
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
	// Answer is the turn's delivered DAG answer (stream.DeliveredAnswerMeta), "" when none;
	// AnswerAt is when it was delivered.
	Answer    string
	AnswerAt  time.Time
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

// turnGroup is one turn's content extracted from session events. Builders, not strings: appending streamed
// chunks with += is O(n^2) per turn (197 MB for one 2,000-event turn).
type turnGroup struct {
	userText, asstText, asstThink                                              strings.Builder
	toolCalls                                                                  []ToolCallRecord
	promptTokens, completionTokens, reasoningTokens, cachedTokens, totalTokens int32
	// answers: delivered answers seen in this group, by the turn id their marker names ("" = this group's).
	answers map[string]markedAnswer
}

type markedAnswer struct {
	text string
	at   time.Time
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
				appendUserPart(cur, p)
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
			addUsage(cur, ev.UsageMetadata)
		}
		if turnID, ok := deliveredTurn(ev); ok {
			// Not narration: a retry's answer belongs to its plan's turn, and the last one wins.
			if cur.answers == nil {
				cur.answers = map[string]markedAnswer{}
			}
			keepLatest(cur.answers, turnID, markedAnswer{text: plainText(ev.Content), at: deliveredAt(ev)})
			continue
		}
		for _, p := range ev.Content.Parts {
			recordAssistantPart(cur, p)
		}
	}
	return groups
}

// deliveredTurn reads a delivered-answer marker: the turn id it names, "" for the event's own turn.
func deliveredTurn(ev *session.Event) (string, bool) {
	switch v := ev.CustomMetadata[stream.DeliveredAnswerMeta].(type) {
	case string:
		return v, true
	case bool:
		return "", v
	}
	return "", false
}

// deliveredAt is the marker's own delivery time, falling back to the event's (coarser) timestamp.
func deliveredAt(ev *session.Event) time.Time {
	if s, ok := ev.CustomMetadata[stream.DeliveredAtMeta].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return ev.Timestamp
}

// keepLatest stores a unless m already holds a later delivery for key; on a tie the one seen
// later in session order wins.
func keepLatest(m map[string]markedAnswer, key string, a markedAnswer) {
	if prev, ok := m[key]; !ok || !a.at.Before(prev.at) {
		m[key] = a
	}
}

func plainText(c *genai.Content) string {
	var sb strings.Builder
	for _, p := range c.Parts {
		if p != nil && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// keyedAnswers merges every group's turn-keyed delivered answers; the latest delivery wins.
func keyedAnswers(groups []turnGroup) map[string]markedAnswer {
	out := map[string]markedAnswer{}
	for _, g := range groups {
		for turnID, a := range g.answers {
			if turnID != "" {
				keepLatest(out, turnID, a)
			}
		}
	}
	return out
}

// appendUserPart: one user-message part into cur.userText - plain text, or the
// FunctionResponse payload of a clarification/HITL answer (Role:user).
func appendUserPart(cur *turnGroup, p *genai.Part) {
	if p == nil {
		return
	}
	// Clarification or HITL answer arrives as FunctionResponse (Role:user).
	if p.FunctionResponse != nil {
		switch p.FunctionResponse.Name {
		case choiceToolName:
			if c, ok := p.FunctionResponse.Response[choiceAnswerKey].(string); ok {
				cur.userText.WriteString(c)
			}
		case nodeInputCallName:
			if c, ok := p.FunctionResponse.Response[nodeInputPayloadKey].(string); ok {
				cur.userText.WriteString(c)
			}
		}
		return
	}
	if !p.Thought && p.FunctionCall == nil {
		cur.userText.WriteString(p.Text)
	}
}

// addUsage: fold one event's usage metadata into cur's token counters.
func addUsage(cur *turnGroup, u *genai.GenerateContentResponseUsageMetadata) {
	cur.promptTokens += u.PromptTokenCount
	cur.completionTokens += u.CandidatesTokenCount
	cur.reasoningTokens += u.ThoughtsTokenCount
	cur.cachedTokens += u.CachedContentTokenCount
	cur.totalTokens += u.TotalTokenCount
}

// recordAssistantPart: one gate-author part into cur - tool call, paired tool
// result, thought, or plain assistant text.
func recordAssistantPart(cur *turnGroup, p *genai.Part) {
	if p == nil {
		return
	}
	switch {
	case p.FunctionCall != nil:
		if p.FunctionCall.Name == transferTool {
			return
		}
		cur.toolCalls = append(cur.toolCalls, ToolCallRecord{
			CallID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Args: p.FunctionCall.Args,
		})
	case p.FunctionResponse != nil:
		if p.FunctionResponse.Name == transferTool {
			return
		}
		// Pair to the earlier call by ID (a call always precedes its response).
		for i := range cur.toolCalls {
			if cur.toolCalls[i].CallID == p.FunctionResponse.ID {
				cur.toolCalls[i].Result = p.FunctionResponse.Response
				break
			}
		}
	case p.Thought:
		cur.asstThink.WriteString(p.Text)
	default:
		cur.asstText.WriteString(p.Text)
	}
}

// Store wraps the relational DB and ADK session service.
type Store struct {
	db       *gorm.DB
	Sessions session.Service
	// Identifies this store for node-ownership tracking (random per New, or overridden).
	instanceID string
	// artifacts: nil unless SetArtifactService was called - DeleteChat cascades into it when set.
	artifacts artifact.Service
	// walLedger: the WAL's fail-closed AppendIntent path for chat/turn creation and plan saves; nil = no WAL.
	walLedger ledger.LedgerStore
}

// SetWALLedger wires the WAL into CreateChat/SaveTurn/SaveDagPlan. Pass nil unless store is a postgres-backed
// LedgerStore, same as dag.Executor.SetWALLedger/recordstore.WithLedger.
func (s *Store) SetWALLedger(store ledger.LedgerStore) { s.walLedger = store }

// Checkpoint is chatID's last folded ledger state: one row replaced every turn end (derived state, not a WAL fact).
// LastSeq mirrors Payload for inspection only; fold trusts Payload's own LastSeq, so a torn write stays safe.
type Checkpoint struct {
	ChatID string `gorm:"column:chat_id;primaryKey"`
	// Chat is constraint-only - see ChatTurn.Chat's doc.
	Chat          Chat   `gorm:"foreignKey:ChatID;references:ID;constraint:OnDelete:CASCADE"`
	LastSeq       int64  `gorm:"column:last_seq"`
	SchemaVersion int    `gorm:"column:schema_version"`
	Payload       string `gorm:"column:payload"`
	UpdatedAt     time.Time
}

func (Checkpoint) TableName() string { return "ledger_checkpoints" }

// loadCheckpointSeed returns nil on any problem: the fallback is folding from scratch. Callers pass 0 as `from`,
// not the seed's LastSeq, or ApplySeeded's "seed only if newer than from" guard skips seeding.
func (s *Store) loadCheckpointSeed(ctx context.Context, chatID string) *fold.Result {
	var row Checkpoint
	if err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Take(&row).Error; err != nil {
		return nil
	}
	var res fold.Result
	if err := json.Unmarshal([]byte(row.Payload), &res); err != nil {
		return nil
	}
	return &res
}

// upsertCheckpoint replaces chatID's single checkpoint row.
func (s *Store) upsertCheckpoint(ctx context.Context, chatID string, lastSeq int64, payload []byte) error {
	row := Checkpoint{ChatID: chatID, LastSeq: lastSeq, SchemaVersion: ledger.EntrySchemaVersion, Payload: string(payload), UpdatedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_seq", "schema_version", "payload", "updated_at"}),
	}).Create(&row).Error
}

// WriteCheckpoint folds chatID from its last checkpoint and replaces the row, so the next fold starts here.
// Called at every turn end; a no-op without a WAL. Best-effort: a failed write only costs a slower fold.
func (s *Store) WriteCheckpoint(ctx context.Context, chatID string) error {
	if s.walLedger == nil {
		return nil
	}
	seed := s.loadCheckpointSeed(ctx, chatID)
	res, err := fold.ApplySeeded(ctx, s.walLedger, chatID, seed, 0)
	if err != nil {
		return fmt.Errorf("store: checkpoint fold for chat %q: %w", chatID, err)
	}
	payload, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("store: checkpoint encode for chat %q: %w", chatID, err)
	}
	if err := s.upsertCheckpoint(ctx, chatID, res.LastSeq, payload); err != nil {
		return fmt.Errorf("store: checkpoint upsert for chat %q: %w", chatID, err)
	}
	return nil
}

// SetArtifactService wires the artifact service DeleteChat cascades into; it is built separately and may be
// a different store than this one.
func (s *Store) SetArtifactService(svc artifact.Service) { s.artifacts = svc }

// Artifacts returns the wired artifact service, nil if SetArtifactService was never called.
func (s *Store) Artifacts() artifact.Service { return s.artifacts }

// chatFKTables are the tables gaining the chats(id) ON DELETE CASCADE FK, in New()'s migration order.
var chatFKTables = []string{"chat_turns", "dag_plans", "chat_events", "projection_watermarks", "ledger_checkpoints", "dag_exec_plans"}

// chatFKModels maps each of chatFKTables to the struct sweepOrphanChatRows checks
// HasConstraint against - every one carries a field named "Chat" for exactly this FK.
var chatFKModels = map[string]any{
	"chat_turns":            &ChatTurn{},
	"dag_plans":             &DagPlan{},
	"chat_events":           &ChatEvent{},
	"projection_watermarks": &ProjectionWatermark{},
	"ledger_checkpoints":    &Checkpoint{},
	"dag_exec_plans":        &DagExecPlan{},
}

// sweepOrphanChatRows deletes rows with no owning chat before AutoMigrate adds the cascade FK, which an orphan
// would fail (a boot crash loop). Skips a table once its constraint exists: the anti-join scan is costly.
func sweepOrphanChatRows(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Chat{}) {
		return nil
	}
	for _, table := range chatFKTables {
		if !db.Migrator().HasTable(table) {
			continue
		}
		if db.Migrator().HasConstraint(chatFKModels[table], "Chat") {
			continue
		}
		res := db.Exec(fmt.Sprintf("DELETE FROM %s WHERE chat_id NOT IN (SELECT id FROM chats)", table))
		if res.Error != nil {
			return fmt.Errorf("store: sweep orphan %s rows: %w", table, res.Error)
		}
		if res.RowsAffected > 0 {
			slog.Warn("store: swept orphan rows with no owning chat before migration",
				"component", "store", "table", table, "count", res.RowsAffected)
		}
	}
	return nil
}

// New opens the persistence store, runs migrations, and returns it.
func New(kind, url string) (*Store, error) {
	dialector, err := dialectorFor(kind, url)
	if err != nil {
		return nil, err
	}
	gormCfg := &gorm.Config{Logger: slogGormLogger(), TranslateError: true}
	db, err := gorm.Open(dialector(), gormCfg)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := sweepOrphanChatRows(db); err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&Chat{}, &ChatTurn{}, &DagPlan{}, &DagNode{}, &ChatEvent{}, &MemoryOp{}, &ProjectionWatermark{}, &Checkpoint{}, &DagExecPlan{}); err != nil {
		return nil, err
	}
	sessions, err := database.NewSessionService(dialector(), gormCfg)
	if err != nil {
		return nil, err
	}
	if err := database.AutoMigrate(sessions); err != nil {
		return nil, err
	}
	// The ADK events PK leads with id, so Sessions.Get's WHERE on the other three columns walks the whole index.
	// Additive, idempotent.
	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_events_session_lookup ON events (app_name, user_id, session_id, timestamp)").Error; err != nil {
		return nil, err
	}
	s.Sessions = sessions
	s.instanceID = uuid.NewString()
	return s, nil
}

// DB exposes the underlying *gorm.DB for a caller owning its own schema on this database (pluginreg's DB
// backends) instead of opening a second pool.
func (s *Store) DB() *gorm.DB { return s.db }

// InstanceID identifies this Store for node-ownership tracking.
func (s *Store) InstanceID() string { return s.instanceID }

// SetInstanceID overrides the random default. Call once with a persisted identity
// (LoadOrCreateInstanceID) before any node writes; ephemeral CLIs keep the default.
func (s *Store) SetInstanceID(id string) { s.instanceID = id }

// slogGormLogger routes GORM's slow-query warnings through slog, for New and the standalone artifact opener.
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
		if _, err := pgdial.Open(url); err != nil {
			return nil, fmt.Errorf("store: parse postgres url: %w", err)
		}
		return func() gorm.Dialector {
			// Reparses url each call: New() calls this factory twice and each needs its own *sql.DB pool.
			d, _ := pgdial.Open(url)
			return d
		}, nil
	case "sqlite":
		sqlDB, err := sql.Open(sqlite.DriverName, sqlitedsn.Build(url))
		if err != nil {
			return nil, fmt.Errorf("store: open sqlite: %w", err)
		}
		sqlDB.SetMaxOpenConns(1) // one writer; serialize the two pools onto it
		return func() gorm.Dialector { return &sqlite.Dialector{Conn: sqlDB} }, nil
	default:
		return nil, fmt.Errorf("store: unsupported kind %q (postgres or sqlite)", kind)
	}
}

// CreateChat inserts a new chat. Fail-closed on the WAL: with a ledger wired, the chat.created intent must
// land first, so a failed append creates no chat.
func (s *Store) CreateChat(ctx context.Context, systemPrompt string) (*Chat, error) {
	now := time.Now().UTC()
	id := uuid.NewString()
	if s.walLedger != nil {
		payload, err := json.Marshal(struct {
			SystemPrompt string `json:"system_prompt"`
		}{systemPrompt})
		if err != nil {
			return nil, fmt.Errorf("store: marshal chat.created payload: %w", err)
		}
		if _, err := s.walLedger.AppendIntent(ctx, ledger.Entry{
			ChatID: id, Kind: ledger.KindChatCreated, Key: id, At: now, Payload: payload,
		}); err != nil {
			return nil, fmt.Errorf("store: chat.created WAL append: %w", err)
		}
	}
	c := &Chat{ID: id, SystemPrompt: systemPrompt, CreatedAt: now, UpdatedAt: now}
	if err := s.db.WithContext(ctx).Create(c).Error; err != nil {
		return nil, err
	}
	return c, nil
}

// ChatsPageDefaultLimit is used when a caller passes limit <= 0.
const ChatsPageDefaultLimit = 20

// ChatsPageMaxLimit bounds limit regardless of what a caller requests.
const ChatsPageMaxLimit = 100

// ErrInvalidPageToken: the token doesn't decode, or was issued under a different ordering or scope.
var ErrInvalidPageToken = errors.New("invalid page token")

// chatsSort names the ordering a page token was issued under, so a token is never replayed against another.
type chatsSort string

const chatsSortUpdatedAtDesc chatsSort = "updated_at_desc"

// ChatsScope selects which chats ListChats considers, in the SQL predicate. Zero value means Active-only;
// both true means no archived predicate at all.
type ChatsScope struct {
	Active   bool `json:"a"`
	Archived bool `json:"r"`
}

// chatsPageToken anchors on the immutable ID; UpdatedAt rides along only to locate that ID in the
// updated_at-sorted list. Scope is a struct of flags, so its JSON is canonical.
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

// decodeChatsPageToken rejects a token issued for another sort or scope; a zero Scope (minted before
// scoping) is read as {Active: true}.
func decodeChatsPageToken(s string, scope ChatsScope) (chatsPageToken, error) {
	var t chatsPageToken
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return t, fmt.Errorf("%w: %w", ErrInvalidPageToken, err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, fmt.Errorf("%w: %w", ErrInvalidPageToken, err)
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

// ListChats returns up to limit chats in scope, newest-updated first, after pageToken, plus the next token
// ("" at the end). Keyset pagination with the scope in SQL, so pages are always full and never skip rows.
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

// rowsExist reports whether any row of model matches "col = val".
func (s *Store) rowsExist(ctx context.Context, model any, col, val string) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(model).Where(col+" = ?", val).Count(&count).Error
	return count > 0, err
}

// ChatExists reports whether id has a live chats row, so boot resume never revives a chat hard-deleted by raw
// SQL from leftover dag_plans/dag_nodes rows.
func (s *Store) ChatExists(ctx context.Context, id string) (bool, error) {
	return s.rowsExist(ctx, &Chat{}, "id", id)
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
		if err := tx.Where("chat_id = ?", id).Delete(&DagExecPlan{}).Error; err != nil {
			return err
		}
		if err := tx.Where("chat_id = ?", id).Delete(&ChatTurn{}).Error; err != nil {
			return err
		}
		if err := tx.Where("chat_id = ?", id).Delete(&ChatEvent{}).Error; err != nil {
			return err
		}
		// Otherwise a leaked row per deleted chat: UUIDv4 ids never
		// reuse, so nothing would ever read it back.
		if err := tx.Where("chat_id = ?", id).Delete(&Checkpoint{}).Error; err != nil {
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
	reapNodeSessionsWarn(s, ctx, id, "chat deleted")
	s.deleteChatArtifacts(ctx, id, sessionUser)
	return nil
}

// reapNodeSessionsWarn: the best-effort per-node worker-session reaping shared
// by ArchiveChat and DeleteChat - a reap failure must never fail the chat op.
func reapNodeSessionsWarn(s *Store, ctx context.Context, id, what string) {
	if err := s.ReapNodeSessions(ctx, id); err != nil {
		slog.Warn(what+" but its per-node worker sessions could not be reaped",
			"component", "store", "chat", id, "err", err)
	}
}

// ReapNodeSessions deletes the chat's per-node worker ("<chatID>:<nodeID>") and retry ("<chatID>::retry") sessions.
// Hard-codes ADK's session schema: one sweep on sessions.id across app/user, events cascade on delete.
func (s *Store) ReapNodeSessions(ctx context.Context, chatID string) error {
	return s.db.WithContext(ctx).Exec("DELETE FROM sessions WHERE id = ? OR id LIKE ? ESCAPE '\\'",
		chatID, likeEscape(chatID)+":%").Error
}

// likeEscape escapes LIKE wildcards so a chat id containing % or _ can't widen a prefix match to another
// chat's rows. Pair with `ESCAPE '\'`.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// deleteChatArtifacts best-effort cascades chat deletion into the artifact service: log and move on.
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

// SetChatOrigin upserts an extension chat's SessionUser and Origin JSON, creating the row if missing
// (dispatch may precede the chat); SessionUser is fixed at creation.
func (s *Store) SetChatOrigin(ctx context.Context, id, sessionUser, originJSON string) error {
	now := time.Now().UTC()
	c := &Chat{ID: id, CreatedAt: now, UpdatedAt: now, SessionUser: sessionUser, Origin: originJSON}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"origin", "updated_at"}),
	}).Create(c).Error
}

// MaxTitleLen caps every persisted chat title, one backstop for every caller (including a titler that ignores
// its own "3-6 words" instruction).
const MaxTitleLen = 80

// UpdateTitle sets the human-readable title for a chat, truncated to
// MaxTitleLen runes (see its doc comment).
func (s *Store) UpdateTitle(ctx context.Context, id, title string) error {
	return s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", id).Update("title", truncateTitle(title, MaxTitleLen)).Error
}

// truncateTitle cuts title to maxLen runes, backing off to the last space so it doesn't sever mid-word,
// then appends an ellipsis.
func truncateTitle(title string, maxLen int) string {
	r := []rune(title)
	if len(r) <= maxLen {
		return title
	}
	cut := maxLen - 1 // room for the ellipsis
	if cut < 0 {
		cut = 0
	}
	if sp := strings.LastIndex(string(r[:cut]), " "); sp > 0 {
		cut = len([]rune(string(r[:cut])[:sp]))
	}
	return string(r[:cut]) + "…"
}

// ArchiveChat uses UpdateColumn, which skips GORM's UpdatedAt stamp, so archiving doesn't reorder the list.
// Archiving also best-effort reaps the chat's per-node worker sessions: nothing reuses them after archive.
func (s *Store) ArchiveChat(ctx context.Context, id string, archived bool) error {
	if err := s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", id).UpdateColumn("archived", archived).Error; err != nil {
		return err
	}
	if archived {
		reapNodeSessionsWarn(s, ctx, id, "chat archived")
	}
	return nil
}

// SaveTurn persists a new turn at the next sequence. userText lives on the row so the view survives a
// ResetHistory wiping the session events.
func (s *Store) SaveTurn(ctx context.Context, chatID, turnID, userText string) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&ChatTurn{}).Where("chat_id = ?", chatID).Count(&count).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	if s.walLedger != nil {
		payload, err := json.Marshal(struct {
			UserText string `json:"user_text"`
			Seq      int    `json:"seq"`
		}{userText, int(count)})
		if err != nil {
			return fmt.Errorf("store: marshal turn.created payload: %w", err)
		}
		if _, err := s.walLedger.AppendIntent(ctx, ledger.Entry{
			ChatID: chatID, TurnID: turnID, Kind: ledger.KindTurnCreated, Key: turnID, At: now, Payload: payload,
		}); err != nil {
			return fmt.Errorf("store: turn.created WAL append: %w", err)
		}
	}
	t := &ChatTurn{ID: turnID, ChatID: chatID, Seq: int(count), CreatedAt: now, UserText: userText}
	return s.db.WithContext(ctx).Create(t).Error
}

// TurnUsage is the orchestrator's own per-turn token usage (DAG turns credit
// tokens per-node on DagNode instead).
type TurnUsage struct {
	PromptTokens, CompletionTokens, ReasoningTokens, TotalTokens, CachedTokens int32
}

// SetTurnUsage stamps model + token usage on the turn row at run end; ADK drops both on read.
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

// SaveDagPlan persists a DAG plan linked to the turn that created it. A later turn extending it updates
// plan_json and LastTurnID; a boot resume re-yielding the same plan changes nothing.
func (s *Store) SaveDagPlan(ctx context.Context, chatID, planID, turnID, planJSON string) error {
	now := time.Now().UTC()
	if s.walLedger != nil {
		payload, err := json.Marshal(struct {
			PlanJSON string `json:"plan_json"`
			TurnID   string `json:"turn_id"`
		}{planJSON, turnID})
		if err != nil {
			return fmt.Errorf("store: marshal plan.saved payload: %w", err)
		}
		// IdempotencyKey = planID: a boot resume re-saves the same plan, so the WAL append must be skip-if-exists
		// like the DB write, or each resume adds a plan.saved entry.
		_, err = s.walLedger.AppendIntent(ctx, ledger.Entry{
			ChatID: chatID, TurnID: turnID, Kind: ledger.KindPlanSaved, Key: planID,
			At: now, Payload: payload, IdempotencyKey: "plan.saved:" + planID + ":" + contentKey(turnID, planJSON),
		})
		var dup *ledger.DuplicateIntentError
		if err != nil && !errors.As(err, &dup) {
			return fmt.Errorf("store: plan.saved WAL append: %w", err)
		}
	}
	p := &DagPlan{ID: planID, ChatID: chatID, TurnID: turnID, LastTurnID: turnID, PlanJSON: planJSON, CreatedAt: now}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_turn_id", "plan_json"}),
	}).Create(p).Error
}

// contentKey is a short hash of a plan save's turn and body, so only a changed plan re-logs its intent.
func contentKey(turnID, planJSON string) string {
	sum := sha256.Sum256([]byte(turnID + "\x00" + planJSON))
	return hex.EncodeToString(sum[:8])
}

// SaveExecPlan records chatID's latest full dag.Plan JSON, replacing any earlier plan's
// (and an earlier step of the same growing plan's).
func (s *Store) SaveExecPlan(ctx context.Context, chatID, planID, execJSON string) error {
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"plan_id", "plan_json"}),
	}).Create(&DagExecPlan{ChatID: chatID, PlanID: planID, PlanJSON: execJSON}).Error
}

// LoadExecPlan returns planID's full dag.Plan as SaveExecPlan stored it; false when there
// is none (superseded by a later plan, or run before this table existed).
func (s *Store) LoadExecPlan(ctx context.Context, planID string) (dag.Plan, bool) {
	var row DagExecPlan
	if err := s.db.WithContext(ctx).Where("plan_id = ?", planID).First(&row).Error; err != nil {
		return dag.Plan{}, false
	}
	var plan dag.Plan
	if err := json.Unmarshal([]byte(row.PlanJSON), &plan); err != nil || plan.ID != planID {
		return dag.Plan{}, false
	}
	return plan, true
}

// UpsertDagNode creates or updates a DAG node's execution state.
func (s *Store) UpsertDagNode(ctx context.Context, node DagNode) error {
	// One Omit call: gorm's Omit REPLACES the list rather than appending, so
	// chained calls silently drop every earlier guard.
	omit := []string{"pause_reason", "pending_question", "queued_messages"}
	// Never let a nil StartedAt/InstanceID erase a real one from an earlier write.
	if node.StartedAt == nil {
		omit = append(omit, "started_at")
	}
	if node.InstanceID == "" {
		omit = append(omit, "instance_id")
	}
	if !node.TraceIDSet {
		omit = append(omit, "trace_id")
	}
	t := time.Now().UTC()
	node.UpdatedAt = &t
	return s.db.WithContext(ctx).Omit(omit...).Save(&node).Error
}

// InsertChatEvent persists one run event. Caller assigns Seq and serializes inserts.
func (s *Store) InsertChatEvent(ctx context.Context, ev ChatEvent) error {
	return s.db.WithContext(ctx).Create(&ev).Error
}

// InsertChatEvents persists a batch of run events in one multi-row INSERT (single-row inserts capped the drain
// at ~139 ev/s).
func (s *Store) InsertChatEvents(ctx context.Context, evs []ChatEvent) error {
	if len(evs) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Create(&evs).Error
}

// LoadChatEvents returns events with seq > afterSeq (afterSeq=0 for full run).
func (s *Store) LoadChatEvents(ctx context.Context, chatID string, afterSeq int64) ([]ChatEvent, error) {
	var evs []ChatEvent
	err := s.db.WithContext(ctx).
		Where("chat_id = ? AND seq > ?", chatID, afterSeq).
		Order("seq asc").Find(&evs).Error
	return evs, err
}

// ChatEventsExist reports whether chatID has any SSE row regardless of seq: a chat with rows but none newer
// than fromSeq is caught up, and must not resend the whole reconstructed history.
func (s *Store) ChatEventsExist(ctx context.Context, chatID string) (bool, error) {
	return s.rowsExist(ctx, &ChatEvent{}, "chat_id", chatID)
}

// DeleteChatEvents drops a chat's run events (fresh start for new run).
func (s *Store) DeleteChatEvents(ctx context.Context, chatID string) error {
	return s.db.WithContext(ctx).Where("chat_id = ?", chatID).Delete(&ChatEvent{}).Error
}

// TrimChatEvents drops events at or below upToSeq (window long runs to replay ceiling).
func (s *Store) TrimChatEvents(ctx context.Context, chatID string, upToSeq int64) error {
	return s.db.WithContext(ctx).Where("chat_id = ? AND seq <= ?", chatID, upToSeq).Delete(&ChatEvent{}).Error
}

// InTx runs fn in one transaction, so a projection write and its watermark advance commit atomically.
func (s *Store) InTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return s.db.WithContext(ctx).Transaction(fn)
}

// InsertChatEventTx is InsertChatEvent against an existing transaction.
func InsertChatEventTx(tx *gorm.DB, ev ChatEvent) error {
	return tx.Create(&ev).Error
}

// GetProjectionWatermark returns projection's folded_seq for chatID, 0 if no row exists yet.
func (s *Store) GetProjectionWatermark(ctx context.Context, chatID, projection string) (int64, error) {
	var w ProjectionWatermark
	err := s.db.WithContext(ctx).Where("chat_id = ? AND projection = ?", chatID, projection).First(&w).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return w.FoldedSeq, err
}

// SetProjectionWatermarkTx upserts (chatID, projection)'s folded_seq in tx, alongside the caller's own
// projection write.
func SetProjectionWatermarkTx(tx *gorm.DB, chatID, projection string, foldedSeq int64) error {
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}, {Name: "projection"}},
		DoUpdates: clause.AssignmentColumns([]string{"folded_seq", "updated_at"}),
	}).Create(&ProjectionWatermark{ChatID: chatID, Projection: projection, FoldedSeq: foldedSeq, UpdatedAt: time.Now().UTC()}).Error
}

// SetProjectionWatermark upserts (chatID, projection)'s folded_seq outside
// any caller-owned transaction - `quack ledger rebuild`'s own writes.
func (s *Store) SetProjectionWatermark(ctx context.Context, chatID, projection string, foldedSeq int64) error {
	return SetProjectionWatermarkTx(s.db.WithContext(ctx), chatID, projection, foldedSeq)
}

// UpsertNodeTerminalStatusTx sets a node's done/failed status in tx, bypassing SetNodeStatus's transitions,
// for `quack ledger rebuild`. Creates a lost row with status only (same lossy ceiling as fold.NodeState).
func UpsertNodeTerminalStatusTx(tx *gorm.DB, planID, nodeID, status string) error {
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "plan_id"}, {Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"status"}),
	}).Create(&DagNode{PlanID: planID, NodeID: nodeID, Status: status}).Error
}

// ResetProjectionWatermark sets (chatID, projection) to 0 so the next fold repopulates it.
func (s *Store) ResetProjectionWatermark(ctx context.Context, chatID, projection string) error {
	return SetProjectionWatermarkTx(s.db.WithContext(ctx), chatID, projection, 0)
}

// SeedProjectionWatermarks marks existing chats caught up to their ledger MAX(seq) (never chat_events.seq: a
// per-run counter), so a first watermark-gated fold never replays live-written rows. Idempotent.
func (s *Store) SeedProjectionWatermarks(ctx context.Context, ledgerStore ledger.LedgerStore) error {
	seeds := []struct {
		projection, listChats string
	}{
		// "sse" reads chats, not chat_events: every chat_events row has a chats row and chats.id is the PK,
		// avoiding a huge DISTINCT scan.
		{"sse", "SELECT id AS chat_id FROM chats"},
		{"artifact", "SELECT DISTINCT session_id AS chat_id FROM artifacts"},
		{"node_state", "SELECT DISTINCT dp.chat_id AS chat_id FROM dag_nodes dn JOIN dag_plans dp ON dn.plan_id = dp.id"},
	}
	now := time.Now().UTC()
	// A chat can appear in several projections and MaxSeq is a round trip, so cache it.
	maxSeqCache := map[string]int64{}
	for _, sd := range seeds {
		var chatIDs []string
		if err := s.db.WithContext(ctx).Raw(sd.listChats).Scan(&chatIDs).Error; err != nil {
			return fmt.Errorf("store: list chats for %s projection seed: %w", sd.projection, err)
		}
		for _, chatID := range chatIDs {
			maxSeq, ok := maxSeqCache[chatID]
			if !ok {
				var err error
				maxSeq, err = ledgerStore.MaxSeq(ctx, chatID)
				if err != nil {
					return fmt.Errorf("store: max seq for %s seed (chat %s): %w", sd.projection, chatID, err)
				}
				maxSeqCache[chatID] = maxSeq
			}
			if maxSeq == 0 {
				continue // no ledger history for this chat yet; nothing to catch up on
			}
			err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "chat_id"}, {Name: "projection"}},
				DoNothing: true,
			}).Create(&ProjectionWatermark{ChatID: chatID, Projection: sd.projection, FoldedSeq: maxSeq, UpdatedAt: now}).Error
			// artifact/node_state read chat ids from tables the chats FK doesn't cover; skip a dangling one instead of
			// aborting every other chat's seed.
			if errors.Is(err, gorm.ErrForeignKeyViolated) {
				slog.Warn("projection watermark seed: chat row is gone; skipping", "component", "store", "projection", sd.projection, "chat", chatID)
				continue
			}
			if err != nil {
				return fmt.Errorf("store: seed %s projection watermark (chat %s): %w", sd.projection, chatID, err)
			}
		}
	}
	return nil
}

// staleNodeCeiling: dead-man's-switch for orphaned nodes. Generous (runs finish in minutes).
const staleNodeCeiling = 12 * time.Hour

// ResumableNode names one node a boot reconcile decided to hand back to the
// scheduler, with the chat it belongs to.
type ResumableNode struct {
	ChatID, PlanID, NodeID string
	Reason                 dag.PauseReason
}

// UnresumableNode is a node the reconcile could not hand back, and why.
type UnresumableNode struct {
	PlanID, NodeID, Reason string
}

// ResumeReport is what a boot reconcile did, for the caller to log and act on.
type ResumeReport struct {
	Start         []ResumableNode   // paused/user | paused/shutdown | hard-kill orphans - re-enter the graph
	AwaitingInput []ResumableNode   // paused/awaiting_input - re-armed, NOT started; needs an answer
	Failed        []UnresumableNode // genuinely unresumable, marked failed with the reason in `error`
}

// ResumePausedDagNodes resumes what the last process left suspended: paused/needs_input nodes, and running ones
// from this instance's previous life or past staleNodeCeiling (re-stamped paused/shutdown). Queued nodes wait.
func (s *Store) ResumePausedDagNodes(ctx context.Context, resumable func(chatID, pauseReason string) (bool, string)) (ResumeReport, error) {
	var rep ResumeReport
	cutoff := time.Now().UTC().Add(-staleNodeCeiling)
	var nodes []DagNode
	// IS NULL covers no-default added-column rows. In a shared DB a booting instance must not resume a live
	// peer's nodes; a dead peer's are picked up past staleNodeCeiling.
	owned := s.db.Where("instance_id IS NULL OR instance_id = ? OR instance_id = ? OR updated_at < ?", "", s.instanceID, cutoff)
	err := s.db.WithContext(ctx).Model(&DagNode{}).
		Where("status IN ?", []string{string(dag.StatusPaused), string(dag.StatusNeedsInput), string(dag.StatusRunning)}).
		Where(owned).
		Find(&nodes).Error
	if err != nil {
		return rep, err
	}

	chatOf := map[string]string{} // planID -> chatID, "" = plan row gone
	chatLive := map[string]bool{} // chatID -> chats row still exists
	for _, n := range nodes {
		chatID, ok := chatOf[n.PlanID]
		if !ok {
			var p DagPlan
			if e := s.db.WithContext(ctx).Where("id = ?", n.PlanID).First(&p).Error; e == nil {
				chatID = p.ChatID
			}
			chatOf[n.PlanID] = chatID
		}
		if chatID == "" {
			rep.Failed = append(rep.Failed, s.FailUnresumable(ctx, "", n, "plan row is gone"))
			continue
		}
		// Never resume into a chat deleted by raw SQL (bypassing the cascade), or the run writes a phantom's events.
		live, ok := chatLive[chatID]
		if !ok {
			var e error
			live, e = s.ChatExists(ctx, chatID)
			if e != nil {
				live = true // DB hiccup: fail open to the existing behavior, not a false skip
			}
			chatLive[chatID] = live
		}
		if !live {
			slog.Warn("resume paused dag nodes: chat row is gone; skipping", "component", "store",
				"chat", chatID, "plan", n.PlanID, "node", n.NodeID)
			rep.Failed = append(rep.Failed, s.FailUnresumable(ctx, chatID, n, "chat row is gone"))
			continue
		}
		if resumable != nil {
			if ok, why := resumable(chatID, n.PauseReason); !ok {
				rep.Failed = append(rep.Failed, s.FailUnresumable(ctx, chatID, n, why))
				continue
			}
		}
		reason := dag.PauseReason(n.PauseReason)
		status := dag.NodeStatus(n.Status)
		if status == dag.StatusRunning {
			// Hard kill: no shutdown ran, so nothing stamped the pause. Do it now.
			reason, status = dag.PauseShutdown, dag.StatusPaused
			if e := s.SetNodeStatus(ctx, n.PlanID, n.NodeID, dag.StatusPaused, reason, n.PendingQuestion); e != nil {
				slog.Warn("resume paused dag nodes: hard-kill re-stamp failed", "component", "store",
					"plan", n.PlanID, "node", n.NodeID, "err", e)
				continue
			}
		}
		s.syncDagNodeRecord(ctx, chatID, n.PlanID, n.NodeID, status)
		rn := ResumableNode{ChatID: chatID, PlanID: n.PlanID, NodeID: n.NodeID, Reason: reason}
		if reason == dag.PauseAwaitingInput || n.Status == string(dag.StatusNeedsInput) {
			rep.AwaitingInput = append(rep.AwaitingInput, rn)
			continue
		}
		rep.Start = append(rep.Start, rn)
	}
	return rep, nil
}

// FailUnresumable marks one node failed with why in `error`, on both the row and
// the dag_node record, so no later boot tries to resume it again.
func (s *Store) FailUnresumable(ctx context.Context, chatID string, n DagNode, why string) UnresumableNode {
	if err := s.db.WithContext(ctx).Model(&DagNode{}).
		Where("plan_id = ? AND node_id = ?", n.PlanID, n.NodeID).
		Updates(map[string]any{"status": string(dag.StatusFailed), "error": "cannot resume: " + why}).Error; err != nil {
		slog.Warn("resume paused dag nodes: fail stamp failed", "component", "store",
			"plan", n.PlanID, "node", n.NodeID, "err", err)
	}
	s.syncDagNodeRecord(ctx, chatID, n.PlanID, n.NodeID, dag.StatusFailed)
	s.appendNodeFailed(ctx, chatID, n)
	return UnresumableNode{PlanID: n.PlanID, NodeID: n.NodeID, Reason: why}
}

// appendNodeFailed records the settle in the ledger so a ledger-only replay agrees with the row;
// boot wires the WAL before its reconcile for exactly this.
func (s *Store) appendNodeFailed(ctx context.Context, chatID string, n DagNode) {
	if s.walLedger == nil || chatID == "" {
		return
	}
	var p DagPlan
	_ = s.db.WithContext(ctx).Where("id = ?", n.PlanID).First(&p).Error
	payload, _ := json.Marshal(struct {
		NodeID string `json:"node_id"`
		Turn   string `json:"turn"`
	}{n.NodeID, p.RunTurnID()})
	if _, err := s.walLedger.AppendIntent(ctx, ledger.Entry{
		ChatID: chatID, TurnID: p.RunTurnID(), NodeID: n.NodeID, Kind: ledger.KindNodeFailed, Payload: payload,
	}); err != nil {
		slog.Warn("resume paused dag nodes: node.failed append failed", "component", "store",
			"chat", chatID, "node", n.NodeID, "err", err)
	}
}

// SyncTerminalDagNodeRecords points every finished node of each chat's latest plan created
// since then at its row's status, repairing records an earlier process left behind; returns how many it checked.
func (s *Store) SyncTerminalDagNodeRecords(ctx context.Context, since time.Time) (int, error) {
	var rows []struct {
		ChatID, PlanID, NodeID, Status string
	}
	err := s.db.WithContext(ctx).Table("dag_nodes").
		Select("dag_plans.chat_id, dag_nodes.plan_id, dag_nodes.node_id, dag_nodes.status").
		Joins("JOIN dag_plans ON dag_plans.id = dag_nodes.plan_id").
		Where("dag_nodes.status IN ?", []string{string(dag.StatusDone), string(dag.StatusFailed), string(dag.StatusCancelled)}).
		Where("dag_plans.created_at >= ?", since).
		Where("dag_plans.created_at = (SELECT MAX(p2.created_at) FROM dag_plans p2 WHERE p2.chat_id = dag_plans.chat_id)").
		Scan(&rows).Error
	if err != nil {
		return 0, err
	}
	users := map[string]string{}
	for _, r := range rows {
		if _, ok := users[r.ChatID]; !ok {
			users[r.ChatID] = s.SessionUserForChat(ctx, r.ChatID)
		}
		s.syncRecord(ctx, r.ChatID, users[r.ChatID], r.NodeID, dag.NodeStatus(r.Status))
	}
	return len(rows), nil
}

// syncDagNodeRecord mirrors a boot reconcile's row status onto the dag_node record
// (list_nodes and the artifact panel read it); best-effort like every record write.
func (s *Store) syncDagNodeRecord(ctx context.Context, chatID, planID, nodeID string, status dag.NodeStatus) {
	if chatID == "" {
		return
	}
	// Node ids recur across plans (reuse), so only the latest plan's node owns the record.
	if p, err := s.GetLatestDagPlan(ctx, chatID); err != nil || p == nil || p.ID != planID {
		return
	}
	s.syncRecord(ctx, chatID, s.SessionUserForChat(ctx, chatID), nodeID, status)
}

// syncRecord is syncDagNodeRecord for a caller that already knows planID is the latest.
func (s *Store) syncRecord(ctx context.Context, chatID, userID, nodeID string, status dag.NodeStatus) {
	if err := dag.SyncDagNodeStatus(ctx, s.artifacts, chatAppName, userID, chatID, nodeID, status); err != nil {
		slog.Warn("boot reconcile: dag_node record sync failed", "component", "store",
			"chat", chatID, "node", nodeID, "err", err)
	}
}

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

// CountDagPlans returns chatID's DAG plan count: rebuild's guard, since node IDs recur across plans.
func (s *Store) CountDagPlans(ctx context.Context, chatID string) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&DagPlan{}).Where("chat_id = ?", chatID).Count(&n).Error
	return n, err
}

// buildTurnContent joins a ChatTurn with its session group (nil if outside the window or wiped), plan and
// nodes; shared by both turn loaders so they can't drift.
func buildTurnContent(t ChatTurn, g *turnGroup, plan *DagPlan, nodesByPlan map[string][]DagNode, answers map[string]markedAnswer) TurnContent {
	tc := TurnContent{
		ID: t.ID, CreatedAt: t.CreatedAt, Model: t.Model,
		// Stamped by SetTurnUsage; turns without the stamp fall back to the session walk below.
		PromptTokens: t.PromptTokens, CompletionTokens: t.CompletionTokens,
		ReasoningTokens: t.ReasoningTokens, TotalTokens: t.TotalTokens, CachedTokens: t.CachedTokens,
	}
	if g != nil {
		tc.UserText = g.userText.String()
		tc.AsstText = g.asstText.String()
		tc.AsstThink = g.asstThink.String()
		if a, ok := g.answers[""]; ok {
			tc.Answer, tc.AnswerAt = a.text, a.at
		}
		tc.ToolCalls = g.toolCalls
		if tc.PromptTokens == 0 && tc.CompletionTokens == 0 {
			tc.PromptTokens = g.promptTokens
			tc.CompletionTokens = g.completionTokens
			tc.ReasoningTokens = g.reasoningTokens
			tc.CachedTokens = g.cachedTokens
			tc.TotalTokens = g.totalTokens
		}
	}
	// No group for this turn - fall back to the user's own text stamped on
	// the turn row at SaveTurn.
	if tc.UserText == "" {
		tc.UserText = t.UserText
	}
	if a, ok := answers[t.ID]; ok {
		tc.Answer, tc.AnswerAt = a.text, a.at
	}
	if plan != nil {
		tc.Plan = plan
		tc.Nodes = nodesByPlan[plan.ID]
	}
	return tc
}

// lastTurnWindow is getLastTurnGroup's starting window, grown 8x until a user event is in view.
// 512 covers a median turn (~280 events) in one round trip.
const lastTurnWindow = 512

// getLastTurnGroup fetches only the session tail; a window truncated mid-turn yields no group, so it grows.
// A missing/unreadable session means no group: the caller falls back to the ChatTurn row's own text.
func (s *Store) getLastTurnGroup(ctx context.Context, appName, userID, chatID string) (turnGroup, bool) {
	for n := lastTurnWindow; ; n *= 8 {
		resp, err := s.Sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: chatID, NumRecentEvents: n})
		if err != nil || resp == nil {
			return turnGroup{}, false
		}
		groups := groupSessionEvents(resp.Session.Events().All())
		if len(groups) > 0 {
			return groups[len(groups)-1], true
		}
		if resp.Session.Events().Len() < n {
			// Fetched the whole session and still found no user event - genuinely empty.
			return turnGroup{}, false
		}
	}
}

// GetLastTurnWithContent returns just the newest turn's content, without loading the rest of the chat.
func (s *Store) GetLastTurnWithContent(ctx context.Context, appName, userID, chatID string) (*TurnContent, error) {
	var t ChatTurn
	err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Order("seq desc").Limit(1).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var gPtr *turnGroup
	if g, ok := s.getLastTurnGroup(ctx, appName, userID, chatID); ok {
		gPtr = &g
	}

	var plan *DagPlan
	nodesByPlan := map[string][]DagNode{}
	var p DagPlan
	err = s.db.WithContext(ctx).Where("chat_id = ? AND turn_id = ?", chatID, t.ID).First(&p).Error
	if id := executedPlanID(gPtr); errors.Is(err, gorm.ErrRecordNotFound) && id != "" {
		err = s.db.WithContext(ctx).Where("chat_id = ? AND id = ?", chatID, id).First(&p).Error
	}
	if err == nil {
		plan = &p
		var nodes []DagNode
		if err := s.db.WithContext(ctx).Where("plan_id = ?", p.ID).Find(&nodes).Error; err != nil {
			return nil, err
		}
		nodesByPlan[p.ID] = nodes
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	var answers map[string]markedAnswer
	if gPtr != nil {
		answers = keyedAnswers([]turnGroup{*gPtr})
	}
	tc := buildTurnContent(t, gPtr, plan, nodesByPlan, answers)
	return &tc, nil
}

// executedPlanID is the plan a turn ran a step of: the plan_id its execute call names, else the one its
// create_plan/edit_plan returned. A turn extending an earlier turn's plan finds its card through it.
func executedPlanID(g *turnGroup) string {
	if g == nil {
		return ""
	}
	id, executed := "", false
	for _, c := range g.toolCalls {
		switch c.Name {
		case "execute":
			executed = true
			if pid, _ := c.Args["plan_id"].(string); pid != "" {
				return pid
			}
		case "create_plan", "edit_plan":
			if pid, _ := c.Result["plan_id"].(string); pid != "" {
				id = pid
			}
		}
	}
	if !executed {
		return ""
	}
	return id
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
	planByID := make(map[string]*DagPlan, len(plans))
	planIDs := make([]string, len(plans))
	for i := range plans {
		planByTurn[plans[i].TurnID] = &plans[i]
		planByID[plans[i].ID] = &plans[i]
		planIDs[i] = plans[i].ID
	}

	// One IN query for all plans' nodes instead of one GetDagNodes per turn (N+1).
	var allNodes []DagNode
	if len(planIDs) > 0 {
		_ = s.db.WithContext(ctx).Where("plan_id IN ?", planIDs).Find(&allNodes).Error
	}
	nodesByPlan := make(map[string][]DagNode, len(planIDs))
	for _, n := range allNodes {
		nodesByPlan[n.PlanID] = append(nodesByPlan[n.PlanID], n)
	}

	// ResetHistory only removes OLDER events, so surviving groups line up with the most recent turns:
	// align from the end, or a reset shifts content onto the wrong turn.
	offset := len(turns) - len(groups)
	answers := keyedAnswers(groups)
	result := make([]TurnContent, len(turns))
	for i, t := range turns {
		var g *turnGroup
		if gi := i - offset; gi >= 0 && gi < len(groups) {
			g = &groups[gi]
		}
		plan := planByTurn[t.ID]
		if plan == nil {
			plan = planByID[executedPlanID(g)]
		}
		result[i] = buildTurnContent(t, g, plan, nodesByPlan, answers)
	}
	return result, nil
}

// GetTurnWithContent returns one turn's joined content, trying the tail-only loader first since the common
// request is the newest turn.
func (s *Store) GetTurnWithContent(ctx context.Context, appName, userID, chatID, turnID string) (*TurnContent, error) {
	if last, err := s.GetLastTurnWithContent(ctx, appName, userID, chatID); err != nil {
		return nil, err
	} else if last != nil && last.ID == turnID {
		return last, nil
	}
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

// ChatsUsageTotals sums each chat's total tokens (turns + DAG nodes) in two GROUP BY queries.
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
