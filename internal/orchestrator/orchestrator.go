// Package orchestrator: request entrypoint. Runs as ADK llmagent with plan/execute tools.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
)

const AppName = "quack"

// AgentName: gen_ai metrics "agent" value for the orchestrator's own model calls.
const AgentName = "orchestrator"

const orchestratorName = AgentName

// SourceApp is the token usage/cost "source" for a direct UI/REST/MCP chat; an
// extension-dispatched run uses its registration name.
const SourceApp = "app"

// Orchestrator: ADK llmagent that selects direct answer or plan + execute.
type Orchestrator struct {
	planLoader  func(ctx context.Context, planID string) (dag.Plan, bool)
	sessions    session.Service
	model       model.LLM
	sysPrompt   func(context.Context) string
	planner     *dag.Planner
	executor    *dag.Executor
	skillTS     tool.Toolset
	userMem     *memory.Store
	taskMem     *memory.Store
	memAgent    adkagent.Agent
	decisions   *decide.Decider
	artifacts   artifact.Service
	ledgerStore ledger.LedgerStore
	schemas     *artifactschema.Registry
	renderUI    bool
	// nodeSessions best-effort reaps a chat's "<chatID>:<nodeID>" sessions on reset; nil skips it.
	nodeSessions func(ctx context.Context, chatID string) error
	// compaction is reused on every turn's runner.Config; nil leaves the chat session uncompacted.
	compaction *compaction.Config
	// Optional extension hooks; nil until an active extension implements them.
	assignmentFreshness tools.AssignmentFreshnessFunc
	assignmentMeta      tools.AssignmentMetaFunc
}

// SetAssignmentFreshnessCheck wires execute's per-reused-node staleness check (an extension's
// BeforeAssignment hook); nil treats every reused node as fresh.
func (o *Orchestrator) SetAssignmentFreshnessCheck(fn tools.AssignmentFreshnessFunc) {
	o.assignmentFreshness = fn
}

// SetAssignmentMetaHook wires create_plan/edit_plan's per-assignment meta stamp (an extension's
// OnAssignment hook); nil leaves assignment.meta unset.
func (o *Orchestrator) SetAssignmentMetaHook(fn tools.AssignmentMetaFunc) {
	o.assignmentMeta = fn
}

// SetCompaction wires adk runner-level compaction onto the orchestrator's runner so the
// persistent chat session compacts too, not only per-node ones. nil disables it.
func (o *Orchestrator) SetCompaction(cfg *compaction.Config) { o.compaction = cfg }

// SetNodeSessionReaper wires store.ReapNodeSessions for ResetSession: session.Service has no
// pattern-delete, and orchestrator must not import store.
func (o *Orchestrator) SetNodeSessionReaper(fn func(ctx context.Context, chatID string) error) {
	o.nodeSessions = fn
}

// SetArtifacts wires an artifact.Service into the orchestrator's runner and its load_artifacts tool.
func (o *Orchestrator) SetArtifacts(svc artifact.Service) { o.artifacts = svc }

// SetLedger wires the WAL's fail-closed AppendIntent into the orchestrator's write tools, so a
// direct-chat write records parent_revision like every gated node does.
func (o *Orchestrator) SetLedger(store ledger.LedgerStore) { o.ledgerStore = store }

// SetSchemas wires registered-schema enforcement into the orchestrator's own
// write_artifact/write_<kind>/edit_artifact tools. Mirrors SetLedger.
func (o *Orchestrator) SetSchemas(reg *artifactschema.Registry) { o.schemas = reg }

// SetRenderUI offers render_ui (A2UI surfaces) on the orchestrator's own turns.
func (o *Orchestrator) SetRenderUI(on bool) { o.renderUI = on }

// failSoftListArtifacts degrades List errors to "no artifacts": load_artifacts lists on every
// request, and a transient store outage must not fail the whole turn.
type failSoftListArtifacts struct{ artifact.Service }

func (s failSoftListArtifacts) List(ctx context.Context, req *artifact.ListRequest) (*artifact.ListResponse, error) {
	resp, err := s.Service.List(ctx, req)
	if err != nil {
		slog.Warn("orchestrator: artifact List failed; offering no artifacts this turn", "err", err)
		// Stamp it for DeriveTerminalStatus so the degraded outage doesn't vanish as a silent gap.
		inference.RecordStoreFailure(req.SessionID, err)
		return &artifact.ListResponse{}, nil
	}
	inference.ClearStoreFailure(req.SessionID)
	return resp, nil
}

// Load degrades any failure to a text part: ADK's loadartifactstool loads every name in one
// errgroup, so one error would cancel its siblings and fail the turn.
func (s failSoftListArtifacts) Load(ctx context.Context, req *artifact.LoadRequest) (*artifact.LoadResponse, error) {
	resp, err := s.Service.Load(ctx, req)
	if err != nil {
		slog.Warn("orchestrator: artifact Load failed; reporting unavailable instead of failing the turn", "artifact", req.FileName, "err", err)
		return unavailableArtifactResponse(req.FileName, err), nil
	}
	if resp != nil && resp.Part != nil && resp.Part.InlineData != nil {
		if n := len(resp.Part.InlineData.Data); n > artifactref.InlineMaxBytes {
			err := fmt.Errorf("%d bytes, exceeds the %d byte load_artifacts limit", n, artifactref.InlineMaxBytes)
			slog.Warn("orchestrator: artifact Load oversize; reporting unavailable instead of failing the turn", "artifact", req.FileName, "err", err)
			return unavailableArtifactResponse(req.FileName, err), nil
		}
	}
	return resp, nil
}

func unavailableArtifactResponse(name string, err error) *artifact.LoadResponse {
	return &artifact.LoadResponse{Part: genai.NewPartFromText(fmt.Sprintf("artifact %q unavailable: %v", name, err))}
}

func (o *Orchestrator) SetUserMemoryHook(memAgent adkagent.Agent) {
	o.memAgent = memAgent
}

// SetDecisions attaches the decision intercept points; nil (the default) disables them.
func (o *Orchestrator) SetDecisions(d *decide.Decider) { o.decisions = d }

// newSafeYield serializes node goroutines onto one yield and stops after a panic: re-entering a
// panicked yield makes Go replace the real panic value and kill the process.
func newSafeYield(yield func(stream.SSEEvent, error) bool) func(stream.SSEEvent, error) bool {
	var mu sync.Mutex
	stopped := false
	return func(ev stream.SSEEvent, e error) (ok bool) {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return false
		}
		defer func() {
			if r := recover(); r != nil {
				// Re-panic: swallowing a loop-body panic makes the runtime panic at the range site instead.
				stopped = true
				slog.Error("orchestrator: panic in stream consumer, run aborted",
					"component", "orchestrator", "panic", r, "stack", string(debug.Stack()))
				panic(r)
			}
		}()
		// false: the consumer stopped ranging; calling the exhausted closure again panics.
		if !yield(ev, e) {
			stopped = true
			return false
		}
		return true
	}
}

// CancelNode stops one node of the active DAG run. Cooperative: next gate boundary.
func (o *Orchestrator) CancelNode(chatID, nodeID string) bool {
	return o.executor.CancelNode(chatID, nodeID)
}

// NodeIsLive reports whether nodeID already has a dispatch running it. A nil
// receiver (no orchestrator wired) has nothing live, by definition.
func (o *Orchestrator) NodeIsLive(chatID, nodeID string) bool {
	if o == nil {
		return false
	}
	return o.executor.NodeIsLive(chatID, nodeID)
}

// PauseNode suspends a node at its next gate boundary; resumable.
func (o *Orchestrator) PauseNode(chatID, nodeID string, reason dag.PauseReason) bool {
	return o.executor.PauseNode(chatID, nodeID, reason)
}

// StopNode cancels a node into the terminal cancelled state.
func (o *Orchestrator) StopNode(chatID, nodeID string) bool {
	return o.executor.StopNode(chatID, nodeID)
}

// QueueNodeMessage appends a message to a running node's queue.
func (o *Orchestrator) QueueNodeMessage(chatID, nodeID, text string) (dag.QueuedMessage, bool) {
	return o.executor.QueueNodeMessage(chatID, nodeID, text)
}

// EditQueuedMessage rewrites a not-yet-delivered queued message.
func (o *Orchestrator) EditQueuedMessage(chatID, nodeID, messageID, text string) bool {
	return o.executor.EditQueuedMessage(chatID, nodeID, messageID, text)
}

// RemoveQueuedMessage drops a not-yet-delivered queued message.
func (o *Orchestrator) RemoveQueuedMessage(chatID, nodeID, messageID string) bool {
	return o.executor.RemoveQueuedMessage(chatID, nodeID, messageID)
}

// SetNodeTaskOverride edits a not-yet-started node's task text.
func (o *Orchestrator) SetNodeTaskOverride(chatID, nodeID, task string) bool {
	return o.executor.SetNodeTaskOverride(chatID, nodeID, task)
}

// RetryNode re-runs a finished node and its descendants with optional guidance. A non-empty
// planID names the node's own plan, so a stale stash never runs another plan's task.
func (o *Orchestrator) RetryNode(ctx context.Context, userID, chatID, planID string, seeded map[string]string, nodeID, guidance string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		ctx, done := o.executor.Pin(ctx)
		defer done()
		// A retry is its own run and needs its own trace, not the earlier run's stale trace_id.
		ctx = runCoords(ctx, chatID, userID)
		var span oteltrace.Span
		ctx, span = otelobs.Start(ctx, "run", attribute.String(otelobs.ChatIDKey, chatID))
		defer otelobs.End(span, nil)
		otelobs.RunStarted()
		defer otelobs.RunFinished()
		plan, err := o.planFor(ctx, userID, chatID, planID, nodeID)
		if err != nil {
			yield(stream.Errorf("retry: "+err.Error()), nil)
			return
		}
		// Lead with the plan snapshot so runlog.Drive-based callers (boot resume) persist the re-run nodes'
		// state. Emitted before the guidance: persisted plans keep the original task, so it never piles up.
		yield(tools.DagPlanEvent(ctx, plan), nil)
		if guidance = strings.TrimSpace(guidance); guidance != "" {
			for i := range plan.Nodes {
				if plan.Nodes[i].ID == nodeID {
					plan.Nodes[i].Task += "\n\n[Retry guidance]: " + guidance
				}
			}
		}
		nodeOutputs := make(map[string]string)
		retryNode := workflow.NewDynamicNode[any, string]("__retry",
			func(nctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
				out, rerr := o.executor.RetryPlanInNode(nctx, plan, chatID, nodeID, seeded)
				if rerr != nil {
					return "", rerr
				}
				for k, v := range out {
					nodeOutputs[k] = v
				}
				return "done", nil
			}, workflow.NodeConfig{})
		wf, err := workflowagent.New(workflowagent.Config{Name: "orchestrator-retry", Edges: workflow.Chain(workflow.Start, retryNode)})
		if err != nil {
			yield(stream.Errorf("orchestrator: retry workflow: "+err.Error()), nil)
			return
		}
		runSess := chatID + "::retry"
		r, err := runner.New(runner.Config{AppName: AppName, Agent: wf, SessionService: o.sessions, AutoCreateSession: true})
		if err != nil {
			yield(stream.Errorf("orchestrator: retry runner: "+err.Error()), nil)
			return
		}
		safeYield := newSafeYield(yield)
		ctx = stream.WithYield(ctx, func(ev stream.SSEEvent) { safeYield(ev, nil) })
		ds := o.executor.NewDagStream(ctx, plan, AppName, userID, runSess, chatID, safeYield, nodeOutputs)
		ds.ScopeToRetry(nodeID)
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "retry " + nodeID}}}
		for ev, rerr := range r.Run(ctx, userID, runSess, content, adkagent.RunConfig{}) {
			if rerr != nil {
				ds.Abort(rerr)
				safeYield(stream.Errorf(rerr.Error()), nil)
				return
			}
			if ev == nil {
				continue
			}
			ds.Handle(ev)
		}
		ds.Finish()
		o.settleRetried(ctx, userID, chatID, plan, ds.Started(), nodeOutputs)
		started := ds.Started()
		outputs, recStopped := o.withRecordedSinks(ctx, userID, chatID, plan, nodeOutputs, func(id string) bool { return started[id] })
		if answer, _ := o.sinkAnswer(plan, outputs, chatID, recStopped); answer != "" {
			o.persistAnswer(ctx, userID, chatID, answer)
		}
	}
}

// settleRetried records a retry's fresh outputs on the dag_plan record, clearing an earlier stop:
// otherwise later deliveries mask, and later nodes seed from, the stale stopped draft.
func (o *Orchestrator) settleRetried(ctx context.Context, userID, chatID string, plan dag.Plan, started map[string]bool, outputs map[string]string) {
	if o.artifacts == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	rec, _, ok, err := dag.LoadDagPlanRecord(ctx, o.artifacts, artifactref.AppName, userID, chatID)
	if err != nil || !ok || rec.PlanID != plan.ID {
		return
	}
	ran := func(id string) bool { return started[id] }
	// A restart mid-step left the record on the step before: this run completes that step, so it records it.
	sinks, stale := staleSinks(rec, plan, ran)
	changed := stale
	if stale {
		rec.Sinks = sinks
	}
	for i := range rec.Assignments {
		a := &rec.Assignments[i]
		out, stopped := outputs[a.NodeID], o.executor.NodeStopped(chatID, a.NodeID)
		switch {
		case !started[a.NodeID]:
		case a.TaskID == "":
			tools.ApplyAssignmentOutcome(a, out, false, stopped)
			changed = true
		case strings.TrimSpace(out) != "" && !stopped:
			a.Result, a.Stopped, changed = out, false, true
		}
	}
	if !changed {
		return
	}
	if _, _, err := dag.SaveDagPlanRecord(ctx, o.artifacts, artifactref.AppName, userID, chatID, "", rec); err != nil {
		slog.Warn("retry: dag_plan update failed", "component", "orchestrator", "chat", chatID, "err", err)
	}
}

// Pin holds the executor's current agent roster on ctx until done; a caller that
// builds a bound plan and runs it later pins across both so they agree.
func (o *Orchestrator) Pin(ctx context.Context) (context.Context, func()) { return o.executor.Pin(ctx) }

// BuildBoundPlan builds a Plan from a workflow-catalog-bound node list: no plan judge, no review
// fanout, no orchestrator LLM turn. allowedKinds nil means unrestricted.
func (o *Orchestrator) BuildBoundPlan(ctx context.Context, nodes []dag.RawNode, message string, attachments []*genai.Part, allowedKinds []string) (*dag.Plan, error) {
	ctx, done := o.executor.Pin(ctx)
	defer done()
	return o.planner.BuildBound(ctx, nodes, nil, nil, message, attachments, allowedKinds)
}

// RunBoundPlan runs a bound Plan straight through the graph executor with no orchestrator LLM
// turn; every node still passes the trust gate via RunPlanAsGraph.
func (o *Orchestrator) RunBoundPlan(ctx context.Context, userID, sessionID, source string, plan dag.Plan) iter.Seq2[stream.SSEEvent, error] {
	// An earlier unbound turn's plan rejection must not leak into this run's terminal status.
	inference.ClearPlanRejection(sessionID)
	return func(yield func(stream.SSEEvent, error) bool) {
		ctx, done := o.executor.Pin(ctx)
		defer done()
		var span oteltrace.Span
		// Clear a prior turn's planning failure: this path makes no model call that would clear it.
		inference.ClearFailure(sessionID, "", "")
		// Coords first: the root span reads them for gen_ai.conversation.id/user.id.
		ctx = ledger.WithCoords(ctx, ledger.Coords{ChatID: sessionID, User: userID, Source: source})
		ctx, span = otelobs.Start(ctx, "run.bound", attribute.String(otelobs.ChatIDKey, sessionID))
		otelobs.RunStarted()
		defer func() {
			otelobs.RunFinished()
			otelobs.End(span, nil)
		}()
		origYield := yield
		yield = func(ev stream.SSEEvent, err error) bool {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return origYield(ev, err)
		}
		// Concurrent DAG nodes all funnel through this one yield.
		safeYield := newSafeYield(yield)

		o.executor.ResetNodeCancels(sessionID)

		ctx = stream.WithYield(ctx, func(ev stream.SSEEvent) { safeYield(ev, nil) })
		safeYield(tools.DagPlanEvent(ctx, plan), nil)

		// No llmagent turn appends this turn's user event; without it, store.GetTurnsWithContent
		// misaligns every later turn's content.
		o.persistUserMessage(ctx, userID, sessionID, plan.UserMessage)

		// No execute tool call exists to fail into, so surface setup failure on the stream.
		if perr := o.executor.Provision(ctx, userID, sessionID, &plan); perr != nil {
			safeYield(stream.Errorf("orchestrator: bound plan setup: "+perr.Error()), nil)
			return
		}

		nodeOutputs := make(map[string]string)
		paused, err := o.executor.RunPlanAsGraph(ctx, plan, AppName, userID, sessionID, nil, safeYield, nodeOutputs, nil)
		if err != nil {
			safeYield(stream.Errorf("orchestrator: bound plan run: "+err.Error()), nil)
			return
		}
		// Stash like execute does so a HITL resume finds it; only after RunPlanAsGraph, whose runner
		// auto-creates the session.
		o.stashPlanForResume(ctx, userID, sessionID, plan)
		if !paused {
			answer := o.finalizeAnswer(ctx, plan, nodeOutputs, sessionID, nil)
			o.persistAnswer(ctx, userID, sessionID, answer)
		}
		safeYield(stream.Done(), nil)
	}
}

// stashPlanForResume stores plan under tools.ExecPlanKey so a bound run parked on HITL resumes
// like a model-authored one.
func (o *Orchestrator) stashPlanForResume(ctx context.Context, userID, sessionID string, plan dag.Plan) {
	planJSON, err := json.Marshal(plan)
	if err != nil {
		slog.Warn("orchestrator: stash bound plan failed: marshal", "component", "orchestrator", "chat", sessionID, "err", err)
		return
	}
	persistCtx := context.WithoutCancel(ctx)
	resp, err := o.sessions.Get(persistCtx, &session.GetRequest{AppName: AppName, UserID: userID, SessionID: sessionID})
	if err != nil || resp == nil {
		slog.Warn("orchestrator: stash bound plan failed: session load", "component", "orchestrator", "chat", sessionID, "err", err)
		return
	}
	ev := session.NewEvent(persistCtx, "")
	ev.Author = orchestratorName
	ev.Actions.StateDelta[tools.ExecPlanKey] = string(planJSON)
	if err := o.sessions.AppendEvent(persistCtx, resp.Session, ev); err != nil {
		slog.Warn("orchestrator: stash bound plan failed: append event", "component", "orchestrator", "chat", sessionID, "err", err)
	}
}

func New(sessions session.Service, m model.LLM, sysPrompt func(context.Context) string, planner *dag.Planner, executor *dag.Executor, skillTS tool.Toolset, userMem, taskMem *memory.Store) *Orchestrator {
	return &Orchestrator{
		sessions:  sessions,
		model:     m,
		sysPrompt: sysPrompt,
		planner:   planner,
		executor:  executor,
		skillTS:   skillTS,
		taskMem:   taskMem,
		userMem:   userMem,
	}
}

// Run processes message as the orchestrator agent and yields SSE events. source attributes
// token usage/cost: an extension's registration name, or SourceApp.
func (o *Orchestrator) Run(ctx context.Context, userID, sessionID, source, message string, attachments []*genai.Part) iter.Seq2[stream.SSEEvent, error] {
	// An earlier turn's plan rejection must not explain this turn's failures.
	inference.ClearPlanRejection(sessionID)
	return func(yield func(stream.SSEEvent, error) bool) {
		ctx, done := o.executor.Pin(ctx)
		defer done()
		var span oteltrace.Span
		// Clear a prior turn's planning failure, or a turn that ends silent without another model
		// call would report the stale reason.
		inference.ClearFailure(sessionID, "", "")
		// Coords first: the root span reads them for gen_ai.conversation.id/user.id.
		ctx = ledger.WithCoords(ctx, ledger.Coords{ChatID: sessionID, User: userID, Source: source})
		ctx, span = otelobs.Start(ctx, "run", attribute.String(otelobs.ChatIDKey, sessionID))
		otelobs.RunStarted()
		defer func() {
			otelobs.RunFinished()
			otelobs.End(span, nil)
		}()
		origYield := yield
		yield = func(ev stream.SSEEvent, err error) bool {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return origYield(ev, err)
		}

		o.executor.ResetNodeCancels(sessionID)
		s := &orchRun{o: o, ctx: ctx, userID: userID, sessionID: sessionID, source: source, message: message, attachments: attachments}
		s.planCache = tools.NewPlanCache()
		repeats := tools.NewRepeatStates()
		// Set by the repeat guard's hard stop - marks this as an unbreakable
		// loop, not a retryable blank turn.
		var guardStopped atomic.Bool
		s.guardStopped = &guardStopped
		guardTripped := func(_, _, msg string) bool {
			guardStopped.Store(true)
			// Reuses the plan-rejection give-up path (store.DeriveTerminalStatus) - a
			// hard stop is the same "known reason, no DagNode" shape as a rejected plan.
			inference.RecordPlanRejection(sessionID, msg)
			return true
		}
		memTurnEnded := o.maybeMineUserMemory(ctx, userID, sessionID, source, message)
		defer memTurnEnded()
		prior := o.PriorEvents(ctx, userID, sessionID)
		pending, hasPending := LatestPendingQuestion(prior)
		if hasPending {
			if pend, isNode := pending.NodeInterrupt(); isNode {
				o.resumeNodeRun(ctx, userID, sessionID, message, pend, yield)
				return
			}
		} else if pend, ok := o.pendingStepInterrupt(ctx, userID, sessionID); ok {
			o.startIncrementalNodeRun(ctx, userID, sessionID, message, pend, yield)
			return
		}
		s.history = buildHistory(prior)
		if key := waivedPlanShape(pending, hasPending, message); key != "" {
			slog.Info("plan judge waived for the rejected plan: the user chose to run it as is", "component", "orchestrator", "chat", sessionID, "shape", key)
			s.ctx = tools.WithWaivedPlanShape(s.ctx, key)
		}
		var githubSetup *dag.Setup
		if ghs, ok := tools.GitHubSetupFromContext(ctx); ok {
			githubSetup = &ghs
		}
		if e := s.buildDagTools(githubSetup); e != "" {
			yield(stream.Errorf(e), nil)
			return
		}
		if e := s.buildMemoryArtifactTools(githubSetup); e != "" {
			yield(stream.Errorf(e), nil)
			return
		}
		var toolsets []tool.Toolset
		if o.skillTS != nil {
			// Same repeats/guardTripped as s.toolList below - load_skill is as loop-prone as any hand-built tool.
			toolsets = []tool.Toolset{tools.RepeatWrapToolset(o.skillTS, repeats, guardTripped, tools.CallScope{})}
		}
		s.toolsets = toolsets
		// Hand-built tools skip tools.Build, so wrap every one in the repeat guard here.
		for i, t := range s.toolList {
			// Request-mutating-only tools (memory.NewPreload) have no Run to repeat.
			if !tools.SupportsRepeatGuard(t) {
				continue
			}
			wrapped, err := tools.RepeatWrap(t, repeats, guardTripped)
			if err != nil {
				yield(stream.Errorf("orchestrator: repeat guard: "+err.Error()), nil)
				return
			}
			s.toolList[i] = wrapped
		}
		if e := s.buildRunner(); e != "" {
			yield(stream.Errorf(e), nil)
			return
		}
		// Node goroutines (e.g. onQueued) call the ctx yield, so it must be the serialized one.
		s.safeYield = newSafeYield(yield)
		s.ctx = stream.WithYield(s.ctx, func(ev stream.SSEEvent) { s.safeYield(ev, nil) })
		s.ctx = tools.WithNodeStopped(s.ctx, func(nodeID string) bool { return o.executor.NodeStopped(sessionID, nodeID) })

		content := s.buildContent(pending, hasPending)
		s.translator = stream.NewTranslator()
		s.safeYield(stream.SSEEvent{Name: stream.EventAgentStart, Data: stream.AgentStartData{
			RunID: orchRunID, Agent: "orchestrator", Stage: stream.StageWorker, StartedAtMs: time.Now().UnixMilli(),
			TraceID: otelobs.TraceIDOf(s.ctx),
		}}, nil)

		produced, stop := s.invoke(content)
		if _, terminated := s.finishLoop(produced, stop); terminated {
			return
		}

		// The execute tool itself ran every step's assignments (dag.Executor.RunPlanStep)
		// and set planCache.Delivered once a step declared delivery - nothing left to run here.
		s.o.persistAnswer(s.ctx, s.userID, s.sessionID, s.planCache.Delivered())
		s.safeYield(stream.Done(), nil)
	}
}

const maxOrchestratorContinues = 3

const continuationMarker = "CONTINUE - your last turn produced no plan and no answer."

// planExhaustedNotice is the fixed reply when planning never produced an acceptable plan; never
// built from the plan judge's reason, which is internal machinery talk.
const planExhaustedNotice = "I could not produce a workable plan for this request."

// minRejectionsForExhaustion: below this, one rejection then an answer is the model correctly
// pivoting away from a plan, not exhaustion.
const minRejectionsForExhaustion = 2

func continuationContent() *genai.Content {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: continuationMarker + "\n\n" +
		"Nothing ran and the user is still waiting. You have already loaded the skills you need - do not load " +
		"more, and do not think silently. Do ONE of these now:\n If you have already called `create_plan` or `edit_plan` and it returned a plan_id, you have NOT done the work: call `execute` with that plan_id NOW. Describing the plan, or saying it looks good, is not executing it." +
		"- Call `create_plan` with the assignments, then call `execute` with the plan_id it returns.\n" +
		"- Or, if no plan is needed, answer the user directly in text.\n\n" +
		"Do not end this turn without a plan call or an answer."}}}
}

// turnProduced reports whether an event carries answer text or a clarification.
func turnProduced(ev *session.Event) bool {
	if ev == nil || ev.Content == nil || ev.Author == "user" {
		return false
	}
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if p.FunctionCall != nil && p.FunctionCall.Name == tools.ChoiceToolName {
			return true
		}
		if !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil && strings.TrimSpace(p.Text) != "" {
			return true
		}
	}
	return false
}

// runCoords stamps the chat, user and source a run's ledger records and root span file under, as
// Run does - retry, resume and node starts enter without Run's stamp. ctx wins per field.
func runCoords(ctx context.Context, chatID, userID string) context.Context {
	return ledger.WithCoords(ctx, ledger.FillBlankCoords(ledger.CoordsFromContext(ctx), ledger.Coords{ChatID: chatID, User: userID, Source: SourceApp}))
}

// SetPlanLoader wires the store's copy of each plan's full dag.Plan (store.LoadExecPlan).
func (o *Orchestrator) SetPlanLoader(load func(ctx context.Context, planID string) (dag.Plan, bool)) {
	o.planLoader = load
}

// planFor returns plan planID holding nodeID (empty planID: whatever is stashed). The store's
// copy wins: the stash commits only with execute's tool response, so it can lag.
func (o *Orchestrator) planFor(ctx context.Context, userID, chatID, planID, nodeID string) (dag.Plan, error) {
	plan, ok := dag.Plan{}, false
	if planID != "" && o.planLoader != nil {
		plan, ok = o.planLoader(ctx, planID)
	}
	if !ok {
		plan, ok = o.stashedPlan(ctx, userID, chatID)
		ok = ok && (planID == "" || plan.ID == planID)
	}
	if !ok {
		return plan, fmt.Errorf("no plan %s to run", planID)
	}
	if !slices.ContainsFunc(plan.Nodes, func(n dag.Node) bool { return n.ID == nodeID }) {
		return plan, fmt.Errorf("plan %s has no node %s", plan.ID, nodeID)
	}
	return plan, nil
}

// stashedPlan loads the dag.Plan the execute tool stored in session state.
func (o *Orchestrator) stashedPlan(ctx context.Context, userID, chatID string) (dag.Plan, bool) {
	var plan dag.Plan
	if resp, err := o.sessions.Get(ctx, &session.GetRequest{AppName: AppName, UserID: userID, SessionID: chatID}); err == nil && resp != nil {
		if st := resp.Session.State(); st != nil {
			if v, gerr := st.Get(tools.ExecPlanKey); gerr == nil {
				if s, ok := v.(string); ok {
					_ = json.Unmarshal([]byte(s), &plan)
				}
			}
		}
	}
	return plan, len(plan.Nodes) > 0
}

type pendingInterrupt struct {
	id      string
	nodeID  string
	message string
}

var hitlIDRe = regexp.MustCompile(`^(?:hitl|confirm)-(.+)-r\d+$`)

// latestPendingNodeInterrupt scans for the most recent unanswered HITL request.
func latestPendingNodeInterrupt(events []*session.Event) (pendingInterrupt, bool) {
	answered := map[string]bool{}
	for _, ev := range events {
		if ev == nil || ev.Author != "user" || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == workflow.WorkflowInputFunctionCallName {
				answered[p.FunctionResponse.ID] = true
			}
		}
	}
	var out pendingInterrupt
	found := false
	for _, ev := range events {
		if ev == nil || ev.RequestedInput == nil || answered[ev.RequestedInput.InterruptID] {
			continue
		}
		if m := hitlIDRe.FindStringSubmatch(ev.RequestedInput.InterruptID); m != nil {
			out = pendingInterrupt{id: ev.RequestedInput.InterruptID, nodeID: m[1], message: ev.RequestedInput.Message}
			found = true
		}
	}
	return out, found
}

// persistUserMessage appends the user event a bound-plan run would otherwise never write.
func (o *Orchestrator) persistUserMessage(ctx context.Context, userID, sessionID, message string) {
	if message == "" {
		return
	}
	persistCtx := context.WithoutCancel(ctx)
	if resp, gerr := o.sessions.Get(persistCtx, &session.GetRequest{AppName: AppName, UserID: userID, SessionID: sessionID}); gerr == nil && resp != nil {
		uev := session.NewEvent(persistCtx, "")
		uev.Author = "user"
		uev.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: message}}}
		_ = o.sessions.AppendEvent(persistCtx, resp.Session, uev)
	}
}

// persistAnswer appends the delivered answer to the chat session as the orchestrator's model message.
func (o *Orchestrator) persistAnswer(ctx context.Context, userID, sessionID, answer string) {
	if answer == "" {
		return
	}
	persistCtx := context.WithoutCancel(ctx)
	if resp, gerr := o.sessions.Get(persistCtx, &session.GetRequest{AppName: AppName, UserID: userID, SessionID: sessionID}); gerr == nil && resp != nil {
		aev := session.NewEvent(persistCtx, "")
		aev.Author = orchestratorName
		aev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: answer}}}
		// Keyed by the turn the run answers: a retry or resume appends with no user event of its own.
		aev.CustomMetadata = map[string]any{
			stream.DeliveredAnswerMeta: stream.TurnIDFromContext(ctx),
			stream.DeliveredAtMeta:     time.Now().UTC().Format(time.RFC3339Nano),
		}
		_ = o.sessions.AppendEvent(persistCtx, resp.Session, aev)
	}
}

// resumeNodeRun delivers a paused node's answer and streams the resumed graph.
func (o *Orchestrator) resumeNodeRun(ctx context.Context, userID, sessionID, message string, pend pendingInterrupt, yield func(stream.SSEEvent, error) bool) {
	o.startNodeRun(ctx, userID, sessionID, "", message, &pend, pend.nodeID, yield)
}

// StartNode re-enters the stashed plan's graph at a paused node; message answers a parked HITL
// question. A node paused mid-incremental-step resumes through its plan-step session instead.
func (o *Orchestrator) StartNode(ctx context.Context, userID, sessionID, planID, nodeID, message string, yield func(stream.SSEEvent, error) bool) {
	ctx, done := o.executor.Pin(ctx)
	defer done()
	ctx = runCoords(ctx, sessionID, userID)
	o.executor.StartNode(sessionID, nodeID)
	if p, ok := latestPendingNodeInterrupt(o.PriorEvents(ctx, userID, sessionID)); ok && p.nodeID == nodeID {
		o.startNodeRun(ctx, userID, sessionID, planID, message, &p, nodeID, yield)
		return
	}
	if p, ok := o.pendingStepInterrupt(ctx, userID, sessionID); ok && p.nodeID == nodeID {
		o.startIncrementalNodeRun(ctx, userID, sessionID, message, p, yield)
		return
	}
	o.startNodeRun(ctx, userID, sessionID, planID, message, nil, nodeID, yield)
}

// pendingStepInterrupt checks the plan-step session, where RunPlanStep runs: a nested
// runner.Run on the live chat session would corrupt its event bookkeeping.
func (o *Orchestrator) pendingStepInterrupt(ctx context.Context, userID, sessionID string) (pendingInterrupt, bool) {
	return latestPendingNodeInterrupt(o.PriorEvents(ctx, userID, dag.PlanStepSessionID(sessionID)))
}

// startIncrementalNodeRun answers a node paused mid-step and records its result like execute()
// does; it finalizes only when the plan's declared delivery covers it, as the plan may be partial.
func (o *Orchestrator) startIncrementalNodeRun(ctx context.Context, userID, sessionID, message string, pend pendingInterrupt, yield func(stream.SSEEvent, error) bool) {
	plan, rec, recordSvc, errMsg := o.loadResumePlan(ctx, userID, sessionID, pend.nodeID)
	if errMsg != "" {
		yield(stream.Errorf(errMsg), nil)
		return
	}
	run := map[string]bool{pend.nodeID: true}
	seeded := seededFrom(rec)

	safeYield := newSafeYield(yield)
	ctx = stream.WithYield(ctx, func(ev stream.SSEEvent) { safeYield(ev, nil) })
	safeYield(tools.DagPlanEvent(ctx, plan), nil)

	outputs, needsInput, started, err := o.executor.ResumePlanStep(ctx, plan, AppName, userID, sessionID, seeded, run, pend.id, message)
	if err != nil {
		safeYield(stream.Errorf("resume: "+err.Error()), nil)
		return
	}
	if len(needsInput) > 0 {
		yield(stream.Done(), nil)
		return
	}

	var anyFailed bool
	out := outputs[pend.nodeID]
	for i := range rec.Assignments {
		if rec.Assignments[i].NodeID == pend.nodeID && started[pend.nodeID] {
			// Same success/failure decision as execute: an empty resumed output fails, never finalizes.
			if tools.ApplyAssignmentOutcome(&rec.Assignments[i], out, false, o.executor.NodeStopped(sessionID, pend.nodeID)) == "failed" {
				anyFailed = true
			}
		}
	}
	if _, _, serr := dag.SaveDagPlanRecord(ctx, recordSvc, artifactref.AppName, userID, sessionID, "", rec); serr != nil {
		slog.Warn("resume: dag_plan update failed", "component", "orchestrator", "err", serr)
	}

	// Completion may unblock dependents the resume never ran; finalizing now would deliver a
	// terminal node's still-empty result.
	turnEnded, moreFailed := o.driveUnblocked(ctx, plan, rec, recordSvc, userID, sessionID, safeYield)
	anyFailed = anyFailed || moreFailed
	if turnEnded {
		yield(stream.Done(), nil)
		return
	}

	if !anyFailed && rec.Delivery != nil {
		o.deliverFromRecord(ctx, userID, sessionID, plan, rec)
	}
	yield(stream.Done(), nil)
}

// deliverFromRecord finalizes a resumed plan from its record; a stopped assignment's draft is masked
// there because the executor's own stop flag is gone once the turn that stopped it ended.
func (o *Orchestrator) deliverFromRecord(ctx context.Context, userID, sessionID string, plan dag.Plan, rec dag.DagPlanRecord) {
	final, stopped := tools.DeliverableResults(rec.Assignments)
	answer, _ := o.sinkAnswer(plan, onlySinks(final, rec.Sinks), sessionID, stopped)
	o.persistAnswer(ctx, userID, sessionID, answer)
}

// onlySinks keeps just the recorded step's sinks, so an extended plan's earlier turns' sinks stay out
// of this turn's answer; a record without them keeps every output.
func onlySinks(outputs map[string]string, sinks []string) map[string]string {
	if len(sinks) == 0 {
		return outputs
	}
	own := make(map[string]string, len(sinks))
	for _, id := range sinks {
		own[id] = outputs[id]
	}
	return own
}

// withRecordedSinks narrows a retry or resume to the sinks the plan's latest step delivers, taking the
// record's result for each one this run did not run; a stopped sibling's draft stays masked.
func (o *Orchestrator) withRecordedSinks(ctx context.Context, userID, chatID string, plan dag.Plan, outputs map[string]string, ran func(string) bool) (map[string]string, map[string]bool) {
	terminals := dag.TerminalIDs(plan.Nodes)
	rec, hasRec := o.planRecord(ctx, userID, chatID, plan.ID)
	sinks := terminals
	if hasRec && len(rec.Sinks) > 0 {
		sinks = rec.Sinks
		if ranSinks, stale := staleSinks(rec, plan, ran); stale {
			sinks = ranSinks
		}
	}
	if len(sinks) < 2 && slices.Equal(sinks, terminals) {
		return outputs, nil
	}
	final, stopped := tools.DeliverableResults(rec.Assignments)
	// No record (an older plan's retry): the seeds' unreviewed flags are the only stop marks left.
	unreviewed := dag.UnreviewedSeedsFrom(ctx)
	own, recStopped := make(map[string]string, len(sinks)), map[string]bool{}
	for _, id := range sinks {
		switch out, seeded := outputs[id]; {
		case ran(id):
			own[id] = out
		case hasRec:
			own[id], recStopped[id] = final[id], stopped[id]
		case seeded:
			own[id], recStopped[id] = out, unreviewed[id]
		}
	}
	return own, recStopped
}

// staleSinks recomputes the step sinks from what ran when the record's predate this run's step (a restart
// mid-extension): none of them ran here and a node that did was never settled.
func staleSinks(rec dag.DagPlanRecord, plan dag.Plan, ran func(string) bool) ([]string, bool) {
	ranSinks := dag.SinksAmong(plan.Nodes, ran)
	unsettled := slices.ContainsFunc(rec.Assignments, func(a dag.Assignment) bool { return a.TaskID == "" && ran(a.NodeID) })
	return ranSinks, len(rec.Sinks) > 0 && len(ranSinks) > 0 && !slices.ContainsFunc(rec.Sinks, ran) && unsettled
}

// planRecord is the chat's dag_plan record when it records planID.
func (o *Orchestrator) planRecord(ctx context.Context, userID, chatID, planID string) (dag.DagPlanRecord, bool) {
	if o.artifacts == nil {
		return dag.DagPlanRecord{}, false
	}
	rec, _, ok, err := dag.LoadDagPlanRecord(context.WithoutCancel(ctx), o.artifacts, artifactref.AppName, userID, chatID)
	return rec, err == nil && ok && rec.PlanID == planID
}

// loadResumePlan: the plan and its record for an incremental resume.
// Returns the message to yield on failure, "" on success.
func (o *Orchestrator) loadResumePlan(ctx context.Context, userID, sessionID, nodeID string) (plan dag.Plan, rec dag.DagPlanRecord, recordSvc artifact.Service, errMsg string) {
	recordSvc = o.artifacts
	if recordSvc == nil {
		recordSvc = artifact.InMemoryService()
	}
	rec, _, ok, err := dag.LoadDagPlanRecord(ctx, recordSvc, artifactref.AppName, userID, sessionID)
	if err != nil || !ok {
		return plan, rec, recordSvc, "resume: no plan record to resume"
	}
	plan, err = o.planFor(ctx, userID, sessionID, rec.PlanID, nodeID)
	if err != nil {
		return plan, rec, recordSvc, "resume: " + err.Error()
	}
	return plan, rec, recordSvc, ""
}

// seededFrom: the seeded outputs map for a plan record - every assignment
// that ran (TaskID set) feeds its result to its dependents.
func seededFrom(rec dag.DagPlanRecord) map[string]string {
	seeded := map[string]string{}
	for _, a := range rec.Assignments {
		if a.TaskID != "" {
			seeded[a.NodeID] = a.Result
		}
	}
	return seeded
}

// driveUnblocked: drive newly-unblocked assignments the way execute.go drives a
// fresh step, round by round, until nothing more unblocks or a round pauses/errors.
func (o *Orchestrator) driveUnblocked(ctx context.Context, plan dag.Plan, rec dag.DagPlanRecord, recordSvc artifact.Service, userID, sessionID string, safeYield func(stream.SSEEvent, error) bool) (turnEnded, anyFailed bool) {
	for {
		next := unblockedByDeps(rec.Assignments)
		if len(next) == 0 {
			break
		}
		roundOutputs, roundNeedsInput, roundStarted, rerr := o.executor.RunPlanStep(dag.WithUnreviewedSeeds(ctx, tools.UnreviewedSeeds(rec.Assignments)), plan, AppName, userID, sessionID, seededFrom(rec), next)
		if rerr != nil {
			safeYield(stream.Errorf("resume: "+rerr.Error()), nil)
			turnEnded = true
		}
		for i := range rec.Assignments {
			nid := rec.Assignments[i].NodeID
			if !next[nid] {
				continue
			}
			// Same hazard as execute.go's own step: a sibling pausing first
			// can leave nid requested but never dispatched - leave it untouched.
			if !roundStarted[nid] {
				continue
			}
			if tools.ApplyAssignmentOutcome(&rec.Assignments[i], roundOutputs[nid], roundNeedsInput[nid], o.executor.NodeStopped(sessionID, nid)) == "failed" {
				anyFailed = true
			}
		}
		if _, _, serr := dag.SaveDagPlanRecord(ctx, recordSvc, artifactref.AppName, userID, sessionID, "", rec); serr != nil {
			slog.Warn("resume: dag_plan update failed", "component", "orchestrator", "err", serr)
		}
		if rerr != nil {
			return turnEnded, anyFailed
		}
		if len(roundNeedsInput) > 0 {
			turnEnded = true
			break
		}
	}
	return turnEnded, anyFailed
}

// unblockedByDeps returns not-yet-run assignments whose deps all ran (success or failure). Not
// scoped to the resumed chain: an unrelated dependency-free assignment is just as ready.
func unblockedByDeps(assignments []dag.Assignment) map[string]bool {
	ran := make(map[string]bool, len(assignments))
	for _, a := range assignments {
		if a.TaskID != "" {
			ran[a.NodeID] = true
		}
	}
	next := map[string]bool{}
	for _, a := range assignments {
		if a.TaskID != "" {
			continue
		}
		ready := true
		for _, dep := range a.DependsOn {
			if !ran[dep] {
				ready = false
				break
			}
		}
		if ready {
			next[a.NodeID] = true
		}
	}
	return next
}

func (o *Orchestrator) startNodeRun(ctx context.Context, userID, sessionID, planID, message string, pend *pendingInterrupt, nodeID string, yield func(stream.SSEEvent, error) bool) {
	// Run's resumes already sit inside its "run" span; only a bare StartNode ctx opens one.
	var span oteltrace.Span
	if !oteltrace.SpanFromContext(ctx).SpanContext().IsValid() {
		ctx, span = otelobs.Start(ctx, "run", attribute.String(otelobs.ChatIDKey, sessionID))
	}
	defer func() {
		if span != nil {
			otelobs.End(span, nil)
		}
	}()
	plan, err := o.planFor(ctx, userID, sessionID, planID, nodeID)
	if err != nil {
		yield(stream.Errorf("resume: "+err.Error()), nil)
		return
	}
	safeYield := newSafeYield(yield)
	ctx = stream.WithYield(ctx, func(ev stream.SSEEvent) { safeYield(ev, nil) })
	safeYield(tools.DagPlanEvent(ctx, plan), nil)
	// awaiting_input: the message is the answer to the parked question.
	// user/shutdown pause: nothing to deliver, just re-enter the graph.
	var content *genai.Content
	if pend != nil {
		content = &genai.Content{Role: "user", Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       pend.id,
				Name:     workflow.WorkflowInputFunctionCallName,
				Response: map[string]any{"payload": message},
			},
		}}}
	} else if strings.TrimSpace(message) != "" {
		content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: message}}}
	}
	nodeOutputs := make(map[string]string)
	paused, err := o.executor.RunPlanAsGraph(ctx, plan, AppName, userID, sessionID, content, safeYield, nodeOutputs, []string{nodeID})
	if err != nil {
		safeYield(stream.Errorf("resume: "+err.Error()), nil)
		return
	}
	if !paused {
		outputs, recStopped := o.withRecordedSinks(ctx, userID, sessionID, plan, nodeOutputs, func(id string) bool {
			_, ran := nodeOutputs[id]
			return ran
		})
		answer, _ := o.sinkAnswer(plan, outputs, sessionID, recStopped)
		o.persistAnswer(ctx, userID, sessionID, answer)
	}
	yield(stream.Done(), nil)
}

// ResetSession deletes session history so the next Run starts fresh.
func (o *Orchestrator) ResetSession(ctx context.Context, userID, sessionID string) error {
	if err := o.sessions.Delete(ctx, &session.DeleteRequest{AppName: AppName, UserID: userID, SessionID: sessionID}); err != nil {
		return err
	}
	if o.nodeSessions != nil {
		// sessionID == chatID at AppName (see Run's callers) - best-effort:
		// a reset must still succeed even if the node sweep can't run.
		if err := o.nodeSessions(ctx, sessionID); err != nil {
			slog.Warn("session reset but its per-node worker sessions could not be reaped",
				"component", "orchestrator", "chat", sessionID, "err", err)
		}
	}
	return nil
}

// PriorEvents reads a chat's persisted session events (nil if missing).
func (o *Orchestrator) PriorEvents(ctx context.Context, userID, sessionID string) []*session.Event {
	resp, err := o.sessions.Get(ctx, &session.GetRequest{AppName: AppName, UserID: userID, SessionID: sessionID})
	if err != nil || resp == nil {
		return nil
	}
	var events []*session.Event
	for ev := range resp.Session.Events().All() {
		events = append(events, ev)
	}
	return events
}

// historyBuilder folds session events into history turns (a user event opens a
// turn, later gate events fill its model side).
type historyBuilder struct {
	turns     []dag.HistoryTurn
	userText  strings.Builder
	modelText strings.Builder
	haveTurn  bool
}

func (b *historyBuilder) flush() {
	if !b.haveTurn {
		return
	}
	if t := strings.TrimSpace(b.userText.String()); t != "" {
		b.turns = append(b.turns, dag.HistoryTurn{Role: "user", Text: t})
	}
	if t := strings.TrimSpace(b.modelText.String()); t != "" {
		b.turns = append(b.turns, dag.HistoryTurn{Role: "model", Text: t})
	}
}

func (b *historyBuilder) addUserEvent(ev *session.Event) {
	b.flush()
	b.userText.Reset()
	b.modelText.Reset()
	b.haveTurn = true
	for _, p := range ev.Content.Parts {
		if p != nil && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil {
			b.userText.WriteString(p.Text)
		}
	}
}

func (b *historyBuilder) addModelEvent(ev *session.Event) {
	for _, p := range ev.Content.Parts {
		if p == nil || p.Thought || p.FunctionCall != nil || p.FunctionResponse != nil {
			continue
		}
		b.modelText.WriteString(p.Text)
	}
}

// buildHistory converts prior events into dag.HistoryTurn values for the planner.
func buildHistory(events []*session.Event) []dag.HistoryTurn {
	var b historyBuilder
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		if ev.Author == "user" {
			b.addUserEvent(ev)
		} else if b.haveTurn {
			b.addModelEvent(ev)
		}
	}
	b.flush()
	return b.turns
}

// pendingChoice returns the call ID and question of the most recent unanswered get_user_choice.
func pendingChoice(events []*session.Event) (callID, question string) {
	var pendingID, pendingQuestion string
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil && p.FunctionCall.Name == tools.ChoiceToolName {
				pendingID = p.FunctionCall.ID
				if q, ok := p.FunctionCall.Args["question"].(string); ok {
					pendingQuestion = q
				}
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == tools.ChoiceToolName && p.FunctionResponse.ID == pendingID {
				if _, answered := p.FunctionResponse.Response[tools.ChoiceAnswerKey]; answered {
					pendingID = ""
					pendingQuestion = ""
				}
			}
		}
	}
	return pendingID, pendingQuestion
}

// PendingQuestion is an unanswered question blocking a chat's next turn.
type PendingQuestion struct {
	Message      string
	node         pendingInterrupt
	isNode       bool
	choiceCallID string
}

func (p PendingQuestion) NodeInterrupt() (pendingInterrupt, bool) { return p.node, p.isNode }

// LatestPendingQuestion scans session events for the most recent unanswered question.
func LatestPendingQuestion(events []*session.Event) (PendingQuestion, bool) {
	if pend, ok := latestPendingNodeInterrupt(events); ok {
		return PendingQuestion{Message: pend.message, node: pend, isNode: true}, true
	}
	if callID, question := pendingChoice(events); callID != "" {
		return PendingQuestion{Message: question, choiceCallID: callID}, true
	}
	return PendingQuestion{}, false
}

// pendingSessionEvents merges the chat and plan-step sessions' events by time; safe because Run
// resumes before its tool loop could open an interrupt in the other.
func (o *Orchestrator) pendingSessionEvents(ctx context.Context, userID, sessionID string) []*session.Event {
	events := append(o.PriorEvents(ctx, userID, sessionID), o.PriorEvents(ctx, userID, dag.PlanStepSessionID(sessionID))...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Timestamp.Before(events[j].Timestamp) })
	return events
}

// LatestPendingQuestion exposes the package function's scan merged across the chat and plan-step sessions.
func (o *Orchestrator) LatestPendingQuestion(ctx context.Context, userID, sessionID string) (PendingQuestion, bool) {
	return LatestPendingQuestion(o.pendingSessionEvents(ctx, userID, sessionID))
}

// PendingQuestion is LatestPendingQuestion's message, for callers outside this package.
func (o *Orchestrator) PendingQuestion(ctx context.Context, userID, sessionID string) (string, bool) {
	pq, ok := o.LatestPendingQuestion(ctx, userID, sessionID)
	if !ok {
		return "", false
	}
	return pq.Message, true
}

// LatestAnswer returns the final orchestrator-authored text persisted for a session.
func (o *Orchestrator) LatestAnswer(ctx context.Context, userID, sessionID string) string {
	var latest string
	for _, ev := range o.PriorEvents(ctx, userID, sessionID) {
		if t := answerText(ev); t != "" {
			latest = t
		}
	}
	return latest
}

// answerText is an orchestrator event's visible text, "" for any other event.
func answerText(ev *session.Event) string {
	if ev == nil || ev.Content == nil || ev.Author != orchestratorName {
		return ""
	}
	var sb strings.Builder
	for _, p := range ev.Content.Parts {
		if p != nil && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil {
			sb.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(sb.String())
}

type AgentClients = map[string]adkagent.Agent
