package dag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// Executor runs a Plan as an ADK v2 graph workflow.
type Executor struct {
	sessions  session.Service
	roster    atomic.Pointer[Roster]
	judge     vetting.JudgeFactory
	controls  *runControls
	maxActive int
	setupFn   SetupFunc
	artifacts artifact.Service // ADK's own artifact tools/debug console; see SetArtifacts
	// walLedger: the WAL's fail-closed AppendIntent path; nil = no WAL. Only a postgres-backed
	// ledger should be set here - see vetting.Config.Ledger.
	walLedger ledger.LedgerStore
	// schemas: registered-schema enforcement, stamped on every gate node regardless of its gated setting.
	schemas   *artifactschema.Registry
	decisions *decide.Decider
	admission *Admission
	// judgeSpec: one judge model serves every agent, so it's a single spec, not per-agent.
	judgeSpec AdmissionSpec

	gateResults sync.Map
}

// SetAdmission wires the capacity ledger and the judge's spec (nil admission runs unbounded);
// per-agent worker specs come from Roster.SpecFor.
func (e *Executor) SetAdmission(admission *Admission, judgeSpec AdmissionSpec) {
	e.admission, e.judgeSpec = admission, judgeSpec
}

// SetMaxActive sets the concurrent-node cap (no-op for n < 1).
func (e *Executor) SetMaxActive(n int) {
	if n >= 1 {
		e.maxActive = n
	}
}

// SetArtifacts wires an artifact.Service into the plan graph's runner (adkagent.Context.Artifacts()).
// Node attachments are rerouted separately, at the REST/plan entry boundary (internal/artifactref).
func (e *Executor) SetArtifacts(svc artifact.Service) { e.artifacts = svc }

// SetWALLedger wires the WAL's fail-closed AppendIntent into every gate node. Pass nil unless store is
// postgres-backed: the FS ledger's AppendIntent is best-effort, not fail-closed.
func (e *Executor) SetWALLedger(store ledger.LedgerStore) { e.walLedger = store }

// SetSchemas wires registered-schema enforcement into every gate node, unconditionally.
func (e *Executor) SetSchemas(reg *artifactschema.Registry) { e.schemas = reg }

// SetDecisions attaches the decision intercept points to every gate node; nil disables them.
func (e *Executor) SetDecisions(d *decide.Decider) { e.decisions = d }

// ResetNodeCancels clears user-cancelled node flags for the next turn.
func (e *Executor) ResetNodeCancels(chatID string) { e.controls.resetCancelled(chatID) }

// DagStream translates gate-node events into SSE.
type DagStream struct {
	ctx       context.Context
	ds        *dagStream
	plan      Plan
	agentByID map[string]string
	yield     func(stream.SSEEvent, error) bool
	only      map[string]bool
	shutdown  func() bool
}

// ScopeToRetry restricts the terminal sweep to the retried node and its descendants.
func (s *DagStream) ScopeToRetry(nodeID string) { s.only = retrySet(s.plan, nodeID) }

// ScopeToResume restricts the sweep to resumed nodes and their descendants.
func (s *DagStream) ScopeToResume(nodeIDs []string) {
	s.only = map[string]bool{}
	for _, id := range nodeIDs {
		for k := range retrySet(s.plan, id) {
			s.only[k] = true
		}
	}
}

// ScopeToStep restricts the terminal sweep to exactly run: a step's descendants haven't run yet.
func (s *DagStream) ScopeToStep(run map[string]bool) { s.only = run }

// NewDagStream builds a router for one plan's gate-node events.
func (e *Executor) NewDagStream(ctx context.Context, plan Plan, appName, userID, sessionID, cancelKey string, yield func(stream.SSEEvent, error) bool, nodeOutputs map[string]string) *DagStream {
	agentByID := make(map[string]string, len(plan.Nodes))
	// scopeByID: workspaceNodeID per node, the key RunGatedRefine's recorder uses. It differs from the
	// plan node id for setup/repo-chain implementers, so the failure lookup must use it.
	scopeByID := make(map[string]string, len(plan.Nodes))
	resumedFromByID := make(map[string]string, len(plan.Nodes))
	for _, n := range plan.Nodes {
		agentByID[n.ID] = n.AgentName
		scopeByID[n.ID] = workspaceNodeID(plan, n)
		resumedFromByID[n.ID] = n.ResumedFrom
	}
	ds := newDagStream(otelobs.TraceIDOf(ctx), cancelKey, agentByID, scopeByID, yield, nodeOutputs, func(nodeID string) gateScore {
		return e.gateScore(ctx, appName, userID, sessionID, nodeID)
	}, func(nodeID string) bool {
		return e.controls.wasCancelled(cancelKey, nodeID)
	}, func(nodeID string) PauseReason {
		return e.controls.pauseReason(cancelKey, nodeID)
	}, func(nodeID string, gen int) string {
		return e.NodeQueueGuidance(cancelKey, nodeID, gen)
	})
	ds.deliveredOf = func(nodeID string) bool { return e.controls.wasDelivered(cancelKey, nodeID) }
	ds.draftOf = func(nodeID string) string { return e.controls.draftOf(cancelKey, nodeID) }
	ds.resumedFromByID = resumedFromByID
	shutdown := func() bool { _, ok := e.controls.shutdown.Load(cancelKey); return ok }
	return &DagStream{ctx: ctx, plan: plan, agentByID: agentByID, yield: yield, ds: ds, shutdown: shutdown}
}

// Handle routes gate-node events to SSE (true) or orchestrator events to the caller (false).
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

// Paused reports whether any node in this stream parked on a HITL question.
func (s *DagStream) Paused() bool { return len(s.ds.needsInput) > 0 }

// NeedsInput reports which nodes paused on a HITL question.
func (s *DagStream) NeedsInput() map[string]bool { return s.ds.needsInput }

// Started reports which run-set nodes reached running. A node behind a paused dependency can be
// requested but never dispatched; callers must not treat it as "ran and failed".
func (s *DagStream) Started() map[string]bool { return s.ds.started }

// Finish flushes the last run and emits a terminal event for every unsettled in-scope node.
func (s *DagStream) Finish() { s.settle(false, nil) }

// Abort is Finish for a runner that ended on err. A shutdown cut emits nothing: boot re-stamps
// a still-running row paused/shutdown and resumes it.
func (s *DagStream) Abort(err error) {
	if s.shutdown != nil && s.shutdown() {
		return
	}
	if errors.Is(s.ctx.Err(), context.Canceled) {
		s.settle(true, nil)
		return
	}
	s.settle(false, err)
}

// settle: stopped (a user stop) cancels every unsettled in-scope node; runErr
// (any other runner failure) fails only started ones, per Started's contract.
func (s *DagStream) settle(stopped bool, runErr error) {
	s.ds.flush()
	if !stopped && runErr == nil && len(s.ds.needsInput) == 0 {
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
		if !s.ds.started[n.ID] && (runErr != nil || len(s.ds.needsInput) > 0) {
			continue
		}
		s.emitFinishTerminal(n, stopped, runErr)
	}
}

// emitFinishTerminal: delivered/paused/cancelled checks in that priority order, then failed or done.
func (s *DagStream) emitFinishTerminal(n Node, stopped bool, runErr error) {
	delivered := s.ds.deliveredOf != nil && s.ds.deliveredOf(n.ID)
	if !delivered && s.ds.pauseReasonOf != nil && s.ds.pauseReasonOf(n.ID) != "" {
		s.yield(stream.NodePaused(n.ID), nil)
		return
	}
	empty := strings.TrimSpace(s.ds.outputs[n.ID]) == ""
	if !delivered && (stopped && empty || s.ds.cancelled != nil && s.ds.cancelled(n.ID)) {
		s.yield(s.ds.cancelledEvent(n.ID), nil)
		return
	}
	if !delivered && empty {
		s.yield(stream.WithContextID(stream.NodeFailed(n.ID, s.failMessage(n.ID, runErr)), s.ds.contextOf(n.ID)), nil)
		return
	}
	s.yield(stream.NodeDone(n.ID, s.ds.nodeDoneData(n.ID)), nil)
}

// RetryPlanInNode re-runs the target node and descendants with seeded outputs, in a fresh session:
// a native node's A2A session outlives completion, so retry reaps it (ACP resume is opt-in via ResumedFrom).
func (e *Executor) RetryPlanInNode(ctx adkagent.Context, plan Plan, chatID, nodeID string, seeded map[string]string) (map[string]string, error) {
	// ponytail: only the named target, not a re-cascaded descendant - the same class of staleness
	// could in principle reach one of those too, add if it shows up in practice.
	e.resetNativeWorkerSession(context.WithoutCancel(ctx), plan, chatID, nodeID)
	return e.runSubset(ctx, plan, chatID, seeded, retrySet(plan, nodeID))
}

// RunPlanIncrement runs exactly the nodes in run as fresh dispatches, seeding every other node's
// output from seeded so a new node depending on an already-run one still sees its result.
func (e *Executor) RunPlanIncrement(ctx adkagent.Context, plan Plan, chatID string, seeded map[string]string, run map[string]bool) (map[string]string, error) {
	return e.runSubset(ctx, plan, chatID, seeded, run)
}

// runSubset builds gate nodes for plan and dispatches exactly the ids in run.
func (e *Executor) runSubset(ctx adkagent.Context, plan Plan, chatID string, seeded map[string]string, run map[string]bool) (map[string]string, error) {
	source := ledger.CoordsFromContext(ctx).Source
	var userID string
	artifacts := e.artifacts
	if sess := ctx.Session(); sess != nil {
		userID = sess.UserID()
	} else {
		// No session means no real userID: artifact tools would scope to "" and see nothing.
		artifacts = nil
		slog.Warn("dag: no session, skipping artifact tools", "component", "dag", "chat_id", chatID)
	}
	sink, _ := stream.YieldFromContext(ctx)
	// A subset run never re-runs setup, so nothing to refresh.
	gateNodes, err := e.buildGateNodes(ctx, plan, chatID, userID, source, artifacts, nil, sink)
	if err != nil {
		return nil, err
	}
	return runDAGSubset(ctx, plan, gateNodes, e.maxActive, seeded, run)
}

// resetNativeWorkerSession deletes nodeID's deterministic A2A worker session before a retry.
// Best-effort; a no-op for an ACP node or one that never ran.
func (e *Executor) resetNativeWorkerSession(ctx context.Context, plan Plan, chatID, nodeID string) {
	if e.sessions == nil {
		return
	}
	var agentName string
	for _, n := range plan.Nodes {
		if n.ID == nodeID {
			agentName = n.AgentName
			break
		}
	}
	if agentName == "" {
		return
	}
	workerContextID := quackagent.WorkerSessionID(chatID, nodeID)
	if err := e.sessions.Delete(ctx, &session.DeleteRequest{
		AppName: agentName, UserID: quackagent.WorkerSessionUser(workerContextID), SessionID: workerContextID,
	}); err != nil {
		slog.Debug("retry: no prior native worker session to reap (fine for a first attempt or an ACP node)",
			"component", "dag", "node_id", nodeID, "err", err)
	}
}

// NewExecutor returns a graph Executor over a Gen 0 roster; SetRoster replaces it.
func NewExecutor(sessions session.Service, agents map[string]adkagent.Agent, models map[string]model.LLM, judge vetting.JudgeFactory, cfgFor func(context.Context, string) vetting.Config, mediaAgents map[string]bool) *Executor {
	e := &Executor{sessions: sessions, judge: judge, controls: newRunControls(), maxActive: 2}
	e.roster.Store(&Roster{Agents: agents, Models: models, CfgFor: cfgFor, Media: mediaAgents})
	return e
}

type gateScore struct {
	score  float64
	passed bool
	rounds int
	// contextID: the ACP transport session id this round established; "" for a native node.
	contextID string
}

// SilentGapError: empty output with no failure on record. store.failedDagNodeError matches this
// exact string, so it must stay a comparable sentinel, not a format string.
const SilentGapError = "produced no answer"

// emptyNodeError names an empty completion: the sanitized gateway error ADK's runner swallowed into
// an empty output, or SilentGapError when none was recorded for this node's agent role.
func emptyNodeError(chatID, nodeID, agent string) string {
	if err, streak, dur, ok := inference.LastFailure(chatID, nodeID, agent); ok && streak > 0 {
		inference.ClearFailure(chatID, nodeID, agent)
		class, _ := inference.SanitizeGatewayError(err)
		return fmt.Sprintf("%s on %d consecutive attempts over %s", class, streak, dur.Round(time.Second))
	}
	return SilentGapError
}

// failMessage prefers the node's recorded gateway failure, then the runner's own
// error, sanitized because DagNode.Error reaches RunOutcome text.
func (s *DagStream) failMessage(nodeID string, runErr error) string {
	msg := emptyNodeError(s.ds.chatID, s.ds.scope(nodeID), s.ds.agentByID[nodeID])
	switch {
	case runErr == nil || msg != SilentGapError:
		return msg
	case errors.Is(s.ctx.Err(), context.DeadlineExceeded):
		return "plan run timed out"
	}
	class, _ := inference.SanitizeGatewayError(runErr)
	return "plan run failed: " + class
}

func gateResultKey(chatID, nodeID string) string { return chatID + "\x00" + nodeID }

func (e *Executor) recordGateResult(chatID, nodeID string, score float64, passed bool, rounds int, contextID string) {
	e.gateResults.Store(gateResultKey(chatID, nodeID), gateScore{score: score, passed: passed, rounds: rounds, contextID: contextID})
}

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
	if v, err := st.Get(gateContextKey + nodeID); err == nil {
		g.contextID, _ = v.(string)
	}
	return g
}

// dagStream converts workflow events into SSE, synthesizing per-node worker runs.
type dagStream struct {
	// traceID: the run's OTel trace id, resolved once; every span in the plan run shares it.
	traceID string
	// chatID: real chat scope, used to look up a gateway-failure record for an empty output.
	chatID    string
	agentByID map[string]string
	// scopeByID: per-node workspace scope, the failure tracker's key (see NewDagStream).
	scopeByID map[string]string
	yield     func(stream.SSEEvent, error) bool
	outputs   map[string]string
	scoreOf   func(string) gateScore
	startedAt map[string]time.Time
	cancelled func(string) bool
	// pauseReasonOf: "" if not paused. A shutdown-drain pause doesn't block a delivered node_done
	// the way a live user pause does - see handle()'s switch.
	pauseReasonOf func(string) PauseReason
	steerOf       func(string, int) string
	// deliveredOf: true once RunGatedRefine reached commitDelivery for this node; nil in tests.
	deliveredOf func(string) bool
	// draftOf: the gate's latest draft for a node (NoteDraft); nil in tests.
	draftOf func(string) string
	// resumedFromByID: per-node Node.ResumedFrom; a nil map reads as "", the fresh-node default.
	resumedFromByID map[string]string

	started     map[string]bool
	doneEmitted map[string]bool
	needsInput  map[string]bool
	curRun      map[string]string
	steerSeen   map[string]int
	usage       map[string]*runUsage // open run only; reset on each closeRun
	nodeUsage   map[string]*runUsage // cumulative across all the node's rounds; feeds node_done
	last        string
	stopped     bool

	// toolCallSeen dedups agent_tool_call per node: ACP's start and completion updates both carry
	// the FunctionCall part for the same call_id.
	toolCallSeen map[string]stream.SeenCalls
}

type runUsage struct {
	prompt, completion, reasoning, total, cached int32
	// ctxTokens is the last measured prompt-token count, not summed: a multi-tool-call round's sum
	// overstates the model's actual context occupancy.
	ctxTokens     int32
	model, finish string
	// lastAt: when this run's latest event was handled, the round's real finish; closeRun's call time
	// can lag it by an intervening judge round.
	lastAt time.Time
}

func newDagStream(traceID, chatID string, agentByID, scopeByID map[string]string, yield func(stream.SSEEvent, error) bool, outputs map[string]string, scoreOf func(string) gateScore, cancelled func(string) bool, pauseReasonOf func(string) PauseReason, steerOf func(string, int) string) *dagStream {
	return &dagStream{
		traceID: traceID, chatID: chatID, agentByID: agentByID, scopeByID: scopeByID, yield: yield, outputs: outputs, scoreOf: scoreOf, cancelled: cancelled, pauseReasonOf: pauseReasonOf, steerOf: steerOf,
		started: map[string]bool{}, doneEmitted: map[string]bool{}, needsInput: map[string]bool{}, startedAt: map[string]time.Time{},
		curRun: map[string]string{}, steerSeen: map[string]int{}, usage: map[string]*runUsage{}, nodeUsage: map[string]*runUsage{},
	}
}

// scope returns node's workspace scope, falling back to the raw node id when scopeByID has none.
func (s *dagStream) scope(node string) string {
	if sc, ok := s.scopeByID[node]; ok && sc != "" {
		return sc
	}
	return node
}

// contextOf returns node's resumable transport context id, captured in graph.go on every outcome.
func (s *dagStream) contextOf(node string) string {
	if s.scoreOf == nil {
		return ""
	}
	return s.scoreOf(node).contextID
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

// emitNodeStart: the one-shot NodeStart for a plan node, stamped with resumed-from and trace.
func (s *dagStream) emitNodeStart(node string) bool {
	if !s.started[node] {
		s.started[node] = true
		s.startedAt[node] = time.Now()
		ev := stream.WithResumedFrom(stream.NodeStart(node, s.agentByID[node]), s.resumedFromByID[node])
		if !s.emit(stream.WithTrace(ev, s.traceID)) {
			return false
		}
	}
	return true
}

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
	if !s.emitNodeStart(node) {
		return false
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
			var pauseReason PauseReason
			if s.pauseReasonOf != nil {
				pauseReason = s.pauseReasonOf(node)
			}
			if !s.emitNodeTerminal(node, out, pauseReason) {
				return false
			}

		}
		return true
	}

	runID := segRun(last)
	if !strings.HasPrefix(runID, "worker") {
		return true
	}
	return s.handleWorkerRun(node, runID, ev)
}

// handleWorkerRun: close the prior run, stamp usage, relay a newer steer generation, emit
// agent-start, then accumulate content.
func (s *dagStream) handleWorkerRun(node, runID string, ev *session.Event) bool {
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

func (s *dagStream) accum(node string, ev *session.Event) {
	u := s.usage[node]
	if u == nil {
		return
	}
	u.lastAt = time.Now()
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

// closeRun emits agent_complete for the active worker run and folds its usage into the node's
// cumulative total, which node_done reports across every round.
func (s *dagStream) closeRun(node string) bool {
	runID := s.curRun[node]
	if runID == "" {
		return true
	}
	st, rd := stageRound(runID)
	finishedAt := time.Now()
	if u := s.usage[node]; u != nil && !u.lastAt.IsZero() {
		finishedAt = u.lastAt
	}
	d := stream.AgentCompleteData{RunID: runID, Stage: st, Round: rd, FinishedAtMs: finishedAt.UnixMilli()}
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
			// Overwritten, not accumulated: node_done reports the freshest context occupancy, not the sum.
			if u.ctxTokens > 0 {
				nu.ctxTokens = u.ctxTokens
			}
		}
	}
	s.curRun[node] = ""
	s.usage[node] = nil
	return s.emit(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentComplete, Data: d}, node))
}

func (s *dagStream) flush() bool {
	for node := range s.curRun {
		if !s.closeRun(node) {
			return false
		}
	}
	return !s.stopped
}

// nodeDoneData builds the node_done payload from output, cumulative usage and the judge result.
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
		d.ContextID = g.contextID
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
	return stream.RunIDFromBranch(seg)
}

// stageRound maps a run ID to SSE stage and round. A queued round carries a "-s%d" suffix
// (node.go's sfx) that must come off before the round parses.
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

// cancelledEvent is node_cancelled carrying the draft the node had, so it persists as a stopped draft.
func (s *dagStream) cancelledEvent(node string) stream.SSEEvent {
	ev := stream.NodeCancelled(node)
	d := ev.Data.(stream.NodeCancelledData)
	d.Output = s.outputs[node]
	if d.Output == "" && s.draftOf != nil {
		d.Output = s.draftOf(node)
	}
	d.OutputPreview = preview(d.Output)
	ev.Data = d
	return stream.WithContextID(ev, s.contextOf(node))
}

func preview(s string) string { return safeTruncateBytes(s, 250) }

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

// artifactContextShare/artifactBytesPerToken: a dependent's inlined-artifact budget as a fraction
// of its own context window, not a fixed size.
const artifactContextShare = 0.4
const artifactBytesPerToken = 4

// defaultNodeContextWindow: fallback when a node has no ContextWindow (mirrors vetting's judge default).
const defaultNodeContextWindow = 32_768

// artifactByteBudget: node's total inlined-artifact budget in bytes, shared across its dependencies.
func artifactByteBudget(node Node) int {
	window := node.ContextWindow
	if window <= 0 {
		window = defaultNodeContextWindow
	}
	return int(float64(window)*artifactContextShare) * artifactBytesPerToken
}

// buildTask assembles a node's worker prompt from user request, dependencies and task; cfg's
// artifact connection lets a dependency's own artifact follow its answer.
func buildTask(ctx context.Context, plan Plan, node Node, upstream map[string]string, gateFailed map[string]bool, cfg vetting.Config) string {
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
	budget := artifactByteBudget(node)
	for _, dep := range node.DependsOn {
		if out, ok := upstream[dep]; ok && strings.TrimSpace(out) != "" {
			if gateFailed[dep] {
				sb.WriteString("⚠ WARNING: the following input FAILED independent quality vetting (unverified claims or missing citations). Treat its claims with suspicion and do not present them as verified:\n\n")
			}
			sb.WriteString(out)
			if block, used := appendDependencyArtifact(ctx, plan, cfg, dep, out, budget); block != "" {
				sb.WriteString(block)
				budget -= used
			}
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

// appendDependencyArtifact appends dep's artifact after its answer under an id/revision header.
// "" when there's no match, the content equals the answer, or the budget is spent.
func appendDependencyArtifact(ctx context.Context, plan Plan, cfg vetting.Config, dep, answer string, budget int) (block string, used int) {
	if budget <= 0 {
		return "", 0
	}
	depCfg := cfg
	depCfg.Artifact, depCfg.IsReviewer = "", false
	for _, n := range plan.Nodes {
		if n.ID == dep {
			depCfg.Artifact, depCfg.IsReviewer = n.Artifact, n.AgentName == reviewerAgent
			break
		}
	}
	id, rev, content, ok := vetting.DependencyArtifact(ctx, depCfg, dep)
	if !ok || content == answer {
		return "", 0
	}
	truncated := false
	if len(content) > budget {
		content = safeTruncateBytes(content, budget)
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n[%s's full artifact - %s revision %d]\n", dep, id, rev)
	b.WriteString(content)
	if truncated {
		fmt.Fprintf(&b, "\n\n[... truncated; read_artifact(%q) for the rest]", id)
	}
	// used counts the whole block including header/marker, so the running budget reflects every byte.
	return b.String(), b.Len()
}

// safeTruncateBytes returns content's first n bytes without splitting a UTF-8 sequence.
func safeTruncateBytes(content string, n int) string {
	if n < 0 {
		n = 0
	}
	if n >= len(content) {
		return content
	}
	cut := content[:n]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size != 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
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

func siblingIDs(plan Plan, self string) string {
	var ids []string
	for _, n := range plan.Nodes {
		if n.ID != self {
			ids = append(ids, n.ID)
		}
	}
	return strings.Join(ids, ", ")
}

// ensureTerminal seeds a single-sink plan's sink from fallback when capture missed it; with several
// sinks, fallback may be another sink's output.
func ensureTerminal(plan Plan, nodeOutputs map[string]string, fallback string) {
	sinks := TerminalIDs(plan.Nodes)
	if fallback == "" || len(sinks) != 1 {
		return
	}
	if _, ok := nodeOutputs[sinks[0]]; !ok {
		nodeOutputs[sinks[0]] = fallback
	}
}

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

// terminalSpec: the finished node's terminal event, by priority - delivery, live pause, cancel,
// delivered answer, shutdown pause, then failure.
func (s *dagStream) terminalSpec(node, out string, pauseReason PauseReason) stream.SSEEvent {
	switch {
	case s.deliveredOf != nil && s.deliveredOf(node):
		// MarkDelivered fired inside commitDelivery: authoritative, outranking any pause/cancel flag that raced in.
		return stream.NodeDone(node, s.nodeDoneData(node))
	case pauseReason != "" && pauseReason != PauseShutdown:
		// Live user/HITL pause caught before commitDelivery ran, so the draft was never delivered.
		return stream.NodePaused(node)
	case s.cancelled != nil && s.cancelled(node):
		return s.cancelledEvent(node)
	case out != "":
		// A delivered answer wins over a shutdown-drain pause flipped after the gate loop last checked;
		// serve.DrainActiveRuns pauses exactly this population on SIGTERM.
		return stream.NodeDone(node, s.nodeDoneData(node))
	case pauseReason == PauseShutdown:
		return stream.NodePaused(node)
	default:
		ev := stream.NodeFailed(node, emptyNodeError(s.chatID, s.scope(node), s.agentByID[node]))
		return stream.WithContextID(ev, s.contextOf(node))
	}
}

// emitNodeTerminal emits the terminal event exactly once.
func (s *dagStream) emitNodeTerminal(node, out string, pauseReason PauseReason) bool {
	return s.emit(s.terminalSpec(node, out, pauseReason))
}
