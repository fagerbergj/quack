package ledger

import (
	"encoding/json"
	"time"
)

// Entry kinds. Intents are fail-closed at the call site (no entry, no state
// change); node.* and the observation kinds are best-effort.
const (
	KindArtifactRevision = "artifact.revision"
	// KindArtifactRevisionAborted marks an artifact.revision whose row never
	// materialized (crash or failed write after the intent landed). Folds
	// skip it when picking the parent revision so the id never wedges on a
	// phantom revision; boot recovery appends it for the crash case.
	KindArtifactRevisionAborted = "artifact.revision.aborted"
	KindJudgeRound              = "judge.round"
	KindDeliveryIntent          = "delivery.intent"
	KindDeliveryDone            = "delivery.done"
	KindNodeStarted             = "node.started"
	KindNodeDone                = "node.done"
	KindNodeFailed              = "node.failed"

	// Observation kinds, written by the OTel Exporter from gen_ai.* records.
	KindLLMCall     = "llm.call"
	KindToolCall    = "tool.call"
	KindAgentInvoke = "agent.invoke"
	KindEvalScore   = "eval.score"
)

// IsObservation reports whether kind is one the Exporter writes - the half
// of the log that replay and the recording bundle read.
func IsObservation(kind string) bool {
	switch kind {
	case KindLLMCall, KindToolCall, KindAgentInvoke, KindEvalScore:
		return true
	}
	return false
}

// Entry is the WAL envelope: an intent appended before it is acted on, or an
// observation appended after the fact. Seq is allocated by the store on
// AppendIntent and ignored on input. Key (an artifact id, or a delivery
// idempotency key) makes an intent idempotent on replay. NodeID/Agent/Round
// are the replay stream identity, stamped onto each record by the emitting
// object (SetLedgerCoords on the traced model/tools/ACP client) - a ctx value
// set inside a node body never crosses the RunNode scheduling boundary.
type Entry struct {
	Seq     int64           `json:"seq"`
	ChatID  string          `json:"chat_id"`
	TurnID  string          `json:"turn_id,omitempty"`
	NodeID  string          `json:"node_id,omitempty"`
	Agent   string          `json:"agent,omitempty"`
	Round   string          `json:"round,omitempty"`
	Kind    string          `json:"kind"`
	Key     string          `json:"key,omitempty"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// LLMCallPayload is a KindLLMCall entry's payload (one gen_ai "chat" call).
// Input/Output/SystemInstructions/ToolDefinitions are the JSON strings the
// emitter built, kept verbatim so replay hands back exactly what was seen.
type LLMCallPayload struct {
	Provider           string  `json:"provider,omitempty"`
	RequestModel       string  `json:"request_model"`
	ResponseModel      string  `json:"response_model,omitempty"`
	ResponseID         string  `json:"response_id,omitempty"`
	FinishReason       string  `json:"finish_reason,omitempty"`
	InputTokens        int64   `json:"input_tokens,omitempty"`
	OutputTokens       int64   `json:"output_tokens,omitempty"`
	Temperature        float64 `json:"temperature,omitempty"`
	MaxTokens          int64   `json:"max_tokens,omitempty"`
	PromptName         string  `json:"prompt_name,omitempty"`
	PromptVersion      string  `json:"prompt_version,omitempty"`
	SystemInstructions string  `json:"system_instructions,omitempty"`
	ToolDefinitions    string  `json:"tool_definitions,omitempty"`
	Input              string  `json:"input,omitempty"`
	Output             string  `json:"output,omitempty"`
	Error              string  `json:"error,omitempty"`
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
