package vetting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

// NewWorkerNode: wraps worker agent for use inside gated refine loop.
func NewWorkerNode(worker adkagent.Agent) (workflow.Node, error) {
	n, err := workflow.NewAgentNode(worker, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("vetting: build worker node: %w", err)
	}
	return n, nil
}

// GateResult: trust-gate outcome for node_done and continue-but-warn.
type GateResult struct {
	Passed   bool
	Score    float64
	Feedback string
	Rounds   int
	// ChecksSkipReason: raw skipChecks reason ("" if checks ran, or ran but
	// were never computed - e.g. no judge round). Filtered/worded for
	// display by checksSkipNote before it reaches the delivered artifact.
	ChecksSkipReason string
}

var ErrNodeEmpty = errors.New("vetting: node produced no answer")

var ErrNodePaused = errors.New("vetting: node paused")

// judgeStatusUnavailable/judgeStatusNoVerdict: agent_complete Status values for
// a judge round that ended without a verdict - "" means scored normally.
const (
	judgeStatusUnavailable = "unavailable"
	judgeStatusNoVerdict   = "no_verdict"
)

// judgeFailureFeedback distinguishes a judge that never got to run (transport/model
// outage) from one that ran and simply never committed a verdict (ErrJudgeNoVerdict) -
// judge.go is the only place that knows which happened, so it returns a typed sentinel rather than this checking the error string (#779).
func judgeFailureFeedback(jerr error) (status, feedback string) {
	if errors.Is(jerr, ErrJudgeNoVerdict) {
		return judgeStatusNoVerdict, "quack's judge ran but exhausted its iteration budget without reaching a verdict, so this answer could not be scored: " + jerr.Error()
	}
	return judgeStatusUnavailable, "quack's judge was unavailable, so this answer could not be scored: " + jerr.Error()
}

type NodeControl interface {
	Cancelled() bool
	Paused() bool
	TakeQueued() string
	// PauseForInput parks the node on a worker question, persisting it
	// before the pause is acted on (dag.PauseAwaitingInput).
	PauseForInput(question string)
	// MarkDelivered records that commitDelivery ran for this node, so
	// dagStream can report NodeDone even if a pause/cancel flag races in
	// right after (the delivered==true call site below).
	MarkDelivered()
	// RepeatFailure reports (and clears) a hard-stop message the repeat
	// guard left via dag.Executor.RepeatGuardTripped, so the round's error
	// is reported as a real failure, not the user-cancel path's silent
	// empty continue-but-warn.
	RepeatFailure() (string, bool)
}

const AskToolName = "ask_user"

const memoryCommitTimeout = 3 * time.Minute

// envScaffoldRe strips a leading <env>...</env> preamble an ACP agent echoes into its answer.
var envScaffoldRe = regexp.MustCompile(`(?s)^\s*<env>.*?</env>\s*`)

// stripLeadingEnvScaffold drops a leading <env> block so an answer that is
// nothing but environment preamble reads as empty, not as real content (#709).
func stripLeadingEnvScaffold(answer string) string {
	return envScaffoldRe.ReplaceAllString(answer, "")
}

// hitlInterruptID: (invocation, node, round) is unique, so this is collision-free.
func hitlInterruptID(nodeID string, round int) string {
	return fmt.Sprintf("hitl-%s-r%d", nodeID, round)
}

// hitlTurn: one ask/answer exchange. answer is "" until the pause resolves.
type hitlTurn struct {
	question string
	answer   string
}

type hitlScan struct {
	turns  []hitlTurn
	pauses int
}

func scanNodeAsks(sess session.Session, invocationID, nodeID string) hitlScan {
	var s hitlScan
	if sess == nil {
		return s
	}
	prefix := "hitl-" + nodeID + "-r"
	answers := map[string]string{}
	for ev := range sess.Events().All() {
		if ev == nil || ev.Content == nil || ev.InvocationID != invocationID {
			continue
		}
		if ev.Author == "user" {
			for _, p := range ev.Content.Parts {
				if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != workflow.WorkflowInputFunctionCallName {
					continue
				}
				if !strings.HasPrefix(p.FunctionResponse.ID, prefix) {
					continue
				}
				if payload, ok := p.FunctionResponse.Response["payload"].(string); ok {
					answers[p.FunctionResponse.ID] = payload
				}
			}
			continue
		}
		if !pathHasNode(ev, nodeID) {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil || p.FunctionCall == nil {
				continue
			}
			switch p.FunctionCall.Name {
			case AskToolName:
				q := ""
				if qq, ok := p.FunctionCall.Args["question"].(string); ok {
					q = strings.TrimSpace(qq)
				}
				s.turns = append(s.turns, hitlTurn{question: q})
			case workflow.WorkflowInputFunctionCallName:
				if strings.HasPrefix(p.FunctionCall.ID, prefix) {
					s.pauses++
				}
			}
		}
	}
	for i := range s.turns {
		s.turns[i].answer = answers[hitlInterruptID(nodeID, i+1)]
	}
	return s
}

// pathHasNode: is event under graph node? (NodeInfo.Path: "name@run").
func pathHasNode(ev *session.Event, nodeID string) bool {
	if ev.NodeInfo == nil {
		return false
	}
	for _, seg := range strings.Split(ev.NodeInfo.Path, "/") {
		if i := strings.IndexByte(seg, '@'); i >= 0 {
			seg = seg[:i]
		}
		if seg == nodeID {
			return true
		}
	}
	return false
}

// withUserAnswer: folds Q&A transcript into prompt.
func withUserAnswer(prompt string, turns []hitlTurn) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n--- You previously asked the user question(s) and they answered ---\n")
	for _, t := range turns {
		if t.answer == "" {
			continue // not yet resolved; shouldn't happen for a round we're folding in
		}
		b.WriteString("Q: " + t.question + "\nA: " + t.answer + "\n")
	}
	b.WriteString("\nUse these answers and complete the task now. Do not ask again unless something new and genuinely blocking comes up.")
	return b.String()
}

// replyString: coerces HITL payload to text.
func replyString(reply any) string {
	if s, ok := reply.(string); ok {
		return s
	}
	if reply == nil {
		return ""
	}
	return fmt.Sprintf("%v", reply)
}

// appendNodeEvent is the WAL's node.* observational path (#1090 §4.9): a
// best-effort AppendIntent call, Warn-logged and otherwise ignored - it must
// never affect the run, unlike an artifact.revision save (including a judge_round one), which is fail-closed. No-op when cfg.Ledger is unset.
func appendNodeEvent(ctx context.Context, cfg Config, nodeID, turnID, kind string, rounds int) {
	if cfg.Ledger == nil {
		return
	}
	payload, err := json.Marshal(struct {
		NodeID      string `json:"node_id"`
		Turn        string `json:"turn"`
		Round       int    `json:"round"`
		ResumedFrom string `json:"resumed_from,omitempty"`
	}{NodeID: nodeID, Turn: turnID, Round: rounds, ResumedFrom: cfg.ResumedFrom})
	if err != nil {
		return
	}
	if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
		ChatID: cfg.ChatID, TurnID: turnID, NodeID: nodeID, Kind: kind, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		slog.Warn("ledger node event append failed (observational; run unaffected)", "component", "vetting", "node", nodeID, "kind", kind, "err", err)
	}
}

// deliveryTarget resolves the recordstore artifact backing this node's delivery, if any: the code_review subject for a reviewer, or cfg.Artifact
// for a document node. false when this delivery has no backing artifact
// (a plain PR-only delivery) - the WAL/delivery_record path is skipped entirely in that case (#1093: idempotency key = artifact id + revision, nothing to key on without one).
func deliveryTarget(ctx context.Context, cfg Config) (id string, revision int, ok bool) {
	c := recordClient(cfg)
	if c == nil {
		return "", 0, false
	}
	var targetID string
	var err error
	switch {
	case cfg.IsReviewer:
		targetID, err = recordstore.IdentityFor(kindCodeReview, nil, SubjectHint(cfg.ChatID))
	case cfg.Artifact != "":
		targetID, err = recordstore.IdentityFor(cfg.Artifact, nil, documentHint(cfg.ChatID))
	default:
		return "", 0, false
	}
	if err != nil {
		return "", 0, false
	}
	_, rev, exists, lerr := c.Latest(ctx, targetID)
	if lerr != nil || !exists {
		return "", 0, false
	}
	return targetID, rev, true
}

// deliveryIdempotencyKey: target artifact id + revision (#1090 V4 §4.9) -
// unambiguous since "@" never appears in an artifact id (ids use ":").
func deliveryIdempotencyKey(targetID string, revision int) string {
	return targetID + "@" + strconv.Itoa(revision)
}

// appendDeliveryIntent is the WAL's delivery.intent entry (#1090 §4.9,
// fail-closed): appended right before the gate pushes/hands staged items to
// the extension. A non-nil error means the caller must not deliver at all.
func appendDeliveryIntent(ctx context.Context, cfg Config, nodeID, key, targetID string, revision int, cloneURL string, issueNumber int) error {
	if cfg.Ledger == nil {
		return nil
	}
	// CloneURL/IssueNumber (#1093 finding 4): the minimal DeliveryContext
	// fields `quack ledger recover` needs to rebuild one offline, since it
	// has no live worker activity to derive them from after a crash.
	payload, err := json.Marshal(struct {
		TargetID    string `json:"target_id"`
		Revision    int    `json:"revision"`
		Key         string `json:"idempotency_key"`
		CloneURL    string `json:"clone_url,omitempty"`
		IssueNumber int    `json:"issue_number,omitempty"`
	}{TargetID: targetID, Revision: revision, Key: key, CloneURL: cloneURL, IssueNumber: issueNumber})
	if err != nil {
		return fmt.Errorf("vetting: marshal delivery.intent payload: %w", err)
	}
	if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
		ChatID: cfg.ChatID, NodeID: nodeID, Kind: ledger.KindDeliveryIntent, Key: key, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		return fmt.Errorf("vetting: delivery.intent WAL append for node %s: %w", nodeID, err)
	}
	return nil
}

// gateRun: the mutable state of one gated worker run - setup, the draft and
// continuation stages, the judge loop, and the delivery tail.
type gateRun struct {
	ctx              adkagent.Context
	nodeCtx          context.Context
	workerNode       workflow.Node
	workerModel      model.LLM
	judge            JudgeFactory
	cfg              Config
	prompt           string
	basePrompt       string
	attachments      []*genai.Part
	ctrl             NodeControl
	emit             func(*session.Event) error
	nodeID           string
	log              *slog.Logger
	turnID           string
	markerLine       string
	advisorToken     string
	nodeDir          string
	receivedMemories []memory.Delivered
	sink             func(stream.SSEEvent)
	promptEmit       func(*session.Event) error
	activity         func() workerActivity
	actFor           func(string) workerActivity
	cancelled        func() bool
	paused           func() bool
	repeatFailed     func() (error, bool)
	queueAttempt     int
	delivered        bool
}

// newGateRun: everything RunGatedRefine sets up before the gate loop - the
// node span, advisor marker, memory recall, preloads, and the activity closures.
func newGateRun(ctx adkagent.Context, nodeID string, workerNode workflow.Node, workerModel model.LLM, judge JudgeFactory, cfg Config, prompt string, attachments []*genai.Part, ctrl NodeControl, emit func(*session.Event) error) (*gateRun, oteltrace.Span) {
	g := &gateRun{ctx: ctx, nodeID: nodeID, workerNode: workerNode, workerModel: workerModel, judge: judge, cfg: cfg, prompt: prompt, basePrompt: prompt, attachments: attachments, ctrl: ctrl, emit: emit, log: slog.With("component", "vetting", "node", nodeID)}
	// cfg.NodeID (workspaceNodeID), NOT nodeID: the recorder keys every
	// generate() call on it, and a stale failure record must not leak.
	inference.ClearFailure(cfg.ChatID, cfg.NodeID, cfg.Agent)
	nodeCtx, span := otelobs.StartNode(ctx,
		attribute.String(otelobs.ChatIDKey, cfg.ChatID),
		attribute.String("node_id", nodeID),
		attribute.String(otelobs.GenAIAgentName, cfg.Agent),
		attribute.String(otelobs.QuackModel, modelName(workerModel)),
	)
	g.nodeCtx = nodeCtx
	// turnID: closest stand-in for the store row's turn_id (#1090 V4.2) - no
	// chat-turn id is plumbed this deep; the ADK invocation id is per-run.
	g.turnID = ctx.InvocationID()
	appendNodeEvent(nodeCtx, cfg, nodeID, g.turnID, ledger.KindNodeStarted, 0)
	// Re-attach advisor-thread marker for tool-bearing rounds.
	if token, ok := ParseAdvisorThread(prompt); ok {
		g.markerLine = "\n\n" + AdvisorThreadMarker(token)
		g.advisorToken = token
	}
	// cfg is a per-call copy; stamping only reaches this node's judge rounds.
	cfg.AdvisorToken = g.advisorToken
	cfg.NodeBaseSHA = cloneHeadSHA(cfg)
	if g.advisorToken != "" {
		// Draft round: seed round=1 coords before the first worker call so a
		// tool write during draft (before any judge round) gets real lineage (#1091 finding #4).
		SetAdvisorThreadRound(g.advisorToken, 1, g.turnID, cfg.NodeBaseSHA, "")
		if cfg.RoundCoordsSink != nil {
			cfg.RoundCoordsSink(1, g.turnID, cfg.NodeBaseSHA, "")
		}
	}
	// User attribution: the ADK session identity (mirrors MemoryScope below) -
	// not caller-set, so a node can never claim to run as someone it isn't.
	if s := ctx.Session(); s != nil {
		cfg.User = s.UserID()
	}
	g.cfg = cfg
	g.recallWorkerMemory()
	g.applyPreloads()
	// basePrompt must reflect the prefill recall/preloads: a queued-message
	// re-run rebuilds from it, and they are not re-recalled (#1404 review).
	g.basePrompt = g.prompt
	// Per-node workspace dir prevents concurrent node collision.
	nodeDir := workspace.NodeDir(cfg.NodeID)
	if cfg.Workspace != nil && nodeDir != "" {
		if _, err := cfg.Workspace.EnsureDir(cfg.WorkspaceUserID, cfg.ChatID, nodeDir); err != nil {
			g.log.Warn("could not create the node's working directory", "dir", nodeDir, "err", err)
		}
	}
	g.nodeDir = nodeDir
	// Replay-ledger coords for gate's disk probes.
	probeCtx := ledger.WithCoords(ctx, ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, Round: probeRound, User: cfg.User, Source: cfg.Source})
	g.activity = func() workerActivity {
		act := activityFromSessionAt(ctx.Session(), nodeDir)
		augmentFromRepo(probeCtx, &act, cfg)
		return act
	}
	// actFor folds in the staged review (tool-staged first, then answer-tail fallback).
	g.actFor = func(answer string) workerActivity {
		act := g.activity()
		augmentFromReviewStage(&act, g.advisorToken)
		augmentFromAnswer(&act, cfg, answer)
		augmentFromPRStage(&act, g.advisorToken)
		return act
	}
	g.cancelled = func() bool { return ctrl != nil && ctrl.Cancelled() }
	g.paused = func() bool { return ctrl != nil && ctrl.Paused() }
	// repeatFailed checks the repeat guard's hard-stop note before the generic
	// cancelled() check - a repeat-guard abort must surface as a real failure.
	g.repeatFailed = func() (error, bool) {
		if ctrl == nil {
			return nil, false
		}
		if msg, ok := ctrl.RepeatFailure(); ok {
			return errors.New(msg), true
		}
		return nil, false
	}
	// Judge SSE stage:judge (never written to session).
	g.sink, _ = stream.YieldFromContext(ctx)
	// Session events only for A2A workers.
	g.promptEmit = emit
	if !cfg.DeliverPromptEvent {
		g.promptEmit = nil
	}
	return g, span
}

// recallWorkerMemory: ACP memory recall, appended after the prompt so sibling
// nodes' shared BACKGROUND prefix (dag.buildTask) stays a cache hit.
func (g *gateRun) recallWorkerMemory() {
	if !g.cfg.ExternalWorker || !g.cfg.CommitMemory {
		return
	}
	_, recallSpan := otelobs.Start(g.nodeCtx, "memory.recall",
		attribute.String(otelobs.ChatIDKey, g.cfg.ChatID), attribute.String("node_id", g.nodeID))
	rec, hits := g.cfg.Memory.RecallWithHits(g.ctx, MemoryScope(g.ctx, g.cfg), g.cfg.Task)
	recallSpan.SetAttributes(attribute.Bool("hit", rec != ""))
	recallSpan.End()
	otelobs.RecordMemoryRecall(rec != "")
	if rec == "" {
		return
	}
	g.prompt = g.prompt + "\n\n" + rec
	g.log.Info("recalled memory injected into the worker prompt", "bytes", len(rec))
	g.receivedMemories = hits
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	g.cfg.Memory.RecordRecall(g.nodeCtx, ids)
	recallLedgerEntry(g.nodeCtx, g.cfg, g.nodeID, 0, "prefill", hits)
}

// applyPreloads: episodic record preload (#1006) - review for reviewer nodes,
// body for reMarkable-style stage nodes (outside a clone, no git filter).
func (g *gateRun) applyPreloads() {
	if p := BuildReviewPreload(g.nodeCtx, g.cfg, g.nodeID); p != "" {
		g.prompt = g.prompt + p
	}
	if p := BuildBodyPreload(g.nodeCtx, g.cfg, g.nodeID); p != "" {
		g.prompt = g.prompt + p
	}
}

// gateExit: a staged stop - RunGatedRefine returns these values verbatim.
type gateExit struct {
	answer string
	res    GateResult
	err    error
}

// runWorkerOnce: one traced worker run with the shared error tri-state - a
// repeat-guard abort is a hard failure; a mid-flight CancelNode is not.
func (g *gateRun) runWorkerOnce(input any, runID, stage, termMsg, failMsg string, extra []any) (string, *gateExit) {
	answer, err := runWorkerNodeTraced(g.ctx, g.nodeCtx, g.cfg, g.workerModel, g.workerNode, input, runID, stage, g.promptEmit)
	if err == nil {
		return answer, nil
	}
	if lerr, ok := g.repeatFailed(); ok {
		// No run attr here: the original repeat-guard logs never carried one.
		g.log.Error(termMsg, "err", lerr)
		return "", &gateExit{"", GateResult{}, lerr}
	}
	if g.cancelled() {
		return "", &gateExit{"", GateResult{}, nil} // round aborted mid-flight by CancelNode, not a real failure
	}
	// Log before returning (ADK swallows node errors into silent empty completion).
	g.log.Error(failMsg, append(append([]any{}, extra...), "err", err)...)
	return "", &gateExit{"", GateResult{}, err}
}

// fillHitlReply: record the resumed reply on the last ask turn while its
// answer is still empty (ResumedInput is the authority, this is the fold).
func fillHitlReply(turns []hitlTurn, reply any) {
	if n := len(turns); n > 0 && turns[n-1].answer == "" {
		turns[n-1].answer = replyString(reply)
	}
}

// fillConfirmReply: the confirm-turn twin of fillHitlReply.
func fillConfirmReply(turns []confirmTurn, reply any) {
	if n := len(turns); n > 0 && turns[n-1].answer == "" {
		turns[n-1].answer = replyString(reply)
	}
}

// confirmResume: the guard-confirm twin of the HITL block above.
func (g *gateRun) confirmResume(sfx string) (string, *gateExit, bool) {
	cscan := scanNodeConfirms(g.ctx.Session(), g.ctx.InvocationID(), g.nodeID)
	if cscan.pauses == 0 {
		return "", nil, false
	}
	if reply, ok := g.ctx.ResumedInput(confirmInterruptID(g.nodeID, cscan.pauses)); ok {
		turns := cscan.turns
		fillConfirmReply(turns, reply)
		a, exit := g.resumeRun("confirm", fmt.Sprintf("worker-confirm-r%d%s", cscan.pauses, sfx), "node resumed with confirm decision", "post-decision worker run terminated: repeat guard", "post-decision worker run failed", cscan.pauses, workerInput(withConfirmDecision(g.prompt, turns), g.attachments))
		if exit != nil {
			return "", exit, false
		}
		return a, nil, true
	}
	return "", nil, false
}

// draftOrResume: HITL answer / guard-confirm resume re-runs the worker with
// the recorded Q&A, else a fresh draft; the shared park check always runs.
func (g *gateRun) draftOrResume(sfx string) (string, *gateExit) {
	// All three paths converge on the shared post-worker park check below:
	// a resume can itself raise a new guard confirm (DIFFERS) that must park.
	answer := ""
	ran := false
	if scan := scanNodeAsks(g.ctx.Session(), g.ctx.InvocationID(), g.nodeID); scan.pauses > 0 {
		if reply, ok := g.ctx.ResumedInput(hitlInterruptID(g.nodeID, scan.pauses)); ok {
			turns := scan.turns
			fillHitlReply(turns, reply)
			a, exit := g.resumeRun("hitl", fmt.Sprintf("worker-hitl-r%d%s", scan.pauses, sfx), "node resumed with user answer", "post-answer worker run terminated: repeat guard", "post-answer worker run failed", scan.pauses, workerInput(withUserAnswer(g.prompt, turns), g.attachments))
			if exit != nil {
				return "", exit
			}
			answer, ran = a, true
		}
	}
	if !ran {
		if a, exit, did := g.confirmResume(sfx); did {
			answer, ran = a, true
		} else if exit != nil {
			return "", exit
		}
	}
	if !ran {
		a, exit := g.runWorkerOnce(workerInput(g.prompt, g.attachments), "worker-r0"+sfx, "draft", "worker draft terminated: repeat guard", "worker draft failed", []any{"run", "worker-r0"})
		if exit != nil {
			return "", exit
		}
		answer = a
	}
	// HITL/guard pause: park when ask_user or guard confirmation raised. Draft discarded; resume re-runs with Q&A.
	if paused, ierr := pauseIfWorkerRaisedHITL(g.ctx, g.nodeID, g.ctrl, g.emit, g.log); paused {
		return "", &gateExit{"", GateResult{}, ierr} // ErrNodePaused (wrapping ADK's park sentinel)
	}
	return answer, nil
}

// resumeRun: one HITL/confirm resume run - log the resumed round, then the
// shared worker run (a resume can still raise a new guard confirm).
func (g *gateRun) resumeRun(mode, runID, logMsg, termMsg, failMsg string, rounds int, content any) (string, *gateExit) {
	g.log.Info(logMsg, "round", rounds)
	return g.runWorkerOnce(content, runID, mode, termMsg, failMsg, nil)
}

// continueWorker: tool-bearing continuation rounds while workIncomplete says
// the task isn't done; parks when a continuation proposes a guarded delivery.
func (g *gateRun) continueWorker(sfx, answer string) (string, *gateExit) {
	hasDeliverTarget := g.cfg.Deliver != nil
	if !workIncomplete(answer, g.cfg.Task, g.actFor(answer), g.cfg.ReadOnly, hasDeliverTarget, g.cfg.IsReviewer, g.cfg.ExistingPR) {
		return answer, nil
	}
	_, contSpan := otelobs.Start(g.nodeCtx, "gate.continuation",
		attribute.String(otelobs.ChatIDKey, g.cfg.ChatID), attribute.String("node_id", g.nodeID))
	contAttempts := 0
	for attempt := 1; attempt <= maxContinueRounds && workIncomplete(answer, g.cfg.Task, g.actFor(answer), g.cfg.ReadOnly, hasDeliverTarget, g.cfg.IsReviewer, g.cfg.ExistingPR); attempt++ {
		contAttempts = attempt
		act := g.actFor(answer)
		g.log.Warn("work not finished; continuing the worker with its tools",
			"attempt", attempt, "empty", strings.TrimSpace(answer) == "", "committed", act.committed, "pushed", act.pushed)
		var err error
		answer, err = runWorkerNodeTraced(g.ctx, g.nodeCtx, g.cfg, g.workerModel, g.workerNode, buildContinuationPrompt(g.cfg.Task, act, g.cfg.Checks, g.cfg.ReadOnly, hasDeliverTarget, g.cfg.IsReviewer, g.cfg.ExistingPR)+g.markerLine,
			fmt.Sprintf("worker-cont%d%s", attempt, sfx), "continuation", g.promptEmit)
		if err != nil {
			if lerr, ok := g.repeatFailed(); ok {
				g.log.Error("worker continuation terminated: repeat guard", "attempt", attempt, "err", lerr)
				contSpan.End()
				return "", &gateExit{"", GateResult{}, lerr}
			}
			if g.cancelled() {
				contSpan.End()
				return "", &gateExit{"", GateResult{}, nil} // round aborted mid-flight by CancelNode, not a real failure
			}
			g.log.Error("worker continuation failed", "attempt", attempt, "err", err)
			contSpan.End()
			return "", &gateExit{"", GateResult{}, err}
		}
		// A continuation is where the worker finally proposes its guarded
		// delivery step (git_commit/git_push) - park for the human as elsewhere.
		if paused, ierr := pauseIfWorkerRaisedHITL(g.ctx, g.nodeID, g.ctrl, g.emit, g.log); paused {
			contSpan.End()
			return "", &gateExit{"", GateResult{}, ierr} // ErrNodePaused (wrapping ADK's park sentinel)
		}
	}
	contSpan.SetAttributes(attribute.Int("attempts", contAttempts))
	contSpan.End()
	return answer, nil
}

// writerRecovery: last-resort tool-less writer when the worker came up empty
// after the continuation budget.
func (g *gateRun) writerRecovery(question *genai.Content, answer string) (string, error) {
	if strings.TrimSpace(answer) != "" {
		return answer, nil
	}
	g.log.Warn("worker still empty after continuation; falling back to the tool-less writer", "rounds", maxContinueRounds)
	answer, err := runWriterFresh(g.ctx, g.workerModel, buildFinalizeContent(question, g.activity()), g.cfg.ChatID)
	if err != nil {
		g.log.Error("writer recovery failed", "err", err)
		return "", err
	}
	if strings.TrimSpace(stripLeadingEnvScaffold(answer)) == "" {
		g.log.Error("worker produced NO answer; writer recovery also empty", "rounds", maxContinueRounds)
		return "", ErrNodeEmpty
	}
	return answer, nil
}

// Gate-loop boundary actions from boundaryCheck.
const (
	boundaryProceed = iota
	boundaryStopped
	boundaryPaused
	boundaryQueued
)

// boundaryCheck: turn-boundary control - a paused/cancelled/queued node must
// be honored even when no judge round runs at all (JudgeRounds == 0).
func (g *gateRun) boundaryCheck() (int, string) {
	if g.ctrl == nil {
		return boundaryProceed, ""
	}
	if g.ctrl.Cancelled() {
		return boundaryStopped, ""
	}
	if g.ctrl.Paused() {
		return boundaryPaused, ""
	}
	if q := g.ctrl.TakeQueued(); strings.TrimSpace(q) != "" {
		return boundaryQueued, q
	}
	return boundaryProceed, ""
}

// commitFinal: the delivery tail - advisor staged-memory drain, pass-only
// memory commit, the judge-less node's one episodic round, then delivery.
func (g *gateRun) commitFinal(answer string, res GateResult, episodicRoundsWritten int) string {
	act := g.actFor(answer)
	// Fold in ACP memory MCP stage_memory from all rounds; unregister after drain (straggler calls fail).
	if g.advisorToken != "" {
		if t, ok := LookupAdvisorThread(g.advisorToken); ok && t.MemSecret != "" {
			if ms, ok := LookupMemSession(t.MemSecret); ok {
				if ms.Staged != nil {
					act.staged = append(act.staged, ms.Staged.Drain()...)
				}
				UnregisterMemSession(t.MemSecret)
			}
		}
	}
	if res.Passed {
		commitMemoryOnPass(g.ctx, g.nodeCtx, g.cfg, g.nodeID, answer, act.staged)
	}
	// A judge-less node (JudgeRounds == 0, e.g. a deterministic-only reMarkable
	// stage) never entered the round loop - write its one round here (#1090 P2).
	if episodicRoundsWritten == 0 && strings.TrimSpace(stripLeadingEnvScaffold(answer)) != "" {
		saveEpisodicRound(g.nodeCtx, g.cfg, g.nodeID, g.turnID, 1, answer, act.stagedDelivery["review"], nil)
	}
	// Deliver even on judge FAIL (graceful degradation). Memory stays pass-only.
	g.delivered = true
	if g.ctrl != nil {
		// Before commitDelivery: a pause/cancel landing during it must still
		// see delivered==true (dagStream's terminal-event race, #1340 review).
		g.ctrl.MarkDelivered()
	}
	act.answer = answer
	commitDelivery(g.nodeCtx, g.sink, g.cfg, g.nodeID, act, res)
	// commitDelivery already ran on the full answer (memory, episodic record,
	// delivery render); only the chat-visible return value collapses on a restate.
	return dedupeAnswerAgainstStaged(answer, act.stagedDelivery)
}

// finish: the node span close-out - verdict attributes, EndNode, and the
// node.done/node.failed ledger event (node.started at entry used the same id).
func (g *gateRun) finish(span oteltrace.Span, res GateResult, err error) {
	span.SetAttributes(
		attribute.Bool("verdict_passed", res.Passed),
		attribute.Float64(otelobs.GenAIEvaluationScore, res.Score),
		attribute.Int("gate_rounds", res.Rounds),
	)
	otelobs.EndNode(span, err)
	doneKind := ledger.KindNodeDone
	if err != nil {
		doneKind = ledger.KindNodeFailed
	}
	appendNodeEvent(g.nodeCtx, g.cfg, g.nodeID, g.turnID, doneKind, res.Rounds)
}

// resolveAborted: register a never-delivered reviewer as a failed fan-out
// sibling; a delivered node or pause sentinel skips it (it may still resume).
func (g *gateRun) resolveAborted(answer string, err error) {
	if g.delivered || isReviewerPauseSentinel(err) {
		return
	}
	resolveAbortedReviewer(g.nodeCtx, g.sink, g.cfg, g.nodeID, g.actFor(answer))
}

// queueSuffix: the -sN run-id suffix after a queued-message re-run.
func queueSuffix(attempt int) string {
	if attempt > 0 {
		return fmt.Sprintf("-s%d", attempt)
	}
	return ""
}

// RunGatedRefine: the gate - draft (or resume), continuation, the judge/revise
// loop, and delivery of the gated answer.
func RunGatedRefine(ctx adkagent.Context, nodeID string, workerNode workflow.Node, workerModel model.LLM, judge JudgeFactory, cfg Config, prompt string, attachments []*genai.Part, ctrl NodeControl, emit func(*session.Event) error) (answer string, res GateResult, err error) {
	g, span := newGateRun(ctx, nodeID, workerNode, workerModel, judge, cfg, prompt, attachments, ctrl, emit)
	// Must run unconditionally (#942): a staged review lives in this process,
	// not the dying ACP subprocess; declared after the span close-out (LIFO first).
	defer func() { g.finish(span, res, err) }()
	defer func() { g.resolveAborted(answer, err) }()
	for {
		if g.cancelled() {
			return "", GateResult{}, nil // cancelled before drafting → empty (continue-but-warn)
		}
		if g.paused() {
			return "", GateResult{}, ErrNodePaused // paused before drafting → keep whatever this node has (nothing yet)
		}
		// Fresh run ID after queue delivery.
		sfx := queueSuffix(g.queueAttempt)
		question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: g.prompt}}}
		a, exit := g.draftOrResume(sfx)
		if exit != nil {
			return exit.answer, exit.res, exit.err
		}
		answer, exit = g.continueWorker(sfx, a)
		if exit != nil {
			return exit.answer, exit.res, exit.err
		}
		answer, err = g.writerRecovery(question, answer)
		if err != nil {
			return "", GateResult{}, err
		}
		if action, q := g.boundaryCheck(); action != boundaryProceed {
			if action == boundaryPaused {
				return answer, GateResult{}, ErrNodePaused
			}
			if action == boundaryStopped {
				return answer, GateResult{}, nil
			}
			g.log.Info("node has a queued message; re-running with it", "node", g.nodeID)
			g.queueAttempt++
			g.prompt = g.basePrompt + "\n\n--- Queued user message (address this before continuing) ---\n" + q
			continue
		}
		// Judge/revise loop: judge, fold deterministic criteria, revise on fail.
		outcome := runJudgeRounds(g, question, answer, sfx)
		if outcome.err != nil {
			return "", GateResult{}, outcome.err
		}
		if outcome.exit {
			return answer, outcome.res, nil
		}
		if outcome.paused {
			return answer, outcome.res, ErrNodePaused
		}
		if outcome.queuedText != "" {
			g.log.Info("node has a queued message; re-running with it", "node", g.nodeID)
			g.queueAttempt++
			g.prompt = g.basePrompt + "\n\n--- Queued user message (address this before continuing) ---\n" + outcome.queuedText
			continue // re-run the whole gate with the message folded in (fresh run IDs)
		}
		answer = outcome.answer
		res = outcome.res
		res.ChecksSkipReason = outcome.checksSkipReason
		return g.commitFinal(answer, res, outcome.episodicRoundsWritten), res, nil
	}
}

// judgeRounds: the mutable state of one RunGatedRounds invocation - the round
// loop, its verdict/revision helpers, and how the loop closed (outcome).
type judgeRounds struct {
	ctx              adkagent.Context
	nodeCtx          context.Context
	emit             func(*session.Event) error
	cfg              Config
	judge            JudgeFactory
	question         *genai.Content
	answer           string
	markerLine       string
	advisorToken     string
	turnID           string
	sfx              string
	receivedMemories []memory.Delivered
	sink             func(stream.SSEEvent)
	promptEmit       func(*session.Event) error
	workerNode       workflow.Node
	workerModel      model.LLM
	actFor           func(string) workerActivity
	repeatFailed     func() (error, bool)
	ctrl             NodeControl
	nodeID           string
	log              *slog.Logger

	res                   GateResult
	checksSkipReason      string
	episodicState         *episodicRoundState
	episodicRoundsWritten int
	outcome               *judgeRoundOutcome
}

// runJudgeRounds: the judge/revise loop - judge, fold deterministic criteria, revise on fail.
func runJudgeRounds(g *gateRun, question *genai.Content, answer, sfx string) judgeRoundOutcome {
	j := &judgeRounds{ctx: g.ctx, nodeCtx: g.nodeCtx, emit: g.emit, cfg: g.cfg, judge: g.judge, question: question, answer: answer, markerLine: g.markerLine, advisorToken: g.advisorToken, turnID: g.turnID, sfx: sfx, receivedMemories: g.receivedMemories, sink: g.sink, promptEmit: g.promptEmit, workerNode: g.workerNode, workerModel: g.workerModel, actFor: g.actFor, repeatFailed: g.repeatFailed, ctrl: g.ctrl, nodeID: g.nodeID, log: g.log}
	// JudgeRounds counts revisions: round r judges, on fail revises (N rounds = N revisions / N+1 judgments).
	for round := 1; j.judge != nil && j.cfg.JudgeRounds > 0 && round <= j.cfg.JudgeRounds+1; round++ {
		if !j.roundGate(round) {
			break
		}
		runID, judgeCtx, jspan, ledgerCtx, act := j.prepareJudge(round)
		v, det, jerr := j.runJudge(round, runID, judgeCtx, ledgerCtx, act)
		if j.outcome != nil { // admission swap aborted the round; the ctx error rides the outcome
			break
		}
		if jerr != nil {
			j.applyJudgeFailure(round, runID, jspan, jerr)
			break
		}
		stop, env, jr := j.recordRoundVerdict(round, runID, jspan, ledgerCtx, v, det, act)
		if stop {
			break
		}
		proceed, hardErr := j.reviseRound(round, act, v, env, jr)
		if hardErr != nil {
			return judgeRoundOutcome{err: hardErr}
		}
		if !proceed {
			break
		}
	}
	if o := j.outcome; o != nil {
		if o.err != nil {
			return judgeRoundOutcome{answer: j.answer, res: j.res, err: o.err}
		}
		if o.queuedText != "" {
			return judgeRoundOutcome{answer: j.answer, queuedText: o.queuedText}
		}
		if o.paused {
			return judgeRoundOutcome{answer: j.answer, res: j.res, paused: true}
		}
		return judgeRoundOutcome{answer: j.answer, res: j.res, exit: true}
	}
	return judgeRoundOutcome{answer: j.answer, res: j.res, checksSkipReason: j.checksSkipReason, episodicRoundsWritten: j.episodicRoundsWritten}
}

// roundGate: cooperative cancel/pause/queue before the round, the empty-answer
// guard, and this round's advisor/coordinator stamp. false stops the loop.
func (j *judgeRounds) roundGate(round int) bool {
	// Cooperative cancel/pause/queue before each judge round.
	if j.ctrl != nil {
		if j.ctrl.Cancelled() {
			j.outcome = &judgeRoundOutcome{exit: true}
			return false
		}
		if j.ctrl.Paused() {
			j.outcome = &judgeRoundOutcome{paused: true}
			return false
		}
		if q := j.ctrl.TakeQueued(); strings.TrimSpace(q) != "" {
			j.outcome = &judgeRoundOutcome{queuedText: q}
			return false
		}
	}
	if strings.TrimSpace(stripLeadingEnvScaffold(j.answer)) == "" {
		return false // still nothing to judge after recovery
	}
	if j.advisorToken != "" {
		var trigger string
		if j.episodicState != nil {
			trigger = j.episodicState.triggerAnnotation
		}
		// Intentional: this round's revise (on judge fail) also stamps Round=round -
		// a revision belongs to the judgment that required it, not the later reader.
		SetAdvisorThreadRound(j.advisorToken, round, j.turnID, j.cfg.NodeBaseSHA, trigger)
		if j.cfg.RoundCoordsSink != nil {
			j.cfg.RoundCoordsSink(round, j.turnID, j.cfg.NodeBaseSHA, trigger)
		}
	}
	return true
}

// prepareJudge: per-round act/memories scan, the always-written episodic
// revision, the judge span, and the replay-ledger coords.
func (j *judgeRounds) prepareJudge(round int) (runID string, judgeCtx context.Context, jspan *stageSpan, ledgerCtx context.Context, act workerActivity) {
	act = j.actFor(j.answer)
	// recall_memory hits merge in fresh every round, from wherever this round's answer
	// came from - native full-session re-scan, or a live ACP MemSession snapshot (#1255 P2).
	j.receivedMemories = mergeMemoryHits(j.receivedMemories, act.recalled)
	if j.advisorToken != "" {
		if t, ok := LookupAdvisorThread(j.advisorToken); ok && t.MemSecret != "" {
			if ms, ok := LookupMemSession(t.MemSecret); ok && ms.Recalled != nil {
				j.receivedMemories = mergeMemoryHits(j.receivedMemories, ms.Recalled.Snapshot())
			}
		}
	}
	// Every judge round writes a revision, gate-passed or not - only delivery stays
	// gate-passed-only (#1090 P2), and every gated node writes one (#1095).
	j.episodicState = saveEpisodicRound(j.nodeCtx, j.cfg, j.nodeID, j.turnID, round, j.answer, act.stagedDelivery["review"], j.episodicState)
	j.episodicRoundsWritten++
	runID = fmt.Sprintf("judge-r%d", round)
	judgeCtx, jspan = startStageSpan(j.nodeCtx, j.sink, j.cfg, j.nodeID, "judge", stream.StageJudge, runID, round)
	// Replay-ledger coords (via context.WithValue): Node is cfg.NodeID, not nodeID -
	// it must match the worker recorder's own key for setup/repo-chain plans.
	judgeCoords := ledger.Coords{ChatID: j.cfg.ChatID, Node: j.cfg.NodeID, Agent: "judge", BundleHash: j.cfg.BundleHash, Round: runID, User: j.cfg.User, Source: j.cfg.Source}
	ledgerCtx = ledger.WithCoords(j.ctx, judgeCoords)
	// Same belt-and-suspenders as runWorkerNodeTraced's workerModel stamp.
	if cs, ok := j.cfg.JudgeModel.(interface{ SetLedgerCoords(ledger.Coords) }); ok {
		cs.SetLedgerCoords(judgeCoords)
	}
	// Nothing reads a "judge"-role failure record today - clear it on entry so a
	// failed judge round leaves no permanent orphan (#1109).
	inference.ClearFailure(j.cfg.ChatID, j.cfg.NodeID, "judge")
	return runID, judgeCtx, jspan, ledgerCtx, act
}

// runJudge: deterministic criteria up front, then the judge call itself -
// skipped on the terminal round when a criterion already fails by weakest-link.
func (j *judgeRounds) runJudge(round int, runID string, judgeCtx context.Context, ledgerCtx context.Context, act workerActivity) (verdict, map[string]criterionScore, error) {
	// Compute deterministic criteria before judge runs.
	det, skip := computeDeterministicCriteria(judgeCtx, j.answer, act, j.cfg)
	if skip != "" {
		j.checksSkipReason = skip
	}
	// Terminal round only (no revise ever reads its feedback): a deterministic
	// criterion below threshold decides by weakest-link, so skip the judge call.
	detFailedTerminal := false
	if round > j.cfg.JudgeRounds {
		for _, c := range det {
			if c.Score < j.cfg.Threshold {
				detFailedTerminal = true
				break
			}
		}
	}
	if detFailedTerminal {
		j.log.Info("terminal round has a failing deterministic criterion; skipping the judge", "round", round)
		return verdict{}, det, nil
	}
	// Render-check screenshot evidence (#1211): attached only when this node's
	// own rubric scores them; judge-only, never touches the worker's content.
	shots := renderScreenshotEvidence(judgeCtx, j.cfg, j.nodeID, skip == "", act)
	// Judge generates too: hold its own spec for this call so the freed
	// worker slot can't admit a second worker while it runs.
	if j.cfg.ReleaseWorker != nil && j.cfg.AdmitJudge != nil {
		j.cfg.ReleaseWorker()
		if !j.cfg.AdmitJudge(j.ctx) {
			j.outcome = &judgeRoundOutcome{err: j.ctx.Err()}
			return verdict{}, det, nil
		}
	}
	v, jerr := runJudgeAgent(ledgerCtx, j.judge, j.cfg, attachScreenshots(j.question, shots), j.answer, act, det, j.receivedMemories, judgePartEmitter(j.sink, j.nodeID, runID))
	if j.cfg.ReleaseJudge != nil && j.cfg.AdmitWorker != nil {
		j.cfg.ReleaseJudge()
		if !j.cfg.AdmitWorker(j.ctx) {
			j.outcome = &judgeRoundOutcome{err: j.ctx.Err()}
		}
	}
	return v, det, jerr
}

// applyJudgeFailure: judge call failed - answer goes out unvetted, fail-closed
// score, span closed with the error, unavailability metric.
func (j *judgeRounds) applyJudgeFailure(round int, runID string, jspan *stageSpan, jerr error) {
	// Judge failure means answer goes out unvetted - loud ERROR, not Warn.
	j.log.Error("judge failed; surfacing answer unvetted", "round", round, "err", jerr)
	status, feedback := judgeFailureFeedback(jerr)
	jspan.end(stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Status: status, Reason: jerr.Error()}, jerr)
	otelobs.RecordJudgeUnavailable(j.cfg.Agent)
	// Fail closed but fall through to deliver-with-caveat (only that path writes the review verdict marker).
	j.res = GateResult{Score: 0, Passed: false, Feedback: feedback, Rounds: round}
}

// recordRoundVerdict: fold the verdict, record the round, emit its events.
// Returns true when the loop stops (WAL fail-closed, pass, or terminal round).
func (j *judgeRounds) recordRoundVerdict(round int, runID string, jspan *stageSpan, ledgerCtx context.Context, v verdict, det map[string]criterionScore, act workerActivity) (bool, verdictEnvelope, JudgeRoundRecord) {
	if isNonDeliveringSlice(j.cfg) {
		// Fan-out (#1092, design V4 §4.6): a reviewer feeding a synthesizer never owns the
		// delivered verdict, so its VERDICT-consistency score would gate on something it doesn't control.
		v = dropCriteria(v, "structured_verdict")
	}
	v = sanitizeAnchors(v, j.answer, j.cfg)
	v = mergeDeterministic(v, det, j.cfg)
	v = applyRubricSpecs(v, j.cfg.RubricSpecs)
	env, feedback := composeFeedback(v, j.cfg.Threshold, round)
	j.res = GateResult{Passed: env.Passed, Score: v.Score, Feedback: feedback, Rounds: round}
	var scored []ScoredRef
	if j.episodicState != nil {
		scored = j.episodicState.roundWrites
	}
	// judge_round record (#1144 P2): this SaveStructured call IS the WAL entry for
	// this round's verdict (recordstore appends artifact.revision before the row).
	jr := buildJudgeRoundRecord(j.turnID, round, j.res.Passed, j.res.Score, scored, v, det, j.answer)
	jrID, _, saveErr := saveJudgeRoundRecord(j.nodeCtx, j.cfg, j.nodeID, j.turnID, round, jr)
	if saveErr != nil && j.cfg.Ledger != nil {
		// Fail-closed (#1090 §4.9), WAL-scoped only: with no ledger configured this
		// failure stays fail-open (Warned inside saveJudgeRoundRecord).
		j.log.Error("judge_round WAL save failed; stopping the round loop", "round", round, "err", saveErr)
		verdictWord := "failed"
		if env.Passed {
			verdictWord = "passed"
		}
		j.res.Passed = false
		j.res.Feedback = fmt.Sprintf("Round %d %s (score %.2f) but could not be recorded in the write-ahead log; treating as failed.", round, verdictWord, v.Score)
		return true, env, jr
	}
	if j.res.Passed {
		// Memory votes (#1255 P1): applied only on the round that actually passes -
		// a failed round (incl. one flipped by the WAL fail-closed above) records nothing.
		if missingMemoryVotes(memoryIDs(j.receivedMemories), v) {
			// #1259: the in-session nudge already tried once; still incomplete means the judge ignored it.
			j.log.Warn("judge received memories but left some unvoted after the nudge", "round", round, "received", len(j.receivedMemories), "voted", len(v.Memories))
		}
		applyMemoryVotesOnPass(j.nodeCtx, j.cfg, j.nodeID, round, j.receivedMemories, v.Memories)
	}
	for _, sr := range scored {
		emitArtifactRevision(j.sink, sr.ArtifactID, sr.Revision, recordstore.KindOf(sr.ArtifactID), j.nodeID, round)
	}
	if jrID != "" {
		// Next round's writes point back at THIS round's verdict
		// (design V4 §7 case 3's trigger_annotation chain).
		if j.episodicState != nil {
			j.episodicState.triggerAnnotation = jrID
		}
		emitJudgeRound(j.sink, jrID, j.res.Passed, j.res.Score, scored)
	}
	emitEvaluationResults(ledgerCtx, runID, v)
	jspan.end(stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Score: j.res.Score, Passed: j.res.Passed, Feedback: j.res.Feedback, Envelope: env}, nil)
	j.log.Info("judge round done", "round", round, "score", v.Score, "passed", j.res.Passed)
	otelobs.RecordJudgeVerdict(j.cfg.Agent, j.res.Score, j.res.Passed)
	// Debug: per-criterion reasoning for diagnosable gate failures.
	if len(v.Criteria) > 0 && j.log.Enabled(context.Background(), slog.LevelDebug) {
		j.log.Debug("judge verdict detail", "round", round, "criteria", formatCriteriaDetail(v.Criteria), "feedback", strings.TrimSpace(v.Feedback))
	}
	return j.res.Passed || round > j.cfg.JudgeRounds, env, jr
}

// reviseRound: one revise attempt after a failed verdict. false stops the
// loop (outcome or no-op revise); hardErr is repeat-guard or a self-ask pause.
func (j *judgeRounds) reviseRound(round int, act workerActivity, v verdict, env verdictEnvelope, jr JudgeRoundRecord) (bool, error) {
	// Re-check control now that the verdict failed: the judge's own model call
	// can run long, and a cancel landing in it must stop here too, not next round (#879).
	if j.ctrl != nil {
		if j.ctrl.Cancelled() {
			j.outcome = &judgeRoundOutcome{exit: true}
			return false, nil
		}
		if j.ctrl.Paused() {
			j.outcome = &judgeRoundOutcome{paused: true}
			return false, nil
		}
		if q := j.ctrl.TakeQueued(); strings.TrimSpace(q) != "" {
			j.outcome = &judgeRoundOutcome{queuedText: q}
			return false, nil
		}
	}
	// #762: undo this round's commits before another try, only if commit_h hygiene
	// says it swept in off-task work - an ordinary wrong round keeps its commits.
	resetCloneToNodeBase(j.cfg, v)
	revisePrompt := contentPlainText(buildRevisionContent(j.cfg.Constitution, j.question, j.answer, env, act, citationOnlyFailure(v, j.cfg.Threshold), jr.Notes)) + j.markerLine
	reviseRunID := fmt.Sprintf("worker-r%d%s", round, j.sfx)
	// gate.revise spans the round through gate.judge's choke point; sink is nil -
	// the SSE for this run already comes from dagStream off the worker session.
	reviseCtx, rspan := startStageSpan(j.nodeCtx, nil, j.cfg, j.nodeID, "revise", stream.StageRevise, reviseRunID, round)
	revised, rerr := runWorkerNodeTraced(j.ctx, reviseCtx, j.cfg, j.workerModel, j.workerNode, revisePrompt, reviseRunID, "revise", j.promptEmit)
	rspan.end(stream.AgentCompleteData{RunID: reviseRunID, Stage: stream.StageRevise, Round: round}, rerr)
	if rerr != nil {
		if lerr, ok := j.repeatFailed(); ok {
			j.log.Error("revision worker terminated: repeat guard", "round", round, "err", lerr)
			return false, lerr
		}
		j.log.Error("revision worker failed; keeping prior answer", "round", round, "err", rerr)
		j.outcome = &judgeRoundOutcome{exit: true} // revision failed; keep the prior answer
		return false, nil
	}
	// A revision can itself raise ask_user/guard confirmation - park exactly as draft-time check does.
	if paused, ierr := pauseIfWorkerRaisedHITL(j.ctx, j.nodeID, j.ctrl, j.emit, j.log); paused {
		return false, ierr // ErrNodePaused (wrapping ADK's park sentinel)
	}
	if strings.TrimSpace(revised) == "" || revised == j.answer {
		// No-op revise (empty or identical) only skips re-judging if act is
		// unchanged too - a tool call can move act without the text changing.
		if reflect.DeepEqual(act, j.actFor(j.answer)) {
			j.log.Info("revise produced no change; keeping current verdict", "round", round)
			return false, nil
		}
		j.log.Info("revise produced no text change but staged new activity; re-judging unchanged answer against it", "round", round)
		return true, nil
	}
	j.answer = revised
	return true, nil
}

// judgeRoundOutcome: how the judge/revise loop closed. exit maps to
// (answer, res, nil), paused to ErrNodePaused, err to a hard failure.
type judgeRoundOutcome struct {
	answer                string
	res                   GateResult
	exit                  bool
	paused                bool
	queuedText            string
	err                   error
	checksSkipReason      string
	episodicRoundsWritten int
}

// pauseIfWorkerRaisedHITL: parks node on new ask_user/guard confirmation. Runs after every worker run.
func pauseIfWorkerRaisedHITL(ctx adkagent.Context, nodeID string, ctrl NodeControl, emit func(*session.Event) error, log *slog.Logger) (bool, error) {
	if emit == nil {
		return false, nil
	}
	if scan := scanNodeAsks(ctx.Session(), ctx.InvocationID(), nodeID); len(scan.turns) > scan.pauses {
		q := scan.turns[len(scan.turns)-1].question
		log.Info("worker asked the user; pausing node", "question", q, "round", scan.pauses+1)
		_, ierr := workflow.ResumeOrRequestInput(ctx, emit, session.RequestInput{
			InterruptID: hitlInterruptID(nodeID, scan.pauses+1),
			Message:     q,
		})
		return true, parkForInput(ctrl, q, ierr)
	}
	if cscan := scanNodeConfirms(ctx.Session(), ctx.InvocationID(), nodeID); len(cscan.turns) > cscan.pauses {
		t := cscan.turns[len(cscan.turns)-1]
		// Prefer the guard's own hint (carries call-specific warnings).
		question := t.hint
		if question == "" {
			question = fmt.Sprintf("Approve running %s? Reply \"approve\" or \"deny\".", t.tool)
		}
		msg := fmt.Sprintf("%s\n\nArguments: %v", question, t.args)
		log.Info("worker proposed a guarded operation; pausing node", "tool", t.tool, "round", cscan.pauses+1)
		_, ierr := workflow.ResumeOrRequestInput(ctx, emit, session.RequestInput{
			InterruptID: confirmInterruptID(nodeID, cscan.pauses+1),
			Message:     msg,
		})
		return true, parkForInput(ctrl, msg, ierr)
	}
	return false, nil
}

// parkForInput folds an ADK HITL park into the node state machine: mark the control paused/awaiting_input with the question (persisted before the park
// is acted on), then wrap ADK's sentinel in ErrNodePaused so quack code sees
// one sentinel. ErrNodeInterrupted stays in the chain because ADK's engine keys the park itself off it - dropping it would fail the node instead.
func parkForInput(ctrl NodeControl, question string, ierr error) error {
	if !errors.Is(ierr, workflow.ErrNodeInterrupted) {
		return ierr // emit failure, not a park
	}
	if ctrl != nil {
		ctrl.PauseForInput(question)
	}
	return fmt.Errorf("%w: %w", ErrNodePaused, ierr)
}

// recallMemoryHits: parses a native recall_memory FunctionResponse (a tools.recallMemoryResult round-tripped through session-event JSON) back
// into the hits it delivered - a JSON roundtrip rather than manual map
// assertions, since ADK's own representation of a nested slice varies by path (live run vs replay) and json.Marshal handles either uniformly.
func recallMemoryHits(resp map[string]any) []memory.Delivered {
	if resp == nil {
		return nil
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	var out struct {
		Hits []memory.Delivered `json:"hits"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out.Hits
}

// stagedCandidate: parses stage_memory args into memory candidate. Bucket routes the write.
func stagedCandidate(fc *genai.FunctionCall) (memory.Candidate, bool) {
	c, ok := fc.Args["content"].(string)
	if !ok || strings.TrimSpace(c) == "" {
		return memory.Candidate{}, false
	}
	cand := memory.Candidate{Content: strings.TrimSpace(c)}
	set := func(key, val string) {
		if val == "" {
			return
		}
		if cand.Metadata == nil {
			cand.Metadata = map[string]string{}
		}
		cand.Metadata[key] = val
	}
	k, _ := fc.Args["kind"].(string)
	set("kind", k)
	b, _ := fc.Args["bucket"].(string)
	set("bucket", b)
	return cand, true
}

// stagedDeliveryTarget: parses stage_*/unstage calls into target key + item. unstage=true means drop.
func stagedDeliveryTarget(fc *genai.FunctionCall) (target string, item StagedDelivery, unstage bool, ok bool) {
	switch fc.Name {
	case "stage_pr":
		title, _ := fc.Args["title"].(string)
		if strings.TrimSpace(title) == "" {
			return "", StagedDelivery{}, false, false
		}
		body, _ := fc.Args["body"].(string)
		return "pr", StagedDelivery{Kind: "pull_request", Title: strings.TrimSpace(title), Body: body}, false, true
	case "stage_review":
		event, _ := fc.Args["event"].(string)
		event = strings.ToLower(strings.TrimSpace(event))
		body, _ := fc.Args["body"].(string)
		return "review", StagedDelivery{Kind: "review", Event: event, Body: body}, false, true
	case "stage_comment":
		slot, _ := fc.Args["slot"].(string)
		slot = strings.TrimSpace(slot)
		if slot == "" {
			return "", StagedDelivery{}, false, false
		}
		body, _ := fc.Args["body"].(string)
		return "comment:" + slot, StagedDelivery{Kind: "comment", Slot: slot, Body: body}, false, true
	case "unstage":
		t, _ := fc.Args["target"].(string)
		t = strings.TrimSpace(t)
		if t == "" {
			return "", StagedDelivery{}, false, false
		}
		return t, StagedDelivery{}, true, true
	}
	return "", StagedDelivery{}, false, false
}

// commitMemoryOnPass: fires staged knowledge into shared memory on gate pass (fire-and-forget).
func commitMemoryOnPass(ctx adkagent.Context, spanCtx context.Context, cfg Config, author, answer string, staged []memory.Candidate) {
	if cfg.Memory == nil || !cfg.CommitMemory || strings.TrimSpace(answer) == "" {
		return
	}
	sc := MemoryScope(ctx, cfg)
	prov := memory.Provenance{ChatID: cfg.ChatID, NodeID: author, Source: cfg.Source}
	// Fire-and-forget: link span to node span (separate trace, node may finish before goroutine does).
	parentSC := oteltrace.SpanContextFromContext(spanCtx)
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), memoryCommitTimeout)
		defer cancel()
		// Own trace root (detached ctx, no coords) - name the chat explicitly.
		cctx, commitSpan := otelobs.StartLinked(cctx, "memory.commit", parentSC,
			attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String(otelobs.GenAIAgentName, author))
		n, err := cfg.Memory.Commit(cctx, sc, author, prov, staged, answer)
		otelobs.End(commitSpan, err)
		if err != nil {
			reason := otelobs.ClassifyMemoryCommitError(err)
			otelobs.RecordMemoryCommitFailure(author, reason)
			slog.Warn("memory commit failed", "component", "vetting", "node", author, "err", err, "staged", len(staged), "reason", reason)
			return
		}
		if n > 0 {
			slog.Info("memory committed", "component", "vetting", "node", author,
				"count", n, "repo", sc.Repo, "role", sc.Role, "user", sc.User)
		}
	}()
}

// resolveCloneCoordinates: the repo/branch this node itself cloned, setup-provisioned
// or not. Shared by commitDelivery's own dc-building and the review fan-in's
// RecordClone (#1059) - same precedence, single source of truth.
func resolveCloneCoordinates(cfg Config, act workerActivity) (cloneURL, branch string) {
	if cfg.Setup != nil {
		return cfg.Setup.Repo, cfg.Setup.WorkBranch
	}
	if len(act.clonedRepos) > 0 {
		return act.clonedRepos[0], act.currentBranch
	}
	return "", act.currentBranch
}

// commitDelivery: posts final staged delivery exactly once. Blocking (delivery failure is user-visible).
func commitDelivery(ctx context.Context, sink func(stream.SSEEvent), cfg Config, nodeID string, act workerActivity, res GateResult) {
	// Multi-reviewer plan (#867): a synthesizer node hands its consolidated review to
	// the fan-in, which delivers the merged, worst-of-verdict review exactly once.
	if cfg.ReviewFanout != nil {
		if commitFanoutStage(ctx, sink, &cfg, nodeID, &act, res) {
			return
		}
	}
	if !deliveryReady(cfg, act) {
		emitNoDelivery(ctx, sink, nodeID, cfg, res)
		return
	}
	// Render from the durable record instead of the worker's own restatement (#1093 P6/P10): a
	// draft-on-fail delivery renders and records the same revision it posts, never staged text.
	renderedFromStaged := act.skipArtifactRender
	if !renderedFromStaged {
		act.stagedDelivery, renderedFromStaged = artifactRenderedDelivery(ctx, cfg, nodeID, act.stagedDelivery)
	}
	spanCtx, span := otelobs.Start(ctx, "delivery",
		attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", nodeID))
	defer span.End()
	traceID := otelobs.TraceIDOf(spanCtx)
	dc := buildDeliveryContext(cfg, act, res, nodeID)
	if dropVerdictlessReviews(&dc, sink, nodeID, traceID, cfg, res) {
		return
	}
	// Permission boundary: drop ungranted items before they reach cfg.Deliver.
	if dropUngrantedKinds(&dc, sink, nodeID, traceID, cfg, res) {
		return
	}
	kinds := make([]string, len(dc.Items))
	for i, item := range dc.Items {
		kinds[i] = item.Kind
	}
	span.SetAttributes(attribute.StringSlice("staged_kinds", kinds))
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// #1093: artifact-backed deliveries get a fail-closed WAL delivery.intent entry; a
	// staged-text render is never treated as artifact-backed, even when a target exists.
	targetID, targetRev, hasTarget := deliveryTarget(ctx, cfg)
	hasTarget = hasTarget && !renderedFromStaged
	if hasTarget {
		idemKey := deliveryIdempotencyKey(targetID, targetRev)
		dc.IdempotencyKey = idemKey
		if walErr := appendDeliveryIntent(cctx, cfg, nodeID, idemKey, targetID, targetRev, dc.CloneURL, dc.IssueNumber); walErr != nil {
			slog.Error("delivery.intent WAL append failed; not delivering", "component", "vetting", "node", nodeID, "err", walErr)
			failDeliveryOutcomes(sink, nodeID, dc, traceID, walErr)
			recordDeliveryOutcomeMetric(cfg, res, true, false)
			otelobs.End(span, walErr)
			return
		}
	}
	// Gate-owned push: a push failure still reaches Deliver (carried on dc.PushError) - the
	// extension is the only thing that can tell the human on GitHub a delivery failed (#1155).
	if pushErr := ensurePush(cctx, cfg, &dc); pushErr != nil {
		slog.Error("gate push failed", "component", "vetting", "node", nodeID, "err", pushErr, "branch", dc.Branch)
		dc.PushError = pushErr.Error()
	}
	itemOutcomes, err := cfg.Deliver(cctx, dc)
	err = pushErrorWins(dc.PushError, err)
	span.SetAttributes(attribute.Bool("delivered", err == nil))
	otelobs.End(span, err)
	if hasTarget {
		postDeliveryRecord(ctx, cfg, nodeID, dc, itemOutcomes, res, renderedFromStaged, targetID, targetRev, err)
	}
	emitDeliveryOutcomes(sink, nodeID, dc, itemOutcomes, traceID, cfg, res, err)
	if err != nil {
		slog.Error("delivery failed", "component", "vetting", "node", nodeID, "err", err, "items", len(dc.Items))
		return
	}
	slog.Info("delivery committed", "component", "vetting", "node", nodeID, "count", len(dc.Items))
}

// commitFanoutStage hands this node's review to the run's ReviewFanout (#867):
// a synthesizer's consolidated answer (#965), or one reviewer's staged review.
func commitFanoutStage(ctx context.Context, sink func(stream.SSEEvent), cfg *Config, nodeID string, act *workerActivity, res GateResult) bool {
	if !cfg.IsReviewer {
		// The structured code_review record is read here, not act.answer - a native
		// write_code_review leaves no VERDICT tail to parse from it.
		rec, haveRec := LatestCodeReviewRecord(ctx, *cfg)
		merged, deliverNow := cfg.ReviewFanout.FinishSynthesis(act.answer, rec, haveRec)
		if deliverNow {
			deliverMergedReview(ctx, sink, *cfg, nodeID, merged)
		}
		if len(act.stagedDelivery) == 0 {
			recordDeliveryOutcomeMetric(*cfg, res, false, false)
			return true
		}
		cfg.ReviewFanout = nil
		return false
	}
	item, hasItem := act.stagedDelivery["review"]
	if hasItem {
		clone := make(map[string]StagedDelivery, len(act.stagedDelivery)-1)
		for k, v := range act.stagedDelivery {
			if k != "review" {
				clone[k] = v
			}
		}
		act.stagedDelivery = clone
	}
	cloneURL, branch := resolveCloneCoordinates(*cfg, *act)
	cfg.ReviewFanout.RecordClone(cloneURL, branch)
	if scope := resolveReviewScope(*cfg); scope.ok {
		cfg.ReviewFanout.RecordScope(scope.head, scope.fileCount, firstCodeReviewDelivery(ctx, *cfg))
	}
	merged, deliverNow := cfg.ReviewFanout.Finish(nodeID, item, hasItem, false)
	if deliverNow {
		deliverMergedReview(ctx, sink, *cfg, nodeID, merged)
	}
	if len(act.stagedDelivery) == 0 {
		recordDeliveryOutcomeMetric(*cfg, res, false, false)
		return true
	}
	return false
}

// pushErrorWins: a PushError outweighs a Deliver that reported success -
// the push itself never landed.
func pushErrorWins(pushErr string, err error) error {
	if pushErr != "" && err == nil {
		return errors.New(pushErr)
	}
	return err
}

// deliveryReady reports whether this node has a delivery target and staged items.
func deliveryReady(cfg Config, act workerActivity) bool {
	return cfg.Deliver != nil && len(act.stagedDelivery) > 0
}

// emitNoDelivery records the no-delivery outcome and flags a phantom success.
func emitNoDelivery(ctx context.Context, sink func(stream.SSEEvent), nodeID string, cfg Config, res GateResult) {
	recordDeliveryOutcomeMetric(cfg, res, false, false)
	// Phantom-success: delivery-capable node with judge-passed work that staged nothing.
	if !cfg.ReadOnly && res.Passed {
		emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeNone, "", "", "", otelobs.TraceIDOf(ctx)))
	}
}

// buildDeliveryContext assembles the delivery context (items, clone coordinates,
// clone dir) from the node's activity.
func buildDeliveryContext(cfg Config, act workerActivity, res GateResult, nodeID string) DeliveryContext {
	dc := DeliveryContext{
		NodeID: nodeID, ChatID: cfg.ChatID, Items: sortedStagedDelivery(act.stagedDelivery), IssueNumber: act.prNumber,
		GatePassed: res.Passed, GateFeedback: res.Feedback, ChecksSkipNote: checksSkipNote(res.ChecksSkipReason),
	}
	dc.CloneURL, dc.Branch = resolveCloneCoordinates(cfg, act)
	if cfg.Setup != nil {
		// Deliver on setup branch (worker's git-tracking ledger is off-limits for setup-provisioned workers).
		if cfg.Workspace != nil {
			// Use cfg.NodeID (workspace scope), not nodeID argument.
			if abs, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID)); err == nil {
				dc.CloneDir = abs
			}
		}
	} else {
		dc.Branch = act.currentBranch
		if len(act.clonedRepos) > 0 {
			dc.CloneURL = act.clonedRepos[0]
		}
		if cfg.Workspace != nil && len(act.clonedDirs) > 0 {
			if abs, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, act.clonedDirs[0]); err == nil {
				dc.CloneDir = abs
			}
		}
	}
	return dc
}

// dropVerdictlessReviews drops staged reviews with no verdict (a loud refusal, per item);
// all-dropped means nothing left to deliver.
func dropVerdictlessReviews(dc *DeliveryContext, sink func(stream.SSEEvent), nodeID, traceID string, cfg Config, res GateResult) bool {
	// A review with comments/findings but no verdict is not a reviewed PR - drop just
	// that item so a sibling pr/comment item in the same delivery still ships (#1198 C).
	verdictless, rest := partitionEmptyVerdictReview(dc.Items)
	if len(verdictless) == 0 {
		return false
	}
	for _, item := range verdictless {
		slog.Error("delivery refused: staged review has no verdict", "component", "vetting", "node", nodeID)
		emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeFailed,
			item.Kind, "", "you staged comments but no verdict - call stage_review with an event (approve/request_changes/comment) before this round ends", traceID))
	}
	dc.Items = rest
	if len(dc.Items) == 0 {
		recordDeliveryOutcomeMetric(cfg, res, true, false)
		return true
	}
	return false
}

// dropUngrantedKinds drops items of ungranted delivery kinds (loud refusal, per item);
// all-dropped means nothing left to deliver.
func dropUngrantedKinds(dc *DeliveryContext, sink func(stream.SSEEvent), nodeID, traceID string, cfg Config, res GateResult) bool {
	allowed, refused, reasons := partitionByAllowedKinds(dc.Items, cfg.AllowedDeliveryKinds)
	if len(refused) == 0 {
		return false
	}
	for i, item := range refused {
		slog.Error("delivery refused: ungranted kind", "component", "vetting",
			"node", nodeID, "kind", item.Kind, "reason", reasons[i])
		emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeFailed,
			item.Kind, "", "delivery refused: "+reasons[i], traceID))
	}
	dc.Items = allowed
	if len(dc.Items) == 0 {
		recordDeliveryOutcomeMetric(cfg, res, true, false)
		return true
	}
	return false
}

// failDeliveryOutcomes emits one failed outcome per staged item (the WAL append failed).
func failDeliveryOutcomes(sink func(stream.SSEEvent), nodeID string, dc DeliveryContext, traceID string, walErr error) {
	itemOutcomes := make([]DeliveryItemOutcome, len(dc.Items))
	for i, item := range dc.Items {
		itemOutcomes[i] = DeliveryItemOutcome{Kind: item.Kind, Error: "delivery.intent WAL append failed: " + walErr.Error()}
		emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeFailed, item.Kind, "", itemOutcomes[i].Error, traceID))
	}
}

// postDeliveryRecord writes the delivery_record revision on the context detached from
// the run's cancellation (#1187: the GitHub side effect already happened).
func postDeliveryRecord(ctx context.Context, cfg Config, nodeID string, dc DeliveryContext, itemOutcomes []DeliveryItemOutcome, res GateResult, renderedFromStaged bool, targetID string, targetRev int, err error) {
	if ctx.Err() != nil {
		slog.Warn("run context cancelled before post-delivery bookkeeping; continuing on a detached context",
			"component", "vetting", "node", nodeID, "err", ctx.Err(), "cause", context.Cause(ctx))
	}
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer bcancel()
	if err == nil {
		var remoteURL string
		for _, io := range itemOutcomes {
			if io.URL != "" {
				remoteURL = io.URL
				break
			}
		}
		// This save IS the delivery.intent's completion (#1144 P2 - the delivery_record
		// artifact is the record, not a delivery.done entry).
		saveDeliveryRecord(bctx, cfg, nodeID, DeliveryRecord{
			TargetID: targetID, DeliveredRevision: targetRev, RemoteURL: remoteURL, PRNumber: dc.IssueNumber, At: time.Now().UTC(),
			GatePassed: res.Passed, RenderedFromStaged: renderedFromStaged, HeadSHA: cloneHeadSHA(cfg),
		})
	} else {
		// No successful delivery_record revision: `quack ledger recover` finds this intent
		// still open and can reconcile the attempt.
		saveDeliveryRecord(bctx, cfg, nodeID, DeliveryRecord{
			TargetID: targetID, DeliveredRevision: targetRev, PRNumber: dc.IssueNumber, At: time.Now().UTC(),
			GatePassed: res.Passed, RenderedFromStaged: renderedFromStaged, Error: err.Error(), HeadSHA: cloneHeadSHA(cfg),
		})
	}
}

// emitDeliveryOutcomes emits one outcome per item (extension outcomes, or synthetic
// fallbacks when the extension reported nothing) and records the metric.
func emitDeliveryOutcomes(sink func(stream.SSEEvent), nodeID string, dc DeliveryContext, itemOutcomes []DeliveryItemOutcome, traceID string, cfg Config, res GateResult, err error) {
	// Extension's record is authoritative; fall back to synthetic outcomes only when
	// the extension reported nothing.
	if len(itemOutcomes) == 0 {
		itemOutcomes = make([]DeliveryItemOutcome, len(dc.Items))
		for i, item := range dc.Items {
			itemOutcomes[i] = DeliveryItemOutcome{Kind: item.Kind}
			if err != nil {
				itemOutcomes[i].Error = err.Error()
			}
		}
	}
	anyDelivered := false
	for _, io := range itemOutcomes {
		outcome := stream.DeliveryOutcomeDelivered
		switch {
		case io.Error != "":
			outcome = stream.DeliveryOutcomeFailed
		case !res.Passed:
			outcome = stream.DeliveryOutcomeDraft
		default:
			anyDelivered = true
		}
		emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, outcome, io.Kind, io.URL, io.Error, traceID))
	}
	recordDeliveryOutcomeMetric(cfg, res, true, anyDelivered || (err == nil && res.Passed))
}

// deliverMergedReview: posts the fan-in's merged review as this node's own single-item delivery. cfg.ReviewFanout is cleared first so the recursive
// commitDelivery call goes straight through instead of re-intercepting. A merge with nothing valid in it (every reviewer node failed/staged
// nothing) posts nothing - there is nothing true to tell the reader.
func deliverMergedReview(ctx context.Context, sink func(stream.SSEEvent), cfg Config, nodeID string, merged StagedDelivery) {
	cloneURL, branch := cfg.ReviewFanout.Clone()
	cfg.ReviewFanout.forget()
	if strings.TrimSpace(merged.Body) == "" && len(merged.Comments) == 0 {
		slog.Warn("review fan-in produced nothing to deliver; every reviewer node failed or staged nothing",
			"component", "vetting", "node", nodeID)
		return
	}
	cfg.ReviewFanout = nil
	// The delivering node (often a synthesizer) may have cloned nothing
	// itself - fall back to a reviewer sibling's clone coordinates (#1059). The merge is already the final worst-of text; a reviewer-node terminal
	// (latent plan shape, see #1118 review) must never let the render clobber it with that node's own individual code_review record.
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{"review": merged}, currentBranch: branch, skipArtifactRender: true}
	if cloneURL != "" {
		act.clonedRepos = []string{cloneURL}
	}
	commitDelivery(ctx, sink, cfg, nodeID, act, GateResult{Passed: true})
}

// isReviewerPauseSentinel: true when err means "parked, may still resume" - not a terminal outcome for the review fan-in. Both of RunGatedRefine's own
// early-return sentinel ErrNodePaused (every quack pause, HITL included) and ADK's own workflow.ErrNodeInterrupted (which the scheduler can still raise
// on its own, so it stays recognised here) mean the node is not done - registering it as failed here would let the fan-in deliver without it, then silently discard its real verdict on resume.
func isReviewerPauseSentinel(err error) bool {
	return errors.Is(err, ErrNodePaused) || errors.Is(err, workflow.ErrNodeInterrupted)
}

// abortedRoundNote: flags a delivered review as coming from a dead round,
// not a clean pass (#942).
const abortedRoundNote = "_The round that produced this review ended abnormally (worker error, timeout, or cancel) after the verdict below was staged. Delivering it as-is rather than discarding it._\n\n"

// resolveAbortedReviewer: delivers a dead node's staged review (#942) instead
// of discarding it. No ReviewFanout → deliver directly; otherwise resolve
// this node's fanout slot so a dead sibling can't block the merge (#867).
func resolveAbortedReviewer(ctx context.Context, sink func(stream.SSEEvent), cfg Config, nodeID string, act workerActivity) {
	item, hasItem := act.stagedDelivery["review"]
	if hasItem {
		item.Body = abortedRoundNote + item.Body
	}
	if cfg.ReviewFanout == nil {
		if !hasItem {
			return
		}
		commitDelivery(ctx, sink, cfg, nodeID, workerActivity{stagedDelivery: map[string]StagedDelivery{"review": item}, skipArtifactRender: true}, GateResult{})
		return
	}
	var merged StagedDelivery
	var deliverNow bool
	if cfg.IsReviewer {
		merged, deliverNow = cfg.ReviewFanout.Finish(nodeID, item, hasItem, !hasItem)
	} else {
		// Empty answer falls the merge back to per-node concatenation (#965).
		merged, deliverNow = cfg.ReviewFanout.FinishSynthesis("", CodeReviewRecord{}, false)
	}
	if deliverNow {
		deliverMergedReview(ctx, sink, cfg, nodeID, merged)
	}
}

// partitionByAllowedKinds: splits staged items by the delivery-kind
// allowlist. nil allowedKinds permits everything.
func partitionByAllowedKinds(items []StagedDelivery, allowedKinds []string) (allowed, refused []StagedDelivery, reasons []string) {
	for _, item := range items {
		if allowedKinds == nil || slices.Contains(allowedKinds, item.Kind) {
			allowed = append(allowed, item)
		} else {
			refused = append(refused, item)
			reasons = append(reasons, fmt.Sprintf("delivery kind %q not in allowed set", item.Kind))
		}
	}
	return allowed, refused, reasons
}

// partitionEmptyVerdictReview splits staged items, dropping a "review" item
// whose Event is empty (#1198 part C) - GitHub has no "no verdict" review, and posting one anyway is the markers-only bug. Per-item, like
// partitionByAllowedKinds: a sibling pr/comment item in the same delivery still ships.
func partitionEmptyVerdictReview(items []StagedDelivery) (verdictless, rest []StagedDelivery) {
	for _, item := range items {
		if item.Kind == "review" && strings.TrimSpace(item.Event) == "" {
			verdictless = append(verdictless, item)
		} else {
			rest = append(rest, item)
		}
	}
	return verdictless, rest
}

// emitDeliveryResult: sends delivery_result SSE event (SSE-only, never written to session).
func emitDeliveryResult(sink func(stream.SSEEvent), nodeID string, ev stream.SSEEvent) {
	if sink != nil {
		sink(ev)
	}
}

// emitArtifactRevision sends one artifact_revision SSE event (#1092) for a
// revision this round wrote, before the round's artifact_judge_round event -
// the record it was scored under references a revision that already exists.
func emitArtifactRevision(sink func(stream.SSEEvent), id string, revision int, kind, nodeID string, round int) {
	if sink == nil {
		return
	}
	sink(stream.SSEEvent{Name: stream.EventArtifactRevision, Data: stream.ArtifactRevisionData{
		ID: id, Revision: revision, Kind: kind, NodeID: nodeID, Round: round,
	}})
}

// emitJudgeRound sends the artifact_judge_round SSE event (#1092), after
// every artifact_revision event for the round's own scored writes.
func emitJudgeRound(sink func(stream.SSEEvent), id string, passed bool, score float64, scored []ScoredRef) {
	if sink == nil {
		return
	}
	refs := make([]stream.ScoredRef, len(scored))
	for i, s := range scored {
		refs[i] = stream.ScoredRef{ArtifactID: s.ArtifactID, Revision: s.Revision}
	}
	sink(stream.SSEEvent{Name: stream.EventArtifactJudgeRound, Data: stream.ArtifactJudgeRoundData{
		ID: id, Passed: passed, Score: score, Scored: refs,
	}})
}

// isNonDeliveringSlice reports whether cfg is a reviewer node that is part
// of a fan-out with a downstream synthesizer (#1092, design V4 §4.6) - such
// a node's own verdict is never what delivery renders, so it isn't judged on structured_verdict.
func isNonDeliveringSlice(cfg Config) bool {
	return cfg.IsReviewer && cfg.ReviewFanout != nil && cfg.ReviewFanout.SynthExpected()
}

// recordDeliveryOutcomeMetric: records quack.delivery.outcome. Scoped to delivery-capable agents.
func recordDeliveryOutcomeMetric(cfg Config, res GateResult, attempted, delivered bool) {
	if cfg.ReadOnly {
		return
	}
	switch {
	case attempted && delivered && res.Passed:
		otelobs.RecordDeliveryOutcome(otelobs.DeliveryDelivered)
	case attempted && delivered:
		otelobs.RecordDeliveryOutcome(otelobs.DeliveryDraft)
	case attempted:
		otelobs.RecordDeliveryOutcome(otelobs.DeliveryFailed)
	case res.Passed:
		otelobs.RecordDeliveryOutcome(otelobs.DeliveryNone)
	}
}

// MemoryScope: node's memory entitlement (repo, role, user). Exported for ACP
// MCP surface. Legacy is deliberately unset here - it exists only so
// memories committed before per-scope buckets (keyed by agent NAME, e.g. "web-researcher") still recall; a node id is not that key.
func MemoryScope(ctx adkagent.Context, cfg Config) memory.Scope {
	sc := memory.Scope{Role: cfg.MemoryRole}
	if s := ctx.Session(); s != nil {
		sc.User = s.UserID()
	}
	if cfg.Workspace != nil {
		sc.Repo = cfg.Workspace.RepoKey(cfg.WorkspaceUserID, cfg.ChatID)
	}
	return sc
}

// runWorkerNode: runs worker as sub-branched child, strips thinking content.
// attachments are artifactref reference parts by the time they arrive here
// (rerouted at the REST/plan entry boundary) - real bytes are swapped back in only at the model boundary (internal/inference's hydratingModel).
func workerInput(prompt string, attachments []*genai.Part) any {
	if len(attachments) == 0 {
		return prompt
	}
	return &genai.Content{Role: "user", Parts: append([]*genai.Part{{Text: prompt}}, attachments...)}
}

// gatePromptAuthor authors the prompt-delivery events emitPrompt writes. NOT
// "user" (a user-authored event would split a chat turn in store. groupSessionEvents and confuse the runner's turn detection) and never an
// agent's name (remoteagent presents foreign-authored events to the remote model as user messages - exactly what a prompt should be).
const gatePromptAuthor = "quack-gate"

// emitPrompt writes the worker's prompt into the session as a gate-authored event, immediately before the RunNode that consumes it. A local llmagent takes
// RunNode input directly, but production workers are A2A REMOTE agents, which build their outbound message from SESSION EVENTS ONLY - without this event a
// remote worker never sees its task, and an empty session tail skips the dispatch entirely. emit completes durably before it returns, so there is no ordering race. The event is filtered everywhere else by its author/branch.
func emitPrompt(ctx adkagent.Context, emit func(*session.Event) error, input any) {
	if emit == nil {
		return
	}
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Author = gatePromptAuthor
	ev.Branch = ctx.Branch()
	switch v := input.(type) {
	case string:
		ev.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: v}}}
	case *genai.Content:
		ev.Content = v
	default:
		return
	}
	if err := emit(ev); err != nil {
		slog.Warn("prompt event emit failed; a remote worker may not see its task", "component", "vetting", "err", err)
	}
}

func runWorkerNode(ctx adkagent.Context, workerNode workflow.Node, input any, runID string, emit func(*session.Event) error) (string, error) {
	t0 := time.Now()
	emitPrompt(ctx, emit, input)
	// IsolationScopeFromNodePath hides sibling events from concurrent workers (ADK v2.0 pivot scan unfiltered).
	out, err := workflow.RunNode[string](ctx, workerNode, input,
		workflow.WithUseSubBranch(), workflow.WithRunID(runID),
		workflow.WithIsolationScopeFromNodePath())
	if err != nil {
		return "", err
	}
	stripped := stream.StripThinking(out)
	// ms=~0 means RunNode short-circuited (no model call); raw_len>0 & stripped_len=0
	// means StripThinking nuked an inline <think>. Debug: hot path, one line per run.
	slog.DebugContext(ctx, "worker run", "run", runID, "ms", time.Since(t0).Milliseconds(),
		"raw_len", len(out), "stripped_len", len(stripped))
	return stripped, nil
}

// modelName extracts a model identifier for span/metric attributes; "" for a
// nil model.LLM (e.g. an ACP agent whose worker isn't backed by a local model.LLM).
func modelName(m model.LLM) string {
	if m == nil {
		return ""
	}
	return m.Name()
}

// runWorkerNodeTraced: wraps runWorkerNode with "quack.worker.round" span and replay-ledger coords.
func runWorkerNodeTraced(ctx adkagent.Context, spanCtx context.Context, cfg Config, workerModel model.LLM, workerNode workflow.Node, input any, runID, stage string, emit func(*session.Event) error) (string, error) {
	_, ts := otelobs.StartTimedSpan(spanCtx, "worker.round",
		attribute.String(otelobs.ChatIDKey, cfg.ChatID),
		attribute.String("node_id", cfg.NodeID),
		attribute.String("run_id", runID),
		attribute.String(otelobs.GenAIAgentName, cfg.Agent),
		attribute.String(otelobs.QuackModel, modelName(workerModel)),
		attribute.String("stage", stage),
	)
	coords := ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, BundleHash: cfg.BundleHash, Round: runID, User: cfg.User, Source: cfg.Source, SpanContext: ts.Span.SpanContext()}
	gctx := ctx.WithAgentContext(ledger.WithCoords(ctx, coords))
	// WithAgentContext stamp does not survive RunNode scheduling; inference models get stamped directly.
	if cs, ok := workerModel.(interface{ SetLedgerCoords(ledger.Coords) }); ok {
		cs.SetLedgerCoords(coords)
	}
	out, err := runWorkerNode(gctx, workerNode, input, runID, emit)
	d := ts.End(err)
	otelobs.RecordRoundDuration(cfg.Agent, modelName(workerModel), stage, d)
	return out, err
}

// checksPassCriterionTraced: wraps checksPassCriterion with gate.checks span and replay coords.
func checksPassCriterionTraced(ctx context.Context, cfg Config) (criterionScore, bool) {
	spanCtx, span := otelobs.Start(ctx, "gate.checks",
		attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", cfg.NodeID))
	defer span.End()
	probeCtx := ledger.WithCoords(spanCtx, ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, Round: probeRound, User: cfg.User, Source: cfg.Source})
	c, ok := checksPassCriterion(probeCtx, cfg)
	span.SetAttributes(attribute.Bool("applicable", ok), attribute.Float64("score", c.Score))
	return c, ok
}

// emitJudge: sends judge-stage SSE event scoped to nodeID.
func emitJudge(sink func(stream.SSEEvent), nodeID string, ev stream.SSEEvent) {
	if sink != nil {
		sink(stream.ScopeToNode(ev, nodeID))
	}
}

// judgePartEmitter: forwards judge's streamed parts to SSE sink. nil-sink-safe, never writes to session.
func judgePartEmitter(sink func(stream.SSEEvent), nodeID, runID string) func(*genai.Part) bool {
	var seen stream.SeenCalls
	return func(p *genai.Part) bool {
		if sink == nil || p == nil {
			return true
		}
		switch {
		case p.Thought && p.Text != "":
			sink(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentThinking, Data: stream.AgentThinkingData{RunID: runID, Text: p.Text}}, nodeID))
		case p.Text != "":
			sink(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentToken, Data: stream.AgentTokenData{RunID: runID, Text: p.Text}}, nodeID))
		case p.FunctionCall != nil:
			// ACP's start+completion updates both carry the FunctionCall part for one call_id.
			if seen.Add(p.FunctionCall.ID) {
				return true
			}
			sink(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentToolCall, Data: stream.AgentToolCallData{RunID: runID, CallID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Args: p.FunctionCall.Args}}, nodeID))
		case p.FunctionResponse != nil:
			sink(stream.ScopeToNode(stream.SSEEvent{Name: stream.EventAgentToolResult, Data: stream.AgentToolResultData{RunID: runID, CallID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name, Result: p.FunctionResponse.Response}}, nodeID))
		}
		return true
	}
}

// computeDeterministicCriteria: computes code-owned criteria before judge runs.
// checksSkipReason is the raw checksPassCriterion skip reason ("" if checks
// ran), for the caller to attach to GateResult (#780).
func computeDeterministicCriteria(ctx context.Context, answer string, act workerActivity, cfg Config) (det map[string]criterionScore, checksSkipReason string) {
	det = map[string]criterionScore{}
	if ls := lengthScore(answer); ls < 1.0 {
		det["sufficient_length"] = criterionScore{Score: ls, Reason: fmt.Sprintf(
			"deterministic: %d chars, need at least %d (non-empty)", len(strings.TrimSpace(answer)), minAnswerChars)}
	}
	// Zero-retrieval answers are ungrounded (model memory or unverifiable citations). Clone/file reads count as retrieval.
	if cfg.RequireRetrieval && len(act.fetched) == 0 && len(act.seen) == 0 && len(act.clonedRepos) == 0 && len(act.paths) == 0 {
		det["grounded_in_retrieval"] = criterionScore{Score: 0, Reason: "deterministic: no web_search/web_fetch activity this session - " +
			"research the task and cite what you retrieve; if you are blocked on information only the user has, call ask_user (never write a question to the user as your answer)"}
	}
	if cs, details, hasCites := citationScore(answer, act); hasCites {
		evidence := make([]evidenceItem, len(details))
		for i, d := range details {
			evidence[i] = evidenceItem{Ref: d.url, Score: d.score}
		}
		det["cites_sources"] = criterionScore{Score: cs, Reason: citeReason(cs, details), Evidence: evidence}
	}
	// Deterministic gate checks: planner's checks or derived from repo.
	if c, ok := checksPassCriterionTraced(ctx, cfg); ok {
		det["checks_pass"] = c
	} else {
		checksSkipReason = c.Reason
	}
	// Added test files that name no production identifier are vacuous by construction (#716).
	if c, ok := vacuousTestsCriterion(cfg); ok {
		det["no_vacuous_tests"] = c
	}
	// Mermaid validity: checked against answer and staged delivery bodies. Added only on failure.
	if c, ok := mermaidCriterion(answer, act); ok {
		det["mermaid_valid"] = c
	}
	// Answer-shape check: leaked or malformed tool-call fragment in deliverable.
	if c, ok := toolCallSyntaxCriterion(answer, act); ok {
		det["no_tool_call_syntax"] = c
	}
	// Answer-shape check: a pointer to a file this run wrote but never committed.
	if c, ok := danglingDeliverablePathCriterion(answer, act, workspace.NodeDir(cfg.NodeID)); ok {
		det["no_dangling_deliverable_path"] = c
	}
	// Delivery: commit/push/PR must show in ledger; review must be submitted.
	for name, c := range incompleteCriteria(cfg.Task, act, cfg.ReadOnly, cfg.Deliver != nil, cfg.IsReviewer, cfg.ExistingPR) {
		det[name] = c
	}
	return det, checksSkipReason
}

// deterministicCriterionSpec: definition/fix declared per deterministic
// criterion name (#941). A static table rather than editing each of the ~10 constructor sites (checks.go, mermaid.go, shape.go, vacuoustests.go,
// delivery.go) - the criterion names are a fixed, code-owned set, so one lookup keyed by name is a smaller diff with the same effect.
var deterministicCriterionSpec = map[string]struct {
	definition string
	fix        string
}{
	"sufficient_length":            {"The answer must be non-empty.", "Write a substantive answer."},
	"grounded_in_retrieval":        {"Claims must trace to retrieval performed this session (web fetch/search or file reads), not model memory.", "Research the task and cite what you retrieve; call ask_user if blocked on information only the user has."},
	"checks_pass":                  {"The node's configured or derived build/test checks must exit zero.", "Fix the failing check(s) named in the failure output."},
	"no_vacuous_tests":             {"An added test file must exercise a real production identifier, not assert trivially.", "Rewrite the test to call/assert against actual production code."},
	"mermaid_valid":                {"Mermaid diagrams in the deliverable must be syntactically valid.", "Fix the invalid mermaid diagram(s) named in the failure - iterate with the check_mermaid tool until it reports ok, then resubmit."},
	"no_tool_call_syntax":          {"The deliverable must not contain a leaked or malformed tool-call fragment.", "Remove the leaked tool-call fragment from the answer."},
	"no_dangling_deliverable_path": {"A deliverable must not point to a file that exists only in this run's discarded working directory.", "State the result in the answer text itself, or commit the file so it survives the run."},
	"delivery_complete":            {"The task's delivery step (commit/push/PR) must actually show in the session ledger.", "Complete the delivery step the task asked for - commit, push, or open the PR."},
	"review_posted":                {"A review task must actually submit its verdict via github_submit_review.", "Post the review with github_add_review_comment/github_submit_review, not just in the answer text."},
	"behaviour_verified":           {"A code-review task must execute the change (tests, a throwaway harness) before judging it.", "Run the change - its tests or a small harness - before asserting it works."},
}

// citesSourcesBands: the cites_sources tier legend, moved out of the reason
// string and into structured bands per #941 (must stop being re-emitted per round).
var citesSourcesBands = []bandSpec{
	{Min: 0.00, Max: 0.24, Meaning: "never seen anywhere - the citation is fabricated"},
	{Min: 0.25, Max: 0.74, Meaning: "same host seen but this exact page never fetched - likely invented"},
	{Min: 0.75, Max: 1.00, Meaning: "fetched or seen in search - backed"},
}

const citesSourcesFix = "Fetch each source, or remove the citation and any claim resting on it."

// mergeDeterministic: folds deterministic criteria into verdict and re-aggregates.
// Stamps Deterministic here (not in computeDeterministicCriteria) since det's
// keys are exactly the code-owned set - composeFeedback reads it to tell a
// code-owned failure from a judge-scored one (#791). Also stamps each
// criterion's declared definition/bands/fix (#941): cfg.RubricSpecs/RubricFixes
// (loaded from the node's rubric.yaml) win when the rubric names the criterion (e.g. cites_sources in web-researcher/synthesizer's rubric.yaml);
// deterministicCriterionSpec/citesSourcesBands below are the fallback for criteria no rubric declares (checks_pass, delivery_complete, ...) or when no structured rubric was loaded for this node at all.
func mergeDeterministic(v verdict, det map[string]criterionScore, cfg Config) verdict {
	if v.Criteria == nil {
		v.Criteria = map[string]criterionScore{}
	}
	for name, c := range det {
		c.Deterministic = true
		if spec, ok := deterministicCriterionSpec[name]; ok {
			c.Definition = spec.definition
			c.Fix = spec.fix
		}
		if name == "cites_sources" {
			c.Definition = "Every cited link is backed by a page the run actually fetched."
			c.Bands = citesSourcesBands
			c.Fix = citesSourcesFix
			c.Scale = &scaleSpec{Min: 0, Max: 1}
		}
		if spec, ok := cfg.RubricSpecs[name]; ok {
			c.Definition = spec.Definition
			c.Bands = spec.Bands
			c.Scale = spec.Scale
		}
		if fix, ok := cfg.RubricFixes[name]; ok {
			c.Fix = fix
		}
		v.Criteria[name] = c
	}
	return aggregateVerdict(v)
}

// composeFeedback builds the #941 structured envelope from v and a rendered one-paragraph summary for callers that still want prose: AgentCompleteData.Feedback
// (kept for one release so the UI does not blank) and GateResult.Feedback (the
// delivery-caveat text). The envelope itself - not this summary - is what buildRevisionContent hands the worker.
func composeFeedback(v verdict, threshold float64, round int) (verdictEnvelope, string) {
	env := buildEnvelope(v, threshold, round)
	return env, renderFeedbackSummary(env, v.Feedback, v.Findings)
}

// renderFeedbackSummary: prose rendering of the envelope, in the same
// deterministic-leads/judge-follows shape composeFeedback used before #941 -
// a code-owned failure has one correct fix, a low judge score is arguable, and collapsing them together misrepresents the judge's opinion as decided (#791).
func renderFeedbackSummary(env verdictEnvelope, judgeFeedback string, findings []findingVerdict) string {
	var detFails, judgeFails []string
	for _, f := range env.DeterministicFailures {
		if s := strings.TrimSpace(f.Shortfall); s != "" {
			detFails = append(detFails, fmt.Sprintf("- %s: %s", f.Criterion.Name, s))
		}
	}
	for _, f := range env.JudgeFailures {
		if s := strings.TrimSpace(f.Shortfall); s != "" {
			judgeFails = append(judgeFails, fmt.Sprintf("- %s: %s", f.Criterion.Name, s))
		}
	}
	sort.Strings(detFails) // stable order (buildEnvelope already sorts by name, but keep this local to the function's own contract)
	sort.Strings(judgeFails)
	findingsFeedback := composeFindingsFeedback(findings)
	if len(detFails) == 0 && len(judgeFails) == 0 && findingsFeedback == "" {
		return judgeFeedback
	}
	var sb strings.Builder
	if len(detFails) > 0 {
		sb.WriteString("Deterministic check failures (code-owned, already decided - fix these):\n")
		sb.WriteString(strings.Join(detFails, "\n"))
	}
	if fb := strings.TrimSpace(judgeFeedback); fb != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		if len(detFails) > 0 {
			sb.WriteString("Judge's assessment of the remaining criteria (the deterministic failures above were excluded from its scoring):\n")
		}
		sb.WriteString(fb)
	}
	if len(judgeFails) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("Other criteria the judge scored below threshold:\n")
		sb.WriteString(strings.Join(judgeFails, "\n"))
	}
	if findingsFeedback != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(findingsFeedback)
	}
	return sb.String()
}

// citationOnlyFailure: only cites_sources below threshold - answer is substantively fine, just needs URL formatting.
func citationOnlyFailure(v verdict, threshold float64) bool {
	failing := 0
	citesFailed := false
	for name, c := range v.Criteria {
		if c.Score < threshold {
			failing++
			if name == "cites_sources" {
				citesFailed = true
			}
		}
	}
	return citesFailed && failing == 1
}

// activityFromSession: reconstructs worker's retrieval and workspace ledger from session events.
func joinWritten(cwd, p string) string {
	if strings.HasPrefix(p, "/") {
		return strings.TrimPrefix(p, "/")
	}
	if cwd == "" || cwd == "." {
		return p
	}
	return filepath.Join(cwd, p)
}

// writtenRel resolves a worker's write/edit path to a CHAT-relative path for Jail.Resolve - the read-side mirror of tools.jailPath. Worker paths are
// NODE-relative (the node dir is invisible to the model), re-applied here; a
// leading "/" is the chat-root escape hatch, ignoring both. Must match the judge's resolution, or it silently reads NOTHING (has bitten twice).
func writtenRel(nodeDir, cwd, p string) string {
	if strings.HasPrefix(p, "/") {
		return strings.TrimPrefix(p, "/")
	}
	return joinWritten(nodeDir, joinWritten(cwd, p))
}

// activityFromSessionAt: replays worker's session inside nodeDir. Paths come back chat-relative.
func activityFromSessionAt(sess session.Session, nodeDir string) workerActivity {
	s := &activityScanner{
		act:           workerActivity{fetched: map[string]struct{}{}, seen: map[string]string{}, paths: map[string]bool{}},
		nodeDir:       nodeDir,
		writtenSeen:   map[string]bool{},
		pending:       map[string]string{},
		pendingWs:     map[string]map[string]any{},
		pendingWsTool: map[string]string{},
		pendingCd:     map[string]bool{},
	}
	if sess == nil {
		return s.act
	}
	for ev := range sess.Events().All() {
		s.scanEvent(ev)
	}
	return s.act
}

// scanEvent scans one session event for worker activity; nil content is skipped.
func (s *activityScanner) scanEvent(ev *session.Event) {
	if ev == nil || ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		s.scanPart(p)
	}
}

// scanPart dispatches one part to the call/response scanners.
func (s *activityScanner) scanPart(p *genai.Part) {
	if p.FunctionCall != nil {
		s.scanCall(p.FunctionCall)
	}
	if p.FunctionResponse != nil {
		s.scanResponse(p.FunctionResponse)
	}
}

// scanCall records the tool calls that drive the activity ledger.
func (s *activityScanner) scanCall(fc *genai.FunctionCall) {
	switch fc.Name {
	case "web_search":
		s.recordSearch(fc.Args)
	case "web_fetch":
		if u, ok := fc.Args["url"].(string); ok && strings.TrimSpace(u) != "" {
			s.pending[fc.ID] = strings.TrimSpace(u)
		}
		// Route into workspace ledger (web_fetch signals web-sourced claims).
		s.pendingWs[fc.ID] = fc.Args
		s.pendingWsTool[fc.ID] = "web_fetch"
	case "stage_memory":
		if cand, ok := stagedCandidate(fc); ok {
			s.act.staged = append(s.act.staged, cand)
		}
	case "stage_pr", "stage_review", "stage_comment", "unstage":
		s.applyDelivery(fc)
	case "cd":
		s.pendingCd[fc.ID] = true
	default:
		if isWorkspaceTool(fc.Name) {
			s.pendingWs[fc.ID] = fc.Args
			s.pendingWsTool[fc.ID] = fc.Name
		}
	}
}

// scanResponse pairs tool responses with their pending calls; only completed
// call/response pairs enter the ledger.
func (s *activityScanner) scanResponse(fr *genai.FunctionResponse) {
	switch {
	case fr.Name == "web_fetch":
		if url, known := s.pending[fr.ID]; known {
			delete(s.pending, fr.ID)
			s.recordFetch(url, fr.Response)
		}
	case fr.Name == "web_search":
		recordSearchResults(s.act.seen, fr.Response)
	case fr.Name == "recall_memory":
		s.act.recalled = append(s.act.recalled, recallMemoryHits(fr.Response)...)
	case fr.Name == "cd":
		if s.pendingCd[fr.ID] {
			delete(s.pendingCd, fr.ID)
			s.recordCd(fr.Response)
		}
	}
	if isWorkspaceTool(fr.Name) {
		// Only completed call/response pairs enter the ledger.
		if args, known := s.pendingWs[fr.ID]; known && s.pendingWsTool[fr.ID] == fr.Name {
			delete(s.pendingWs, fr.ID)
			delete(s.pendingWsTool, fr.ID)
			s.recordWorkspace(fr.Name, args, fr.Response)
		}
	}
}

// activityScanner: accumulates one worker's activity. Recorders reached from both session-event and replay paths.
type activityScanner struct {
	act           workerActivity
	nodeDir       string
	curCwd        string          // node-relative cwd ("" = node root)
	writtenSeen   map[string]bool // dedup for written
	pending       map[string]string
	pendingWs     map[string]map[string]any
	pendingWsTool map[string]string
	pendingCd     map[string]bool
}

// recordPRNumber: captures pull_number for delivery target. First call wins.
func (s *activityScanner) recordPRNumber(args map[string]any) {
	if s.act.prNumber != 0 {
		return
	}
	if n, ok := args["pull_number"].(float64); ok && n > 0 {
		s.act.prNumber = int(n)
	}
}

// applyDelivery: upserts or drops a delivery target. Later stage_* replaces earlier; unstage removes.
func (s *activityScanner) applyDelivery(fc *genai.FunctionCall) {
	target, item, unstage, ok := stagedDeliveryTarget(fc)
	if !ok {
		return
	}
	if unstage {
		delete(s.act.stagedDelivery, target)
		return
	}
	if s.act.stagedDelivery == nil {
		s.act.stagedDelivery = map[string]StagedDelivery{}
	}
	s.act.stagedDelivery[target] = item
}

func (s *activityScanner) recordSearch(args map[string]any) {
	if q, ok := args["query"].(string); ok && strings.TrimSpace(q) != "" {
		s.act.searches = append(s.act.searches, strings.TrimSpace(q))
	}
}

func (s *activityScanner) recordFetch(url string, resp map[string]any) {
	if result, ok := resp["result"].(string); ok && strings.TrimSpace(result) != "" {
		s.act.fetched[url] = struct{}{}
	}
}

// recordCd: tracks cwd for writtenRel resolution (node-relative slash path).
func (s *activityScanner) recordCd(resp map[string]any) {
	if _, failed := resp["error"]; failed {
		return
	}
	if d, ok := resp["dir"].(string); ok {
		if d == "." {
			d = ""
		}
		s.curCwd = d
	}
}

// recordWorkspace: ledger entry + grounding/delivery capture. Failures recorded (must be contradictable).
func (s *activityScanner) recordWorkspace(name string, args, resp map[string]any) {
	s.act.workspace = append(s.act.workspace, recordWsOp(name, args, resp))
	// Grounding capture (successful ops only - failed ops stay in ledger for claim-checking but back no citation).
	if _, failed := resp["error"]; failed {
		return
	}
	switch name {
	case "git_commit":
		s.act.committed = true
	case "git_push":
		s.act.pushed = true
	case "github_add_review_comment":
		s.act.reviewCommented = true
		s.recordPRNumber(args)
	case "github_submit_review":
		s.act.reviewSubmitted = true
		s.recordPRNumber(args)
	case "run_command":
		s.act.ranCommand = true
	case "git_clone":
		if u, ok := args["url"].(string); ok && strings.TrimSpace(u) != "" {
			s.act.clonedRepos = append(s.act.clonedRepos, strings.TrimSpace(u))
		}
		dir, _ := resp["dir"].(string)
		if strings.TrimSpace(dir) == "" {
			dir, _ = args["dir"].(string)
		}
		// Resolved against cwd at clone time via writtenRel.
		if d := normalizePath(writtenRel(s.nodeDir, s.curCwd, dir)); d != "" {
			s.act.clonedDirs = append(s.act.clonedDirs, d)
		}
	case "git_checkout":
		// commitDelivery needs the branch name the worker checked out.
		if br, ok := resp["branch"].(string); ok && strings.TrimSpace(br) != "" {
			s.act.currentBranch = strings.TrimSpace(br)
		}
	case "git_branch":
		if cur, ok := resp["current"].(string); ok && strings.TrimSpace(cur) != "" {
			s.act.currentBranch = strings.TrimSpace(cur)
		}
	case "read_file", "write_file", "edit_file", "delete_path":
		// grounded_in_retrieval treats any read/written path as retrieval evidence (node.go RequireRetrieval check).
		pth, ok := args["path"].(string)
		if !ok {
			return
		}
		if np := normalizePath(pth); np != "" {
			s.act.paths[np] = true
		}
		// Record jail-relative path for judge re-read (buildChangedFilesSection).
		if name == "write_file" || name == "edit_file" {
			if jr := writtenRel(s.nodeDir, s.curCwd, pth); jr != "" && !s.writtenSeen[jr] {
				s.writtenSeen[jr] = true
				s.act.written = append(s.act.written, jr)
			}
		}
	}
}
