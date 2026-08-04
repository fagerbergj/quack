// Package store is Quack's persistence layer. The ADK database SessionService
// (postgres or sqlite) is the source of truth for conversation events; a thin `chats`
// table holds the REST resource surface. A chat's ID is also its ADK session ID,
// so chat history is derived from the session's events (no duplicate table).
package store

import (
	"context"
	"database/sql"
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
	// GithubRepo/GithubURL are set only for GitHub-originated chats (id
	// github-<owner>-<repo>-<number>) via SetChatGitHub.
	GithubRepo string `json:"github_repo,omitempty"`
	GithubURL  string `json:"github_url,omitempty"`
	// SessionUser is the ADK session identity this chat's turns/events/session
	// were written under, recorded once at creation (SetChatGitHub: the GitHub
	// commenter's login, #512). Empty for chats that predate this column or
	// were never GitHub-dispatched - SessionUserFor falls back to id shape.
	// Column is adk_session_user, not session_user (#525): unquoted,
	// session_user collides with Postgres' built-in SESSION_USER function.
	SessionUser string `gorm:"column:adk_session_user" json:"session_user,omitempty"`
}

// ChatTurn is one user→assistant exchange. Its ID is the response_id exposed
// in the REST API. Sequence is 0-based insertion order within the chat.
type ChatTurn struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	ChatID    string    `gorm:"index" json:"chat_id"`
	Seq       int       `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
	// Model is the model that produced the orchestrator's own plain reply this
	// turn, stamped from the live stream at run end (ADK's event storage drops
	// ModelVersion on read, so it can't be recovered from session events later).
	// Empty for DAG turns - their models live per-node on DagNode.
	Model string `json:"model,omitempty"`
}

// GithubSnapshot stores the full GitHub state fetched at a github-origin
// chat's last dispatch (internal/github/snapshot.go), keyed by ChatID
// (github-<owner>-<repo>-<number>) - the ground truth a resume diffs against
// to compute the turn's delta. JSON is the whole snapshot, opaque to this
// package (mirrors DagPlan.PlanJSON below).
type GithubSnapshot struct {
	ChatID    string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	JSON      string    `json:"json"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GithubReviewBaseline stores the patch-ids of the PR commits quack has
// actually REVIEWED for one PR chat - separate from GithubSnapshot, which
// advances on EVERY dispatch (review or not). Only a dispatch that actually
// DELIVERS a review advances this row (internal/github's
// advanceReviewBaseline), so a conversational dispatch between two reviews
// can never make the next review under-scope itself.
type GithubReviewBaseline struct {
	ChatID    string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	PatchIDs  string    `json:"patch_ids"` // JSON array of strings; row absent = never reviewed
	UpdatedAt time.Time `json:"updated_at"`
}

// GithubFixState tracks the CI auto-heal loop bound (#254, redesigned #656)
// for one PR chat. Durable on purpose: quack's own fix push re-runs CI and
// the next failure arrives as a fresh webhook, possibly after a process
// restart - in-memory state would reset and thrash forever. LastSHA dedups
// multiple failing workflows on one head commit (CI usually runs several).
// Stopped marks the ONE-attempt guard having tripped: the commit that just
// failed CI was itself a fix quack pushed, so it stops rather than fix the
// fix (see internal/github/cifix.go's autoHeal) - cleared the moment a NEW
// (non-quack) commit fails, which is what re-arms auto-heal without needing
// a human to re-apply anything.
type GithubFixState struct {
	ChatID    string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	LastSHA   string    `gorm:"column:last_sha" json:"last_sha"`
	Stopped   bool      `json:"stopped"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GithubMergeIntent records a standing authorization to merge a PR chat once
// quack's own review approves it - what applying quack:merge BEFORE a review
// exists becomes, instead of a dead end (internal/github's mergeIfApproved).
// Durable for the same reason GithubFixState is: the approving review may
// land after a process restart. RequestedBy is surfaced in the eventual merge
// comment so it still names who authorized it.
type GithubMergeIntent struct {
	ChatID      string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	RequestedBy string    `json:"requested_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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

// ChatEvent is one persisted SSE run event, ordered per chat by a monotonic Seq
// the run loop assigns. It backs the hub's replay durably: after a restart (when
// the in-memory hub is empty) SubscribeChatStream replays a run from here. Event
// is the serialized stream event ({name,data}), replayed verbatim. A new run on
// the chat clears the prior run's rows, so the table holds one run per chat
// (mirroring the hub's reset-on-new-run) and is windowed to MaxReplay rows.
type ChatEvent struct {
	ChatID    string    `gorm:"primaryKey;column:chat_id" json:"chat_id"`
	Seq       int64     `gorm:"primaryKey;autoIncrement:false" json:"seq"`
	Event     string    `json:"event"`
	CreatedAt time.Time `json:"created_at"`
}

// DagNode stores the execution state of one DAG node. Status is the string
// value of a dag.NodeStatus (queued | running | needs_input | done | failed |
// cancelled); every write to this field routes through dag.CanTransition (see
// internal/server/rest/handler.go persistNodeEvent and UpdateNodeStatus).
type DagNode struct {
	NodeID        string `gorm:"primaryKey;column:node_id" json:"node_id"`
	PlanID        string `gorm:"primaryKey;column:plan_id" json:"plan_id"`
	Status        string `json:"status"` // dag.NodeStatus value
	OutputPreview string `json:"output_preview"`
	// Output is the node's FULL vetted text (OutputPreview is truncated to 250
	// chars for display).
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
	// InstanceID is the writing Store's own identity (Store.instanceID),
	// stamped when a node is queued or starts running - see UpsertDagNode and
	// FailStaleDagNodes. Empty on a row written before this column existed;
	// treated the same as "no live owner" so an upgrading database doesn't
	// grow immortal rows (#683).
	InstanceID string `gorm:"column:instance_id" json:"-"`
	// UpdatedAt is bumped on every UpsertDagNode write (queued, running, or
	// terminal) - the dead-man's-switch FailStaleDagNodes falls back to when
	// a node's owning instance never returns to reconcile it by InstanceID.
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
	// Usage is the orchestrator's own token usage for this turn (its conversational
	// session - a DAG turn's per-node tokens are separate, surfaced via Nodes).
	PromptTokens, CompletionTokens, ReasoningTokens int32
	// Model is the orchestrator's own model for a plain-reply turn (from the
	// ChatTurn row, stamped at run end); empty for DAG turns.
	Model string
}

// ToolCallRecord is one orchestrator tool call recovered from the session events,
// with its result paired in by call ID. Surfaced so chat history can render the
// orchestrator's activity (plan/execute/get_user_choice) after a reload.
type ToolCallRecord struct {
	CallID string
	Name   string
	Args   map[string]any
	Result map[string]any
}

// transferTool is ADK's internal agent-transfer tool; it is noise in the activity
// log, so it is excluded (mirrors the live stream translator).
const transferTool = "transfer_to_agent"

// choiceToolName / choiceAnswerKey mirror tools.ChoiceToolName / ChoiceAnswerKey:
// a clarification answer is resumed as a get_user_choice FunctionResponse on a
// user-authored event, carrying the chosen option under the answer key. We surface
// that option as the turn's user text (otherwise the answer turn looks empty).
const (
	choiceToolName  = "get_user_choice"
	choiceAnswerKey = "choice"
)

// nodeInputCallName / nodeInputPayloadKey mirror ADK's adk_request_input resume
// shape: a mid-node HITL answer is delivered as a FunctionResponse on a
// user-authored event with the answer text under "payload". Surfaced as the
// turn's user text, same as a clarification answer.
const (
	nodeInputCallName   = "adk_request_input"
	nodeInputPayloadKey = "payload"
)

// orchestratorAuthor mirrors orchestrator.orchestratorName: the Author stamped on
// BOTH the orchestrator llmagent's own events (it's wrapped in a workflow.AgentNode
// too, so it carries NodeInfo like everything else - NodeInfo alone can't
// distinguish it) and the delivered-answer event persistAnswer appends. Everything
// else authored differently (a plan node's worker/advisor/judge-adjacent activity,
// or the plan-graph wrapper's own structural events) is gate-internal and never
// the user-facing message.
const orchestratorAuthor = "orchestrator"

// turnGroup is the per-turn content extracted from a session's events.
type turnGroup struct {
	userText, asstText, asstThink string
	toolCalls                     []ToolCallRecord
	// Usage accumulated from the orchestrator's OWN model events in this turn
	// (gate-internal node runs are excluded, same as asstText/toolCalls below -
	// their tokens are already surfaced per-node via DagNodeState).
	promptTokens, completionTokens, reasoningTokens int32
}

// groupSessionEvents buckets a session's events into per-turn groups, split on
// user-authored events. For assistant events it separates text from thinking and
// collects tool calls (pairing each FunctionResponse to its earlier FunctionCall
// by call ID); transfer_to_agent is excluded as activity-log noise. Pure (no DB)
// so the extraction is unit-testable.
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
				// A clarification answer (get_user_choice) or a mid-node HITL answer
				// (adk_request_input) arrives as a FunctionResponse (Role:user); surface
				// the answer as the user's message text.
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
		// Gate-internal activity (worker drafts, advisor consults, revisions) is
		// authored by something other than the orchestrator and is never the
		// user-facing message - skip it so it can't get glued into asstText or
		// listed as top-level activity. NodeInfo alone can't distinguish this: the
		// orchestrator's own replies carry NodeInfo too.
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

// Store wraps the relational DB (chat metadata) and the ADK session service.
type Store struct {
	db       *gorm.DB
	Sessions session.Service
	// instanceID identifies this Store to node-ownership tracking (InstanceID
	// column, FailStaleDagNodes): random per New() call, so a fresh process
	// never matches an existing row unless SetInstanceID overrides it with a
	// persisted identity (LoadOrCreateInstanceID) - see instance.go.
	instanceID string
}

// New opens the persistence store for the given backend kind ("postgres" or
// "sqlite"; empty defaults to postgres), runs migrations for both the app tables
// and the ADK session/event tables, and returns it. The GORM dialector is the
// portability seam: both the app handle and ADK's session service are built from
// a fresh dialector for the same DSN.
func New(kind, url string) (*Store, error) {
	dialector, err := dialectorFor(kind, url)
	if err != nil {
		return nil, err
	}
	// Route GORM's slow-query warnings through the same slog handler as the rest
	// of the app. NewLogLogger adapts slog into the *log.Logger gorm/logger wants.
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

// InstanceID is this Store's own identity, stamped onto every DAG node it
// writes to running/queued (UpsertDagNode) and used by FailStaleDagNodes to
// tell "my own prior incarnation" apart from a peer's live rows.
func (s *Store) InstanceID() string { return s.instanceID }

// SetInstanceID overrides the random default New() assigned. Call it once,
// before any node writes, with a persisted identity (LoadOrCreateInstanceID)
// - only a process that means to own the DAG run loop should: an ephemeral
// CLI bootstrap has no prior incarnation to reconcile against, so it should
// keep the random default (which can never collide with an existing row).
func (s *Store) SetInstanceID(id string) { s.instanceID = id }

// dialectorFor returns a factory that yields a GORM dialector for kind+url. The
// dialector is consumed twice - once for the app handle, once for ADK's session
// service. For postgres each call opens its own pool (postgres handles concurrent
// writers). For sqlite both calls SHARE one *sql.DB capped to a single
// connection: SQLite allows only one writer, so serializing every app +
// ADK-session write through one connection is what prevents SQLITE_BUSY under the
// concurrent DAG / gate / memory writers (busy_timeout alone wasn't enough - two
// pools could still collide). WAL + busy_timeout stay (durability + reads).
// ponytail: single local file, single instance - no-docker only.
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

// sqliteDSN enables WAL + a busy timeout. A caller who supplies their own query
// params is left untouched.
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

// chatAppName mirrors orchestrator.AppName as a literal - store is a
// dependency of the orchestrator package, so it can't import it back.
const chatAppName = "quack"

// SessionUserFor resolves the ADK session identity a chat's turns/events/
// session were written under: the per-chat SessionUser recorded at creation
// (#512 - the GitHub commenter's login for a dispatched chat), or the id-shape
// default ("github"/"local") for chats that predate that column. Mirrors
// internal/github.runUserID's fallback; duplicated rather than shared since
// store can't import the rest/github packages.
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
// A GetChat miss or error (deleted mid-flight, transient DB hiccup) falls
// back to the id-shape default rather than failing the caller.
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

// DeleteChat removes a chat and everything #352 found it otherwise strands:
// the chats row is the only REST-visible record, but its turns, DAG
// plan/node state, durable SSE event log, and ADK session (turns/events)
// live in their own tables/service and outlive it. Runs in one transaction -
// a partial cascade would leave the same orphans this fixes, just fewer of
// them. The ADK session delete is a best-effort append AFTER the transaction
// commits: it lives in a separate service (possibly a separate database) that
// can't join the local transaction, and a chat already gone from every local
// table is not worth failing the whole delete over a session-service hiccup.
func (s *Store) DeleteChat(ctx context.Context, id string) error {
	// Resolve BEFORE the transaction removes the chats row - SessionUserForChat
	// reads that row, and a miss falls back to the id-shape default, which would
	// silently mis-resolve a #512 per-chat SessionUser after the row is gone.
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

// SetChatGitHub upserts the originating GitHub repo/URL onto a chat. The
// webhook dispatch may fire before the chat row exists (first message of a
// new session), so this creates the row if missing rather than only updating.
// sessionUser is the ADK identity THIS dispatch is writing session/turn/memory
// content under (#512: the commenter's login) - recorded only on the
// create path (deliberately absent from DoUpdates below) so a chat's
// session user is fixed at creation and later dispatches on the same
// thread (possibly a different commenter) don't retroactively move where
// its existing history is read from.
func (s *Store) SetChatGitHub(ctx context.Context, id, repo, url, sessionUser string) error {
	now := time.Now().UTC()
	c := &Chat{ID: id, CreatedAt: now, UpdatedAt: now, GithubRepo: repo, GithubURL: url, SessionUser: sessionUser}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"github_repo", "github_url", "updated_at"}),
	}).Create(c).Error
}

// GetGithubSnapshot returns the stored snapshot JSON for a github-origin
// chat, or ("", false, nil) when none exists yet (first dispatch on this
// session - the caller seeds instead of diffing).
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

// SetGithubSnapshot upserts the current snapshot JSON for a github-origin
// chat - called after every dispatch diffs against the prior one, so the
// NEXT resume compares against what GitHub looked like this time.
func (s *Store) SetGithubSnapshot(ctx context.Context, chatID, json string) error {
	row := &GithubSnapshot{ChatID: chatID, JSON: json, UpdatedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"json", "updated_at"}),
	}).Create(row).Error
}

// GetGithubReviewBaseline returns the JSON patch-id list quack last
// DELIVERED a review at for a PR chat, or ("", false, nil) when it has never
// delivered a review here (the caller then reviews everything).
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

// SetGithubReviewBaseline upserts the JSON patch-id list for a PR chat -
// called ONLY when a dispatch actually delivered a review this run, never on
// every dispatch (that's GithubSnapshot's job).
func (s *Store) SetGithubReviewBaseline(ctx context.Context, chatID, patchIDsJSON string) error {
	row := &GithubReviewBaseline{ChatID: chatID, PatchIDs: patchIDsJSON, UpdatedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"patch_ids", "updated_at"}),
	}).Create(row).Error
}

// GetGithubFixState returns the auto-heal state for a PR chat, or (nil, nil)
// when no fix run has ever been counted here.
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

// SetGithubFixState upserts the auto-heal state for a PR chat. The caller
// persists BEFORE dispatching a fix run so a crash mid-run never refunds an
// attempt.
func (s *Store) SetGithubFixState(ctx context.Context, st GithubFixState) error {
	st.UpdatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_sha", "stopped", "updated_at"}),
	}).Create(&st).Error
}

// DeleteGithubFixState re-arms auto-heal for a PR chat - called when a human
// (re-)applies the fix label, the documented retry convention.
func (s *Store) DeleteGithubFixState(ctx context.Context, chatID string) error {
	return s.db.WithContext(ctx).Where("chat_id = ?", chatID).Delete(&GithubFixState{}).Error
}

// GetGithubMergeIntent returns the standing merge authorization for a PR
// chat, or (nil, nil) when none is recorded (the label was never applied
// before an approval, or it already got consumed/cleared).
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

// SetGithubMergeIntent upserts the standing merge authorization for a PR
// chat - applying quack:merge records/refreshes it, naming whoever most
// recently applied the label.
func (s *Store) SetGithubMergeIntent(ctx context.Context, chatID, requestedBy string) error {
	now := time.Now().UTC()
	row := &GithubMergeIntent{ChatID: chatID, RequestedBy: requestedBy, CreatedAt: now, UpdatedAt: now}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"requested_by", "updated_at"}),
	}).Create(row).Error
}

// DeleteGithubMergeIntent clears the standing merge authorization for a PR
// chat - called once it has been consumed by a merge.
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

// SetTurnModel stamps the model that produced the orchestrator's own reply on
// the turn row. Called at run end from the live stream's accumulated
// ModelVersion - the only place it exists, since ADK's event storage drops it.
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

// SaveDagPlan persists a DAG plan linked to the given turn.
func (s *Store) SaveDagPlan(ctx context.Context, chatID, planID, turnID, planJSON string) error {
	now := time.Now().UTC()
	p := &DagPlan{ID: planID, ChatID: chatID, TurnID: turnID, PlanJSON: planJSON, CreatedAt: now}
	return s.db.WithContext(ctx).Create(p).Error
}

// UpsertDagNode creates or updates a DAG node's execution state.
func (s *Store) UpsertDagNode(ctx context.Context, node DagNode) error {
	db := s.db.WithContext(ctx)
	// Save() writes EVERY column, so a later event that doesn't carry StartedAt
	// (node_done, node_failed) would erase the start time recorded at node_start.
	// Never let a nil StartedAt erase a real one. Same for InstanceID: only the
	// queued/running claim stamps it (see runlog.PersistNodeEvent), so a later
	// event must not blank out who claimed the node.
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

// InsertChatEvent persists one run event. The caller assigns Seq (per-chat
// monotonic) and serializes inserts per chat so order is stable.
func (s *Store) InsertChatEvent(ctx context.Context, ev ChatEvent) error {
	return s.db.WithContext(ctx).Create(&ev).Error
}

// LoadChatEvents returns a chat's persisted events with seq > afterSeq, ordered
// by seq. afterSeq=0 returns the whole stored run (for Last-Event-ID resume the
// client passes its last-seen seq).
func (s *Store) LoadChatEvents(ctx context.Context, chatID string, afterSeq int64) ([]ChatEvent, error) {
	var evs []ChatEvent
	err := s.db.WithContext(ctx).
		Where("chat_id = ? AND seq > ?", chatID, afterSeq).
		Order("seq asc").Find(&evs).Error
	return evs, err
}

// DeleteChatEvents drops a chat's persisted run events. Called at the start of a
// new run so its events start fresh at seq 1 (mirrors the hub's per-run reset).
func (s *Store) DeleteChatEvents(ctx context.Context, chatID string) error {
	return s.db.WithContext(ctx).Where("chat_id = ?", chatID).Delete(&ChatEvent{}).Error
}

// TrimChatEvents drops a chat's events at or below upToSeq - used to window a very
// long run to the durable replay ceiling, mirroring the hub's bounded buffer.
func (s *Store) TrimChatEvents(ctx context.Context, chatID string, upToSeq int64) error {
	return s.db.WithContext(ctx).Where("chat_id = ? AND seq <= ?", chatID, upToSeq).Delete(&ChatEvent{}).Error
}

// staleNodeCeiling is the dead-man's-switch: a queued/running node untouched
// this long is failed regardless of InstanceID, so a node whose owning
// instance never comes back (its persisted identity lost, not merely
// restarted - see LoadOrCreateInstanceID) doesn't stay in-flight forever.
// Generous on purpose - real runs finish in minutes; webhook runs are capped
// at RunTimeoutMinutes (config default 120).
const staleNodeCeiling = 12 * time.Hour

// FailStaleDagNodes marks failed any node still queued/running that (a) was
// written before InstanceID existed, (b) belongs to THIS Store's own
// instance (a restart reconciling what it left mid-run last time), or (c)
// has been untouched past staleNodeCeiling. It never touches a node another
// live instance currently owns - #683: a read-only CLI subcommand sharing
// the same database used to infer "orphaned" from status alone, which failed
// a node a running server was actively updating. queued→failed and
// running→failed are both legal per dag.CanTransition; a bulk SQL UPDATE
// can't invoke it per row, so the statuses are named via the dag constants
// instead of literals to keep the one enum as the single source of truth.
func (s *Store) FailStaleDagNodes(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().Add(-staleNodeCeiling)
	res := s.db.WithContext(ctx).Model(&DagNode{}).
		Where("status IN ?", []string{string(dag.StatusQueued), string(dag.StatusRunning)}).
		// instance_id IS NULL covers a bare ALTER TABLE ADD COLUMN (no default
		// applied to existing rows on some backends) as well as instance_id = ''.
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

// GetDagNode returns one node's persisted state, or (nil, nil) if it has no
// row yet (a node that hasn't started is implicitly dag.StatusQueued).
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

// GetLatestDagPlan returns the most-recent DAG plan for a chat (the one a retry
// targets), or (nil, nil) if the chat has no plan.
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

// GetTurnsWithContent returns fully-joined turn data for a chat: ADK event
// text grouped by turn, with the associated DAG plan and nodes when present.
// Turns are matched to ADK event groups by sequence order.
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
