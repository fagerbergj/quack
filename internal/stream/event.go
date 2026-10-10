// Package stream defines Quack's wire-level event vocabulary and translates gate ADK session events into it.
// Client contract: frontend/src/state/agentStream.ts; clients group by node and pair tools by call_id.
package stream

import (
	"encoding/json"
	"strings"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// ADK's built-in dispatch tool; suppressed (Quack dispatches via DAG executor).
const transferTool = "transfer_to_agent"

// Agent run stages within a node.
const (
	StageWorker = "worker"
	StageJudge  = "judge"
	StageRevise = "revise"
)

// Gate marker tool names delimit each run as function-response parts; the Translator drops keepalive.
const (
	agentStartTool    = "record_agent_start"
	agentCompleteTool = "record_agent_complete"
	keepaliveTool     = "_quack_keepalive"
)

// Event names. Mirrors frontend/src/state/agentStream.ts.
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
	EventNodeAdmitted   = "node_admitted"
	EventNodeStart      = "node_start"
	EventNodeDone       = "node_done"
	EventNodeNeedsInput = "node_needs_input"
	EventNodeFailed     = "node_failed"
	EventNodeCancelled  = "node_cancelled"
	EventNodePaused     = "node_paused"
	EventNodeSteered    = "node_steered"

	// EventDeliveryResult reports one staged item's push/PR/review outcome, independent of the judge verdict,
	// so a passed gate is distinguishable from a failed delivery.
	EventDeliveryResult = "delivery_result"

	// EventCompaction reports a worker session compacted by adk's runner, observed via a decorated
	// session.Service since neither native strategy yields the summary into the event stream.
	EventCompaction = "compaction"

	// EventArtifactRevision is emitted before the round's EventArtifactJudgeRound, so the revision exists first.
	EventArtifactRevision = "artifact_revision"
	// EventArtifactJudgeRound reports the judge_round record a round wrote,
	// referencing the revisions EventArtifactRevision already announced.
	EventArtifactJudgeRound = "artifact_judge_round"
)

// Delivery outcome values.
const (
	DeliveryOutcomeDelivered = "delivered"
	DeliveryOutcomeDraft     = "draft" // successful delivery, but gate verdict failed - opened as draft
	DeliveryOutcomeFailed    = "failed"
	DeliveryOutcomeNone      = "none" // judge passed but no delivery was recorded
)

// One server-sent event: name + JSON-serializable payload.
type SSEEvent struct {
	Name string
	Data any
}

// ── agent-run events ───

// AgentStartData opens an agent run within a node.
type AgentStartData struct {
	NodeID string `json:"node_id,omitempty"`
	RunID  string `json:"run_id"`
	Agent  string `json:"agent"`
	Stage  string `json:"stage"` // worker | judge | revise
	Round  int    `json:"round,omitempty"`
	// Server wall-clock (epoch ms) the run began - anchors the per-run timer across reconnect/replay.
	StartedAtMs int64 `json:"started_at_ms,omitempty"`
	// TraceID cross-references the OTel trace for this run; "" when otel is disabled.
	TraceID string `json:"trace_id,omitempty"`
}

// Reasoning streamed during a run.
type AgentThinkingData struct {
	NodeID string `json:"node_id,omitempty"`
	RunID  string `json:"run_id"`
	Text   string `json:"text"`
}

// Answer/output text. The final vetted answer has an empty RunID (belongs to the node, not a run).
type AgentTokenData struct {
	NodeID string `json:"node_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	Text   string `json:"text"`
}

// Tool invocation during a run; pairs with a result by CallID.
type AgentToolCallData struct {
	NodeID string         `json:"node_id,omitempty"`
	RunID  string         `json:"run_id"`
	CallID string         `json:"call_id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

// Result of a tool call, matched by CallID.
type AgentToolResultData struct {
	NodeID string `json:"node_id,omitempty"`
	RunID  string `json:"run_id"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Result any    `json:"result"`
}

// AgentCompleteData closes an agent run: model/usage for worker/revise, score/passed/feedback for judge,
// status/reason for abnormal completion.
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
	CachedTokens     int32  `json:"cached_tokens,omitempty"` // subset of PromptTokens served from the model's prompt cache
	// ContextTokens is the LAST measured prompt-token count (context occupancy), not summed like PromptTokens.
	ContextTokens int32  `json:"context_tokens,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`

	Score    float64 `json:"score,omitempty"`    // judge
	Passed   bool    `json:"passed,omitempty"`   // judge
	Feedback string  `json:"feedback,omitempty"` // judge; rendered one-paragraph summary, kept for one release so the UI does not blank
	// Envelope is the structured verdict: a *vetting envelope or nil, typed any to avoid an import cycle.
	Envelope any `json:"envelope,omitempty"` // judge

	Status string `json:"status,omitempty"` // "" ok | "unavailable" (judge unreachable) | "no_verdict" (judge ran, never committed one)
	Reason string `json:"reason,omitempty"`

	// FinishedAtMs is the server close time (epoch ms), so a replaying client computes duration from server clocks.
	FinishedAtMs int64 `json:"finished_at_ms,omitempty"`
}

// MarshalJSON keeps judge score/passed/feedback even at zero values, which omitempty would drop.
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

// `error` event payload.
type ErrorData struct {
	Error string `json:"error"`
}

// ── DAG / static ───

// DagNodeDef is the wire representation of one node in a DAG plan.
type DagNodeDef struct {
	ID        string   `json:"id"`
	Agent     string   `json:"agent"`
	Task      string   `json:"task"`
	DependsOn []string `json:"depends_on"`
	// ContextWindow is the assigned agent's configured context_window (0 if
	// unset) - the context meter's static limit.
	ContextWindow int `json:"context_window,omitempty"`
	// Artifact is the node's declared output artifact kind - the record name
	// its output is saved as on gate pass; absent when the node declares none.
	Artifact string `json:"artifact,omitempty"`
}

// Wire representation of one edge in a DAG plan.
type DagEdgeDef struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// `dag_plan` event payload.
type DagPlanData struct {
	PlanID string       `json:"plan_id"`
	Nodes  []DagNodeDef `json:"nodes"`
	Edges  []DagEdgeDef `json:"edges"`
	// Server wall-clock (epoch ms) the run began - anchors total-run timer across reconnect/replay.
	StartedAtMs int64 `json:"started_at_ms,omitempty"`
	// TraceID cross-references the OTel trace for this run; "" when otel is disabled.
	TraceID string `json:"trace_id,omitempty"`
	// ExecPlan is the full dag.Plan JSON, persisted server-side so a resume can run
	// the node's own plan (see store.DagExecPlan); never sent to clients.
	ExecPlan json.RawMessage `json:"-"`
}

// `node_queued` event payload.
type NodeQueuedData struct {
	NodeID string `json:"node_id"`
}

// NodeQueued builds a node_queued event, emitted at admission-attempt time so
// a node waiting on capacity reads as "waiting", not hung.
func NodeQueued(nodeID string) SSEEvent {
	return SSEEvent{Name: EventNodeQueued, Data: NodeQueuedData{NodeID: nodeID}}
}

// `node_admitted` event payload.
type NodeAdmittedData struct {
	NodeID string `json:"node_id"`
}

// NodeAdmitted builds a node_admitted event: a node_queued node resumed
// running - distinct from node_start, which fires only once per node.
func NodeAdmitted(nodeID string) SSEEvent {
	return SSEEvent{Name: EventNodeAdmitted, Data: NodeAdmittedData{NodeID: nodeID}}
}

// `node_start` event payload.
type NodeStartData struct {
	NodeID string `json:"node_id"`
	Agent  string `json:"agent"`
	// Server wall-clock (epoch ms) the node began - anchors per-node timer across reconnect/replay.
	StartedAtMs int64 `json:"started_at_ms,omitempty"`
	// TraceID cross-references the OTel trace for this node; "" when otel is disabled.
	TraceID string `json:"trace_id,omitempty"`
	// ResumedFrom is the prior node id this dispatch continues on; "" for a freshly minted node.
	ResumedFrom string `json:"resumed_from,omitempty"`
}

// `node_done` event payload. Completion stats are summed across all runs; omitted when zero.
type NodeDoneData struct {
	NodeID        string `json:"node_id"`
	OutputPreview string `json:"output_preview,omitempty"`
	// Full vetted text - store persists it for downstream rehydration. Frontend ignores it (streams via agent_token).
	Output           string  `json:"output,omitempty"`
	Model            string  `json:"model,omitempty"`
	PromptTokens     int32   `json:"prompt_tokens,omitempty"`
	CompletionTokens int32   `json:"completion_tokens,omitempty"`
	ReasoningTokens  int32   `json:"reasoning_tokens,omitempty"`
	TotalTokens      int32   `json:"total_tokens,omitempty"`
	CachedTokens     int32   `json:"cached_tokens,omitempty"`
	ContextTokens    int32   `json:"context_tokens,omitempty"` // last measured, not summed - see AgentCompleteData
	FinishReason     string  `json:"finish_reason,omitempty"`
	DurationMs       int64   `json:"duration_ms,omitempty"`
	JudgeRounds      int32   `json:"judge_rounds,omitempty"`
	JudgeFinalScore  float64 `json:"judge_final_score,omitempty"`
	JudgePassed      bool    `json:"judge_passed,omitempty"`
	// FinishedAtMs is the server wall-clock (epoch ms) the node finished - see
	// AgentCompleteData.FinishedAtMs.
	FinishedAtMs int64 `json:"finished_at_ms,omitempty"`
	// ContextID is the node's resumable transport context id at completion; "" for native nodes or ACP without one.
	ContextID string `json:"context_id,omitempty"`
}

// `node_failed` event payload.
type NodeFailedData struct {
	NodeID string `json:"node_id"`
	Error  string `json:"error"`
	// FinishedAtMs is the server wall-clock (epoch ms) the node failed - see
	// AgentCompleteData.FinishedAtMs.
	FinishedAtMs int64 `json:"finished_at_ms,omitempty"`
	// ContextID: see NodeDoneData.ContextID - a failed node can still have
	// established a real transport session before failing.
	ContextID string `json:"context_id,omitempty"`
}

// `node_cancelled` event payload: node stopped by the user, rendered neutrally (not as red failure).
type NodeCancelledData struct {
	NodeID string `json:"node_id"`
	// FinishedAtMs is the server wall-clock (epoch ms) the node was cancelled - see
	// AgentCompleteData.FinishedAtMs.
	FinishedAtMs int64 `json:"finished_at_ms,omitempty"`
	// ContextID: see NodeDoneData.ContextID.
	ContextID string `json:"context_id,omitempty"`
	// Output is the draft the node had when stopped - unreviewed, never the answer.
	Output        string `json:"output,omitempty"`
	OutputPreview string `json:"output_preview,omitempty"`
}

// NodeCancelled builds a node_cancelled event, stamping FinishedAtMs now (see NodeDone).
func NodeCancelled(nodeID string) SSEEvent {
	return SSEEvent{Name: EventNodeCancelled, Data: NodeCancelledData{NodeID: nodeID, FinishedAtMs: time.Now().UnixMilli()}}
}

// NodeSteeredData: queued messages were delivered at the node's next turn boundary; a fresh node_start follows.
type NodeSteeredData struct {
	NodeID   string `json:"node_id"`
	Guidance string `json:"guidance"`
}

// NodeSteered builds a node_steered event.
func NodeSteered(nodeID, guidance string) SSEEvent {
	return SSEEvent{Name: EventNodeSteered, Data: NodeSteeredData{NodeID: nodeID, Guidance: guidance}}
}

// `node_paused` event payload: node suspended by the user, keeping accumulated work. Resumable.
type NodePausedData struct {
	NodeID string `json:"node_id"`
}

// NodePaused builds a node_paused event.
func NodePaused(nodeID string) SSEEvent {
	return SSEEvent{Name: EventNodePaused, Data: NodePausedData{NodeID: nodeID}}
}

// DeliveryResultData is one staged item's outcome as the delivering extension observed it, never the worker's.
type DeliveryResultData struct {
	NodeID  string `json:"node_id"`
	Outcome string `json:"outcome"` // delivered | draft | failed | none
	Kind    string `json:"kind,omitempty"`
	URL     string `json:"url,omitempty"`
	Error   string `json:"error,omitempty"`
	// Cross-references into the OTel trace for the same delivery; "" when otel is disabled.
	TraceID string `json:"trace_id,omitempty"`
}

// DeliveryResult builds a delivery_result event.
func DeliveryResult(nodeID, outcome, kind, url, errMsg, traceID string) SSEEvent {
	return SSEEvent{Name: EventDeliveryResult, Data: DeliveryResultData{
		NodeID: nodeID, Outcome: outcome, Kind: kind, URL: url, Error: errMsg, TraceID: traceID,
	}}
}

// ArtifactRevisionData: `artifact_revision` event payload.
type ArtifactRevisionData struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	Kind     string `json:"kind"`
	NodeID   string `json:"node_id"`
	Round    int    `json:"round"`
}

// ScoredRef mirrors vetting.ScoredRef for the SSE wire shape, avoiding a
// stream -> vetting import.
type ScoredRef struct {
	ArtifactID string `json:"artifact_id"`
	Revision   int    `json:"revision"`
}

// ArtifactJudgeRoundData: `artifact_judge_round` event payload.
type ArtifactJudgeRoundData struct {
	ID     string      `json:"id"`
	Passed bool        `json:"passed"`
	Score  float64     `json:"score"`
	Scored []ScoredRef `json:"scored"`
}

// ChatTitleData is the `chat_title` event payload.
type ChatTitleData struct {
	Title string `json:"title"`
}

// CompactionData carries exactly what adk's EventCompaction and UsageMetadata expose; adk reports no
// before/after size.
type CompactionData struct {
	NodeID string `json:"node_id,omitempty"`
	// RunID is the adk invocation that triggered the compaction, not a quack
	// worker-run id (adk native compaction isn't scoped to one of those).
	RunID               string `json:"run_id,omitempty"`
	StartTimestamp      string `json:"start_timestamp,omitempty"`
	EndTimestamp        string `json:"end_timestamp,omitempty"`
	SummaryInputTokens  int32  `json:"summary_input_tokens,omitempty"`
	SummaryOutputTokens int32  `json:"summary_output_tokens,omitempty"`
}

// RunIDFromBranch extracts the run id from an ADK branch segment "<name>@<runID>", so a compaction row's
// run id matches its round's agent_start run id.
func RunIDFromBranch(branch string) string {
	if i := strings.Index(branch, "@"); i >= 0 {
		return branch[i+1:]
	}
	return ""
}

// Compaction builds a compaction event; zero times and token counts are omitted, as adk leaves them unset.
func Compaction(nodeID, runID string, start, end time.Time, inputTokens, outputTokens int32) SSEEvent {
	d := CompactionData{NodeID: nodeID, RunID: runID, SummaryInputTokens: inputTokens, SummaryOutputTokens: outputTokens}
	if !start.IsZero() {
		d.StartTimestamp = start.UTC().Format(time.RFC3339)
	}
	if !end.IsZero() {
		d.EndTimestamp = end.UTC().Format(time.RFC3339)
	}
	return SSEEvent{Name: EventCompaction, Data: d}
}

// ── event constructors ───

// `response_created` event payload: the first event of a run, naming the turn (response_id) for cancellation.
type ResponseCreatedData struct {
	ResponseID string `json:"response_id"`
}

// ResponseCreated builds the response_created event that opens a run.
func ResponseCreated(responseID string) SSEEvent {
	return SSEEvent{Name: EventResponseCreated, Data: ResponseCreatedData{ResponseID: responseID}}
}

// Builds a dag_plan event with StartedAtMs stamped now so the timer anchors to real time across replay.
func DagPlan(planID string, nodes []DagNodeDef, edges []DagEdgeDef) SSEEvent {
	return SSEEvent{Name: EventDagPlan, Data: DagPlanData{
		PlanID: planID, Nodes: nodes, Edges: edges, StartedAtMs: time.Now().UnixMilli(),
	}}
}

// Builds a node_start event with StartedAtMs stamped now.
func NodeStart(nodeID, agent string) SSEEvent {
	return SSEEvent{Name: EventNodeStart, Data: NodeStartData{
		NodeID: nodeID, Agent: agent, StartedAtMs: time.Now().UnixMilli(),
	}}
}

// WithTrace stamps a trace id onto payloads with a TraceID field. Only the raw id travels, never a URL,
// so a replayed event survives trace-backend changes.
func WithTrace(ev SSEEvent, traceID string) SSEEvent {
	if traceID == "" {
		return ev
	}
	switch d := ev.Data.(type) {
	case AgentStartData:
		d.TraceID = traceID
		ev.Data = d
	case NodeStartData:
		d.TraceID = traceID
		ev.Data = d
	case DagPlanData:
		d.TraceID = traceID
		ev.Data = d
	}
	return ev
}

// WithResumedFrom stamps a node_start event's ResumedFrom, keeping NodeStart two-arg for fresh nodes.
func WithResumedFrom(ev SSEEvent, resumedFrom string) SSEEvent {
	if resumedFrom == "" {
		return ev
	}
	if d, ok := ev.Data.(NodeStartData); ok {
		d.ResumedFrom = resumedFrom
		ev.Data = d
	}
	return ev
}

// WithContextID stamps a node_failed/node_cancelled ContextID: a node can open a transport session before
// failing, and that id must reach the dag_node record like NodeDoneData's does.
func WithContextID(ev SSEEvent, contextID string) SSEEvent {
	if contextID == "" {
		return ev
	}
	switch d := ev.Data.(type) {
	case NodeFailedData:
		d.ContextID = contextID
		ev.Data = d
	case NodeCancelledData:
		d.ContextID = contextID
		ev.Data = d
	}
	return ev
}

// NodeDone stamps FinishedAtMs now; runlog.SynthesizeChatEvents builds its own with the ledger entry's At.
func NodeDone(nodeID string, data NodeDoneData) SSEEvent {
	data.NodeID = nodeID
	data.FinishedAtMs = time.Now().UnixMilli()
	return SSEEvent{Name: EventNodeDone, Data: data}
}

// NodeNeedsInputData: the node produced no answer and paused for steering; echo interrupt_id on resolve.
type NodeNeedsInputData struct {
	NodeID      string `json:"node_id"`
	InterruptID string `json:"interrupt_id"`
	Message     string `json:"message"`
}

// NodeNeedsInput builds a node_needs_input event for a paused (empty) node.
func NodeNeedsInput(nodeID, interruptID, message string) SSEEvent {
	return SSEEvent{Name: EventNodeNeedsInput, Data: NodeNeedsInputData{NodeID: nodeID, InterruptID: interruptID, Message: message}}
}

// NodeFailed builds a node_failed event, stamping FinishedAtMs now (see NodeDone).
func NodeFailed(nodeID, errMsg string) SSEEvent {
	return SSEEvent{Name: EventNodeFailed, Data: NodeFailedData{NodeID: nodeID, Error: errMsg, FinishedAtMs: time.Now().UnixMilli()}}
}

// ChatTitle builds a chat_title event.
func ChatTitle(title string) SSEEvent {
	return SSEEvent{Name: EventChatTitle, Data: ChatTitleData{Title: title}}
}

// DeliveredAnswerMeta marks (in session event CustomMetadata) the orchestrator message carrying a
// turn's delivered answer; its value is that turn's id ("" = the turn the event falls in).
const DeliveredAnswerMeta = "quack_delivered_answer"

// DeliveredAtMeta is the delivery's own nanosecond time (RFC3339Nano): stored event timestamps can
// tie between quick successive answers, and the latest delivery for a turn must win deterministically.
const DeliveredAtMeta = "quack_delivered_at"

// SinkAnswer is one sink node's part of a multi-sink answer; a stopped one ships only a note.
type SinkAnswer struct {
	Label   string
	Text    string
	Stopped bool
}

// StoppedSinkNote replaces a stopped sink's draft, which never passed review.
const StoppedSinkNote = "_Stopped, not reviewed._"

// JoinSinkAnswers renders each sink as its own "## label" section, in order; the frontend's sinkSections mirrors it.
func JoinSinkAnswers(sinks []SinkAnswer) string {
	parts := make([]string, 0, len(sinks))
	for _, s := range sinks {
		body := strings.TrimSpace(s.Text)
		switch {
		case s.Stopped:
			body = StoppedSinkNote
		case body == "":
			body = "_No output._"
		}
		parts = append(parts, "## "+s.Label+"\n\n"+body)
	}
	return strings.Join(parts, "\n\n")
}

// Errorf builds an error event.
func Errorf(msg string) SSEEvent { return SSEEvent{Name: EventError, Data: ErrorData{Error: msg}} }

// Done builds the terminal done event.
func Done() SSEEvent { return SSEEvent{Name: EventDone, Data: struct{}{}} }

// Builds a reasoning part the gate yields directly (e.g. judge thinking re-emitted from its isolated run).
func ThinkingPart(text string) *genai.Part { return &genai.Part{Thought: true, Text: text} }

// IsGateMarkerName reports a reserved gate-internal tool name, hidden from the worker session
// (ADK errors on orphan FunctionResponses).
func IsGateMarkerName(name string) bool {
	switch name {
	case agentStartTool, agentCompleteTool, keepaliveTool:
		return true
	}
	return false
}

// ── stateful translation ───

// Translator converts one node's gate event stream into wire events, tracking the current run.
// Not safe for concurrent use.
type Translator struct {
	curRun   string
	curStage string
	curRound int
	curAgent string

	prompt, completion, reasoning, total, cached int32
	model                                        string
	finish                                       string

	// seenToolCalls dedups agent_tool_call by call_id: ACP emits a start update and a
	// completion update that both carry the FunctionCall part, mapping to the same call twice.
	seenToolCalls SeenCalls
}

// SeenCalls collapses ACP's start+completion FunctionCall parts into one agent_tool_call per call_id.
// Empty ids are never deduped: the frontend treats callId=="" as distinct, non-upsertable calls.
type SeenCalls map[string]bool

// Add records callID and reports whether it was already seen (so the caller
// should skip re-emitting).
func (s *SeenCalls) Add(callID string) bool {
	if callID == "" {
		return false
	}
	if (*s)[callID] {
		return true
	}
	if *s == nil {
		*s = make(SeenCalls)
	}
	(*s)[callID] = true
	return false
}

// NewTranslator returns a Translator for one node stream.
func NewTranslator() *Translator { return &Translator{} }

// Returns accumulated model/usage/finish-reason. Counters reset when a new run opens. Safe to call at any point.
func (t *Translator) Usage() (model string, prompt, completion, reasoning, total, cached int32, finishReason string) {
	return t.model, t.prompt, t.completion, t.reasoning, t.total, t.cached, t.finish
}

// Event maps one ADK session event to zero or more wire events.
func (t *Translator) Event(ev *session.Event) []SSEEvent {
	if ev == nil {
		return nil
	}
	t.recordUsage(ev)
	if ev.Content == nil {
		return nil
	}
	var out []SSEEvent
	for _, p := range ev.Content.Parts {
		if p != nil {
			out = t.appendPart(out, p)
		}
	}
	return out
}

// recordUsage accumulates per-run counters; they go out on agent_complete (marker runs) or via
// Usage() (un-gated sessions).
func (t *Translator) recordUsage(ev *session.Event) {
	if ev.UsageMetadata != nil {
		t.prompt += ev.UsageMetadata.PromptTokenCount
		t.completion += ev.UsageMetadata.CandidatesTokenCount
		t.reasoning += ev.UsageMetadata.ThoughtsTokenCount
		t.total += ev.UsageMetadata.TotalTokenCount
		t.cached += ev.UsageMetadata.CachedContentTokenCount
	}
	if ev.ModelVersion != "" {
		t.model = ev.ModelVersion
	}
	if ev.FinishReason != "" && ev.FinishReason != genai.FinishReasonUnspecified {
		t.finish = string(ev.FinishReason)
	}
}

func (t *Translator) appendPart(out []SSEEvent, p *genai.Part) []SSEEvent {
	switch {
	case p.FunctionResponse != nil && p.FunctionResponse.Name == agentStartTool:
		r := p.FunctionResponse.Response
		t.curRun = asString(r["run_id"])
		t.curStage = asString(r["stage"])
		t.curRound = asInt(r["round"])
		t.curAgent = asString(r["agent"])
		t.prompt, t.completion, t.reasoning, t.total, t.cached = 0, 0, 0, 0, 0
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
			ReasoningTokens: t.reasoning, TotalTokens: t.total, CachedTokens: t.cached, FinishReason: t.finish,
			FinishedAtMs: time.Now().UnixMilli(),
		}
		out = append(out, SSEEvent{Name: EventAgentComplete, Data: d})
		t.curRun, t.curStage, t.curRound, t.curAgent = "", "", 0, ""

	case p.FunctionResponse != nil && p.FunctionResponse.Name == keepaliveTool:
		// heartbeat, no wire event

	case p.FunctionCall != nil:
		if p.FunctionCall.Name == transferTool || t.seenToolCalls.Add(p.FunctionCall.ID) {
			return out
		}
		out = append(out, SSEEvent{Name: EventAgentToolCall, Data: AgentToolCallData{
			RunID: t.curRun, CallID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Args: p.FunctionCall.Args,
		}})

	case p.FunctionResponse != nil:
		if p.FunctionResponse.Name == transferTool {
			return out
		}
		out = append(out, SSEEvent{Name: EventAgentToolResult, Data: AgentToolResultData{
			RunID: t.curRun, CallID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name, Result: p.FunctionResponse.Response,
		}})

	case p.Thought && p.Text != "":
		out = append(out, SSEEvent{Name: EventAgentThinking, Data: AgentThinkingData{RunID: t.curRun, Text: p.Text}})

	case p.Text != "":
		// Final answer (gate surfaces only the vetted one, curRun cleared → node-level).
		out = append(out, SSEEvent{Name: EventAgentToken, Data: AgentTokenData{RunID: t.curRun, Text: p.Text}})
	}
	return out
}

// Stamps nodeID onto a wire event's payload for frontend DAG routing. Events without NodeID are unchanged.
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

// ScopeToRun stamps runID onto events lacking one, so un-gated orchestrator activity attaches to a top-level run.
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

// A2A round-trip marshals numbers as float64; these extractors read tolerantly with a zero fallback.
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
