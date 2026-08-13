package vetting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/otelobs"
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

func RunGatedRefine(ctx adkagent.Context, nodeID string, workerNode workflow.Node, workerModel model.LLM, judge JudgeFactory, cfg Config, prompt string, attachments []*genai.Part, ctrl NodeControl, emit func(*session.Event) error) (answer string, res GateResult, err error) {
	log := slog.With("component", "vetting", "node", nodeID)

	nodeCtx, span := otelobs.StartNode(ctx,
		attribute.String(otelobs.ChatIDKey, cfg.ChatID),
		attribute.String("node_id", nodeID),
		attribute.String("agent", cfg.Agent),
		attribute.String("model", modelName(workerModel)),
	)
	defer func() {
		span.SetAttributes(
			attribute.Bool("verdict_passed", res.Passed),
			attribute.Float64("verdict_score", res.Score),
			attribute.Int("gate_rounds", res.Rounds),
		)
		otelobs.EndNode(span, err)
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
						log.Error("post-decision worker run failed", "err", err)
						return "", GateResult{}, err
					}
				}
			}
		}
		if !resumed {
			answer, err = runWorkerNodeTraced(ctx, nodeCtx, cfg, workerModel, workerNode, workerInput(prompt, attachments), "worker-r0"+sfx, "draft", promptEmit)
			if err != nil {
				// Log before returning (ADK swallows node errors into silent empty completion).
				log.Error("worker draft failed", "run", "worker-r0", "err", err)
				return "", GateResult{}, err
			}
		}

		// HITL/guard pause: park when ask_user or guard confirmation raised. Draft discarded; resume re-runs with Q&A.
		if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, emit, log); paused {
			return "", GateResult{}, ierr // ErrNodeInterrupted → park
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
					log.Error("worker continuation failed", "attempt", attempt, "err", err)
					contSpan.End()
					return "", GateResult{}, err
				}
				// A continuation is where the worker finally proposes its guarded delivery
				// step (git_commit/git_push) - park the node for the human exactly as the
				// draft and revise paths do.
				if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, emit, log); paused {
					contSpan.End()
					return "", GateResult{}, ierr // ErrNodeInterrupted → park
				}
			}
			contSpan.SetAttributes(attribute.Int("attempts", contAttempts))
			contSpan.End()
		}

		// Last resort: tool-less writer in fresh runner if worker stuck after continuation budget.
		if strings.TrimSpace(answer) == "" {
			log.Warn("worker still empty after continuation; falling back to the tool-less writer", "rounds", maxContinueRounds)
			answer, err = runWriterFresh(ctx, workerModel, buildFinalizeContent(question, activity()))
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
			act := actFor(answer)
			runID := fmt.Sprintf("judge-r%d", round)
			judgeCtx, jspan := startStageSpan(nodeCtx, sink, cfg, nodeID, "judge", stream.StageJudge, runID, round)
			// Replay-ledger coords for judge round (via context.WithValue, not adkagent.Context).
			judgeCoords := ledger.Coords{ChatID: cfg.ChatID, Node: nodeID, Agent: "judge", Round: runID, User: cfg.User, Source: cfg.Source}
			ledgerCtx := ledger.WithCoords(ctx, judgeCoords)
			// Same belt-and-suspenders as runWorkerNodeTraced's workerModel stamp.
			if cs, ok := cfg.JudgeModel.(interface{ SetLedgerCoords(ledger.Coords) }); ok {
				cs.SetLedgerCoords(judgeCoords)
			}
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
			// Adversarial verify: load-bearing passing criteria get refuted by independent skeptics.
			// ledgerCtx, not judgeCtx: skeptics are their own model calls and need the same coords.
			v = adversarialVerify(ledgerCtx, cfg, question, answer, act, v, judgePartEmitter(sink, nodeID, runID+"-skeptic"))
			v = mergeDeterministic(v, det)
			feedback := composeFeedback(v, cfg.Threshold)
			res = GateResult{Passed: v.Score >= cfg.Threshold, Score: v.Score, Feedback: feedback, Rounds: round}
			emitEvaluationResults(ledgerCtx, runID, v)
			jspan.end(stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Score: res.Score, Passed: res.Passed, Feedback: res.Feedback}, nil)
			log.Info("judge round done", "round", round, "score", v.Score, "passed", res.Passed)
			otelobs.RecordJudgeVerdict(cfg.Agent, res.Score, res.Passed)
			// Debug: per-criterion reasoning for diagnosable gate failures.
			if len(v.Criteria) > 0 && log.Enabled(context.Background(), slog.LevelDebug) {
				parts := make([]string, 0, len(v.Criteria))
				for name, cs := range v.Criteria {
					parts = append(parts, fmt.Sprintf("%s=%.0f (%s)", name, cs.Score, strings.TrimSpace(cs.Reason)))
				}
				sort.Strings(parts)
				log.Debug("judge verdict detail", "round", round, "criteria", strings.Join(parts, " | "), "feedback", strings.TrimSpace(v.Feedback))
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
			revisePrompt := contentPlainText(buildRevisionContent(cfg.Constitution, question, answer, feedback, act, citationOnlyFailure(v, cfg.Threshold))) + markerLine
			revised, rerr := runWorkerNodeTraced(ctx, nodeCtx, cfg, workerModel, workerNode, revisePrompt, fmt.Sprintf("worker-r%d%s", round, sfx), "revise", promptEmit)
			if rerr != nil {
				log.Error("revision worker failed; keeping prior answer", "round", round, "err", rerr)
				return answer, res, nil // revision failed; keep the prior answer
			}
			// A revision can itself raise ask_user/guard confirmation - park exactly as draft-time check does.
			if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, emit, log); paused {
				return "", GateResult{}, ierr // ErrNodeInterrupted → park
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
		// Deliver even on judge FAIL (graceful degradation). Memory stays pass-only.
		commitDelivery(nodeCtx, sink, cfg, nodeID, act, res)
		return answer, res, nil
	}
}

// pauseIfWorkerRaisedHITL: parks node on new ask_user/guard confirmation. Runs after every worker run.
func pauseIfWorkerRaisedHITL(ctx adkagent.Context, nodeID string, emit func(*session.Event) error, log *slog.Logger) (bool, error) {
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
		return true, ierr
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
		return true, ierr
	}
	return false, nil
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
		cctx, commitSpan := otelobs.StartLinked(cctx, "memory.commit", parentSC, attribute.String("agent", author))
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

// commitDelivery: posts final staged delivery exactly once. Blocking (delivery failure is user-visible).
func commitDelivery(ctx context.Context, sink func(stream.SSEEvent), cfg Config, nodeID string, act workerActivity, res GateResult) {
	if cfg.Deliver == nil || len(act.stagedDelivery) == 0 {
		recordDeliveryOutcomeMetric(cfg, res, false, false)
		// Phantom-success: delivery-capable node with judge-passed work that staged nothing.
		if !cfg.ReadOnly && res.Passed {
			emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeNone, "", "", "", otelobs.TraceIDOf(ctx)))
		}
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
	if cfg.Setup != nil {
		// Deliver on setup branch (worker's git-tracking ledger is off-limits for setup-provisioned workers).
		dc.Branch = cfg.Setup.WorkBranch
		dc.CloneURL = cfg.Setup.Repo
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

	// Gate-owned push: lands on the remote before any item reaches the
	// extension. A push failure never reaches Deliver - nothing was attempted.
	var itemOutcomes []DeliveryItemOutcome
	err := ensurePush(cctx, cfg, &dc)
	if err != nil {
		slog.Error("gate push failed", "component", "vetting", "node", nodeID, "err", err, "branch", dc.Branch)
		itemOutcomes = make([]DeliveryItemOutcome, len(dc.Items))
		for i, item := range dc.Items {
			itemOutcomes[i] = DeliveryItemOutcome{Kind: item.Kind, Error: err.Error()}
		}
	} else {
		itemOutcomes, err = cfg.Deliver(cctx, dc)
	}
	span.SetAttributes(attribute.Bool("delivered", err == nil))
	otelobs.End(span, err)

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
		attribute.String("agent", cfg.Agent),
		attribute.String("model", modelName(workerModel)),
		attribute.String("stage", stage),
	)
	coords := ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, Round: runID, User: cfg.User, Source: cfg.Source}
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
	// Read-only external agent that cloned but did zero reads/greps: the clone alone is not exploration.
	if cfg.ExternalWorker && cfg.ReadOnly && len(act.clonedRepos) > 0 && len(act.paths) == 0 && act.greps == 0 {
		det["exploration_grounded"] = criterionScore{Score: 0, Reason: "deterministic: repo cloned but zero read_file/grep calls - " +
			"the answer's findings are not grounded in anything actually read; explore the clone (read_file/grep) before reporting"}
	}
	if cs, details, hasCites := citationScore(answer, act, resolveCiteCloneRoots(cfg, act)); hasCites {
		det["cites_sources"] = criterionScore{Score: cs, Reason: citeReason(cs, details)}
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

// mergeDeterministic: folds deterministic criteria into verdict and re-aggregates.
// Stamps Deterministic here (not in computeDeterministicCriteria) since det's
// keys are exactly the code-owned set - composeFeedback reads it to tell a
// code-owned failure from a judge-scored one (#791).
func mergeDeterministic(v verdict, det map[string]criterionScore) verdict {
	if v.Criteria == nil {
		v.Criteria = map[string]criterionScore{}
	}
	for name, c := range det {
		c.Deterministic = true
		v.Criteria[name] = c
	}
	return aggregateVerdict(v)
}

// foldDeterministic: compute then merge in one step.
func foldDeterministic(ctx context.Context, v verdict, answer string, act workerActivity, cfg Config) verdict {
	det, _ := computeDeterministicCriteria(ctx, answer, act, cfg)
	return mergeDeterministic(v, det)
}

// resolveCiteCloneRoots: resolves clone dir roots for citationScore disk verification.
func resolveCiteCloneRoots(cfg Config, act workerActivity) []string {
	if cfg.Workspace == nil {
		return nil
	}
	var roots []string
	if cfg.Setup != nil {
		if abs, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID)); err == nil {
			roots = append(roots, abs)
		}
	}
	for _, dir := range act.clonedDirs {
		if abs, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, dir); err == nil {
			roots = append(roots, abs)
		}
	}
	return roots
}

// composeFeedback: merges judge feedback with below-threshold criterion reasons for the revise prompt.
// Deterministic (code-owned) and judge-scored failures are kept in separate,
// separately-labelled groups - a code-owned failure has one correct fix, a low
// judge score is arguable, and collapsing them into one "deterministic" list
// misrepresents the judge's opinion as a decided fact (#791). Deterministic
// failures lead, since they are decided; the judge's notes are not.
func composeFeedback(v verdict, threshold float64) string {
	var detFails, judgeFails []string
	for name, c := range v.Criteria {
		if c.Score >= threshold || strings.TrimSpace(c.Reason) == "" {
			continue
		}
		line := fmt.Sprintf("- %s: %s", name, c.Reason)
		if c.Deterministic {
			detFails = append(detFails, line)
		} else {
			judgeFails = append(judgeFails, line)
		}
	}
	sort.Strings(detFails) // stable order across runs (map iteration is random)
	sort.Strings(judgeFails)
	findingsFeedback := composeFindingsFeedback(v.Findings)
	if len(detFails) == 0 && len(judgeFails) == 0 && findingsFeedback == "" {
		return v.Feedback
	}
	var sb strings.Builder
	if len(detFails) > 0 {
		sb.WriteString("Deterministic check failures (code-owned, already decided - fix these):\n")
		sb.WriteString(strings.Join(detFails, "\n"))
	}
	if fb := strings.TrimSpace(v.Feedback); fb != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		// Only scoped when a deterministic failure exists: the judge was told (judgeKnownFailuresHeader)
		// to exclude these from its scoring, so its otherwise-unqualified praise would read as a
		// contradiction of the failures printed above it without this label.
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

// activityFromSession: replays session with no node scope.
func activityFromSession(sess session.Session) workerActivity {
	return activityFromSessionAt(sess, "")
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
				case "search":
					// ACP grep/glob - counted regardless of success/failure.
					s.act.greps++
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
		// citationScore backs a path only if it appears here or under a cloned dir.
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
