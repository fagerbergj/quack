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
	"github.com/fagerbergj/quack/internal/dag"
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

// SourceApp: the gen_ai.client.token.usage/cost "source" value for a direct
// UI/REST/MCP chat - as opposed to an extension-dispatched run, whose source
// is that extension's own registration name.
const SourceApp = "app"

// Orchestrator: ADK llmagent that selects direct answer or plan + execute.
type Orchestrator struct {
	sessions    session.Service
	model       model.LLM
	sysPrompt   string
	planner     *dag.Planner
	executor    *dag.Executor
	skillTS     tool.Toolset
	userMem     *memory.Store
	taskMem     *memory.Store
	memAgent    adkagent.Agent
	artifacts   artifact.Service
	ledgerStore ledger.LedgerStore
	// nodeSessions best-effort reaps a chat's per-DAG-node ADK sessions
	// (deterministic "<chatID>:<nodeID>" ids - see internal/agent.WorkerSessionID)
	// alongside the chat-level one ResetSession already deletes. nil (e.g.
	// tests) skips it - see SetNodeSessionReaper.
	nodeSessions func(ctx context.Context, chatID string) error
	// compaction: built once from config (see SetCompaction) and reused on
	// every turn's runner.Config - nil leaves the chat session uncompacted,
	// same as before #A3.
	compaction *compaction.Config
	// assignmentFreshness/assignmentMeta: optional extension hooks (see
	// SetAssignmentFreshnessCheck/SetAssignmentMetaHook) - nil until an
	// extension implementing the optional interface is active.
	assignmentFreshness tools.AssignmentFreshnessFunc
	assignmentMeta      tools.AssignmentMetaFunc
}

// SetAssignmentFreshnessCheck wires execute's optional per-reused-node
// staleness check (design: an extension's BeforeAssignment hook, e.g. the
// GitHub extension comparing assignment.meta.github.base_sha against the
// branch's current tip). nil (no active extension implements it) means
// every reused node is always treated as fresh.
func (o *Orchestrator) SetAssignmentFreshnessCheck(fn tools.AssignmentFreshnessFunc) {
	o.assignmentFreshness = fn
}

// SetAssignmentMetaHook wires create_plan/edit_plan's optional per-assignment
// meta stamp (design: an extension's OnAssignment hook) - nil skips it, so
// assignment.meta.<extension> stays unset until an extension supplies one.
func (o *Orchestrator) SetAssignmentMetaHook(fn tools.AssignmentMetaFunc) {
	o.assignmentMeta = fn
}

// SetCompaction wires adk/v2's native runner-level compaction (built via
// internal/agent.NativeCompactionConfig, the same helper a worker node's own
// A2AServer uses - internal/agent.Serve) onto the orchestrator's own runner,
// so the chat session that persists across every turn compacts too, not
// only the ephemeral per-node ones (#A3). nil is a valid "disabled" value.
func (o *Orchestrator) SetCompaction(cfg *compaction.Config) { o.compaction = cfg }

// SetNodeSessionReaper wires the store-layer sweep (store.ReapNodeSessions)
// ResetSession uses to also delete a chat's per-node worker sessions -
// store owns the raw SQL because sessions/events are only addressable that
// way by chat id (session.Service has no pattern-delete), and orchestrator
// must not import store (serve already imports both; see New's callers).
func (o *Orchestrator) SetNodeSessionReaper(fn func(ctx context.Context, chatID string) error) {
	o.nodeSessions = fn
}

// SetArtifacts wires an artifact.Service into the orchestrator's own runner
// and, when load_artifacts is in orchestrator.tools, exposes the load_artifacts
// tool. Mirrors dag.Executor.SetArtifacts.
func (o *Orchestrator) SetArtifacts(svc artifact.Service) { o.artifacts = svc }

// SetLedger wires the WAL's fail-closed AppendIntent path into the
// orchestrator's own write_<kind>/write_artifact tools, so a direct-chat
// write records parent_revision like every gated node does (#1153). Mirrors dag.Executor.SetWALLedger.
func (o *Orchestrator) SetLedger(store ledger.LedgerStore) { o.ledgerStore = store }

// failSoftListArtifacts: load_artifacts calls List on every LLM request
// (ADK's loadartifactstool.ProcessRequest), and a List error fails the whole
// orchestrator turn - not just artifact loading. A transient artifact-store outage shouldn't take down ordinary chat, so List degrades to "no artifacts" instead of erroring; Load/Save/Delete/Versions pass through.
type failSoftListArtifacts struct{ artifact.Service }

func (s failSoftListArtifacts) List(ctx context.Context, req *artifact.ListRequest) (*artifact.ListResponse, error) {
	resp, err := s.Service.List(ctx, req)
	if err != nil {
		slog.Warn("orchestrator: artifact List failed; offering no artifacts this turn", "err", err)
		// A dial error that survived the pgdial retry (#1193) must not vanish as a
		// silent gap once this degrades to "no artifacts" - stamp it for DeriveTerminalStatus.
		inference.RecordStoreFailure(req.SessionID, err)
		return &artifact.ListResponse{}, nil
	}
	inference.ClearStoreFailure(req.SessionID)
	return resp, nil
}

// Load degrades to a text-part message on any failure - not found, oversize,
// or a transient store error - instead of returning an error: ADK's
// loadartifactstool runs every requested name's Load in one errgroup, and one error there cancels every sibling load and fails the whole turn (#1225 - one bad name in a model's load_artifacts call killed plan+answer both).
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

// newSafeYield serializes concurrent node goroutines onto one yield and stops
// after a panicking call: a second goroutine re-entering the panicked yield
// makes Go replace the real panic value and kill the process (#1016).
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
				// Log the real value, then resume: swallowing a loop-body panic
				// makes the runtime panic at the range site instead (#1033).
				// stopped keeps racing nodes out of the dead yield (#1016).
				stopped = true
				slog.Error("orchestrator: panic in stream consumer, run aborted",
					"component", "orchestrator", "panic", r, "stack", string(debug.Stack()))
				panic(r)
			}
		}()
		// A false return means the consumer stopped ranging (client gone); calling
		// the exhausted closure again is itself a panic (#1033).
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

// RetryNode re-runs a finished node and its descendants with optional
// guidance; node-level dag.Admission still gates the work.
func (o *Orchestrator) RetryNode(ctx context.Context, userID, chatID string, seeded map[string]string, nodeID, guidance string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		// A retry/resume is its own run, not a continuation of whatever
		// finished run left this node retryable - it needs its own trace so
		// a stale trace_id from the earlier run is never mistaken for this one.
		var span oteltrace.Span
		ctx, span = otelobs.Start(ctx, "run", attribute.String(otelobs.ChatIDKey, chatID))
		defer otelobs.End(span, nil)
		otelobs.RunStarted()
		defer otelobs.RunFinished()
		plan, ok := o.stashedPlan(ctx, userID, chatID)
		if !ok {
			yield(stream.Errorf("retry: no plan in session to retry"), nil)
			return
		}
		if guidance = strings.TrimSpace(guidance); guidance != "" {
			for i := range plan.Nodes {
				if plan.Nodes[i].ID == nodeID {
					plan.Nodes[i].Task += "\n\n[Retry guidance]: " + guidance
				}
			}
		}
		// Lead with the plan snapshot so runlog.Drive-based callers (boot
		// resume) persist the re-run nodes' state; REST persists per-event.
		yield(tools.DagPlanEvent(ctx, plan), nil)
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
				safeYield(stream.Errorf(rerr.Error()), nil)
				return
			}
			if ev == nil {
				continue
			}
			ds.Handle(ev)
		}
		ds.Finish()
		if answer := o.finalizeAnswer(ctx, plan, nodeOutputs, chatID); answer != "" {
			persistCtx := context.WithoutCancel(ctx)
			if resp, gerr := o.sessions.Get(persistCtx, &session.GetRequest{AppName: AppName, UserID: userID, SessionID: chatID}); gerr == nil && resp != nil {
				aev := session.NewEvent(persistCtx, "")
				aev.Author = orchestratorName
				aev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: answer}}}
				_ = o.sessions.AppendEvent(persistCtx, resp.Session, aev)
			}
		}
	}
}

// BuildBoundPlan builds a Plan from a workflow-catalog-bound node list (a
// dispatch naming a shaped workflow) - no plan judge, no review-fanout heuristic, and critically no orchestrator LLM turn: callers pass the result straight to RunBoundPlan instead of Run. allowedKinds: nil = unrestricted, matching AllowedDeliveryKindsFromContext's sentinel on the planner-LLM path.
func (o *Orchestrator) BuildBoundPlan(ctx context.Context, nodes []dag.RawNode, message string, attachments []*genai.Part, allowedKinds []string) (*dag.Plan, error) {
	return o.planner.BuildBound(ctx, nodes, nil, nil, message, attachments, allowedKinds)
}

// RunBoundPlan runs an already-built bound Plan directly through the graph
// executor - the "no planner LLM call per dispatch" path: no orchestrator
// llmagent turn ever runs. The trust gate is unaffected - RunPlanAsGraph is the exact same executor a model-authored plan runs through, so every node still passes through vetting.RunGatedRefine.
func (o *Orchestrator) RunBoundPlan(ctx context.Context, userID, sessionID, source string, plan dag.Plan) iter.Seq2[stream.SSEEvent, error] {
	// Same turn-boundary clear as Run - a bound plan never calls the plan
	// tool itself, but a stale rejection from an earlier unbound turn on this
	// chat must not leak into this one's terminal status.
	inference.ClearPlanRejection(sessionID)
	return func(yield func(stream.SSEEvent, error) bool) {
		var span oteltrace.Span
		// A prior turn's unconsumed planning failure (empty node/agent key,
		// store.orchestratorGiveUpError's read) must not leak into THIS run's
		// silent gap - RunBoundPlan makes no orchestrator model call to ever
		// naturally clear it (#1109 review finding 3 precedent, #1156).
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
		// Concurrent DAG nodes below all funnel through this one yield (#1016);
		// Run/RetryNode wrap it, RunBoundPlan must too.
		safeYield := newSafeYield(yield)

		o.executor.ResetNodeCancels(sessionID)

		ctx = stream.WithYield(ctx, func(ev stream.SSEEvent) { safeYield(ev, nil) })
		safeYield(tools.DagPlanEvent(ctx, plan), nil)

		// A bound plan skips the llmagent turn entirely, so nothing else ever
		// appends this turn's "user" event - without it, groupSessionEvents
		// (store.GetTurnsWithContent) sees zero events for this ChatTurn row and misaligns every later turn's persisted content against it (#1195).
		o.persistUserMessage(ctx, userID, sessionID, plan.UserMessage)

		// A bound plan never passes through the execute tool (no orchestrator
		// LLM turn exists to revise from), so provisioning failure here has no
		// tool call to fail into - surface the human form directly on the stream.
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
		// Stashed exactly like the execute tool stashes a model-authored plan,
		// so a later HITL resume (LatestPendingQuestion -> stashedPlan) finds
		// it regardless of which path the resuming dispatch takes. Only after RunPlanAsGraph: its own runner is what auto-creates the session - nothing exists to stash into before that.
		o.stashPlanForResume(ctx, userID, sessionID, plan)
		if !paused {
			answer := o.finalizeAnswer(ctx, plan, nodeOutputs, sessionID)
			o.persistAnswer(ctx, userID, sessionID, answer)
		}
		safeYield(stream.Done(), nil)
	}
}

// stashPlanForResume persists plan into session state under the same key the
// execute tool uses (tools.ExecPlanKey), so a bound run that parks on a HITL
// node resumes the same way a model-authored one does.
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

// New builds the orchestrator from its dependencies.
func New(sessions session.Service, m model.LLM, sysPrompt string, planner *dag.Planner, executor *dag.Executor, skillTS tool.Toolset, userMem, taskMem *memory.Store) *Orchestrator {
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

// Run processes message as the orchestrator agent and yields SSE events.
// source: the run's origin for gen_ai.client.token.usage/cost attribution -
// an extension's registration name, or SourceApp for a direct UI/REST/MCP chat.
func (o *Orchestrator) Run(ctx context.Context, userID, sessionID, source, message string, attachments []*genai.Part) iter.Seq2[stream.SSEEvent, error] {
	// Bound to this turn (#1181 review): an earlier turn's rejection must
	// never outlive it - a later silent gap or gateway failure on the same
	// chat needs its OWN evidence, not a stale reason from a turn that already ended.
	inference.ClearPlanRejection(sessionID)
	return func(yield func(stream.SSEEvent, error) bool) {
		var span oteltrace.Span
		// A prior turn's unconsumed planning failure (empty node/agent key,
		// store.orchestratorGiveUpError's read) must not leak into THIS run:
		// if this turn itself never calls the model again before ending in its own empty gap (e.g. a pending-choice reply, or a plan that runs but ends silent), the stale record would still be sitting there (#1109 review finding 3 precedent, #1156).
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
		o.maybeMineUserMemory(ctx, userID, sessionID, source, message)
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
			toolsets = []tool.Toolset{o.skillTS}
		}
		s.toolsets = toolsets
		// Hand-built, unlike a worker node's tools.Build path - see RepeatWrap's doc. One pass over the whole toolList, once every tool this turn offers is assembled.
		// The identical-call loop class RepeatWrap guards against applies to any of them, not just the five DAG tools.
		for i, t := range s.toolList {
			// memory.NewPreload() and similar request-mutating-only tools have
			// no Run for a model to repeat - nothing to guard, leave as-is.
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
		// Concurrent DAG nodes funnel through this one yield (#1016); ctx
		// consumers like onQueued call it from a node goroutine, so it must be
		// the wrapped one - #1021 fixed the other three entrypoints but missed Run().
		s.safeYield = newSafeYield(yield)
		s.ctx = stream.WithYield(s.ctx, func(ev stream.SSEEvent) { s.safeYield(ev, nil) })

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

// planExhaustedNotice: the fixed, plain-language reply for a run whose planning
// never produced an acceptable plan (#693). Never build this from the plan
// judge's own reason - that text is internal machinery talk, not an answer.
const planExhaustedNotice = "I could not produce a workable plan for this request."

// minRejectionsForExhaustion: rejections at or above this count mean the
// model kept retrying and failing (NightsOut#97 saw four) - below it, a
// single rejection followed by an answer is the model correctly pivoting away from a plan it didn't need (#760), not exhaustion.
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

// persistUserMessage appends the user-authored event a bound-plan run would
// otherwise never write (see RunBoundPlan's call site). Mirrors persistAnswer's
// own Get-then-AppendEvent shape.
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
		_ = o.sessions.AppendEvent(persistCtx, resp.Session, aev)
	}
}

// resumeNodeRun delivers a paused node's answer and streams the resumed graph.
func (o *Orchestrator) resumeNodeRun(ctx context.Context, userID, sessionID, message string, pend pendingInterrupt, yield func(stream.SSEEvent, error) bool) {
	o.startNodeRun(ctx, userID, sessionID, message, &pend, pend.nodeID, yield)
}

// StartNode is the "start a paused node" transition: it re-enters the
// stashed plan's graph at the node that paused. A node parked on a question
// (pause_reason awaiting_input, i.e. an unanswered HITL interrupt in the session) takes message as the answer; a node paused by a user or by shutdown needs no message and simply resumes at its last gate boundary.
// A node paused mid-incremental-step (dag.PlanStepSessionID, not the chat
// session - see startIncrementalNodeRun) is checked separately, since that
// resume re-enters a structurally different wrapper than the whole-plan graph.
func (o *Orchestrator) StartNode(ctx context.Context, userID, sessionID, nodeID, message string, yield func(stream.SSEEvent, error) bool) {
	o.executor.StartNode(sessionID, nodeID)
	if p, ok := latestPendingNodeInterrupt(o.PriorEvents(ctx, userID, sessionID)); ok && p.nodeID == nodeID {
		o.startNodeRun(ctx, userID, sessionID, message, &p, nodeID, yield)
		return
	}
	if p, ok := o.pendingStepInterrupt(ctx, userID, sessionID); ok && p.nodeID == nodeID {
		o.startIncrementalNodeRun(ctx, userID, sessionID, message, p, yield)
		return
	}
	o.startNodeRun(ctx, userID, sessionID, message, nil, nodeID, yield)
}

// pendingStepInterrupt checks a plan's own dedicated incremental-step
// session for a paused node - RunPlanStep runs there, not on the chat
// session (see dag.PlanStepSessionID's doc: a nested runner.Run on the live
// chat session would risk corrupting its event/branch bookkeeping).
func (o *Orchestrator) pendingStepInterrupt(ctx context.Context, userID, sessionID string) (pendingInterrupt, bool) {
	return latestPendingNodeInterrupt(o.PriorEvents(ctx, userID, dag.PlanStepSessionID(sessionID)))
}

// startIncrementalNodeRun answers a node paused mid-incremental-step
// (dag.Executor.ResumePlanStep) and persists its result onto the dag_plan
// record exactly like execute() does for a freshly-run assignment, so a
// later execute/list_nodes call sees it as done. Unlike startNodeRun (which
// always finalizes once the whole graph stops pausing), finishing here only
// means THIS assignment is done - the plan may still be partial, so the
// answer is only finalized when the plan's own declared delivery covers it.
func (o *Orchestrator) startIncrementalNodeRun(ctx context.Context, userID, sessionID, message string, pend pendingInterrupt, yield func(stream.SSEEvent, error) bool) {
	plan, rec, recordSvc, errMsg := o.loadResumePlan(ctx, userID, sessionID)
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
			// Shares execute.go's own success/failure decision (paused=false:
			// the step already confirmed it isn't) - a resumed node with
			// empty output is exactly as "failed" as a freshly-run one, and
			// must not silently finalize on it (#slice3 review).
			if tools.ApplyAssignmentOutcome(&rec.Assignments[i], out, false) == "failed" {
				anyFailed = true
			}
		}
	}
	if _, _, serr := dag.SaveDagPlanRecord(ctx, recordSvc, artifactref.AppName, userID, sessionID, "", rec); serr != nil {
		slog.Warn("resume: dag_plan update failed", "component", "orchestrator", "err", serr)
	}

	// B's completion may have unblocked dependents (B -> C, C terminal) that
	// the paused-node-only resume dispatch above never ran - without this,
	// finalizing on rec.Assignments right here would deliver a terminal
	// node's still-empty result (#slice3 review: no delivery ever fires).
	turnEnded, moreFailed := o.driveUnblocked(ctx, plan, rec, recordSvc, userID, sessionID, safeYield)
	anyFailed = anyFailed || moreFailed
	if turnEnded {
		yield(stream.Done(), nil)
		return
	}

	if !anyFailed && rec.Delivery != nil {
		final := map[string]string{}
		for _, a := range rec.Assignments {
			final[a.NodeID] = a.Result
		}
		o.persistAnswer(ctx, userID, sessionID, o.finalizeAnswer(ctx, plan, final, sessionID))
	}
	yield(stream.Done(), nil)
}

// loadResumePlan: the plan and its record for an incremental resume.
// Returns the message to yield on failure, "" on success.
func (o *Orchestrator) loadResumePlan(ctx context.Context, userID, sessionID string) (plan dag.Plan, rec dag.DagPlanRecord, recordSvc artifact.Service, errMsg string) {
	plan, ok := o.stashedPlan(ctx, userID, sessionID)
	if !ok {
		return plan, rec, recordSvc, "resume: no plan in session to resume"
	}
	recordSvc = o.artifacts
	if recordSvc == nil {
		recordSvc = artifact.InMemoryService()
	}
	rec, _, ok, err := dag.LoadDagPlanRecord(ctx, recordSvc, artifactref.AppName, userID, sessionID)
	if err != nil || !ok {
		return plan, rec, recordSvc, "resume: no plan record to resume"
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
		roundOutputs, roundNeedsInput, roundStarted, rerr := o.executor.RunPlanStep(ctx, plan, AppName, userID, sessionID, seededFrom(rec), next)
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
			if tools.ApplyAssignmentOutcome(&rec.Assignments[i], roundOutputs[nid], roundNeedsInput[nid]) == "failed" {
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

// unblockedByDeps returns every not-yet-run assignment (TaskID == "") whose
// dependencies (if any) have already run (TaskID set, success or failure -
// "ran" is what unblocks a dependent, same convention partitionAssignments
// and execute.go's own dispatch already use). Not scoped to the resumed
// node's own chain: a dependency-free assignment an unrelated edit_plan call
// added is just as "ready" and comes back here too - same as a fresh
// execute() step would dispatch it.
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

func (o *Orchestrator) startNodeRun(ctx context.Context, userID, sessionID, message string, pend *pendingInterrupt, nodeID string, yield func(stream.SSEEvent, error) bool) {
	// Single choke point for both StartNode (fresh dispatch, bare ctx, needs a
	// real span) and Run's resumeNodeRun (already inside Run's "run" span) -
	// skip opening a redundant child so resumed-node traces don't show run-under-run.
	var span oteltrace.Span
	if !oteltrace.SpanFromContext(ctx).SpanContext().IsValid() {
		ctx, span = otelobs.Start(ctx, "run", attribute.String(otelobs.ChatIDKey, sessionID))
	}
	defer func() {
		if span != nil {
			otelobs.End(span, nil)
		}
	}()
	plan, ok := o.stashedPlan(ctx, userID, sessionID)
	if !ok {
		yield(stream.Errorf("resume: no plan in session to resume"), nil)
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
		answer := o.finalizeAnswer(ctx, plan, nodeOutputs, sessionID)
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

// pendingChoice returns// pendingChoice returns the call ID and question of the most recent unanswered get_user_choice.
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

// pendingSessionEvents merges a chat's events with its plan-step session's
// (dag.PlanStepSessionID) - execute()'s incremental dispatch runs nodes there, not on the chat session.
// The timestamp merge is safe because the two sessions never carry an open interrupt at once:
// Run short-circuits into resume before its tool loop could open the other.
func (o *Orchestrator) pendingSessionEvents(ctx context.Context, userID, sessionID string) []*session.Event {
	events := append(o.PriorEvents(ctx, userID, sessionID), o.PriorEvents(ctx, userID, dag.PlanStepSessionID(sessionID))...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Timestamp.Before(events[j].Timestamp) })
	return events
}

// LatestPendingQuestion exposes the package function's scan merged across the chat and plan-step sessions.
func (o *Orchestrator) LatestPendingQuestion(ctx context.Context, userID, sessionID string) (PendingQuestion, bool) {
	return LatestPendingQuestion(o.pendingSessionEvents(ctx, userID, sessionID))
}

// PendingQuestion is LatestPendingQuestion over a session's prior events, exposed so callers
// outside this package (e.g. the GitHub extension stamping a run's terminal status, #738)
// don't need to reimplement the scan.
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
		if ev == nil || ev.Content == nil || ev.Author != orchestratorName {
			continue
		}
		var sb strings.Builder
		for _, p := range ev.Content.Parts {
			if p != nil && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil {
				sb.WriteString(p.Text)
			}
		}
		if t := strings.TrimSpace(sb.String()); t != "" {
			latest = t
		}
	}
	return latest
}

type AgentClients = map[string]adkagent.Agent
