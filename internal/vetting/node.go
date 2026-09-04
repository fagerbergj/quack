package vetting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
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
// judge.go is the only place that knows which happened, so it returns a typed
// sentinel rather than this checking the error string (#779).
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
}

const AskToolName = "ask_user"

const memoryCommitTimeout = 3 * time.Minute

// envScaffoldRe matches a leading opencode <env>...</env> preamble.
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
// never affect the run, unlike artifact.revision/judge.round which are
// fail-closed. No-op when cfg.Ledger is unset.
func appendNodeEvent(ctx context.Context, cfg Config, nodeID, turnID, kind string, rounds int) {
	if cfg.Ledger == nil {
		return
	}
	payload, err := json.Marshal(struct {
		NodeID string `json:"node_id"`
		Turn   string `json:"turn"`
		Round  int    `json:"round"`
	}{NodeID: nodeID, Turn: turnID, Round: rounds})
	if err != nil {
		return
	}
	if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
		ChatID: cfg.ChatID, TurnID: turnID, NodeID: nodeID, Kind: kind, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		slog.Warn("ledger node event append failed (observational; run unaffected)", "component", "vetting", "node", nodeID, "kind", kind, "err", err)
	}
}

// appendJudgeRound is the WAL's judge.round intent (#1090 §4.9, fail-closed):
// appended right after a round's verdict is known, so it lands before the
// NEXT round's artifact.revision writes. scored lists the code_review/finding
// ids and revisions THIS round wrote (the judge_round artifact itself is
// #1092 - not built here). No-op (nil error) when cfg.Ledger is unset. A
// non-nil error means the caller must treat this round as failed-closed - it
// must not start another revise round on this verdict, mirroring the
// existing judge-unavailable path just above.
// appendJudgeRound's Ledger==nil no-op is intentional fail-open, matching
// every other episodic write: recording happens independently of whether a
// Postgres-backed ledger is configured, not only when one is present.
func appendJudgeRound(ctx context.Context, cfg Config, nodeID, turnID string, round int, passed bool, score float64, scored []ScoredRef) error {
	if cfg.Ledger == nil {
		return nil
	}
	id := judgeRoundHint(turnID, nodeID, round)
	payload, err := json.Marshal(struct {
		ID     string      `json:"id"`
		Passed bool        `json:"passed"`
		Score  float64     `json:"score"`
		Scored []ScoredRef `json:"scored"`
	}{ID: id, Passed: passed, Score: score, Scored: scored})
	if err != nil {
		return fmt.Errorf("vetting: marshal judge.round payload: %w", err)
	}
	if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
		ChatID: cfg.ChatID, TurnID: turnID, NodeID: nodeID, Kind: ledger.KindJudgeRound, Key: id, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		return fmt.Errorf("vetting: judge.round WAL append for node %s round %d: %w", nodeID, round, err)
	}
	return nil
}

// deliveryTarget resolves the recordstore artifact backing this node's
// delivery, if any: the code_review subject for a reviewer, or cfg.Artifact
// for a document node. false when this delivery has no backing artifact
// (a plain PR-only delivery) - the WAL/delivery_record path is skipped
// entirely in that case (#1093: idempotency key = artifact id + revision,
// nothing to key on without one).
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

// appendDeliveryDone is the WAL's delivery.done entry (#1090 §4.9,
// best-effort/retried per the issue's WAL table - the delivery already
// happened by the time this is called, so a failure here must not undo it).
func appendDeliveryDone(ctx context.Context, cfg Config, nodeID, key, remoteURL string) {
	if cfg.Ledger == nil {
		return
	}
	payload, err := json.Marshal(struct {
		RemoteURL string `json:"remote_url,omitempty"`
	}{RemoteURL: remoteURL})
	if err != nil {
		return
	}
	if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
		ChatID: cfg.ChatID, NodeID: nodeID, Kind: ledger.KindDeliveryDone, Key: key, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		slog.Warn("ledger delivery.done append failed (best-effort; delivery already happened)", "component", "vetting", "node", nodeID, "key", key, "err", err)
	}
}

func RunGatedRefine(ctx adkagent.Context, nodeID string, workerNode workflow.Node, workerModel model.LLM, judge JudgeFactory, cfg Config, prompt string, attachments []*genai.Part, ctrl NodeControl, emit func(*session.Event) error) (answer string, res GateResult, err error) {
	log := slog.With("component", "vetting", "node", nodeID)

	// cfg.NodeID (workspaceNodeID), NOT nodeID - the recorder keys every
	// generate() call on cfg.NodeID (line ~1212 below), which for an
	// implementer node in a setup/repo-chain plan is workspace.SharedRepoScope,
	// not the plan node id nodeID carries (#1109 re-review finding). A node
	// id is reused across turns/plans on the same chat - drop any unconsumed
	// failure record from a previous invocation before this one records its
	// own, so a stale streak can't leak into an unrelated future empty
	// completion (PR #1109 review finding 3).
	inference.ClearFailure(cfg.ChatID, cfg.NodeID, cfg.Agent)

	nodeCtx, span := otelobs.StartNode(ctx,
		attribute.String(otelobs.ChatIDKey, cfg.ChatID),
		attribute.String("node_id", nodeID),
		attribute.String(otelobs.GenAIAgentName, cfg.Agent),
		attribute.String(otelobs.QuackModel, modelName(workerModel)),
	)
	// turnID: closest available stand-in for the store row's turn_id column
	// (#1090 V4.2 point 2) - no chat-turn id is plumbed this deep today
	// (dag/orchestrator carry none either), so the ADK invocation id is the
	// best per-run identity RunGatedRefine actually has. Computed here
	// (rather than at its original use site below) so node.started can be
	// stamped with the same id node.done/node.failed close out on.
	turnID := ctx.InvocationID()
	appendNodeEvent(nodeCtx, cfg, nodeID, turnID, ledger.KindNodeStarted, 0)
	defer func() {
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
		appendNodeEvent(nodeCtx, cfg, nodeID, turnID, doneKind, res.Rounds)
	}()

	// Re-attach advisor-thread marker for tool-bearing rounds.
	markerLine := ""
	advisorToken := ""
	if token, ok := ParseAdvisorThread(prompt); ok {
		markerLine = "\n\n" + AdvisorThreadMarker(token)
		advisorToken = token
	}
	// cfg is a per-call copy; stamping only reaches this node's judge rounds.
	cfg.AdvisorToken = advisorToken
	cfg.NodeBaseSHA = cloneHeadSHA(cfg)
	if advisorToken != "" {
		// Draft round: seed round=1 coords before the first worker call so a
		// tool write during draft (before any judge round runs) still gets
		// real lineage (#1091 finding #4).
		SetAdvisorThreadRound(advisorToken, 1, turnID, cfg.NodeBaseSHA, "")
		if cfg.RoundCoordsSink != nil {
			cfg.RoundCoordsSink(1, turnID, cfg.NodeBaseSHA, "")
		}
	}
	// User attribution: the ADK session identity (mirrors MemoryScope below) -
	// not caller-set, so a node can never claim to run as someone it isn't.
	if s := ctx.Session(); s != nil {
		cfg.User = s.UserID()
	}

	// Memory recall for ACP workers; append after the prompt so sibling nodes'
	// shared BACKGROUND prefix (dag.buildTask) stays a cache hit.
	if cfg.ExternalWorker && cfg.CommitMemory {
		_, recallSpan := otelobs.Start(nodeCtx, "memory.recall",
			attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", nodeID))
		rec := cfg.Memory.Recall(ctx, MemoryScope(ctx, cfg, nodeID), cfg.Task)
		recallSpan.SetAttributes(attribute.Bool("hit", rec != ""))
		recallSpan.End()
		otelobs.RecordMemoryRecall(rec != "")
		if rec != "" {
			prompt = prompt + "\n\n" + rec
			log.Info("recalled memory injected into the worker prompt", "bytes", len(rec))
		}
	}
	// Episodic record preload (#1006): review for reviewer nodes (ancestry +
	// per-file validity filtered), body for reMarkable-style stage nodes (no
	// git filter - these nodes run outside a clone).
	if p := BuildReviewPreload(nodeCtx, cfg, nodeID); p != "" {
		prompt = prompt + p
	}
	if p := BuildBodyPreload(nodeCtx, cfg, nodeID); p != "" {
		prompt = prompt + p
	}

	// Per-node workspace dir prevents concurrent node collision.
	nodeDir := workspace.NodeDir(cfg.NodeID)
	if cfg.Workspace != nil && nodeDir != "" {
		if _, err := cfg.Workspace.EnsureDir(cfg.WorkspaceUserID, cfg.ChatID, nodeDir); err != nil {
			log.Warn("could not create the node's working directory", "dir", nodeDir, "err", err)
		}
	}
	// Replay-ledger coords for gate's disk probes.
	probeCtx := ledger.WithCoords(ctx, ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, Round: probeRound, User: cfg.User, Source: cfg.Source})
	activity := func() workerActivity {
		act := activityFromSessionAt(ctx.Session(), nodeDir)
		augmentFromRepo(probeCtx, &act, cfg)
		return act
	}
	// actFor folds in the staged review (tool-staged first, then answer-tail fallback).
	actFor := func(answer string) workerActivity {
		act := activity()
		augmentFromReviewStage(&act, advisorToken)
		augmentFromAnswer(&act, cfg, answer)
		augmentFromPRStage(&act, advisorToken)
		return act
	}

	cancelled := func() bool { return ctrl != nil && ctrl.Cancelled() }
	paused := func() bool { return ctrl != nil && ctrl.Paused() }
	// Judge SSE stage:judge (never written to session).
	sink, _ := stream.YieldFromContext(ctx)

	// Session events only for A2A workers.
	promptEmit := emit
	if !cfg.DeliverPromptEvent {
		promptEmit = nil
	}

	// Multi-reviewer plans (#867): every reviewer node must resolve its
	// terminal outcome into the run's ReviewFanout, not just the ones that
	// reach commitDelivery below. delivered is set true once commitDelivery
	// has handled that; any other return path (worker error, ErrNodeEmpty,
	// cancel) needs to register as a failed sibling here instead, so a dead
	// reviewer node can never block the run's delivery forever. A paused
	// node is NOT terminal - it may still resume and stage its real verdict
	// later, so a pause sentinel is excluded: every quack pause now returns
	// ErrNodePaused (HITL parks wrap ADK's workflow.ErrNodeInterrupted in it),
	// and a bare ErrNodeInterrupted can still surface from ADK's own scheduler.
	// Registering a merely-parked node as "failed" would let the fan-in
	// deliver without it, then silently discard its verdict on resume.
	delivered := false
	// Must run unconditionally (#942): a staged review lives in this
	// process, not the dying ACP subprocess, so it survives a kill.
	defer func() {
		if delivered || isReviewerPauseSentinel(err) {
			return
		}
		resolveAbortedReviewer(nodeCtx, sink, cfg, nodeID, actFor(answer))
	}()

	// Gate-stage boundary control check.
	basePrompt := prompt
	queueAttempt := 0
	for {
		if cancelled() {
			return "", GateResult{}, nil // cancelled before drafting → empty (continue-but-warn)
		}
		if paused() {
			return "", GateResult{}, ErrNodePaused // paused before drafting → keep whatever this node has (nothing yet)
		}
		// Fresh run ID after queue delivery.
		sfx := ""
		if queueAttempt > 0 {
			sfx = fmt.Sprintf("-s%d", queueAttempt)
		}
		question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}

		// HITL resume: ADK re-entered with ResumedInput.
		var answer string
		var err error
		resumed := false
		if scan := scanNodeAsks(ctx.Session(), ctx.InvocationID(), nodeID); scan.pauses > 0 {
			if reply, ok := ctx.ResumedInput(hitlInterruptID(nodeID, scan.pauses)); ok {
				resumed = true
				// Fill in answer from ctx.ResumedInput.
				turns := scan.turns
				if n := len(turns); n > 0 && turns[n-1].answer == "" {
					turns[n-1].answer = replyString(reply)
				}
				log.Info("node resumed with user answer", "round", scan.pauses)
				answer, err = runWorkerNodeTraced(ctx, nodeCtx, cfg, workerModel, workerNode,
					workerInput(withUserAnswer(prompt, turns), attachments),
					fmt.Sprintf("worker-hitl-r%d%s", scan.pauses, sfx), "hitl", promptEmit)
				if err != nil {
					if cancelled() {
						return "", GateResult{}, nil // ACP round aborted mid-flight by CancelNode, not a real failure
					}
					log.Error("post-answer worker run failed", "err", err)
					return "", GateResult{}, err
				}
			}
		}
		if !resumed {
			if cscan := scanNodeConfirms(ctx.Session(), ctx.InvocationID(), nodeID); cscan.pauses > 0 {
				if reply, ok := ctx.ResumedInput(confirmInterruptID(nodeID, cscan.pauses)); ok {
					resumed = true
					turns := cscan.turns
					if n := len(turns); n > 0 && turns[n-1].answer == "" {
						turns[n-1].answer = replyString(reply)
					}
					log.Info("node resumed with confirm decision", "round", cscan.pauses)
					answer, err = runWorkerNodeTraced(ctx, nodeCtx, cfg, workerModel, workerNode,
						workerInput(withConfirmDecision(prompt, turns), attachments),
						fmt.Sprintf("worker-confirm-r%d%s", cscan.pauses, sfx), "confirm", promptEmit)
					if err != nil {
						if cancelled() {
							return "", GateResult{}, nil // ACP round aborted mid-flight by CancelNode, not a real failure
						}
						log.Error("post-decision worker run failed", "err", err)
						return "", GateResult{}, err
					}
				}
			}
		}
		if !resumed {
			answer, err = runWorkerNodeTraced(ctx, nodeCtx, cfg, workerModel, workerNode, workerInput(prompt, attachments), "worker-r0"+sfx, "draft", promptEmit)
			if err != nil {
				if cancelled() {
					return "", GateResult{}, nil // ACP round aborted mid-flight by CancelNode, not a real failure
				}
				// Log before returning (ADK swallows node errors into silent empty completion).
				log.Error("worker draft failed", "run", "worker-r0", "err", err)
				return "", GateResult{}, err
			}
		}

		// HITL/guard pause: park when ask_user or guard confirmation raised. Draft discarded; resume re-runs with Q&A.
		if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, ctrl, emit, log); paused {
			return "", GateResult{}, ierr // ErrNodePaused (wrapping ADK's park sentinel)
		}

		// Continuation loop: tool-bearing turns until work is done (not until model emits text). Tested against cfg.Task.
		hasDeliverTarget := cfg.Deliver != nil
		if workIncomplete(answer, cfg.Task, actFor(answer), cfg.ReadOnly, hasDeliverTarget, cfg.IsReviewer, cfg.ExistingPR) {
			_, contSpan := otelobs.Start(nodeCtx, "gate.continuation",
				attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", nodeID))
			contAttempts := 0
			for attempt := 1; attempt <= maxContinueRounds && workIncomplete(answer, cfg.Task, actFor(answer), cfg.ReadOnly, hasDeliverTarget, cfg.IsReviewer, cfg.ExistingPR); attempt++ {
				contAttempts = attempt
				act := actFor(answer)
				log.Warn("work not finished; continuing the worker with its tools",
					"attempt", attempt, "empty", strings.TrimSpace(answer) == "", "committed", act.committed, "pushed", act.pushed)
				answer, err = runWorkerNodeTraced(ctx, nodeCtx, cfg, workerModel, workerNode, buildContinuationPrompt(cfg.Task, act, cfg.Checks, cfg.ReadOnly, hasDeliverTarget, cfg.IsReviewer, cfg.ExistingPR)+markerLine,
					fmt.Sprintf("worker-cont%d%s", attempt, sfx), "continuation", promptEmit)
				if err != nil {
					if cancelled() {
						contSpan.End()
						return "", GateResult{}, nil // ACP round aborted mid-flight by CancelNode, not a real failure
					}
					log.Error("worker continuation failed", "attempt", attempt, "err", err)
					contSpan.End()
					return "", GateResult{}, err
				}
				// A continuation is where the worker finally proposes its guarded delivery
				// step (git_commit/git_push) - park the node for the human exactly as the
				// draft and revise paths do.
				if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, ctrl, emit, log); paused {
					contSpan.End()
					return "", GateResult{}, ierr // ErrNodePaused (wrapping ADK's park sentinel)
				}
			}
			contSpan.SetAttributes(attribute.Int("attempts", contAttempts))
			contSpan.End()
		}

		// Last resort: tool-less writer in fresh runner if worker stuck after continuation budget.
		if strings.TrimSpace(answer) == "" {
			log.Warn("worker still empty after continuation; falling back to the tool-less writer", "rounds", maxContinueRounds)
			answer, err = runWriterFresh(ctx, workerModel, buildFinalizeContent(question, activity()), cfg.ChatID)
			if err != nil {
				log.Error("writer recovery failed", "err", err)
				return "", GateResult{}, err
			}
			if strings.TrimSpace(stripLeadingEnvScaffold(answer)) == "" {
				log.Error("worker produced NO answer; writer recovery also empty", "rounds", maxContinueRounds)
				return "", GateResult{}, ErrNodeEmpty
			}
		}

		// Turn-boundary control check, even when no judge round runs at all
		// (cfg.JudgeRounds == 0 or judge == nil skips the loop below entirely) -
		// a paused/cancelled/queued node must be honored here too, not just
		// inside the judge loop.
		if ctrl != nil {
			if ctrl.Cancelled() {
				return answer, GateResult{}, nil
			}
			if ctrl.Paused() {
				return answer, GateResult{}, ErrNodePaused
			}
			if q := ctrl.TakeQueued(); strings.TrimSpace(q) != "" {
				log.Info("node has a queued message; re-running with it", "node", nodeID)
				queueAttempt++
				prompt = basePrompt + "\n\n--- Queued user message (address this before continuing) ---\n" + q
				continue
			}
		}

		// Judge/revise loop: judge, fold deterministic criteria, revise on fail.
		var res GateResult
		var checksSkipReason string // last computeDeterministicCriteria skip reason; attached to res below (#780)
		queuedText := ""
		// episodicState: cross-round (and, seeded once, cross-turn) tracking
		// for the episodic record write site below - live findings by hash id
		// plus the last-known revision of every code_review/finding/document
		// id, so a re-review turn stamps true parent_revision instead of
		// fabricating one and correctly marks repeats "unchanged" (#1090 P2).
		// nil until first touched; saveEpisodicRound seeds it from the store
		// on that first call.
		var episodicState *episodicRoundState
		episodicRoundsWritten := 0
		// JudgeRounds counts revisions: round r judges, on fail revises (N rounds = N revisions / N+1 judgments).
		for round := 1; judge != nil && cfg.JudgeRounds > 0 && round <= cfg.JudgeRounds+1; round++ {
			// Cooperative cancel/pause/queue before each judge round.
			if ctrl != nil {
				if ctrl.Cancelled() {
					return answer, res, nil
				}
				if ctrl.Paused() {
					return answer, res, ErrNodePaused
				}
				if q := ctrl.TakeQueued(); strings.TrimSpace(q) != "" {
					queuedText = q
					break
				}
			}
			if strings.TrimSpace(stripLeadingEnvScaffold(answer)) == "" {
				break // still nothing to judge after recovery
			}
			if advisorToken != "" {
				var trigger string
				if episodicState != nil {
					trigger = episodicState.triggerAnnotation
				}
				// Intentional: this round's revise (below, on judge fail) also
				// stamps Round=round, even though its tool writes are first
				// referenced by round+1's code_review - "round r judges, on
				// fail revises" (line 557), so a revision belongs to the
				// judgment that required it, not the round that later reads it.
				SetAdvisorThreadRound(advisorToken, round, turnID, cfg.NodeBaseSHA, trigger)
				if cfg.RoundCoordsSink != nil {
					cfg.RoundCoordsSink(round, turnID, cfg.NodeBaseSHA, trigger)
				}
			}
			act := actFor(answer)
			// Every judge round writes a revision, gate-passed or not - only
			// delivery stays gate-passed-only (#1090 P2: rounds are history).
			// Every gated node writes one, not just reviewer/document nodes
			// (#1095/#1090 P8: saveEpisodicRound falls back to "text:<node>").
			episodicState = saveEpisodicRound(nodeCtx, cfg, nodeID, turnID, round, answer, act.stagedDelivery["review"], episodicState)
			episodicRoundsWritten++
			runID := fmt.Sprintf("judge-r%d", round)
			judgeCtx, jspan := startStageSpan(nodeCtx, sink, cfg, nodeID, "judge", stream.StageJudge, runID, round)
			// Replay-ledger coords for judge round (via context.WithValue, not adkagent.Context).
			// Node: cfg.NodeID (not nodeID) matches the worker recorder's own
			// key (see RunGatedRefine's entry-clear above) - both must agree
			// on the same workspace scope for setup/repo-chain plans.
			judgeCoords := ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: "judge", Round: runID, User: cfg.User, Source: cfg.Source}
			ledgerCtx := ledger.WithCoords(ctx, judgeCoords)
			// Same belt-and-suspenders as runWorkerNodeTraced's workerModel stamp.
			if cs, ok := cfg.JudgeModel.(interface{ SetLedgerCoords(ledger.Coords) }); ok {
				cs.SetLedgerCoords(judgeCoords)
			}
			// Nothing reads a "judge"-role failure record today - clear it on
			// entry so a failed judge round doesn't leave a permanent orphan
			// waiting for a judge success that may never come (#1109
			// re-review suggestion).
			inference.ClearFailure(cfg.ChatID, cfg.NodeID, "judge")
			// Compute deterministic criteria before judge runs.
			det, skip := computeDeterministicCriteria(judgeCtx, answer, act, cfg)
			if skip != "" {
				checksSkipReason = skip
			}
			v, jerr := runJudgeAgent(ledgerCtx, judge, cfg, question, answer, act, det, judgePartEmitter(sink, nodeID, runID))
			if jerr != nil {
				// Judge failure means answer goes out unvetted - loud ERROR, not Warn.
				log.Error("judge failed; surfacing answer unvetted", "round", round, "err", jerr)
				status, feedback := judgeFailureFeedback(jerr)
				jspan.end(stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Status: status, Reason: jerr.Error()}, jerr)
				otelobs.RecordJudgeUnavailable(cfg.Agent)
				// Fail closed but fall through to deliver-with-caveat (only that path writes the review verdict marker).
				res = GateResult{Score: 0, Passed: false, Feedback: feedback, Rounds: round}
				break
			}
			if isNonDeliveringSlice(cfg) {
				// Fan-out (#1092, design V4 §4.6): a reviewer node feeding a
				// synthesizer never owns the delivered verdict, so its own
				// structured_verdict/VERDICT-consistency score would gate on
				// something this node never controls.
				v = dropCriteria(v, "structured_verdict")
			}
			v = sanitizeAnchors(v, answer, cfg)
			v = mergeDeterministic(v, det, cfg)
			v = applyRubricSpecs(v, cfg.RubricSpecs)
			env, feedback := composeFeedback(v, cfg.Threshold, round)
			res = GateResult{Passed: env.Passed, Score: v.Score, Feedback: feedback, Rounds: round}
			var scored []ScoredRef
			if episodicState != nil {
				scored = episodicState.roundWrites
			}
			if walErr := appendJudgeRound(nodeCtx, cfg, nodeID, turnID, round, res.Passed, res.Score, scored); walErr != nil {
				// Fail-closed (#1090 §4.9): the WAL entry for this round's
				// verdict didn't land, so don't start another revise round on
				// it - surface it the same as an unavailable judge.
				log.Error("judge.round WAL append failed; stopping the round loop", "round", round, "err", walErr)
				verdictWord := "failed"
				if env.Passed {
					verdictWord = "passed"
				}
				res.Passed = false
				res.Feedback = fmt.Sprintf("Round %d %s (score %.2f) but could not be recorded in the write-ahead log; treating as failed.", round, verdictWord, v.Score)
				break
			}
			// judge_round record (#1092): written right after the WAL entry
			// above (§4.9 ordering), but independently of it - a nil cfg.Ledger
			// made the WAL append a no-op just above, yet this record and both
			// SSE events below still fire. Intentional fail-open, same as every
			// other episodic write, not a bug: every non-Postgres deploy still
			// gets the record even with no WAL to materialize.
			jr := buildJudgeRoundRecord(turnID, round, res.Passed, res.Score, scored, v, det, answer)
			jrID, _ := saveJudgeRoundRecord(nodeCtx, cfg, nodeID, turnID, round, jr)
			for _, sr := range scored {
				emitArtifactRevision(sink, sr.ArtifactID, sr.Revision, recordstore.KindOf(sr.ArtifactID), nodeID, round)
			}
			if jrID != "" {
				if episodicState != nil {
					// Next round's writes point back at THIS round's verdict
					// (design V4 §7 case 3's trigger_annotation chain).
					episodicState.triggerAnnotation = jrID
				}
				emitJudgeRound(sink, jrID, res.Passed, res.Score, scored)
			}
			emitEvaluationResults(ledgerCtx, runID, v)
			jspan.end(stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Score: res.Score, Passed: res.Passed, Feedback: res.Feedback, Envelope: env}, nil)
			log.Info("judge round done", "round", round, "score", v.Score, "passed", res.Passed)
			otelobs.RecordJudgeVerdict(cfg.Agent, res.Score, res.Passed)
			// Debug: per-criterion reasoning for diagnosable gate failures.
			if len(v.Criteria) > 0 && log.Enabled(context.Background(), slog.LevelDebug) {
				log.Debug("judge verdict detail", "round", round, "criteria", formatCriteriaDetail(v.Criteria), "feedback", strings.TrimSpace(v.Feedback))
			}
			if res.Passed || round > cfg.JudgeRounds {
				break
			}
			// Re-check now that the verdict is known to have failed: the judge's
			// own model call can run long, and a revise round after it is another
			// full worker round - a cancel landing during the judge call must stop
			// here too, not just at the top of the next round (#879).
			if ctrl != nil {
				if ctrl.Cancelled() {
					return answer, res, nil
				}
				if ctrl.Paused() {
					return answer, res, ErrNodePaused
				}
				if q := ctrl.TakeQueued(); strings.TrimSpace(q) != "" {
					queuedText = q
					break
				}
			}
			// #762: undo this round's commits before the worker gets another
			// try, but only if commit_hygiene says it swept in off-task work -
			// an ordinary incomplete/wrong round keeps its commits so revise
			// builds on them instead of redoing the change from scratch.
			resetCloneToNodeBase(cfg, v)
			revisePrompt := contentPlainText(buildRevisionContent(cfg.Constitution, question, answer, env, act, citationOnlyFailure(v, cfg.Threshold), jr.Notes)) + markerLine
			reviseRunID := fmt.Sprintf("worker-r%d%s", round, sfx)
			// gate.revise spans the round through the same choke point gate.judge
			// uses. sink is nil: the matching agent_start/complete SSE for this run
			// already comes from dagStream off the worker's own session events, so
			// this raises only the span half.
			reviseCtx, rspan := startStageSpan(nodeCtx, nil, cfg, nodeID, "revise", stream.StageRevise, reviseRunID, round)
			revised, rerr := runWorkerNodeTraced(ctx, reviseCtx, cfg, workerModel, workerNode, revisePrompt, reviseRunID, "revise", promptEmit)
			rspan.end(stream.AgentCompleteData{RunID: reviseRunID, Stage: stream.StageRevise, Round: round}, rerr)
			if rerr != nil {
				log.Error("revision worker failed; keeping prior answer", "round", round, "err", rerr)
				return answer, res, nil // revision failed; keep the prior answer
			}
			// A revision can itself raise ask_user/guard confirmation - park exactly as draft-time check does.
			if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, ctrl, emit, log); paused {
				return "", GateResult{}, ierr // ErrNodePaused (wrapping ADK's park sentinel)
			}
			if strings.TrimSpace(revised) != "" {
				answer = revised
			}
		}
		if queuedText != "" {
			log.Info("node has a queued message; re-running with it", "node", nodeID)
			queueAttempt++
			prompt = basePrompt + "\n\n--- Queued user message (address this before continuing) ---\n" + queuedText
			continue // re-run the whole gate with the message folded in (fresh run IDs)
		}
		act := actFor(answer)
		// Fold in ACP memory MCP stage_memory from all rounds; unregister after drain (straggler calls fail).
		if advisorToken != "" {
			if t, ok := LookupAdvisorThread(advisorToken); ok && t.MemSecret != "" {
				if ms, ok := LookupMemSession(t.MemSecret); ok {
					if ms.Staged != nil {
						act.staged = append(act.staged, ms.Staged.Drain()...)
					}
					UnregisterMemSession(t.MemSecret)
				}
			}
		}
		res.ChecksSkipReason = checksSkipReason
		if res.Passed {
			commitMemoryOnPass(ctx, nodeCtx, cfg, nodeID, answer, act.staged)
		}
		// A judge-less node (JudgeRounds == 0, e.g. a deterministic-only
		// reMarkable stage) never entered the round loop above - write its
		// one round here so it still gets a code_review/document/text record.
		// Mirrors the round loop's own empty-answer guard (line 658) so an
		// empty/whitespace-only answer doesn't produce an empty text revision.
		if episodicRoundsWritten == 0 && strings.TrimSpace(stripLeadingEnvScaffold(answer)) != "" {
			saveEpisodicRound(nodeCtx, cfg, nodeID, turnID, 1, answer, act.stagedDelivery["review"], nil)
		}
		// Deliver even on judge FAIL (graceful degradation). Memory stays pass-only.
		delivered = true
		act.answer = answer
		commitDelivery(nodeCtx, sink, cfg, nodeID, act, res)
		return answer, res, nil
	}
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

// parkForInput folds an ADK HITL park into the node state machine: mark the
// control paused/awaiting_input with the question (persisted before the park
// is acted on), then wrap ADK's sentinel in ErrNodePaused so quack code sees
// one sentinel. ErrNodeInterrupted stays in the chain because ADK's engine
// keys the park itself off it - dropping it would fail the node instead.
func parkForInput(ctrl NodeControl, question string, ierr error) error {
	if !errors.Is(ierr, workflow.ErrNodeInterrupted) {
		return ierr // emit failure, not a park
	}
	if ctrl != nil {
		ctrl.PauseForInput(question)
	}
	return fmt.Errorf("%w: %w", ErrNodePaused, ierr)
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
	sc := MemoryScope(ctx, cfg, author)
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
	// Multi-reviewer plan (#867): this node's own review never goes out on
	// its own - it's handed to the run's ReviewFanout, which delivers the
	// merged, worst-of-verdict review exactly once, when every reviewer
	// node in the plan has finished.
	if cfg.ReviewFanout != nil && !cfg.IsReviewer {
		// Synthesizer node (#965): its answer is the plan's consolidated
		// review - hand it to the fan-in, which delivers exactly once. The
		// structured code_review verdict (#1184) is read here rather than
		// parsed from act.answer, since a native write_code_review leaves no
		// VERDICT tail in the answer text.
		verdict, _ := LatestCodeReviewVerdict(ctx, cfg)
		merged, deliverNow := cfg.ReviewFanout.FinishSynthesis(act.answer, verdict)
		if deliverNow {
			deliverMergedReview(ctx, sink, cfg, nodeID, merged)
		}
		if len(act.stagedDelivery) == 0 {
			recordDeliveryOutcomeMetric(cfg, res, false, false)
			return
		}
		cfg.ReviewFanout = nil
	}
	if cfg.ReviewFanout != nil {
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
		cloneURL, branch := resolveCloneCoordinates(cfg, act)
		cfg.ReviewFanout.RecordClone(cloneURL, branch)
		merged, deliverNow := cfg.ReviewFanout.Finish(nodeID, item, hasItem, false)
		if deliverNow {
			deliverMergedReview(ctx, sink, cfg, nodeID, merged)
		}
		if len(act.stagedDelivery) == 0 {
			recordDeliveryOutcomeMetric(cfg, res, false, false)
			return
		}
	}
	if cfg.Deliver == nil || len(act.stagedDelivery) == 0 {
		recordDeliveryOutcomeMetric(cfg, res, false, false)
		// Phantom-success: delivery-capable node with judge-passed work that staged nothing.
		if !cfg.ReadOnly && res.Passed {
			emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeNone, "", "", "", otelobs.TraceIDOf(ctx)))
		}
		return
	}
	// Render from the durable record instead of the worker's own restatement
	// (#1093 P6/P10) - every final round writes its code_review/document
	// revision (saveEpisodicRound runs pass or fail), so a draft-on-fail
	// delivery renders and records the SAME revision it posts, never the
	// staged text (finding 2: a staged-text post must never be recorded as
	// an artifact-backed delivery).
	renderedFromStaged := act.skipArtifactRender
	if !renderedFromStaged {
		act.stagedDelivery, renderedFromStaged = artifactRenderedDelivery(ctx, cfg, nodeID, act.stagedDelivery)
	}
	// #1198 part C: a review with comments/findings but no verdict is not a
	// reviewed PR - refuse rather than post an unlabeled GitHub review.
	if item, ok := act.stagedDelivery["review"]; ok && strings.TrimSpace(item.Event) == "" {
		slog.Error("delivery refused: staged review has no verdict", "component", "vetting", "node", nodeID)
		emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeFailed,
			item.Kind, "", "you staged comments but no verdict - call stage_review with an event (approve/request_changes/comment) before this round ends", otelobs.TraceIDOf(ctx)))
		recordDeliveryOutcomeMetric(cfg, res, true, false)
		return
	}
	spanCtx, span := otelobs.Start(ctx, "delivery",
		attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", nodeID))
	defer span.End()
	traceID := otelobs.TraceIDOf(spanCtx)

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
	// Mermaid validity is checked by mermaidCriterion before this point.

	// Permission boundary: drop ungranted items before they reach cfg.Deliver. Refusals are loud, never silent.
	if allowed, refused, reasons := partitionByAllowedKinds(dc.Items, cfg.AllowedDeliveryKinds); len(refused) > 0 {
		for i, item := range refused {
			slog.Error("delivery refused: ungranted kind", "component", "vetting",
				"node", nodeID, "kind", item.Kind, "reason", reasons[i])
			emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeFailed,
				item.Kind, "", "delivery refused: "+reasons[i], traceID))
		}
		dc.Items = allowed
		if len(dc.Items) == 0 {
			recordDeliveryOutcomeMetric(cfg, res, true, false)
			return
		}
	}

	kinds := make([]string, len(dc.Items))
	for i, item := range dc.Items {
		kinds[i] = item.Kind
	}
	span.SetAttributes(attribute.StringSlice("staged_kinds", kinds))

	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// delivery.intent (#1093 §4.9, fail-closed): only when this delivery is
	// tied to a recordstore artifact revision (reviewer or cfg.Artifact
	// nodes) - a plain PR-only delivery with no backing artifact has nothing
	// to key a WAL entry on, and stays exactly as before (no WAL, no
	// recovery to reconcile). A staged-text fallback render (finding 2) is
	// never treated as artifact-backed either, even when a target exists -
	// what got posted is not what the target revision holds.
	targetID, targetRev, hasTarget := deliveryTarget(ctx, cfg)
	hasTarget = hasTarget && !renderedFromStaged
	var idemKey string
	if hasTarget {
		idemKey = deliveryIdempotencyKey(targetID, targetRev)
		dc.IdempotencyKey = idemKey
		if walErr := appendDeliveryIntent(cctx, cfg, nodeID, idemKey, targetID, targetRev, dc.CloneURL, dc.IssueNumber); walErr != nil {
			slog.Error("delivery.intent WAL append failed; not delivering", "component", "vetting", "node", nodeID, "err", walErr)
			itemOutcomes := make([]DeliveryItemOutcome, len(dc.Items))
			for i, item := range dc.Items {
				itemOutcomes[i] = DeliveryItemOutcome{Kind: item.Kind, Error: "delivery.intent WAL append failed: " + walErr.Error()}
				emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeFailed, item.Kind, "", itemOutcomes[i].Error, traceID))
			}
			recordDeliveryOutcomeMetric(cfg, res, true, false)
			otelobs.End(span, walErr)
			return
		}
	}

	// Gate-owned push: lands on the remote before any item reaches the
	// extension. A push failure still reaches Deliver (carried on
	// dc.PushError) instead of short-circuiting it - the extension is the
	// only thing that can tell the human on GitHub a delivery failed (#1155);
	// it's expected to skip the push-dependent items using PushError rather
	// than attempt them against a branch that was never pushed.
	if pushErr := ensurePush(cctx, cfg, &dc); pushErr != nil {
		slog.Error("gate push failed", "component", "vetting", "node", nodeID, "err", pushErr, "branch", dc.Branch)
		dc.PushError = pushErr.Error()
	}
	itemOutcomes, err := cfg.Deliver(cctx, dc)
	if dc.PushError != "" && err == nil {
		// A Deliver that doesn't check PushError yet may report success;
		// the push itself never landed, so that outweighs its own report.
		err = errors.New(dc.PushError)
	}
	span.SetAttributes(attribute.Bool("delivered", err == nil))
	otelobs.End(span, err)

	if hasTarget {
		// #1187: the GitHub side effect already happened by this point, so the
		// bookkeeping below must survive a run-level cancel (shutdown drain,
		// hub.CancelRun) landing between Deliver returning and here - it runs
		// on a context detached from ctx's cancellation, with its own budget.
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
			appendDeliveryDone(bctx, cfg, nodeID, idemKey, remoteURL)
			saveDeliveryRecord(bctx, cfg, nodeID, DeliveryRecord{
				TargetID: targetID, DeliveredRevision: targetRev, RemoteURL: remoteURL, PRNumber: dc.IssueNumber, At: time.Now().UTC(),
				GatePassed: res.Passed, RenderedFromStaged: renderedFromStaged,
			})
		} else {
			// No appendDeliveryDone: the WAL entry stays open so `quack ledger
			// recover` can reconcile this attempt instead of treating it as done.
			saveDeliveryRecord(bctx, cfg, nodeID, DeliveryRecord{
				TargetID: targetID, DeliveredRevision: targetRev, PRNumber: dc.IssueNumber, At: time.Now().UTC(),
				GatePassed: res.Passed, RenderedFromStaged: renderedFromStaged, Error: err.Error(),
			})
		}
	}

	// Extension's record is authoritative; fall back to synthetic outcomes only when extension reported nothing.
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

	if err != nil {
		slog.Error("delivery failed", "component", "vetting", "node", nodeID, "err", err, "items", len(dc.Items))
		return
	}
	slog.Info("delivery committed", "component", "vetting", "node", nodeID, "count", len(dc.Items))
}

// deliverMergedReview: posts the fan-in's merged review as this node's own
// single-item delivery. cfg.ReviewFanout is cleared first so the recursive
// commitDelivery call goes straight through instead of re-intercepting.
// A merge with nothing valid in it (every reviewer node failed/staged
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
	// itself - fall back to a reviewer sibling's clone coordinates (#1059).
	// The merge is already the final worst-of text; a reviewer-node terminal
	// (latent plan shape, see #1118 review) must never let the render
	// clobber it with that node's own individual code_review record.
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{"review": merged}, currentBranch: branch, skipArtifactRender: true}
	if cloneURL != "" {
		act.clonedRepos = []string{cloneURL}
	}
	commitDelivery(ctx, sink, cfg, nodeID, act, GateResult{Passed: true})
}

// isReviewerPauseSentinel: true when err means "parked, may still resume" -
// not a terminal outcome for the review fan-in. Both of RunGatedRefine's own
// early-return sentinel ErrNodePaused (every quack pause, HITL included) and
// ADK's own workflow.ErrNodeInterrupted (which the scheduler can still raise
// on its own, so it stays recognised here) mean
// the node is not done - registering it as failed here would let the fan-in
// deliver without it, then silently discard its real verdict on resume.
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
		merged, deliverNow = cfg.ReviewFanout.FinishSynthesis("", "")
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
// a node's own verdict is never what delivery renders, so it isn't judged on
// structured_verdict.
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

// MemoryScope: node's memory entitlement (repo, role, user, legacy key). Exported for ACP MCP surface.
func MemoryScope(ctx adkagent.Context, cfg Config, author string) memory.Scope {
	sc := memory.Scope{Role: cfg.MemoryRole, Legacy: author}
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
// (rerouted at the REST/plan entry boundary) - real bytes are swapped back
// in only at the model boundary (internal/inference's hydratingModel).
func workerInput(prompt string, attachments []*genai.Part) any {
	if len(attachments) == 0 {
		return prompt
	}
	return &genai.Content{Role: "user", Parts: append([]*genai.Part{{Text: prompt}}, attachments...)}
}

// gatePromptAuthor authors the prompt-delivery events emitPrompt writes. NOT
// "user" (a user-authored event would split a chat turn in store.
// groupSessionEvents and confuse the runner's turn detection) and never an
// agent's name (remoteagent presents foreign-authored events to the remote
// model as user messages - exactly what a prompt should be).
const gatePromptAuthor = "quack-gate"

// emitPrompt writes the worker's prompt into the session as a gate-authored
// event, immediately before the RunNode that consumes it. A local llmagent takes
// RunNode input directly, but production workers are A2A REMOTE agents, which
// build their outbound message from SESSION EVENTS ONLY - without this event a
// remote worker never sees its task, and an empty session tail skips the
// dispatch entirely. emit completes durably before it returns, so there is no
// ordering race. The event is filtered everywhere else by its author/branch.
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
	coords := ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, Round: runID, User: cfg.User, Source: cfg.Source, SpanContext: ts.Span.SpanContext()}
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
// criterion name (#941). A static table rather than editing each of the ~10
// constructor sites (checks.go, mermaid.go, shape.go, vacuoustests.go,
// delivery.go) - the criterion names are a fixed, code-owned set, so one
// lookup keyed by name is a smaller diff with the same effect.
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
// (loaded from the node's rubric.yaml) win when the rubric names the
// criterion (e.g. cites_sources in web-researcher/synthesizer's rubric.yaml);
// deterministicCriterionSpec/citesSourcesBands below are the fallback for
// criteria no rubric declares (checks_pass, delivery_complete, ...) or when
// no structured rubric was loaded for this node at all.
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

// composeFeedback builds the #941 structured envelope from v and a rendered
// one-paragraph summary for callers that still want prose: AgentCompleteData.Feedback
// (kept for one release so the UI does not blank) and GateResult.Feedback (the
// delivery-caveat text). The envelope itself - not this summary - is what
// buildRevisionContent hands the worker.
func composeFeedback(v verdict, threshold float64, round int) (verdictEnvelope, string) {
	env := buildEnvelope(v, threshold, round)
	return env, renderFeedbackSummary(env, v.Feedback, v.Findings)
}

// renderFeedbackSummary: prose rendering of the envelope, in the same
// deterministic-leads/judge-follows shape composeFeedback used before #941 -
// a code-owned failure has one correct fix, a low judge score is arguable, and
// collapsing them together misrepresents the judge's opinion as decided (#791).
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

// writtenRel resolves a worker's write/edit path to a CHAT-relative path for
// Jail.Resolve - the read-side mirror of tools.jailPath. Worker paths are
// NODE-relative (the node dir is invisible to the model), re-applied here; a
// leading "/" is the chat-root escape hatch, ignoring both. Must match the
// judge's resolution, or it silently reads NOTHING (has bitten twice).
func writtenRel(nodeDir, cwd, p string) string {
	if strings.HasPrefix(p, "/") {
		return strings.TrimPrefix(p, "/")
	}
	return joinWritten(nodeDir, joinWritten(cwd, p))
}

// activityFromSessionAt: replays worker's session inside nodeDir. Paths come back chat-relative.
func activityFromSessionAt(sess session.Session, nodeDir string) workerActivity {
	s := &activityScanner{
		act:         workerActivity{fetched: map[string]struct{}{}, seen: map[string]string{}, paths: map[string]bool{}},
		nodeDir:     nodeDir,
		writtenSeen: map[string]bool{},
	}
	if sess == nil {
		return s.act
	}
	pending := map[string]string{}
	pendingWs := map[string]map[string]any{}
	pendingWsTool := map[string]string{}
	pendingCd := map[string]bool{}
	for ev := range sess.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil {
				switch p.FunctionCall.Name {
				case "web_search":
					s.recordSearch(p.FunctionCall.Args)
				case "web_fetch":
					if u, ok := p.FunctionCall.Args["url"].(string); ok && strings.TrimSpace(u) != "" {
						pending[p.FunctionCall.ID] = strings.TrimSpace(u)
					}
					// Route into workspace ledger (web_fetch signals web-sourced claims).
					pendingWs[p.FunctionCall.ID] = p.FunctionCall.Args
					pendingWsTool[p.FunctionCall.ID] = "web_fetch"
				case "stage_memory":
					if cand, ok := stagedCandidate(p.FunctionCall); ok {
						s.act.staged = append(s.act.staged, cand)
					}
				case "stage_pr", "stage_review", "stage_comment", "unstage":
					s.applyDelivery(p.FunctionCall)
				case "cd":
					pendingCd[p.FunctionCall.ID] = true
				default:
					if isWorkspaceTool(p.FunctionCall.Name) {
						pendingWs[p.FunctionCall.ID] = p.FunctionCall.Args
						pendingWsTool[p.FunctionCall.ID] = p.FunctionCall.Name
					}
				}
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "web_fetch" {
				if url, known := pending[p.FunctionResponse.ID]; known {
					delete(pending, p.FunctionResponse.ID)
					s.recordFetch(url, p.FunctionResponse.Response)
				}
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "web_search" {
				recordSearchResults(s.act.seen, p.FunctionResponse.Response)
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "cd" {
				if pendingCd[p.FunctionResponse.ID] {
					delete(pendingCd, p.FunctionResponse.ID)
					s.recordCd(p.FunctionResponse.Response)
				}
			}
			if p.FunctionResponse != nil && isWorkspaceTool(p.FunctionResponse.Name) {
				// Only completed call/response pairs enter the ledger.
				if args, known := pendingWs[p.FunctionResponse.ID]; known && pendingWsTool[p.FunctionResponse.ID] == p.FunctionResponse.Name {
					delete(pendingWs, p.FunctionResponse.ID)
					delete(pendingWsTool, p.FunctionResponse.ID)
					s.recordWorkspace(p.FunctionResponse.Name, args, p.FunctionResponse.Response)
				}
			}
		}
	}
	return s.act
}

// activityScanner: accumulates one worker's activity. Recorders reached from both session-event and replay paths.
type activityScanner struct {
	act         workerActivity
	nodeDir     string
	curCwd      string          // node-relative cwd ("" = node root)
	writtenSeen map[string]bool // dedup for written
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
