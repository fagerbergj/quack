package ledger

import (
	"encoding/json"
	"time"
)

// Entry kinds. Intents are fail-closed at the call site (no entry, no state
// change); node.* and the observation kinds are best-effort.
const (
	KindArtifactRevision = "artifact.revision"
	// KindArtifactRevisionAborted: no longer written (#1144 P4's unique index
	// makes a phantom parent unclaimable instead); kept so fold still reads
	// pre-P4 rows correctly.
	KindArtifactRevisionAborted = "artifact.revision.aborted"
	// delivery.intent's completion is a delivery_record artifact.revision, not a second
	// ledger entry (#1144 P2: one representation per fact) - judge rounds likewise fold
	// via ArtifactRevision.Kind, not a dedicated entry kind.
	KindDeliveryIntent = "delivery.intent"
	KindNodeStarted    = "node.started"
	KindNodeDone       = "node.done"
	KindNodeFailed     = "node.failed"

	// KindMemoryRecall/KindMemoryVote (epic #1255 P1): best-effort like node.* -
	// memory recall/voting must never fail a node. The ledger is the source of truth;
	// point recalls/last_recalled_at and upvotes/downvotes/score/tier are folds of these.
	KindMemoryRecall = "memory.recall"
	KindMemoryVote   = "memory.vote"

	// #1144 P5: the remaining direct-write projections, now covered by
	// fail-closed intents like every other WAL-backed write.
	KindChatCreated = "chat.created"
	KindTurnCreated = "turn.created"
	KindPlanSaved   = "plan.saved"

	// Observation kinds, written by the OTel Exporter from gen_ai.* records.
	KindLLMCall     = "llm.call"
	KindToolCall    = "tool.call"
	KindAgentInvoke = "agent.invoke"
	KindEvalScore   = "eval.score"
)

// EntrySchemaVersion is the current Entry payload shape's version. Rows
// written before it read back as 0, which MigrateEntry treats as version 1
// (not a failure) - no backfill needed on an existing Postgres.
const EntrySchemaVersion = 1

// MigrateEntry upgrades e to EntrySchemaVersion in place - the one hook every store's read
// path shares. Nothing to upgrade yet: a future payload shape change gets exactly one place
// to add a case, not one per reader.
func MigrateEntry(e Entry) Entry {
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}
	return e
}

// IsObservation reports whether kind is one the Exporter writes - the half
// of the log that replay and the recording bundle read.
func IsObservation(kind string) bool {
	switch kind {
	case KindLLMCall, KindToolCall, KindAgentInvoke, KindEvalScore:
		return true
	}
	return false
}

// Entry is the WAL envelope: intents appended before they are acted on, observations after the fact.
// Seq is store-allocated; Key/IdempotencyKey make repeats idempotent per chat (#1144 P4: *DuplicateIntentError).
// NodeID/Agent/Round are the replay stream identity - a ctx value set inside a node body never crosses the RunNode scheduling boundary.
type Entry struct {
	Seq            int64           `json:"seq"`
	ChatID         string          `json:"chat_id"`
	TurnID         string          `json:"turn_id,omitempty"`
	NodeID         string          `json:"node_id,omitempty"`
	Agent          string          `json:"agent,omitempty"`
	Round          string          `json:"round,omitempty"`
	Kind           string          `json:"kind"`
	Key            string          `json:"key,omitempty"`
	At             time.Time       `json:"at"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	// SchemaVersion: set by the store on write (caller input ignored, same
	// as Seq); see EntrySchemaVersion/MigrateEntry (#1144 P5).
	SchemaVersion int `json:"schema_version,omitempty"`
}

// LLMCallPayload is a KindLLMCall entry's payload (one gen_ai "chat" call).
// Input/Output/SystemInstructions/ToolDefinitions are the JSON strings the
// emitter built, kept verbatim so replay hands back exactly what was seen.
type LLMCallPayload struct {
	Provider      string `json:"provider,omitempty"`
	RequestModel  string `json:"request_model"`
	ResponseModel string `json:"response_model,omitempty"`
	ResponseID    string `json:"response_id,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	// CachedTokens: prompt tokens served from the provider's cache, already
	// excluded from InputTokens (see inference.splitPromptTokens) so
	// InputTokens+CachedTokens never double-counts the raw prompt total.
	CachedTokens int64   `json:"cached_tokens,omitempty"`
	Temperature  float64 `json:"temperature,omitempty"`
	MaxTokens    int64   `json:"max_tokens,omitempty"`
	// ReasoningEffort is the resolved effort ("low"/"medium"/"high") sent as
	// gen_ai.request.reasoning_effort - from models.<name>.effort or an
	// explicit ThinkingConfig (e.g. gates.judge.thinking_level); "" = neither set.
	ReasoningEffort    string `json:"reasoning_effort,omitempty"`
	PromptName         string `json:"prompt_name,omitempty"`
	PromptVersion      string `json:"prompt_version,omitempty"`
	SystemInstructions string `json:"system_instructions,omitempty"`
	ToolDefinitions    string `json:"tool_definitions,omitempty"`
	Input              string `json:"input,omitempty"`
	Output             string `json:"output,omitempty"`
	Error              string `json:"error,omitempty"`
	// QuackVersion/BundleHash/CostUSD: provenance added for #1096 - which build and agent
	// bundle produced this call, and what it cost. CostUSD is a pointer so an actual $0 call
	// stays distinguishable from "no config.ModelPricing entry" (nil) - a float+omitempty would conflate the two.
	QuackVersion string   `json:"quack_version,omitempty"`
	BundleHash   string   `json:"bundle_hash,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
}

// ToolCallPayload is a KindToolCall entry's payload (one execute_tool call).
type ToolCallPayload struct {
	Name   string `json:"name"`
	Type   string `json:"type,omitempty"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// AgentInvokePayload is a KindAgentInvoke entry's payload: one ACP round's
// full protocol conversation, both directions as JSON arrays of frames.
type AgentInvokePayload struct {
	Sent     string `json:"sent,omitempty"`
	Received string `json:"received,omitempty"`
	Error    string `json:"error,omitempty"`
}

// EvalScorePayload is a KindEvalScore entry's payload: one judge criterion.
type EvalScorePayload struct {
	ResponseID  string  `json:"response_id,omitempty"`
	Criterion   string  `json:"criterion"`
	Score       float64 `json:"score"`
	Explanation string  `json:"explanation,omitempty"`
}

// MemoryRecallEntry is one memory delivered to a worker (KindMemoryRecall).
// Source is "prefill" (P1) or "tool" (P2's recall_memory).
type MemoryRecallEntry struct {
	ID    string  `json:"id"`
	Score float32 `json:"score,omitempty"`
}

// MemoryRecallPayload: every memory one injection delivered to a node, so a
// chat outcome can later target exactly the recalled set (design decision
// #1255: recall-based, not birth-based, reinforcement).
type MemoryRecallPayload struct {
	Source  string              `json:"source"` // "prefill" | "tool"
	Round   int                 `json:"round,omitempty"`
	Entries []MemoryRecallEntry `json:"entries"`
}

// MemoryVote is one judge (or human) verdict on a recalled memory.
type MemoryVote string

const (
	MemoryVoteSupported    MemoryVote = "supported"
	MemoryVoteContradicted MemoryVote = "contradicted"
	MemoryVoteNotRelevant  MemoryVote = "not_relevant"
)

// MemoryVotePayload is a KindMemoryVote entry's payload: one memory's vote
// on a gate-passed round. Actor is "judge" or "human" (memory.OpsLogActor).
type MemoryVotePayload struct {
	MemoryID string     `json:"memory_id"`
	Vote     MemoryVote `json:"vote"`
	Reason   string     `json:"reason,omitempty"`
	Actor    string     `json:"actor"`
	Round    int        `json:"round,omitempty"`
}
