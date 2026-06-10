// Package stream defines Quack's wire-level event vocabulary and translates
// ADK session events into it. The vocabulary mirrors the frontend contract in
// frontend/src/state/agentStream.ts and is shared by the REST and MCP transports.
//
// Every activity event carries the `agent` that produced it (the ADK event
// author), and the dispatch lifecycle is explicit: `agent_start` when the
// orchestrator transfers to a specialist, `agent_end` when that specialist's turn
// completes. The frontend uses these to nest activity under the right actor
// instead of inferring boundaries from message content.
package stream

import (
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// OrchestratorAuthor is the ADK author of the root dispatcher's events (mirrors
// the agent name in internal/orchestrator). Its turn-completion is its own, not a
// dispatched specialist's, so it does not emit an agent_end.
const OrchestratorAuthor = "orchestrator"

// transferTool is ADK's built-in dispatch tool; its call/response surface as the
// agent_start lifecycle event rather than as a normal tool call.
const transferTool = "transfer_to_agent"

// The trust gate (internal/vetting) emits its self-refine and judge activity as
// session events carrying a single marker function-response part with one of
// these reserved names. Encoding them this way means they ride the same A2A
// artifact path as real tool results, are skipped by the chat-history projection,
// and — as orphan responses with no matching call — are dropped from the worker's
// future LLM context by ADK. Translate decodes them into dedicated wire events.
const (
	judgeTool            = "record_judge_verdict"
	judgeStartTool       = "record_judge_start"
	selfRefineTool       = "record_self_refine"
	selfRefineStartTool  = "record_self_refine_start"
	judgeUnavailableTool = "record_judge_unavailable"
	reviseTool           = "record_revise"
	// keepaliveTool is a heartbeat the gate emits every ~30 s during slow
	// operations (judge model load + generation) to keep the A2A SSE connection
	// alive. Translate drops it — it produces no wire event.
	keepaliveTool = "_quack_keepalive"
)

// Event names. M0 emitted only token / done / error; the rest fill in as later
// milestones emit them.
const (
	EventToken               = "token"
	EventThinking            = "thinking"
	EventToolCall            = "tool_call"
	EventToolResult          = "tool_result"
	EventAgentStart          = "agent_start"
	EventAgentEnd            = "agent_end"
	EventSelfRefineStart     = "self_refine_start"
	EventSelfRefine          = "self_refine"
	EventJudgeStart          = "judge_start"
	EventRevise              = "revise"
	EventJudgeVerdict        = "judge_verdict"
	EventJudgeUnavailable    = "judge_unavailable"
	EventConfirmationRequest = "confirmation_request"
	EventChatTitle           = "chat_title"
	EventError               = "error"
	EventDone                = "done"
	// DAG events (M3).
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

// TokenData is the `token` event payload.
type TokenData struct {
	NodeID string `json:"node_id,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Text   string `json:"text"`
}

// ErrorData is the `error` event payload.
type ErrorData struct {
	Error string `json:"error"`
}

// ThinkingData is the `thinking` event payload.
type ThinkingData struct {
	NodeID string `json:"node_id,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Text   string `json:"text"`
}

// ToolCallData is the `tool_call` event payload.
type ToolCallData struct {
	NodeID string         `json:"node_id,omitempty"`
	Agent  string         `json:"agent,omitempty"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

// ToolResultData is the `tool_result` event payload.
type ToolResultData struct {
	NodeID string `json:"node_id,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Name   string `json:"name"`
	Result any    `json:"result"`
}

// AgentData is the payload for the agent_start / agent_end lifecycle events.
// Completion stats are populated only on agent_end when the model reported them;
// all stat fields are omitted when zero.
type AgentData struct {
	NodeID           string `json:"node_id,omitempty"`
	Agent            string `json:"agent"`
	Model            string `json:"model,omitempty"`
	PromptTokens     int32  `json:"prompt_tokens,omitempty"`
	CompletionTokens int32  `json:"completion_tokens,omitempty"`
	ReasoningTokens  int32  `json:"reasoning_tokens,omitempty"`
	TotalTokens      int32  `json:"total_tokens,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
}

// SelfRefineData is the `self_refine` event payload: the gate ran the worker's
// own self-critique pre-pass, and whether it changed the answer.
type SelfRefineData struct {
	NodeID  string `json:"node_id,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Changed bool   `json:"changed"`
}

// ReviseData is the `revise` event payload: the gate revised the answer in
// response to the judge's feedback before starting the next round.
type ReviseData struct {
	NodeID string `json:"node_id,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Round  int    `json:"round"`
}

// JudgeVerdictData is the `judge_verdict` event payload: the independent judge's
// score for one round, whether it passed the threshold, and revision feedback.
type JudgeVerdictData struct {
	NodeID   string  `json:"node_id,omitempty"`
	Agent    string  `json:"agent,omitempty"`
	Round    int     `json:"round"`
	Score    float64 `json:"score"`
	Passed   bool    `json:"passed"`
	Feedback string  `json:"feedback,omitempty"`
}

// JudgeUnavailableData is the `judge_unavailable` event payload: the judge
// failed and the answer is surfaced unvetted, with a quality warning.
type JudgeUnavailableData struct {
	NodeID string `json:"node_id,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Round  int    `json:"round"`
	Reason string `json:"reason,omitempty"`
}

// JudgeStartData is the `judge_start` event payload: the gate is beginning
// an independent judge round. Pairs with a later judge_verdict to close it.
type JudgeStartData struct {
	NodeID string `json:"node_id,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Round  int    `json:"round"`
}

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

// NodeDoneData is the `node_done` event payload. Completion stats are the
// sum across all LLM calls made during the node's run; omitted when zero.
type NodeDoneData struct {
	NodeID           string `json:"node_id"`
	OutputPreview    string `json:"output_preview,omitempty"`
	Model            string `json:"model,omitempty"`
	PromptTokens     int32  `json:"prompt_tokens,omitempty"`
	CompletionTokens int32  `json:"completion_tokens,omitempty"`
	ReasoningTokens  int32  `json:"reasoning_tokens,omitempty"`
	TotalTokens      int32  `json:"total_tokens,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
	DurationMs       int64  `json:"duration_ms,omitempty"`
}

// NodeFailedData is the `node_failed` event payload.
type NodeFailedData struct {
	NodeID string `json:"node_id"`
	Error  string `json:"error"`
}

// Token builds a token event.
func Token(agent, text string) SSEEvent {
	return SSEEvent{Name: EventToken, Data: TokenData{Agent: agent, Text: text}}
}

// Thinking builds a thinking (reasoning) event.
func Thinking(agent, text string) SSEEvent {
	return SSEEvent{Name: EventThinking, Data: ThinkingData{Agent: agent, Text: text}}
}

// ToolCall builds a tool_call event.
func ToolCall(agent, name string, args map[string]any) SSEEvent {
	return SSEEvent{Name: EventToolCall, Data: ToolCallData{Agent: agent, Name: name, Args: args}}
}

// ToolResult builds a tool_result event.
func ToolResult(agent, name string, result any) SSEEvent {
	return SSEEvent{Name: EventToolResult, Data: ToolResultData{Agent: agent, Name: name, Result: result}}
}

// AgentStart marks the orchestrator dispatching to a specialist agent.
func AgentStart(agent string) SSEEvent {
	return SSEEvent{Name: EventAgentStart, Data: AgentData{Agent: agent}}
}

// AgentEnd marks a specialist agent's turn completing, optionally carrying
// completion metadata extracted from the underlying LLM response.
func AgentEnd(data AgentData) SSEEvent {
	return SSEEvent{Name: EventAgentEnd, Data: data}
}

// SelfRefine builds a self_refine event.
func SelfRefine(agent string, changed bool) SSEEvent {
	return SSEEvent{Name: EventSelfRefine, Data: SelfRefineData{Agent: agent, Changed: changed}}
}

// Revise builds a revise event: the gate revised the worker's answer in
// response to judge feedback before running the next judge round.
func Revise(agent string, round int) SSEEvent {
	return SSEEvent{Name: EventRevise, Data: ReviseData{Agent: agent, Round: round}}
}

// JudgeVerdict builds a judge_verdict event.
func JudgeVerdict(agent string, round int, score float64, passed bool, feedback string) SSEEvent {
	return SSEEvent{Name: EventJudgeVerdict, Data: JudgeVerdictData{
		Agent: agent, Round: round, Score: score, Passed: passed, Feedback: feedback,
	}}
}

// JudgeUnavailable builds a judge_unavailable event: the judge failed and the
// answer is being surfaced anyway with a quality-cannot-be-guaranteed flag.
func JudgeUnavailable(agent string, round int, reason string) SSEEvent {
	return SSEEvent{Name: EventJudgeUnavailable, Data: JudgeUnavailableData{
		Agent: agent, Round: round, Reason: reason,
	}}
}

// SelfRefineStart signals that the gate is beginning a self-refine pass.
// Thinking events that follow belong inside this container until SelfRefine closes it.
func SelfRefineStart(agent string) SSEEvent {
	return SSEEvent{Name: EventSelfRefineStart, Data: AgentData{Agent: agent}}
}

// JudgeStart signals that the gate is beginning an independent judge round.
// Thinking events that follow belong inside this container until JudgeVerdict closes it.
func JudgeStart(agent string, round int) SSEEvent {
	return SSEEvent{Name: EventJudgeStart, Data: JudgeStartData{Agent: agent, Round: round}}
}

// SelfRefinePart encodes a self-refine pass as the marker function-response part
// the trust gate yields (see the judgeTool/selfRefineTool comment).
func SelfRefinePart(changed bool) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     selfRefineTool,
		Response: map[string]any{"changed": changed},
	}}
}

// KeepAlivePart builds the marker part the gate emits during long-running judge
// or revise calls to prevent A2A SSE connection idle timeouts. Translate drops it.
func KeepAlivePart() *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     keepaliveTool,
		Response: map[string]any{},
	}}
}

// SelfRefineStartPart encodes the start of a self-refine pass as a marker
// function-response part the trust gate yields.
func SelfRefineStartPart() *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     selfRefineStartTool,
		Response: map[string]any{},
	}}
}

// JudgeStartPart encodes the start of a judge round as a marker
// function-response part the trust gate yields.
func JudgeStartPart(round int) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     judgeStartTool,
		Response: map[string]any{"round": round},
	}}
}

// ThinkingPart creates a reasoning part for direct emission by the gate during
// self-refine and judge model calls, surfacing as a `thinking` wire event.
func ThinkingPart(text string) *genai.Part {
	return &genai.Part{Thought: true, Text: text}
}

// JudgeVerdictPart encodes one judge verdict as the marker function-response part
// the trust gate yields.
func JudgeVerdictPart(round int, score float64, passed bool, feedback string) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name: judgeTool,
		Response: map[string]any{
			"round": round, "score": score, "passed": passed, "feedback": feedback,
		},
	}}
}

// RevisePart encodes a revision pass as the marker function-response part the
// trust gate yields after the judge requests a revision.
func RevisePart(round int) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     reviseTool,
		Response: map[string]any{"round": round},
	}}
}

// JudgeUnavailablePart encodes a judge-unavailable notice as the marker
// function-response part the trust gate yields before surfacing the answer.
func JudgeUnavailablePart(round int, reason string) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     judgeUnavailableTool,
		Response: map[string]any{"round": round, "reason": reason},
	}}
}

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

// ScopeToNode sets the NodeID field on an activity SSEEvent's data payload,
// routing it to the correct DAG node in the frontend. Events that don't carry
// a NodeID field (dag lifecycle events, done, error) are returned unchanged.
func ScopeToNode(ev SSEEvent, nodeID string) SSEEvent {
	switch d := ev.Data.(type) {
	case TokenData:
		d.NodeID = nodeID
		ev.Data = d
	case ThinkingData:
		d.NodeID = nodeID
		ev.Data = d
	case ToolCallData:
		d.NodeID = nodeID
		ev.Data = d
	case ToolResultData:
		d.NodeID = nodeID
		ev.Data = d
	case AgentData:
		d.NodeID = nodeID
		ev.Data = d
	case SelfRefineData:
		d.NodeID = nodeID
		ev.Data = d
	case ReviseData:
		d.NodeID = nodeID
		ev.Data = d
	case JudgeVerdictData:
		d.NodeID = nodeID
		ev.Data = d
	case JudgeUnavailableData:
		d.NodeID = nodeID
		ev.Data = d
	case JudgeStartData:
		d.NodeID = nodeID
		ev.Data = d
	}
	return ev
}

// ChatTitleData is the `chat_title` event payload.
type ChatTitleData struct {
	Title string `json:"title"`
}

// ChatTitle builds a chat_title event: sent as soon as the title LLM call
// completes so the client can update the sidebar without waiting for done.
func ChatTitle(title string) SSEEvent {
	return SSEEvent{Name: EventChatTitle, Data: ChatTitleData{Title: title}}
}

// Errorf builds an error event.
func Errorf(msg string) SSEEvent { return SSEEvent{Name: EventError, Data: ErrorData{Error: msg}} }

// Done builds the terminal done event.
func Done() SSEEvent { return SSEEvent{Name: EventDone, Data: struct{}{}} }

// Translate maps one ADK session event to zero or more wire events. Each part is
// labeled by kind — reasoning (Thought) → thinking, function calls → tool_call,
// function responses → tool_result, plain text → token — and tagged with the
// event's author so the frontend can attribute it. A dispatch (Actions.
// TransferToAgent) emits agent_start; a specialist's turn completion (TurnComplete
// from a non-orchestrator author) emits agent_end. The built-in transfer tool's
// own call/response are suppressed in favor of those lifecycle events. The caller
// emits the terminal `done` after the stream ends.
func Translate(ev *session.Event) []SSEEvent {
	if ev == nil {
		return nil
	}
	agent := ev.Author
	var out []SSEEvent

	// A dispatch opens the specialist's group before its events arrive.
	if ev.Actions.TransferToAgent != "" {
		out = append(out, AgentStart(ev.Actions.TransferToAgent))
	}

	if ev.Content != nil {
		for _, p := range ev.Content.Parts {
			switch {
			case p == nil:
				continue
			case p.Thought && p.Text != "":
				out = append(out, Thinking(agent, p.Text))
			case p.FunctionCall != nil:
				if p.FunctionCall.Name == transferTool {
					continue // surfaced as agent_start
				}
				out = append(out, ToolCall(agent, p.FunctionCall.Name, p.FunctionCall.Args))
			case p.FunctionResponse != nil:
				switch p.FunctionResponse.Name {
				case transferTool:
					continue // surfaced as agent_start
				case keepaliveTool:
					continue // heartbeat only; no wire event
				case selfRefineStartTool:
					out = append(out, SelfRefineStart(agent))
					continue
				case selfRefineTool:
					r := p.FunctionResponse.Response
					out = append(out, SelfRefine(agent, asBool(r["changed"])))
					continue
				case judgeStartTool:
					r := p.FunctionResponse.Response
					out = append(out, JudgeStart(agent, asInt(r["round"])))
					continue
				case judgeTool:
					r := p.FunctionResponse.Response
					out = append(out, JudgeVerdict(agent, asInt(r["round"]), asFloat(r["score"]), asBool(r["passed"]), asString(r["feedback"])))
					continue
				case judgeUnavailableTool:
					r := p.FunctionResponse.Response
					out = append(out, JudgeUnavailable(agent, asInt(r["round"]), asString(r["reason"])))
					continue
				case reviseTool:
					r := p.FunctionResponse.Response
					out = append(out, Revise(agent, asInt(r["round"])))
					continue
				}
				out = append(out, ToolResult(agent, p.FunctionResponse.Name, p.FunctionResponse.Response))
			case p.Text != "":
				out = append(out, Token(agent, p.Text))
			}
		}
	}

	// A specialist completing its turn closes its group. The orchestrator's own
	// turn-completion is the run itself, closed by the caller, not a dispatch.
	// Attach completion metadata from the underlying LLM response when available.
	if ev.TurnComplete && agent != "" && agent != OrchestratorAuthor {
		end := AgentData{Agent: agent, Model: ev.ModelVersion}
		if ev.FinishReason != "" && ev.FinishReason != genai.FinishReasonUnspecified {
			end.FinishReason = string(ev.FinishReason)
		}
		if ev.UsageMetadata != nil {
			end.PromptTokens = ev.UsageMetadata.PromptTokenCount
			end.CompletionTokens = ev.UsageMetadata.CandidatesTokenCount
			end.ReasoningTokens = ev.UsageMetadata.ThoughtsTokenCount
			end.TotalTokens = ev.UsageMetadata.TotalTokenCount
		}
		out = append(out, AgentEnd(end))
	}
	return out
}

// IsGateMarkerName reports whether name is a reserved gate-internal tool name.
// These events must be hidden from the worker's session view during agentic
// self-refine — they are orphan FunctionResponses (no matching FunctionCall)
// and ADK v1.4.0+ errors if it sees one as the last session event.
func IsGateMarkerName(name string) bool {
	switch name {
	case judgeTool, judgeStartTool, selfRefineTool, selfRefineStartTool,
		judgeUnavailableTool, reviseTool, keepaliveTool:
		return true
	}
	return false
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
