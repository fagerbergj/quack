package dag

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// Executor runs a Plan as an ADK v2 graph workflow.
type Executor struct {
	sessions    session.Service
	agents      map[string]adkagent.Agent
	models      map[string]model.LLM
	judge       vetting.JudgeFactory
	cfgFor      func(agentName string) vetting.Config
	mediaAgents map[string]bool
	controls    *runControls
	maxActive   int
	setupFn     SetupFunc
	artifacts   artifact.Service // ADK's own artifact tools/debug console; see SetArtifacts
	admission   *Admission
	specFor     func(agentName string) AdmissionSpec

	gateResults sync.Map
}

// SetAdmission wires the #1007 capacity ledger and its per-agent spec
// resolver. Nil admission (the zero Executor) runs unbounded, same as before #1007.
func (e *Executor) SetAdmission(admission *Admission, specFor func(agentName string) AdmissionSpec) {
	e.admission, e.specFor = admission, specFor
}

// SetMaxActive: sets concurrent-node cap (no-op for n < 1).
func (e *Executor) SetMaxActive(n int) {
	if n >= 1 {
		e.maxActive = n
	}
}

// SetArtifacts wires an artifact.Service into the plan graph's runner,
// reachable via adkagent.Context.Artifacts(). Node attachments are rerouted
// separately, at the REST/plan entry boundary (internal/artifactref).
func (e *Executor) SetArtifacts(svc artifact.Service) { e.artifacts = svc }

// ResetNodeCancels: clears user-cancelled node flags for the next turn.
func (e *Executor) ResetNodeCancels(chatID string) { e.controls.resetCancelled(chatID) }

// DagStream: translates gate-node events into SSE.
type DagStream struct {
	ctx       context.Context
	ds        *dagStream
	plan      Plan
	agentByID map[string]string
	yield     func(stream.SSEEvent, error) bool
	only      map[string]bool
}

// ScopeToRetry: restricts terminal sweep to retried node and descendants.
func (s *DagStream) ScopeToRetry(nodeID string) { s.only = retrySet(s.plan, nodeID) }

// ScopeToResume: restricts sweep to resumed nodes and descendants.
func (s *DagStream) ScopeToResume(nodeIDs []string) {
	s.only = map[string]bool{}
	for _, id := range nodeIDs {
		for k := range retrySet(s.plan, id) {
			s.only[k] = true
		}
	}
}

// NewDagStream: builds a router for one plan's gate-node events.
func (e *Executor) NewDagStream(ctx context.Context, plan Plan, appName, userID, sessionID, cancelKey string, yield func(stream.SSEEvent, error) bool, nodeOutputs map[string]string) *DagStream {
	agentByID := make(map[string]string, len(plan.Nodes))
	for _, n := range plan.Nodes {
		agentByID[n.ID] = n.AgentName
	}
	return &DagStream{
		ctx: ctx, plan: plan, agentByID: agentByID, yield: yield,
		ds: newDagStream(otelobs.TraceIDOf(ctx), agentByID, yield, nodeOutputs, func(nodeID string) gateScore {
			return e.gateScore(ctx, appName, userID, sessionID, nodeID)
		}, func(nodeID string) bool {
			return e.controls.wasCancelled(cancelKey, nodeID)
		}, func(nodeID string) bool {
			return e.controls.wasPaused(cancelKey, nodeID)
		}, func(nodeID string, gen int) string {
			return e.NodeQueueGuidance(cancelKey, nodeID, gen)
		}),
	}
}

// Handle: routes gate-node events → SSE (true) or orchestrator events → caller (false).
func (s *DagStream) Handle(ev *session.Event) bool {
	if ev == nil {
		return false
	}
	if ev.NodeInfo == nil || planNodeInPath(ev.NodeInfo.Path, s.agentByID) == "" {
		return false // not a gate-node event - the orchestrator's own
	}
	s.ds.handle(ev)
	return true
}

// Finish: flushes last run and emits node_done for remaining nodes. Call after runner loop.
func (s *DagStream) Paused() bool { return len(s.ds.needsInput) > 0 }

func (s *DagStream) Finish() {
	s.ds.flush()
	if len(s.ds.needsInput) == 0 {
		ensureTerminal(s.plan, s.ds.outputs, s.ds.last)
	}
	for _, n := range s.plan.Nodes {
		if s.ds.doneEmitted[n.ID] {
			continue
		}
		if s.only != nil && !s.only[n.ID] {
			continue
		}
		if s.ds.needsInput[n.ID] {
			continue
		}
		if len(s.ds.needsInput) > 0 && !s.ds.started[n.ID] {
			continue
		}
		if s.ds.userPaused != nil && s.ds.userPaused(n.ID) {
			s.yield(stream.NodePaused(n.ID), nil)
			continue
		}
		if s.ds.cancelled != nil && s.ds.cancelled(n.ID) {
			s.yield(stream.NodeCancelled(n.ID), nil)
			continue
		}
		if strings.TrimSpace(s.ds.outputs[n.ID]) == "" {
			s.yield(stream.NodeFailed(n.ID, "produced no answer"), nil)
			continue
		}
		s.yield(stream.NodeDone(n.ID, s.ds.nodeDoneData(n.ID)), nil)
	}
}

// RetryPlanInNode: re-runs target node + descendants with seeded outputs.
func (e *Executor) RetryPlanInNode(ctx adkagent.Context, plan Plan, chatID, nodeID string, seeded map[string]string) (map[string]string, error) {
	source := ledger.CoordsFromContext(ctx).Source
	gateNodes, _, err := buildGateNodes(plan, e.agents, e.models, e.judge, e.cfgFor, e.mediaAgents, e.controls, chatID, source,
		func(nodeID string, score float64, passed bool, rounds int) {
			e.recordGateResult(chatID, nodeID, score, passed, rounds)
		}, e.admission, e.specFor, e.artifacts, nil) // retry never re-runs setup, so nothing to refresh
	if err != nil {
		return nil, err
	}
	return runDAGSubset(ctx, plan, gateNodes, e.maxActive, seeded, retrySet(plan, nodeID))
}

// NewExecutor: returns a graph Executor.
func NewExecutor(sessions session.Service, agents map[string]adkagent.Agent, models map[string]model.LLM, judge vetting.JudgeFactory, cfgFor func(string) vetting.Config, mediaAgents map[string]bool) *Executor {
	return &Executor{sessions: sessions, agents: agents, models: models, judge: judge, cfgFor: cfgFor, mediaAgents: mediaAgents, controls: newRunControls(), maxActive: 2}
}

// gateScore: node's trust-gate result.
type gateScore struct {
	score  float64
	passed bool
	rounds int
}

// gateResultKey: keys gateResults scoped by chat id.
func gateResultKey(chatID, nodeID string) string { return chatID + "\x00" + nodeID }

// recordGateResult: stores node's gate outcome in-process for node_done.
func (e *Executor) recordGateResult(chatID, nodeID string, score float64, passed bool, rounds int) {
	e.gateResults.Store(gateResultKey(chatID, nodeID), gateScore{score: score, passed: passed, rounds: rounds})
}

// gateScore: reads node's persisted judge result.
func (e *Executor) gateScore(ctx context.Context, appName, userID, sessionID, nodeID string) gateScore {
	var g gateScore
	// In-process first: state write is a delta not yet appended when node_done is assembled.
	if v, ok := e.gateResults.Load(gateResultKey(sessionID, nodeID)); ok {
		if got, ok := v.(gateScore); ok {
			return got
		}
	}
	if e.sessions == nil {
		return g
	}
	resp, err := e.sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
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

// dagStream converts workflow events into SSE, synthesizing per-node worker runs.
type dagStream struct {
	// traceID is the run's OTel trace id, resolved once at construction - not
	// per-node/per-round: every span in this plan run shares one trace, so a
	// live context.Context (Finding 4) would add nothing but staleness risk.
	traceID    string
	agentByID  map[string]string
	yield      func(stream.SSEEvent, error) bool
	outputs    map[string]string
	scoreOf    func(string) gateScore
	startedAt  map[string]time.Time
	cancelled  func(string) bool
	userPaused func(string) bool
	steerOf    func(string, int) string

	started     map[string]bool
	doneEmitted map[string]bool
	needsInput  map[string]bool
	curRun      map[string]string
	steerSeen   map[string]int
	usage       map[string]*runUsage // open run only; reset on each closeRun
	nodeUsage   map[string]*runUsage // cumulative across the node's whole life (worker-r0, worker-r1, ...); feeds node_done
	last        string
	stopped     bool

	// toolCallSeen dedups agent_tool_call per node: ACP's start+completion
	// updates both carry the FunctionCall part for the same call_id.
	toolCallSeen map[string]stream.SeenCalls
}

type runUsage struct {
	prompt, completion, reasoning, total, cached int32
	// ctxTokens is the LAST measured prompt-token count seen, not summed like
	// the fields above - a multi-tool-call round sums past the model's actual
	// context occupancy, so the context meter needs this instead.
	ctxTokens     int32
	model, finish string
}

func newDagStream(traceID string, agentByID map[string]string, yield func(stream.SSEEvent, error) bool, outputs map[string]string, scoreOf func(string) gateScore, cancelled func(string) bool, userPaused func(string) bool, steerOf func(string, int) string) *dagStream {
	return &dagStream{
		traceID: traceID, agentByID: agentByID, yield: yield, outputs: outputs, scoreOf: scoreOf, cancelled: cancelled, userPaused: userPaused, steerOf: steerOf,
		started: map[string]bool{}, doneEmitted: map[string]bool{}, needsInput: map[string]bool{}, startedAt: map[string]time.Time{},
		curRun: map[string]string{}, steerSeen: map[string]int{}, usage: map[string]*runUsage{}, nodeUsage: map[string]*runUsage{},
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

// handle: translates one workflow event.
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
		s.startedAt[node] = time.Now()
		if !s.emit(stream.WithTrace(stream.NodeStart(node, s.agentByID[node]), s.traceID)) {
			return false
		}
	}

	if ev.RequestedInput != nil {
		s.closeRun(node)
		s.needsInput[node] = true
		return s.emit(stream.NodeNeedsInput(node, ev.RequestedInput.InterruptID, ev.RequestedInput.Message))
	}

	last := lastSeg(ev.NodeInfo.Path)
	if segName(last) == node {
		if ev.Output != nil && !s.doneEmitted[node] {
			s.closeRun(node)
			s.doneEmitted[node] = true
			out := outputString(ev.Output)
			if out != "" {
				s.outputs[node] = out
				s.last = out
			}
			switch {
			case s.userPaused != nil && s.userPaused(node):
				if !s.emit(stream.NodePaused(node)) {
					return false
				}
			case s.cancelled != nil && s.cancelled(node):
				if !s.emit(stream.NodeCancelled(node)) {
					return false
				}
			case out != "":
				if !s.emit(stream.NodeDone(node, s.nodeDoneData(node))) {
					return false
				}
			default:
				if !s.emit(stream.NodeFailed(node, "produced no answer")) {
					return false
				}
			}
		}
		return true
	}

	runID := segRun(last)
	if !strings.HasPrefix(runID, "worker") {
		return true
	}
	if s.curRun[node] != runID {
		if !s.closeRun(node) {
			return false
		}
		s.curRun[node] = runID
		s.usage[node] = &runUsage{}
		if s.nodeUsage[node] == nil {
			s.nodeUsage[node] = &runUsage{}
		}
		if gen := steerGen(runID); gen > s.steerSeen[node] {
			s.steerSeen[node] = gen
			guidance := ""
			if s.steerOf != nil {
				guidance = s.steerOf(node, gen)
			}
			if !s.emit(stream.NodeSteered(node, guidance)) {
				return false
			}
		}
		st, rd := stageRound(runID)
		ev := stream.WithTrace(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentStart, Data: stream.AgentStartData{
			RunID: runID, Agent: s.agentByID[node], Stage: st, Round: rd, StartedAtMs: time.Now().UnixMilli(),
		}}, node), s.traceID)
		if !s.emit(ev) {
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

// part: translates one content part into SSE.
func (s *dagStream) part(node, runID string, p *genai.Part) bool {
	if p == nil {
		return true
	}
	switch {
	case p.FunctionResponse != nil && stream.IsGateMarkerName(p.FunctionResponse.Name):
		return true
	case p.FunctionCall != nil:
		if p.FunctionCall.Name == "transfer_to_agent" {
			return true
		}
		if s.toolCallSeen == nil {
			s.toolCallSeen = map[string]stream.SeenCalls{}
		}
		seen := s.toolCallSeen[node]
		if seen.Add(p.FunctionCall.ID) {
			return true
		}
		s.toolCallSeen[node] = seen
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

// accum: folds usage/model/finish into the node's worker run.
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
		u.cached += ev.UsageMetadata.CachedContentTokenCount
		if ev.UsageMetadata.PromptTokenCount > 0 {
			u.ctxTokens = ev.UsageMetadata.PromptTokenCount
		}
	}
	if ev.ModelVersion != "" {
		u.model = ev.ModelVersion
	}
	if ev.FinishReason != "" && ev.FinishReason != genai.FinishReasonUnspecified {
		u.finish = string(ev.FinishReason)
	}
}

// closeRun: emits agent_complete for active worker run, and folds its usage
// into the node's cumulative total (node_done reports the whole node's spend
// across every worker/revise round, not just the last one).
func (s *dagStream) closeRun(node string) bool {
	runID := s.curRun[node]
	if runID == "" {
		return true
	}
	st, rd := stageRound(runID)
	d := stream.AgentCompleteData{RunID: runID, Stage: st, Round: rd}
	if u := s.usage[node]; u != nil {
		d.Model, d.FinishReason = u.model, u.finish
		d.PromptTokens, d.CompletionTokens, d.ReasoningTokens, d.TotalTokens, d.CachedTokens = u.prompt, u.completion, u.reasoning, u.total, u.cached
		d.ContextTokens = u.ctxTokens
		if nu := s.nodeUsage[node]; nu != nil {
			nu.prompt += u.prompt
			nu.completion += u.completion
			nu.reasoning += u.reasoning
			nu.total += u.total
			nu.cached += u.cached
			nu.model, nu.finish = u.model, u.finish
			// Overwritten, not accumulated: node_done should report the freshest
			// context occupancy across the node's rounds, not their sum.
			if u.ctxTokens > 0 {
				nu.ctxTokens = u.ctxTokens
			}
		}
	}
	s.curRun[node] = ""
	s.usage[node] = nil
	return s.emit(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentComplete, Data: d}, node))
}

// flush: closes open worker runs at stream end.
func (s *dagStream) flush() bool {
	for node := range s.curRun {
		if !s.closeRun(node) {
			return false
		}
	}
	return !s.stopped
}

// nodeDoneData: builds node_done payload from output, cumulative worker-run
// usage, and judge result.
func (s *dagStream) nodeDoneData(node string) stream.NodeDoneData {
	out := s.outputs[node]
	d := stream.NodeDoneData{Output: out, OutputPreview: preview(out)}
	if t, ok := s.startedAt[node]; ok {
		d.DurationMs = time.Since(t).Milliseconds()
	}
	if u := s.nodeUsage[node]; u != nil {
		d.Model, d.FinishReason = u.model, u.finish
		d.PromptTokens, d.CompletionTokens, d.ReasoningTokens, d.TotalTokens, d.CachedTokens = u.prompt, u.completion, u.reasoning, u.total, u.cached
		d.ContextTokens = u.ctxTokens
	}
	if s.scoreOf != nil {
		g := s.scoreOf(node)
		d.JudgeFinalScore = g.score
		d.JudgePassed = g.passed
		d.JudgeRounds = int32(g.rounds)
	}
	return d
}

// planNodeInPath: first NodeInfo.Path segment naming a plan node, or "".
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

// stageRound: maps run ID to SSE stage + round. A queued round carries a
// "-s%d" suffix (node.go's sfx) that must come off before the round parses.
func stageRound(runID string) (string, int) {
	if strings.HasPrefix(runID, "worker-r") {
		if n := toInt(trimQueueSuffix(runID[len("worker-r"):])); n > 0 {
			return stream.StageRevise, n
		}
	}
	return stream.StageWorker, 0
}

// trimQueueSuffix drops a trailing "-s<digits>" and nothing else.
func trimQueueSuffix(s string) string {
	i := strings.LastIndex(s, "-s")
	if i < 0 {
		return s
	}
	digits := s[i+2:]
	if digits == "" || strings.TrimLeft(digits, "0123456789") != "" {
		return s
	}
	return s[:i]
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

// toFloat/toInt read state values tolerantly (JSON round-trips as float64).
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

// buildTask assembles a node's worker prompt from user request, dependencies, and task.
func buildTask(plan Plan, node Node, upstream map[string]string, gateFailed map[string]bool) string {
	background := plan.WorkerBackground
	if background == "" {
		background = plan.UserMessage
	}
	var sb strings.Builder
	if background != "" {
		sb.WriteString("BACKGROUND - the user's full request, verbatim. This is CONTEXT ONLY, so you " +
			"understand what the overall job is and how your piece fits. MOST OF IT IS NOT YOURS TO DO.\n\n")
		sb.WriteString(background)
		sb.WriteString("\n\n---\n\n")
		if others := siblingIDs(plan, node.ID); others != "" {
			sb.WriteString("The other parts of that request are ALREADY ASSIGNED to these nodes, " +
				"running in parallel with you right now: " + others + ".\n" +
				"Do not do their work. Anything you produce outside your own task below is thrown away.\n\n---\n\n")
		}
	}
	for _, dep := range node.DependsOn {
		if out, ok := upstream[dep]; ok && strings.TrimSpace(out) != "" {
			if gateFailed[dep] {
				sb.WriteString("⚠ WARNING: the following input FAILED independent quality vetting (unverified claims or missing citations). Treat its claims with suspicion and do not present them as verified:\n\n")
			}
			sb.WriteString(out)
			sb.WriteString("\n\n---\n\n")
		} else {
			sb.WriteString("⚠ NOTE: upstream node \"" + dep + "\" produced NO answer - it failed. You have no data for its part of the task; explicitly state that this piece is unavailable rather than omitting it or fabricating content.\n\n---\n\n")
		}
	}
	ctxDetail := matchedContext(plan.ContextItems, node.Task)
	if sb.Len() == 0 && ctxDetail == "" {
		return node.Task
	}
	sb.WriteString("YOUR TASK - do this, and ONLY this:\n")
	sb.WriteString(node.Task)
	sb.WriteString(ctxDetail)
	return sb.String()
}

// matchedContext: detail for context items a node's task names by name.
func matchedContext(items []ContextItem, task string) string {
	lower := strings.ToLower(task)
	var sb strings.Builder
	for _, c := range items {
		if c.Name == "" || !strings.Contains(lower, strings.ToLower(c.Name)) {
			continue
		}
		fmt.Fprintf(&sb, "\n\nCONTEXT for the %q item your task names (other items, if any, belong to other nodes and are not shown here):\n%s", c.Name, c.Detail)
	}
	return sb.String()
}

// siblingIDs: lists plan's other node ids for task scoping.
func siblingIDs(plan Plan, self string) string {
	var ids []string
	for _, n := range plan.Nodes {
		if n.ID != self {
			ids = append(ids, n.ID)
		}
	}
	return strings.Join(ids, ", ")
}

// ensureTerminal: seeds terminal node from fallback when capture missed it.
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

// steerGen: extracts steer generation from run ID's "-sN" suffix.
func steerGen(runID string) int {
	i := strings.LastIndex(runID, "-s")
	if i < 0 || i+2 >= len(runID) {
		return 0
	}
	n := 0
	for _, c := range runID[i+2:] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
