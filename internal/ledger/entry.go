package ledger

import (
	"encoding/json"
	"time"
)

// Entry kinds. Intents are fail-closed at the call site (no entry, no state
// change); node.* and the observation kinds are best-effort.
const (
	KindArtifactRevision = "artifact.revision"
	// KindArtifactRevisionAborted is no longer written; fold still reads old rows of it.
	KindArtifactRevisionAborted = "artifact.revision.aborted"
	// A delivery.intent completes as a delivery_record artifact.revision, not a second entry; judge rounds
	// likewise fold via ArtifactRevision.Kind.
	KindDeliveryIntent = "delivery.intent"
	KindNodeStarted    = "node.started"
	KindNodeDone       = "node.done"
	KindNodeFailed     = "node.failed"
	KindNodeCancelled  = "node.cancelled"

	// KindMemoryRecall/KindMemoryVote are best-effort and must never fail a node. Recall and vote counts
	// are folds of these entries.
	KindMemoryRecall = "memory.recall"
	KindMemoryVote   = "memory.vote"

	// Chat/turn/plan writes go through fail-closed intents like every other WAL-backed write.
	KindChatCreated = "chat.created"
	KindTurnCreated = "turn.created"
	KindPlanSaved   = "plan.saved"

	// Observation kinds, written by the OTel Exporter from gen_ai.* records.
	KindLLMCall     = "llm.call"
	KindToolCall    = "tool.call"
	KindAgentInvoke = "agent.invoke"
	KindEvalScore   = "eval.score"
	KindDecision    = "decision"
)

// EntrySchemaVersion is the current payload shape's version. Older rows read back as 0, which MigrateEntry
// treats as version 1.
const EntrySchemaVersion = 1

// MigrateEntry upgrades e to EntrySchemaVersion: the one hook every store's read path shares.
func MigrateEntry(e Entry) Entry {
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}
	return e
}

// IsObservation reports whether kind is one the Exporter writes - the half
// of the log the recording bundle reader reads.
func IsObservation(kind string) bool {
	switch kind {
	case KindLLMCall, KindToolCall, KindAgentInvoke, KindEvalScore, KindDecision:
		return true
	}
	return false
}

// Entry is the WAL envelope: intents appended before they are acted on, observations after. Seq is
// store-allocated; NodeID/Agent/Round carry stream identity because ctx values don't cross RunNode.
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
	// SchemaVersion is set by the store on write (caller input ignored, like Seq); see MigrateEntry.
	SchemaVersion int `json:"schema_version,omitempty"`
}

// LLMCallPayload is one gen_ai "chat" call. Input/Output/SystemInstructions/ToolDefinitions are kept
// verbatim as the emitter built them.
type LLMCallPayload struct {
	Provider      string `json:"provider,omitempty"`
	RequestModel  string `json:"request_model"`
	ResponseModel string `json:"response_model,omitempty"`
	ResponseID    string `json:"response_id,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	// CachedTokens are already excluded from InputTokens, so the two never double-count the prompt.
	CachedTokens int64 `json:"cached_tokens,omitempty"`
	// ReasoningTokens: thinking-token spend, split out from OutputTokens -
	// from the translator's Usage() (ADK's ThoughtsTokenCount).
	ReasoningTokens int64   `json:"reasoning_tokens,omitempty"`
	Temperature     float64 `json:"temperature,omitempty"`
	MaxTokens       int64   `json:"max_tokens,omitempty"`
	// ReasoningEffort is the resolved effort sent as gen_ai.request.reasoning_effort; "" = none set.
	ReasoningEffort    string `json:"reasoning_effort,omitempty"`
	PromptName         string `json:"prompt_name,omitempty"`
	PromptVersion      string `json:"prompt_version,omitempty"`
	SystemInstructions string `json:"system_instructions,omitempty"`
	ToolDefinitions    string `json:"tool_definitions,omitempty"`
	Input              string `json:"input,omitempty"`
	Output             string `json:"output,omitempty"`
	Error              string `json:"error,omitempty"`
	// CostUSD is a pointer so a real $0 call stays distinct from "no pricing entry" (nil).
	QuackVersion string   `json:"quack_version,omitempty"`
	BundleHash   string   `json:"bundle_hash,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
	// PromptSource/PromptVersionID: the store the system prompt resolved from ("static" or a prompts:
	// store) and its version there, for reproducing the exact bytes sent.
	PromptSource    string `json:"prompt_source,omitempty"`
	PromptVersionID string `json:"prompt_version_id,omitempty"`
	// PromptArtifact: the resolved artifact's name, e.g. "system/code-reviewer" -
	// derived from the bundle directory, not PromptName (the agent name).
	PromptArtifact string `json:"prompt_artifact,omitempty"`
	// Artifacts: every artifact this round resolved through artifactsrc -
	// a superset of PromptSource/PromptVersionID/PromptArtifact, kept for compatibility.
	Artifacts []ArtifactRef `json:"artifacts,omitempty"`
	// Plugins: the plugin registry rows in scope for this native round (ACP rounds carry it on agent.invoke).
	Plugins []PluginRef `json:"plugins,omitempty"`
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
	Sent      string        `json:"sent,omitempty"`
	Received  string        `json:"received,omitempty"`
	Error     string        `json:"error,omitempty"`
	Plugins   []PluginRef   `json:"plugins,omitempty"`
	Artifacts []ArtifactRef `json:"artifacts,omitempty"`
}

// ArtifactRef is one artifact this round resolved through artifactsrc. VersionID is omitted only when the
// resolver couldn't be reached.
type ArtifactRef struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	VersionID string `json:"version_id,omitempty"`
}

// PluginRef is one plugin's provenance on an agent.invoke entry:
// SHA is "" for a local or embedded plugin, which have no clone to pin.
type PluginRef struct {
	Name string `json:"name"`
	SHA  string `json:"sha,omitempty"`
}

// EvalScorePayload is a KindEvalScore entry's payload: one judge criterion.
type EvalScorePayload struct {
	ResponseID  string  `json:"response_id,omitempty"`
	Criterion   string  `json:"criterion"`
	Score       float64 `json:"score"`
	Explanation string  `json:"explanation,omitempty"`
}

// DecisionPayload is a KindDecision entry's payload: one intercept-point
// evaluation (internal/decide). State/Questions are what was sent, so it can be replayed.
type DecisionPayload struct {
	Point     string  `json:"point"`
	Mode      string  `json:"mode"`
	Handler   string  `json:"handler"`
	Outcome   string  `json:"outcome"`
	Confident bool    `json:"confident"`
	Top       string  `json:"top,omitempty"`
	TopP      float64 `json:"top_p,omitempty"`
	// Probabilities: question id -> option -> probability; a noul's options are "true"/"false".
	Probabilities map[string]map[string]float64 `json:"probabilities,omitempty"`
	// Baseline is what quack's own logic decided, in the primary question's option space; beside a
	// skipped_step it is a shadow audit's verdict.
	Baseline string `json:"baseline,omitempty"`
	// SkippedStep names the step an acting decide outcome replaced; null otherwise, so savings can be summed later.
	SkippedStep  *string `json:"skipped_step"`
	RequestBytes int     `json:"request_bytes,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	ServerMS     float64 `json:"server_ms,omitempty"`
	LatencyMS    float64 `json:"latency_ms"`
	Error        string  `json:"error,omitempty"`
	// Reason says why a confident decide answer fell back.
	Reason    string          `json:"reason,omitempty"`
	State     json.RawMessage `json:"state,omitempty"`
	Questions json.RawMessage `json:"questions,omitempty"`
	// Meta is recorded beside State but was never sent to the handler.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// MemoryRecallEntry is one memory delivered to a worker (KindMemoryRecall).
// Source is "prefill" or "tool" (recall_memory).
type MemoryRecallEntry struct {
	ID    string  `json:"id"`
	Score float32 `json:"score,omitempty"`
}

// MemoryRecallPayload lists every memory one injection delivered to a node, so an outcome can target exactly
// the recalled set.
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
