// Package stream defines Quack's wire-level event vocabulary and translates
// the gate's ADK session-event stream into it, mirroring the frontend
// contract in frontend/src/state/agentStream.ts. Shared by REST and MCP.
//
// The model is flat and agent-centric: the DAG (dag_plan + node_* events) is
// the static structure, and within each node the gate runs a SEQUENCE of
// agent invocations ("runs") - worker draft, optional self-refine, each
// judge round, each revision. Every run is delimited by
// agent_start/agent_complete and carries a run_id + stage; its activity
// (agent_thinking, agent_tool_call, agent_tool_result, agent_token)
// references that run_id. The client groups runs by node and pairs tools by
// call_id - no nesting heuristics.
//
// Translation is stateful: the gate yields agent_start/agent_complete marker
// FunctionResponse parts to delimit runs, and a per-node Translator tracks
// the current run so passthrough activity attributes correctly. Token usage
// accumulates from raw model events and reports on agent_complete.
package stream

import (
	"encoding/json"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// transferTool is ADK's built-in dispatch tool; its call/response are suppressed
// (Quack dispatches via the DAG executor, not agent transfer).
const transferTool = "transfer_to_agent"

// Stage names label what an agent run is doing within a node.
const (
	StageWorker = "worker"
	StageJudge  = "judge"
	StageRevise = "revise"
)

// Gate marker tool names: the gate yields these as function-response parts to
// delimit each run; the Translator decodes them into agent_start/agent_complete.
// keepalive is a heartbeat the gate emits during slow operations to keep the A2A
// SSE connection alive; the Translator drops it.
const (
	agentStartTool    = "record_agent_start"
	agentCompleteTool = "record_agent_complete"
	keepaliveTool     = "_quack_keepalive"
)

// Event names.
const (
	EventAgentStart      = "agent_start"
	EventAgentThinking   = "agent_thinking"
	EventAgentToolCall   = "agent_tool_call"
	EventAgentToolResult = "agent_tool_result"
	EventAgentToken      = "agent_token"
	EventAgentComplete   = "agent_complete"

	EventChatTitle       = "chat_title"
	EventError           = "error"
	EventDone            = "done"
	EventResponseCreated = "response_created"

	// DAG / static structure.
	EventDagPlan        = "dag_plan"
	EventNodeQueued     = "node_queued"
	EventNodeStart      = "node_start"
	EventNodeDone       = "node_done"
	EventNodeNeedsInput = "node_needs_input"
	EventNodeFailed     = "node_failed"
	EventNodeCancelled  = "node_cancelled"
	EventNodePaused     = "node_paused"
	EventNodeSteered    = "node_steered"

	// EventDeliveryResult reports one staged item's outward-boundary outcome
	// (push + PR/review/comment) - durable, independent of the judge verdict,
	// so a phantom "the gate passed" success is distinguishable from an actual
	// delivery failure. See DeliveryResultData.
	EventDeliveryResult = "delivery_result"
)

// Delivery outcome values (DeliveryResultData.Outcome).
const (
	DeliveryOutcomeDelivered = "delivered"
	// DeliveryOutcomeDraft mirrors the documented gate-fail behaviour: a
	// successful delivery riding a failed verdict opens as a draft.
	DeliveryOutcomeDraft  = "draft"
	DeliveryOutcomeFailed = "failed"
	// DeliveryOutcomeNone is the phantom-success class: a judge-passed
	// work-request that recorded no delivery attempt at all.
	DeliveryOutcomeNone = "none"
)

// SSEEvent is one server-sent event: a name plus a JSON-serializable payload.
type SSEEvent struct {
	Name string
	Data any
}

// ── agent-run events ─────────────────────────────────────────────────────────

// AgentStartData opens an agent run within a node.
type AgentStartData struct {
	NodeID string `json:"node_id,omitempty"`
	RunID  string `json:"run_id"`
	Agent  string `json:"agent"`
	Stage  string `json:"stage"` // worker | judge | revise
	Round  int    `json:"round,omitempty"`
}

// AgentThinkingData is reasoning streamed during a run.
type AgentThinkingData struct {
	NodeID string `json:"node_id,omitempty"`
	RunID  string `json:"run_id"`
	Text   string `json:"text"`
}

// AgentTokenData is answer/output text. The final vetted answer is emitted with
// an empty RunID (it belongs to the node, not a particular run).
type AgentTokenData struct {
	NodeID string `json:"node_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	Text   string `json:"text"`
}

// AgentToolCallData is a tool invocation during a run; pairs with a result by CallID.
type AgentToolCallData struct {
	NodeID string         `json:"node_id,omitempty"`
	RunID  string         `json:"run_id"`
	CallID string         `json:"call_id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

// AgentToolResultData is the result of a tool call, matched to it by CallID.
type AgentToolResultData struct {
	NodeID string `json:"node_id,omitempty"`
	RunID  string `json:"run_id"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Result any    `json:"result"`
}

// AgentCompleteData closes an agent run. Fields are populated by stage: model +
// usage + finish_reason for model runs (worker/revise), score/passed/
// feedback for judge, and status/reason when a run was not completed normally
// (e.g. the judge was unavailable).
type AgentCompleteData struct {
	NodeID string `json:"node_id,omitempty"`
	RunID  string `json:"run_id"`
	Stage  string `json:"stage"`
	Round  int    `json:"round,omitempty"`

	Model            string `json:"model,omitempty"`
	PromptTokens     int32  `json:"prompt_tokens,omitempty"`
	CompletionTokens int32  `json:"completion_tokens,omitempty"`
	ReasoningTokens  int32  `json:"reasoning_tokens,omitempty"`
	TotalTokens      int32  `json:"total_tokens,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`

	Score    float64 `json:"score,omitempty"`    // judge
	Passed   bool    `json:"passed,omitempty"`   // judge
	Feedback string  `json:"feedback,omitempty"` // judge

	Status string `json:"status,omitempty"` // "" ok | "unavailable"
	Reason string `json:"reason,omitempty"`
}

// MarshalJSON forces judge runs to always serialize score/passed/feedback, even
// at their zero values. A failing verdict legitimately scores 0.0 with
// passed=false; the omitempty tags above would drop those, so the UI could not
// tell a real 0% score from an absent one and rendered no score badge at all
// (only passing, non-zero judges showed a score). Non-judge stages keep the
// omitempty behaviour - they carry no judge result.
func (d AgentCompleteData) MarshalJSON() ([]byte, error) {
	type alias AgentCompleteData // shed the MarshalJSON method to avoid recursion
	b, err := json.Marshal(alias(d))
	if err != nil || d.Stage != StageJudge {
		return b, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for key, val := range map[string]any{"score": d.Score, "passed": d.Passed, "feedback": d.Feedback} {
		if _, present := m[key]; present {
			continue // a non-zero value already survived omitempty
		}
		raw, err := json.Marshal(val)
		if err != nil {
			return nil, err
		}
		m[key] = raw
	}
	return json.Marshal(m)
}

// ErrorData is the `error` event payload.
type ErrorData struct {
	Error string `json:"error"`
}

// ── DAG / static structure ───────────────────────────────────────────────────

// DagNodeDef is the wire representation of one node in a DAG plan.
type DagNodeDef struct {
	ID        string   `json:"id"`
	Agent     string   `json:"agent"`
	Task      string   `json:"task"`
	DependsOn []string `json:"depends_on"`
}

// DagEdgeDef is the wire representation of one edge in a DAG plan.
type DagEdgeDef struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// DagPlanData is the `dag_plan` event payload.
type DagPlanData struct {
	PlanID string       `json:"plan_id"`
	Nodes  []DagNodeDef `json:"nodes"`
	Edges  []DagEdgeDef `json:"edges"`
	// StartedAtMs is the server wall-clock (epoch ms) the run began. The client
	// uses it for the total-run timer so a reconnect/replay shows true elapsed time
	// rather than restarting from when the events were re-processed.
	StartedAtMs int64 `json:"started_at_ms,omitempty"`
}

// NodeQueuedData is the `node_queued` event payload.
type NodeQueuedData struct {
	NodeID string `json:"node_id"`
}

// NodeStartData is the `node_start` event payload.
type NodeStartData struct {
	NodeID string `json:"node_id"`
	Agent  string `json:"agent"`
	// StartedAtMs is the server wall-clock (epoch ms) the node began running. The
	// client uses it as the timer origin so a reconnect/replay shows the node's true
	// elapsed time instead of restarting from the replay moment.
	StartedAtMs int64 `json:"started_at_ms,omitempty"`
}

// NodeDoneData is the `node_done` event payload. Completion stats are the sum
// across all runs made during the node's execution; omitted when zero.
type NodeDoneData struct {
	NodeID        string `json:"node_id"`
	OutputPreview string `json:"output_preview,omitempty"`
	// Output is the node's FULL vetted text. Carried so the store can persist it
	// (downstream rehydration on M5b resume needs the whole output, not just the
	// 250-char preview). The frontend streams the answer via agent_token and
	// ignores this field.
	Output           string  `json:"output,omitempty"`
	Model            string  `json:"model,omitempty"`
	PromptTokens     int32   `json:"prompt_tokens,omitempty"`
	CompletionTokens int32   `json:"completion_tokens,omitempty"`
	ReasoningTokens  int32   `json:"reasoning_tokens,omitempty"`
	TotalTokens      int32   `json:"total_tokens,omitempty"`
	FinishReason     string  `json:"finish_reason,omitempty"`
	DurationMs       int64   `json:"duration_ms,omitempty"`
	JudgeRounds      int32   `json:"judge_rounds,omitempty"`
	JudgeFinalScore  float64 `json:"judge_final_score,omitempty"`
	JudgePassed      bool    `json:"judge_passed,omitempty"`
}

// NodeFailedData is the `node_failed` event payload.
type NodeFailedData struct {
	NodeID string `json:"node_id"`
	Error  string `json:"error"`
}

// NodeCancelledData is the `node_cancelled` event payload: the node was
// stopped by the user (via PUT node status {"status":"cancelled"}), rendered
// neutrally ("stopped"), never as a red failure.
type NodeCancelledData struct {
	NodeID string `json:"node_id"`
}

// NodeCancelled builds a node_cancelled event.
func NodeCancelled(nodeID string) SSEEvent {
	return SSEEvent{Name: EventNodeCancelled, Data: NodeCancelledData{NodeID: nodeID}}
}

// NodeSteeredData is the `node_steered` event payload: the node's queued
// message(s) were delivered at its next turn boundary and it is about to
// re-run with them folded in (its prior session - tool calls and results -
// is retained). A fresh node_start … node_done follows.
type NodeSteeredData struct {
	NodeID   string `json:"node_id"`
	Guidance string `json:"guidance"`
}

// NodeSteered builds a node_steered event.
func NodeSteered(nodeID, guidance string) SSEEvent {
	return SSEEvent{Name: EventNodeSteered, Data: NodeSteeredData{NodeID: nodeID, Guidance: guidance}}
}

// NodePausedData is the `node_paused` event payload: the node was suspended
// by the user (via PUT node status {"status":"paused"}), keeping its
// accumulated work. Resumable via {"status":"running"}.
type NodePausedData struct {
	NodeID string `json:"node_id"`
}

// NodePaused builds a node_paused event.
func NodePaused(nodeID string) SSEEvent {
	return SSEEvent{Name: EventNodePaused, Data: NodePausedData{NodeID: nodeID}}
}

// DeliveryResultData is the `delivery_result` event payload: one staged
// item's ACTUAL outward-boundary outcome, as the delivering extension
// observed it (a real PR/review URL, or a real per-item error) - never the
// worker's self-report. Emitted for BOTH success and failure so a phantom
// "delivered" (judge passed, nothing actually posted) is visible in the
// durable event log, not just inferred from a missing log line.
type DeliveryResultData struct {
	NodeID  string `json:"node_id"`
	Outcome string `json:"outcome"` // delivered | draft | failed | none
	Kind    string `json:"kind,omitempty"`
	URL     string `json:"url,omitempty"`
	Error   string `json:"error,omitempty"`
	// TraceID cross-references this event into the OTel trace (Tempo/Grafana)
	// covering the same delivery - "" when otel is disabled or no span was active.
	TraceID string `json:"trace_id,omitempty"`
}

// DeliveryResult builds a delivery_result event.
func DeliveryResult(nodeID, outcome, kind, url, errMsg, traceID string) SSEEvent {
	return SSEEvent{Name: EventDeliveryResult, Data: DeliveryResultData{
		NodeID: nodeID, Outcome: outcome, Kind: kind, URL: url, Error: errMsg, TraceID: traceID,
	}}
}

// ChatTitleData is the `chat_title` event payload.
type ChatTitleData struct {
	Title string `json:"title"`
}

// ── event constructors ───────────────────────────────────────────────────────

// ResponseCreatedData is the `response_created` event payload: the very first
// event of a run, naming the turn (response_id) so a client can cancel it via
// PUT /chats/{chat_id}/responses/{response_id}/status.
type ResponseCreatedData struct {
	ResponseID string `json:"response_id"`
}

// ResponseCreated builds the response_created event that opens a run.
func ResponseCreated(responseID string) SSEEvent {
	return SSEEvent{Name: EventResponseCreated, Data: ResponseCreatedData{ResponseID: responseID}}
}

// DagPlan builds a dag_plan event carrying the full plan structure. StartedAtMs is
// stamped now (the run's start), so a reconnecting client's total timer is anchored
// to real time and survives replay.
func DagPlan(planID string, nodes []DagNodeDef, edges []DagEdgeDef) SSEEvent {
	return SSEEvent{Name: EventDagPlan, Data: DagPlanData{
		PlanID: planID, Nodes: nodes, Edges: edges, StartedAtMs: time.Now().UnixMilli(),
	}}
}

// NodeStart builds a node_start event. StartedAtMs is stamped now (when the node
// begins), so a reconnecting client's node timer is anchored to real time and
// survives replay.
func NodeStart(nodeID, agent string) SSEEvent {
	return SSEEvent{Name: EventNodeStart, Data: NodeStartData{
		NodeID: nodeID, Agent: agent, StartedAtMs: time.Now().UnixMilli(),
	}}
}

// NodeDone builds a node_done event.
func NodeDone(nodeID string, data NodeDoneData) SSEEvent {
	data.NodeID = nodeID
	return SSEEvent{Name: EventNodeDone, Data: data}
}

// NodeNeedsInputData is the `node_needs_input` payload: a node produced no answer
// and the run is paused for a human to steer it (re-run with guidance) or cancel.
// interrupt_id is the token a resolve call must echo back.
type NodeNeedsInputData struct {
	NodeID      string `json:"node_id"`
	InterruptID string `json:"interrupt_id"`
	Message     string `json:"message"`
}

// NodeNeedsInput builds a node_needs_input event for a paused (empty) node.
func NodeNeedsInput(nodeID, interruptID, message string) SSEEvent {
	return SSEEvent{Name: EventNodeNeedsInput, Data: NodeNeedsInputData{NodeID: nodeID, InterruptID: interruptID, Message: message}}
}

// NodeFailed builds a node_failed event.
func NodeFailed(nodeID, errMsg string) SSEEvent {
	return SSEEvent{Name: EventNodeFailed, Data: NodeFailedData{NodeID: nodeID, Error: errMsg}}
}

// ChatTitle builds a chat_title event.
func ChatTitle(title string) SSEEvent {
	return SSEEvent{Name: EventChatTitle, Data: ChatTitleData{Title: title}}
}

// Errorf builds an error event.
func Errorf(msg string) SSEEvent { return SSEEvent{Name: EventError, Data: ErrorData{Error: msg}} }

// Done builds the terminal done event.
func Done() SSEEvent { return SSEEvent{Name: EventDone, Data: struct{}{}} }

// The v1 gate emitted marker FunctionResponses (agent_start/agent_complete/
// keepalive) that the Translator decodes; the v2 gate no longer emits them, so
// the builders live in event_test.go as decoder fixtures. IsGateMarkerName and
// the tool-name consts stay: the executor still filters marker responses
// defensively and the decoder still recognizes them.

// ThinkingPart builds a reasoning part the gate yields directly (e.g. judge
// thinking re-emitted from its isolated run).
func ThinkingPart(text string) *genai.Part { return &genai.Part{Thought: true, Text: text} }

// IsGateMarkerName reports whether name is a reserved gate-internal tool name.
// These orphan FunctionResponses are hidden from the worker's session view during
// re-invocation (ADK errors on a trailing orphan FunctionResponse).
func IsGateMarkerName(name string) bool {
	switch name {
	case agentStartTool, agentCompleteTool, keepaliveTool:
		return true
	}
	return false
}

// ── stateful translation ─────────────────────────────────────────────────────

// Translator converts one node's gate event stream into wire events. It tracks
// the current run (delimited by agent_start/agent_complete markers) so activity
// is attributed correctly, and accumulates token usage per run to report on
// agent_complete. Create one per node stream; it is not safe for concurrent use.
type Translator struct {
	curRun   string
	curStage string
	curRound int
	curAgent string

	prompt, completion, reasoning, total int32
	model                                string
	finish                               string
}

// NewTranslator returns a Translator for one node stream.
func NewTranslator() *Translator { return &Translator{} }

// Usage returns the model/usage/finish-reason accumulated so far - either since
// the currently-open run started, or (for a caller with no run/marker protocol,
// e.g. the orchestrator's own un-gated direct-answer session) since the last time
// a run opened and reset the counters, i.e. the whole stream fed to this
// Translator. Safe to call at any point, including after Event has returned.
func (t *Translator) Usage() (model string, prompt, completion, reasoning, total int32, finishReason string) {
	return t.model, t.prompt, t.completion, t.reasoning, t.total, t.finish
}

// Event maps one ADK session event to zero or more wire events.
func (t *Translator) Event(ev *session.Event) []SSEEvent {
	if ev == nil {
		return nil
	}

	// Accumulate this event's usage/model/finish. Reported on the run's
	// agent_complete when a marker-delimited run is open; a caller with no run
	// concept at all - the orchestrator's own un-gated direct-answer session,
	// which never emits agent_start/agent_complete markers - reads the running
	// total straight off Usage() instead. Either way the counters reset to zero
	// when a new run opens (below), so this is never double-counted.
	if ev.UsageMetadata != nil {
		t.prompt += ev.UsageMetadata.PromptTokenCount
		t.completion += ev.UsageMetadata.CandidatesTokenCount
		t.reasoning += ev.UsageMetadata.ThoughtsTokenCount
		t.total += ev.UsageMetadata.TotalTokenCount
	}
	if ev.ModelVersion != "" {
		t.model = ev.ModelVersion
	}
	if ev.FinishReason != "" && ev.FinishReason != genai.FinishReasonUnspecified {
		t.finish = string(ev.FinishReason)
	}

	if ev.Content == nil {
		return nil
	}

	var out []SSEEvent
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.FunctionResponse != nil && p.FunctionResponse.Name == agentStartTool:
			r := p.FunctionResponse.Response
			t.curRun = asString(r["run_id"])
			t.curStage = asString(r["stage"])
			t.curRound = asInt(r["round"])
			t.curAgent = asString(r["agent"])
			t.prompt, t.completion, t.reasoning, t.total = 0, 0, 0, 0
			t.model, t.finish = "", ""
			out = append(out, SSEEvent{Name: EventAgentStart, Data: AgentStartData{
				RunID: t.curRun, Agent: t.curAgent, Stage: t.curStage, Round: t.curRound,
			}})

		case p.FunctionResponse != nil && p.FunctionResponse.Name == agentCompleteTool:
			r := p.FunctionResponse.Response
			d := AgentCompleteData{
				RunID: asString(r["run_id"]), Stage: asString(r["stage"]), Round: asInt(r["round"]),
				Score: asFloat(r["score"]), Passed: asBool(r["passed"]),
				Feedback: asString(r["feedback"]), Status: asString(r["status"]), Reason: asString(r["reason"]),
				Model: t.model, PromptTokens: t.prompt, CompletionTokens: t.completion,
				ReasoningTokens: t.reasoning, TotalTokens: t.total, FinishReason: t.finish,
			}
			out = append(out, SSEEvent{Name: EventAgentComplete, Data: d})
			t.curRun, t.curStage, t.curRound, t.curAgent = "", "", 0, ""

		case p.FunctionResponse != nil && p.FunctionResponse.Name == keepaliveTool:
			// heartbeat; no wire event

		case p.FunctionCall != nil:
			if p.FunctionCall.Name == transferTool {
				continue
			}
			out = append(out, SSEEvent{Name: EventAgentToolCall, Data: AgentToolCallData{
				RunID: t.curRun, CallID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Args: p.FunctionCall.Args,
			}})

		case p.FunctionResponse != nil:
			if p.FunctionResponse.Name == transferTool {
				continue
			}
			out = append(out, SSEEvent{Name: EventAgentToolResult, Data: AgentToolResultData{
				RunID: t.curRun, CallID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name, Result: p.FunctionResponse.Response,
			}})

		case p.Thought && p.Text != "":
			out = append(out, SSEEvent{Name: EventAgentThinking, Data: AgentThinkingData{RunID: t.curRun, Text: p.Text}})

		case p.Text != "":
			// Plain text is the final answer (the gate buffers per-run answers and
			// surfaces only the vetted one, with curRun cleared → node-level).
			out = append(out, SSEEvent{Name: EventAgentToken, Data: AgentTokenData{RunID: t.curRun, Text: p.Text}})
		}
	}
	return out
}

// ScopeToNode stamps nodeID onto a wire event's payload so the frontend routes it
// to the right DAG node. Events without a NodeID field are returned unchanged.
func ScopeToNode(ev SSEEvent, nodeID string) SSEEvent {
	switch d := ev.Data.(type) {
	case AgentStartData:
		d.NodeID = nodeID
		ev.Data = d
	case AgentThinkingData:
		d.NodeID = nodeID
		ev.Data = d
	case AgentTokenData:
		d.NodeID = nodeID
		ev.Data = d
	case AgentToolCallData:
		d.NodeID = nodeID
		ev.Data = d
	case AgentToolResultData:
		d.NodeID = nodeID
		ev.Data = d
	case AgentCompleteData:
		d.NodeID = nodeID
		ev.Data = d
	}
	return ev
}

// ScopeToRun stamps runID onto a wire event whose RunID is empty, so activity the
// orchestrator emits from its own (un-gated) run attaches to a single top-level
// run on the client instead of being dropped (the client keys activity by run_id
// and discards events with no matching run). Events that already carry a RunID,
// or have no RunID field, are returned unchanged.
func ScopeToRun(ev SSEEvent, runID string) SSEEvent {
	switch d := ev.Data.(type) {
	case AgentThinkingData:
		if d.RunID == "" {
			d.RunID = runID
			ev.Data = d
		}
	case AgentTokenData:
		if d.RunID == "" {
			d.RunID = runID
			ev.Data = d
		}
	case AgentToolCallData:
		if d.RunID == "" {
			d.RunID = runID
			ev.Data = d
		}
	case AgentToolResultData:
		if d.RunID == "" {
			d.RunID = runID
			ev.Data = d
		}
	}
	return ev
}

// Marker-payload values survive the A2A round-trip as JSON, so numbers may arrive
// as float64; these extractors read a value tolerantly with a zero fallback.
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

func asBool(v any) bool     { b, _ := v.(bool); return b }
func asString(v any) string { s, _ := v.(string); return s }
