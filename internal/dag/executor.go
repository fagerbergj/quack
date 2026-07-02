package dag

import (
	"context"
	"iter"
	"strconv"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// nodeAppName is the ADK application name for a DAG run's workflow session.
const nodeAppName = "quack-nodes"

// Executor runs a Plan as an ADK v2 graph workflow (BuildWorkflow): one
// first-class gated-worker node per plan node, fanned out per DependsOn. It is
// the v2 replacement for the legacy TopoSort + semaphore + per-node-runner
// executor — ADK's scheduler owns concurrency, ordering, and (on a durable
// session store) restart-durable completed-node skipping.
type Executor struct {
	sessions    session.Service
	agents      map[string]adkagent.Agent             // agent name → built (plain) agent
	advisor     adkagent.Agent                        // formative advisor consulted per refine round; nil = disabled
	judge       vetting.JudgeFactory                  // independent judge factory
	cfgFor      func(agentName string) vetting.Config // per-agent gate config (rubric override etc.)
	mediaAgents map[string]bool                       // agents accepting image/audio parts
	controls    *runControls                          // live per-node cancel/steer handles (M5b)
	maxActive   int                                   // concurrent-node cap for the single-runner runDAG path (default 2)
}

// SetMaxActive sets the concurrent-node cap used by RunPlanInNode (config
// dag.max_active_nodes). No-op for values < 1.
func (e *Executor) SetMaxActive(n int) {
	if n >= 1 {
		e.maxActive = n
	}
}

// RunPlanInNode runs a plan's gated nodes via runDAG in the CURRENT workflow
// node's sub-scheduler (single runner) — the entry point for the one-orchestrator-
// workflow path. Returns node ID → vetted output; a gate node's empty-pause
// (ErrNodeInterrupted) propagates up so the whole run pauses for human steer/cancel.
func (e *Executor) RunPlanInNode(ctx adkagent.Context, plan Plan, chatID string) (map[string]string, error) {
	gateNodes, _, err := buildGateNodes(plan, e.agents, e.advisor, e.judge, e.cfgFor, e.mediaAgents, e.controls, chatID)
	if err != nil {
		return nil, err
	}
	return runDAG(ctx, plan, gateNodes, e.maxActive)
}

// NewExecutor returns a graph Executor. agents maps agent name → plain agent
// (no longer pre-wrapped in the gate — the graph wraps each node in the refine
// loop). cfgFor supplies the per-agent trust-gate config.
func NewExecutor(sessions session.Service, agents map[string]adkagent.Agent, advisor adkagent.Agent, judge vetting.JudgeFactory, cfgFor func(string) vetting.Config, mediaAgents map[string]bool) *Executor {
	return &Executor{sessions: sessions, agents: agents, advisor: advisor, judge: judge, cfgFor: cfgFor, mediaAgents: mediaAgents, controls: newRunControls(), maxActive: 2}
}

// Execute builds the plan's workflow, runs it via a fresh runner, and translates
// the workflow's raw session-event stream into Quack's SSE vocabulary:
//   - node_start (live, on a node's first event) + node_done (per node, with the
//     judge score/passed/rounds read from session state);
//   - within each node, the worker's runs as agent_start/agent_complete with
//     agent_thinking / agent_tool_call / agent_tool_result / agent_token activity.
//
// Translation is SSE-only: it NEVER writes back into the workflow session, so it
// cannot re-poison a downstream node's model request the way orphan-marker
// FunctionResponses did in v1 (see the 3a spike finding). The judge runs in its
// own isolated runner (off this stream), so its result is carried via session
// state (dag.gateScoreKey…) rather than the event stream.
//
// nodeOutputs (node ID → vetted answer) is filled for the caller's TerminalOutput.
func (e *Executor) Execute(ctx context.Context, plan Plan, userID, chatID string, nodeOutputs map[string]string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		root, err := BuildWorkflow(plan, e.agents, e.advisor, e.judge, e.cfgFor, e.mediaAgents, e.controls, chatID)
		if err != nil {
			yield(stream.Errorf("dag: "+err.Error()), nil)
			return
		}
		r, err := runner.New(runner.Config{AppName: nodeAppName, Agent: root, SessionService: e.sessions, AutoCreateSession: true})
		if err != nil {
			yield(stream.Errorf("dag: "+err.Error()), nil)
			return
		}
		// Thread-safe yield: fan-out nodes run on concurrent workflow goroutines, and
		// each may stream judge-stage SSE (below) at the same time the main loop yields
		// node lifecycle — serialize all SSE through one mutex.
		var mu sync.Mutex
		safeYield := func(ev stream.SSEEvent, e error) bool { mu.Lock(); defer mu.Unlock(); return yield(ev, e) }
		// Inject an SSE sink so vetting.RunGatedRefine can stream the judge's own
		// run (it executes in an isolated runner OFF this event stream) up to the
		// client as stage:judge activity — SSE-only, never written to the session.
		ctx = stream.WithYield(ctx, func(ev stream.SSEEvent) { safeYield(ev, nil) })
		// The plan tool already emitted dag_plan for this plan_id (and M8 persists
		// it); re-emitting here caused a duplicate insert (dag_plans_pkey). The plan
		// tool owns dag_plan emission — the executor only streams node lifecycle.

		// ponytail: media attachments + per-node History threading are deferred
		// (follow-up). Leaf nodes assemble their prompt from plan.UserMessage via
		// buildTask, so text research works end-to-end now.
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}

		agentByID := make(map[string]string, len(plan.Nodes))
		for _, n := range plan.Nodes {
			agentByID[n.ID] = n.AgentName
		}

		ds := newDagStream(agentByID, safeYield, nodeOutputs, func(nodeID string) gateScore {
			return e.gateScore(ctx, userID, plan.ID, nodeID)
		})

		paused := map[string]bool{} // nodes paused for human steer/cancel (empty output)
		for ev, rerr := range r.Run(ctx, userID, plan.ID, content, adkagent.RunConfig{}) {
			if rerr != nil {
				safeYield(stream.Errorf(rerr.Error()), nil)
				return
			}
			if ev == nil {
				continue
			}
			// Fail-into-steerable pause: a node produced nothing and requested input.
			// Surface it (not a node_done) so the client can steer or cancel it.
			if ev.RequestedInput != nil {
				nid := nodeIDFromPayload(ev.RequestedInput.Payload)
				paused[nid] = true
				if !safeYield(stream.NodeNeedsInput(nid, ev.RequestedInput.InterruptID, ev.RequestedInput.Message), nil) {
					return
				}
				continue
			}
			if !ds.handle(ev) {
				return
			}
		}
		if !ds.flush() {
			return
		}
		// Seed the terminal node from the last output if its own event was missed,
		// so TerminalOutput and its node_done have an answer.
		ensureTerminal(plan, nodeOutputs, ds.last)
		// node_done for every plan node that hasn't already emitted one live.
		for _, n := range plan.Nodes {
			if ds.doneEmitted[n.ID] || paused[n.ID] {
				continue // a paused (needs-input) node isn't done — no node_done
			}
			if !safeYield(stream.NodeDone(n.ID, ds.nodeDoneData(n.ID)), nil) {
				return
			}
		}
	}
}

// nodeIDFromPayload pulls the node_id a paused node stamped on its RequestInput.
func nodeIDFromPayload(p any) string {
	if m, ok := p.(map[string]any); ok {
		if s, ok := m["node_id"].(string); ok {
			return s
		}
	}
	return ""
}

// gateScore is a node's trust-gate result read back from workflow session state.
type gateScore struct {
	score  float64
	passed bool
	rounds int
}

// gateScore reads a node's persisted judge result (written by the gated node via
// dag.gateScoreKey…). Returns the zero value if the session/state/keys are absent.
func (e *Executor) gateScore(ctx context.Context, userID, planID, nodeID string) gateScore {
	var g gateScore
	resp, err := e.sessions.Get(ctx, &session.GetRequest{AppName: nodeAppName, UserID: userID, SessionID: planID})
	if err != nil || resp == nil {
		return g
	}
	st := resp.Session.State()
	if st == nil {
		return g
	}
	if v, err := st.Get(gateScoreKey + nodeID); err == nil {
		g.score = toFloat(v)
	}
	if v, err := st.Get(gatePassedKey + nodeID); err == nil {
		g.passed, _ = v.(bool)
	}
	if v, err := st.Get(gateRoundsKey + nodeID); err == nil {
		g.rounds = toInt(v)
	}
	return g
}

// ── stream translation ───────────────────────────────────────────────────────

// dagStream converts the workflow's raw event stream into SSE, live and SSE-only
// (it never writes to the workflow session). It synthesises per-node worker runs
// (agent_start/agent_complete + activity) from NodeInfo.Path and emits node_done
// per node with the judge score fetched from session state.
type dagStream struct {
	agentByID map[string]string
	yield     func(stream.SSEEvent, error) bool
	outputs   map[string]string      // nodeID → captured output (== caller's nodeOutputs)
	scoreOf   func(string) gateScore // reads a node's persisted judge result

	started     map[string]bool   // node_start emitted
	doneEmitted map[string]bool   // node_done emitted
	curRun      map[string]string // nodeID → active worker runID
	usage       map[string]*runUsage
	last        string // last non-empty output (terminal fallback)
	stopped     bool
}

type runUsage struct {
	prompt, completion, reasoning, total int32
	model, finish                        string
}

func newDagStream(agentByID map[string]string, yield func(stream.SSEEvent, error) bool, outputs map[string]string, scoreOf func(string) gateScore) *dagStream {
	return &dagStream{
		agentByID: agentByID, yield: yield, outputs: outputs, scoreOf: scoreOf,
		started: map[string]bool{}, doneEmitted: map[string]bool{},
		curRun: map[string]string{}, usage: map[string]*runUsage{},
	}
}

func (s *dagStream) emit(ev stream.SSEEvent) bool {
	if s.stopped {
		return false
	}
	if !s.yield(ev, nil) {
		s.stopped = true
		return false
	}
	return true
}

// handle translates one workflow event; returns false if the consumer stopped.
func (s *dagStream) handle(ev *session.Event) bool {
	if s.stopped {
		return false
	}
	if ev.NodeInfo == nil || ev.NodeInfo.Path == "" {
		return true
	}
	node := planNodeInPath(ev.NodeInfo.Path, s.agentByID)
	if node == "" {
		return true // not a plan-node event (root/join/etc.)
	}
	if !s.started[node] {
		s.started[node] = true
		if !s.emit(stream.NodeStart(node, s.agentByID[node])) {
			return false
		}
	}

	last := lastSeg(ev.NodeInfo.Path)
	// The gated node's OWN event (last segment == the plan node): its output marks
	// node completion. node_done fires live here with the judge score from state.
	if segName(last) == node {
		if ev.Output != nil {
			if out := outputString(ev.Output); out != "" {
				s.outputs[node] = out
				s.last = out
			}
		}
		if ev.Output != nil && !s.doneEmitted[node] {
			s.closeRun(node) // end any open worker run first
			s.doneEmitted[node] = true
			if !s.emit(stream.NodeDone(node, s.nodeDoneData(node))) {
				return false
			}
		}
		return true
	}

	// A worker-run child event (path segment "…@worker-rN"): stream its activity as
	// a run under the node.
	runID := segRun(last)
	// worker-rN = the gated worker's draft/revision; advisor-rN = the formative
	// advisor consult (both run via RunNode on this stream). Anything else (e.g. a
	// worker's own sub-agent tool run) isn't a node-level run — skip it.
	if !strings.HasPrefix(runID, "worker") && !strings.HasPrefix(runID, "advisor") {
		return true
	}
	if s.curRun[node] != runID {
		if !s.closeRun(node) {
			return false
		}
		s.curRun[node] = runID
		s.usage[node] = &runUsage{}
		st, rd := stageRound(runID)
		if !s.emit(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentStart, Data: stream.AgentStartData{
			RunID: runID, Agent: s.agentByID[node], Stage: st, Round: rd,
		}}, node)) {
			return false
		}
	}
	s.accum(node, ev)
	if ev.Content == nil {
		return true
	}
	for _, p := range ev.Content.Parts {
		if !s.part(node, runID, p) {
			return false
		}
	}
	return true
}

// part translates one content part of a worker-run event into an SSE event.
func (s *dagStream) part(node, runID string, p *genai.Part) bool {
	if p == nil {
		return true
	}
	switch {
	case p.FunctionResponse != nil && stream.IsGateMarkerName(p.FunctionResponse.Name):
		return true // defensive: legacy markers no longer emitted
	case p.FunctionCall != nil:
		if p.FunctionCall.Name == "transfer_to_agent" {
			return true
		}
		return s.emit(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentToolCall, Data: stream.AgentToolCallData{
			RunID: runID, CallID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Args: p.FunctionCall.Args,
		}}, node))
	case p.FunctionResponse != nil:
		if p.FunctionResponse.Name == "transfer_to_agent" {
			return true
		}
		return s.emit(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentToolResult, Data: stream.AgentToolResultData{
			RunID: runID, CallID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name, Result: p.FunctionResponse.Response,
		}}, node))
	case p.Thought && p.Text != "":
		return s.emit(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentThinking, Data: stream.AgentThinkingData{
			RunID: runID, Text: p.Text,
		}}, node))
	case p.Text != "":
		return s.emit(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentToken, Data: stream.AgentTokenData{
			RunID: runID, Text: p.Text,
		}}, node))
	}
	return true
}

// accum folds an event's usage/model/finish into the node's active worker run.
func (s *dagStream) accum(node string, ev *session.Event) {
	u := s.usage[node]
	if u == nil {
		return
	}
	if ev.UsageMetadata != nil {
		u.prompt += ev.UsageMetadata.PromptTokenCount
		u.completion += ev.UsageMetadata.CandidatesTokenCount
		u.reasoning += ev.UsageMetadata.ThoughtsTokenCount
		u.total += ev.UsageMetadata.TotalTokenCount
	}
	if ev.ModelVersion != "" {
		u.model = ev.ModelVersion
	}
	if ev.FinishReason != "" && ev.FinishReason != genai.FinishReasonUnspecified {
		u.finish = string(ev.FinishReason)
	}
}

// closeRun emits agent_complete for a node's active worker run, if any.
func (s *dagStream) closeRun(node string) bool {
	runID := s.curRun[node]
	if runID == "" {
		return true
	}
	st, rd := stageRound(runID)
	d := stream.AgentCompleteData{RunID: runID, Stage: st, Round: rd}
	if u := s.usage[node]; u != nil {
		d.Model, d.FinishReason = u.model, u.finish
		d.PromptTokens, d.CompletionTokens, d.ReasoningTokens, d.TotalTokens = u.prompt, u.completion, u.reasoning, u.total
	}
	s.curRun[node] = ""
	s.usage[node] = nil
	return s.emit(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentComplete, Data: d}, node))
}

// flush closes any worker runs still open at end of stream.
func (s *dagStream) flush() bool {
	for node := range s.curRun {
		if !s.closeRun(node) {
			return false
		}
	}
	return !s.stopped
}

// nodeDoneData builds a node's node_done payload from its captured output and the
// judge result read from session state.
func (s *dagStream) nodeDoneData(node string) stream.NodeDoneData {
	out := s.outputs[node]
	d := stream.NodeDoneData{Output: out, OutputPreview: preview(out)}
	if s.scoreOf != nil {
		g := s.scoreOf(node)
		d.JudgeFinalScore = g.score
		d.JudgePassed = g.passed
		d.JudgeRounds = int32(g.rounds)
	}
	return d
}

// ── path + value helpers ─────────────────────────────────────────────────────

// planNodeInPath returns the first NodeInfo.Path segment naming a plan node
// (a key of agentByID), or "" if none — worker/join/root segments are ignored.
// A worker event's path is "…/<planNode>@<rid>/<worker>@worker-rN", so the plan
// node is found before the deeper worker segment.
func planNodeInPath(path string, agentByID map[string]string) string {
	for _, seg := range strings.Split(path, "/") {
		if name := segName(seg); name != "" {
			if _, ok := agentByID[name]; ok {
				return name
			}
		}
	}
	return ""
}

func lastSeg(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func segName(seg string) string {
	if i := strings.Index(seg, "@"); i >= 0 {
		return seg[:i]
	}
	return seg
}

func segRun(seg string) string {
	if i := strings.Index(seg, "@"); i >= 0 {
		return seg[i+1:]
	}
	return ""
}

// stageRound maps a run ID to its SSE stage + round: advisor-rN is the formative
// advisor consult; worker-r0 is the initial worker draft; worker-rN (N≥1) is a
// revision; worker-finalize-* is an empty-answer write-up (a worker stage).
func stageRound(runID string) (string, int) {
	switch {
	case strings.HasPrefix(runID, "advisor-r"):
		return stream.StageAdvisor, toInt(runID[len("advisor-r"):])
	case strings.HasPrefix(runID, "worker-r"):
		if n := toInt(runID[len("worker-r"):]); n > 0 {
			return stream.StageRevise, n
		}
	}
	return stream.StageWorker, 0
}

func outputString(o any) string {
	if s, ok := o.(string); ok {
		return stream.StripThinking(s)
	}
	return ""
}

func preview(s string) string {
	const n = 250
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// toFloat/toInt read state values tolerantly (JSON round-trips numbers as float64).
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

// buildTask assembles a node's worker prompt: the verbatim user request, each
// dependency's output (prefixed with a ⚠ warning when it failed vetting), then the
// node's own task. A leaf with no upstream is just its task.
func buildTask(plan Plan, node Node, upstream map[string]string, gateFailed map[string]bool) string {
	var sb strings.Builder
	if plan.UserMessage != "" {
		sb.WriteString("User's request (verbatim):\n")
		sb.WriteString(plan.UserMessage)
		sb.WriteString("\n\n---\n\n")
	}
	for _, dep := range node.DependsOn {
		if out, ok := upstream[dep]; ok && strings.TrimSpace(out) != "" {
			if gateFailed[dep] {
				sb.WriteString("⚠ WARNING: the following input FAILED independent quality vetting (unverified claims or missing citations). Treat its claims skeptically and do not present them as verified:\n\n")
			}
			sb.WriteString(out)
			sb.WriteString("\n\n---\n\n")
		}
	}
	if sb.Len() == 0 {
		return node.Task
	}
	sb.WriteString("Your task: ")
	sb.WriteString(node.Task)
	return sb.String()
}

// ensureTerminal seeds the plan's terminal node (no successors) from fallback
// when per-node capture missed it, so TerminalOutput has an answer.
func ensureTerminal(plan Plan, nodeOutputs map[string]string, fallback string) {
	if fallback == "" {
		return
	}
	hasSucc := map[string]bool{}
	for _, n := range plan.Nodes {
		for _, d := range n.DependsOn {
			hasSucc[d] = true
		}
	}
	for _, n := range plan.Nodes {
		if !hasSucc[n.ID] {
			if _, ok := nodeOutputs[n.ID]; !ok {
				nodeOutputs[n.ID] = fallback
			}
			return
		}
	}
}
