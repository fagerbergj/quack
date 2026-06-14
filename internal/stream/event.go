// Package stream defines Quack's wire-level event vocabulary and translates the
// gate's ADK session-event stream into it. The vocabulary mirrors the frontend
// contract in frontend/src/state/agentStream.ts and is shared by the REST and
// MCP transports.
//
// The model is flat and agent-centric: the DAG (dag_plan + node_* events) is the
// static structure, and within each node the gate runs a SEQUENCE of agent
// invocations ("runs") — the worker draft, optional self-refine, each judge
// round, each revision. Every run is delimited by agent_start / agent_complete
// and carries a run_id + stage; its activity (agent_thinking, agent_tool_call,
// agent_tool_result, agent_token) references that run_id. The client groups runs
// by node and pairs tools by call_id — no nesting heuristics.
//
// Translation is stateful: the gate yields agent_start/agent_complete marker
// FunctionResponse parts to delimit runs, and a per-node Translator tracks the
// current run so the worker's passthrough activity is attributed to it. Token
// usage is accumulated from the raw model events and reported on agent_complete.
package stream

import (
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// transferTool is ADK's built-in dispatch tool; its call/response are suppressed
// (Quack dispatches via the DAG executor, not agent transfer).
const transferTool = "transfer_to_agent"

// Stage names label what an agent run is doing within a node.
const (
	StageWorker     = "worker"
	StageSelfRefine = "self_refine"
	StageJudge      = "judge"
	StageRevise     = "revise"
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

	EventChatTitle = "chat_title"
	EventError     = "error"
	EventDone      = "done"

	// DAG / static structure.
	EventDagPlan    = "dag_plan"
	EventNodeQueued = "node_queued"
	EventNodeStart  = "node_start"
	EventNodeDone   = "node_done"
	EventNodeFailed = "node_failed"
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
	Stage  string `json:"stage"` // worker | self_refine | judge | revise
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
// usage + finish_reason for model runs (worker/self_refine/revise), changed for
// self_refine, score/passed/feedback for judge, and status/reason when a run was
// not completed normally (e.g. the judge was unavailable).
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

	Changed  bool    `json:"changed,omitempty"`  // self_refine
	Score    float64 `json:"score,omitempty"`    // judge
	Passed   bool    `json:"passed,omitempty"`   // judge
	Feedback string  `json:"feedback,omitempty"` // judge

	Status string `json:"status,omitempty"` // "" ok | "unavailable"
	Reason string `json:"reason,omitempty"`
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
}

// NodeQueuedData is the `node_queued` event payload.
type NodeQueuedData struct {
	NodeID string `json:"node_id"`
}

// NodeStartData is the `node_start` event payload.
type NodeStartData struct {
	NodeID string `json:"node_id"`
	Agent  string `json:"agent"`
}

// NodeDoneData is the `node_done` event payload. Completion stats are the sum
// across all runs made during the node's execution; omitted when zero.
type NodeDoneData struct {
	NodeID           string  `json:"node_id"`
	OutputPreview    string  `json:"output_preview,omitempty"`
	Model            string  `json:"model,omitempty"`
	PromptTokens     int32   `json:"prompt_tokens,omitempty"`
	CompletionTokens int32   `json:"completion_tokens,omitempty"`
	ReasoningTokens  int32   `json:"reasoning_tokens,omitempty"`
	TotalTokens      int32   `json:"total_tokens,omitempty"`
	FinishReason     string  `json:"finish_reason,omitempty"`
	DurationMs       int64   `json:"duration_ms,omitempty"`
	SelfRefined      bool    `json:"self_refined,omitempty"`
	JudgeRounds      int32   `json:"judge_rounds,omitempty"`
	JudgeFinalScore  float64 `json:"judge_final_score,omitempty"`
	JudgePassed      bool    `json:"judge_passed,omitempty"`
}

// NodeFailedData is the `node_failed` event payload.
type NodeFailedData struct {
	NodeID string `json:"node_id"`
	Error  string `json:"error"`
}

// ChatTitleData is the `chat_title` event payload.
type ChatTitleData struct {
	Title string `json:"title"`
}

// ── event constructors ───────────────────────────────────────────────────────

// DagPlan builds a dag_plan event carrying the full plan structure.
func DagPlan(planID string, nodes []DagNodeDef, edges []DagEdgeDef) SSEEvent {
	return SSEEvent{Name: EventDagPlan, Data: DagPlanData{PlanID: planID, Nodes: nodes, Edges: edges}}
}

// NodeQueued builds a node_queued event.
func NodeQueued(nodeID string) SSEEvent {
	return SSEEvent{Name: EventNodeQueued, Data: NodeQueuedData{NodeID: nodeID}}
}

// NodeStart builds a node_start event.
func NodeStart(nodeID, agent string) SSEEvent {
	return SSEEvent{Name: EventNodeStart, Data: NodeStartData{NodeID: nodeID, Agent: agent}}
}

// NodeDone builds a node_done event.
func NodeDone(nodeID string, data NodeDoneData) SSEEvent {
	data.NodeID = nodeID
	return SSEEvent{Name: EventNodeDone, Data: data}
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

// ── gate marker parts (yielded by the gate, decoded by the Translator) ────────

// AgentStartPart encodes the start of an agent run.
func AgentStartPart(runID, agent, stage string, round int) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     agentStartTool,
		Response: map[string]any{"run_id": runID, "agent": agent, "stage": stage, "round": round},
	}}
}

// AgentCompletePart encodes the end of an agent run with its stage-specific
// result. Token usage / model / finish_reason are filled in by the Translator
// from the run's model events, so the gate need not supply them.
func AgentCompletePart(d AgentCompleteData) *genai.Part {
	resp := map[string]any{"run_id": d.RunID, "stage": d.Stage, "round": d.Round}
	if d.Changed {
		resp["changed"] = d.Changed
	}
	if d.Stage == StageJudge {
		resp["score"] = d.Score
		resp["passed"] = d.Passed
		resp["feedback"] = d.Feedback
	}
	if d.Status != "" {
		resp["status"] = d.Status
		resp["reason"] = d.Reason
	}
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{Name: agentCompleteTool, Response: resp}}
}

// KeepAlivePart builds the heartbeat marker the gate emits during long runs.
func KeepAlivePart() *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{Name: keepaliveTool, Response: map[string]any{}}}
}

// AgentTokenPart builds a plain-text part the gate yields for the final answer.
func AgentTokenPart(text string) *genai.Part { return &genai.Part{Text: text} }

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

// Event maps one ADK session event to zero or more wire events.
func (t *Translator) Event(ev *session.Event) []SSEEvent {
	if ev == nil {
		return nil
	}

	// Accumulate this event's usage/model/finish into the current run; reported
	// on the run's agent_complete. (Model events carry these; markers don't.)
	if t.curRun != "" {
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
				Changed: asBool(r["changed"]), Score: asFloat(r["score"]), Passed: asBool(r["passed"]),
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
