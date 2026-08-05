package vetting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
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

// NewWorkerNode wraps a worker agent as an AgentNode for use as the worker inside
// a gated refine loop (see RunGatedRefine).
func NewWorkerNode(worker adkagent.Agent) (workflow.Node, error) {
	n, err := workflow.NewAgentNode(worker, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("vetting: build worker node: %w", err)
	}
	return n, nil
}

// GateResult summarizes a node's trust-gate outcome: whether the final verdict
// cleared cfg.Threshold, the final score/feedback, and how many judge rounds ran.
// The DAG builder writes these to session state so Execute can surface the score
// on node_done and dependents can warn about unvetted upstream (continue-but-warn).
type GateResult struct {
	Passed   bool
	Score    float64
	Feedback string
	Rounds   int
}

// RunGatedRefine runs the trust-gate refine loop: draft → deterministic
// checks → independent judge → revise, until the score clears cfg.Threshold
// or cfg.JudgeRounds is exhausted. Returns (answer, result, err) for
// node_done + continue-but-warn. ErrNodeEmpty means no answer even after the
// empty-recovery retry - callers pause rather than cascade an empty output.
var ErrNodeEmpty = errors.New("vetting: node produced no answer")

// ErrNodePaused is returned by RunGatedRefine when the user paused the node
// (NodeControl.Paused). The node body catches it and keeps the accumulated
// answer, same as a cancel, but the DAG marks the node "paused" (resumable)
// instead of "cancelled" - see dag.Executor.PauseNode's ponytail note on why
// this is a graceful stop-and-resume-fresh rather than a literal frozen
// checkpoint.
var ErrNodePaused = errors.New("vetting: node paused")

// NodeControl lets a caller cancel, pause, or queue a message for a running
// gate, checked cooperatively at stage boundaries only - mid-call cancel
// isn't possible on ADK v2 since a plan's nodes share one runner/event
// stream. Cancelled is a BACKSTOP: the TOOL layer (cancelguard.go) refuses
// its next tool call so it arrives here promptly; Paused/queued get no such
// shortcut by design - neither may interrupt mid-turn.
type NodeControl interface {
	// Cancelled reports whether this node should stop (keep its current answer).
	Cancelled() bool
	// Paused reports whether this node should suspend (keep its current answer,
	// resumable - see ErrNodePaused).
	Paused() bool
	// TakeQueued drains every not-yet-delivered queued message, joined into one
	// guidance block ("" if the queue had nothing pending).
	TakeQueued() string
}

// AskToolName is the mid-node HITL tool a worker calls to ask the user a
// question. The tool itself only records the question (in its call args) and ends
// the worker's turn; the GATE detects the call and pauses the node via
// workflow.ResumeOrRequestInput under a round-stable InterruptID, so ADK routes
// the user's answer back to this node on the next turn.
const AskToolName = "ask_user"

// memoryCommitTimeout bounds the consolidation call. Generous because it runs on
// a local model against a whole answer, and it fires AFTER delivery - the run's
// output is already on the PR, so waiting costs the user nothing while timing out
// silently drops the node's durable memory (observed at 60s).
const memoryCommitTimeout = 3 * time.Minute

// hitlInterruptID is the STABLE per-node, per-round interrupt key for a mid-node
// HITL pause. Node IDs repeat across plans and rounds repeat within a node, but
// (invocation, node, round) is unique - and ADK scopes resume rehydration by
// invocation, so this is collision-free.
func hitlInterruptID(nodeID string, round int) string {
	return fmt.Sprintf("hitl-%s-r%d", nodeID, round)
}

// hitlTurn is one ask/answer exchange within a node's HITL history. answer is ""
// until the corresponding pause is resolved.
type hitlTurn struct {
	question string
	answer   string
}

// hitlScan summarizes a node's FULL HITL history within ONE invocation: every
// question the worker has asked so far (turns, in order) and how many of those
// the gate has already paused for (pauses). Derived entirely from session events
// (no state keys) so it survives resume re-entry and node-ID reuse across plans.
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
	answers := map[string]string{} // interruptID → the user's answer text
	for ev := range sess.Events().All() {
		if ev == nil || ev.Content == nil || ev.InvocationID != invocationID {
			continue
		}
		// Resume deliveries are user-authored FunctionResponses, not under the
		// node's own path - collect them by interrupt ID so each round's answer
		// can be paired to the question that raised it (round i ↔ turns[i-1]).
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

// pathHasNode reports whether the event was emitted under the given graph node
// (NodeInfo.Path segments are "name@run"; the worker's child runs nest below
// the gated node's segment).
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

// withUserAnswer builds the self-contained prompt for the post-answer worker run,
// folding in the FULL Q&A transcript (every round asked so far, not just the
// latest) so a worker several rounds deep still has what it already asked and was
// told. The worker runs in a fresh sub-branch each round, so this must ride the
// prompt - the ask_user calls live in earlier rounds' branches and are filtered
// from this run's LLM history.
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

// replyString coerces a resumed HITL payload to text.
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

	// nodeCtx is the otel-decorated PLAIN context.Context this node's span tree
	// nests under; it is never fed back into ADK calls (ctx, the adkagent.Context
	// param, stays untouched for those) - it exists purely so child spans opened
	// below (worker round, gate stage, judge round, delivery) parent correctly
	// under "quack.node" instead of becoming siblings of it.
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

	// Continuation and revise rounds build a FRESH prompt from cfg.Task, dropping
	// the advisor-thread marker the draft prompt carried. That marker is the ONLY
	// channel telling the worker's file/git tools their per-node workspace scope
	// (internal/tools scopeFromContext) - without it a continuation/revise round
	// re-clones into the bare user root instead of resuming the draft's clone.
	// Re-attach it to every tool-bearing round.
	markerLine := ""
	advisorToken := ""
	if token, ok := ParseAdvisorThread(prompt); ok {
		markerLine = "\n\n" + AdvisorThreadMarker(token)
		advisorToken = token
	}
	// cfg is a per-call copy (RunGatedRefine takes it by value), so stamping it
	// here only reaches this node's own judge rounds below - see
	// Config.AdvisorToken.
	cfg.AdvisorToken = advisorToken

	// Gate-side recall for an EXTERNAL worker - the preload_memory twin (native
	// agents get preload via their runner; an ACP subprocess has no runner, so
	// the gate front-loads the recalled notes into the round-0 prompt). Riding
	// `prompt` means the notes also reach the judge (its `question` is this
	// prompt) and every revise round's content. Best-effort by construction.
	if cfg.ExternalWorker && cfg.CommitMemory {
		_, recallSpan := otelobs.Start(nodeCtx, "memory.recall",
			attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", nodeID))
		rec := cfg.Memory.Recall(ctx, MemoryScope(ctx, cfg, nodeID), cfg.Task)
		recallSpan.SetAttributes(attribute.Bool("hit", rec != ""))
		recallSpan.End()
		otelobs.RecordMemoryRecall(rec != "")
		if rec != "" {
			prompt = rec + "\n\n" + prompt
			log.Info("recalled memory injected into the worker prompt", "bytes", len(rec))
		}
	}

	// Per-NODE workspace scope: a plan's nodes run concurrently in ONE chat, so
	// each gets its own directory under the chat scope (<root>/<user>/<chat>/
	// <node>/) - the default cwd its tools resolve relative paths against (they
	// derive the SAME dir from the advisor-thread marker; see internal/tools
	// scopeFromContext). Materialised here, at node entry, so the worker's first
	// `list_dir .` sees an empty dir rather than a "no such file". Best-effort: a
	// creation failure just means the tools create it on first write.
	nodeDir := workspace.NodeDir(cfg.NodeID)
	if cfg.Workspace != nil && nodeDir != "" {
		if _, err := cfg.Workspace.EnsureDir(cfg.WorkspaceUserID, cfg.ChatID, nodeDir); err != nil {
			log.Warn("could not create the node's working directory", "dir", nodeDir, "err", err)
		}
	}
	// probeCtx carries this node's replay-ledger coordinates for the gate's OWN
	// disk probes (augmentFromRepo below, checksPassCriterionTraced) - Round is
	// a fixed marker, not a worker/judge runID, since a probe re-reads disk
	// state on every activity() call rather than belonging to one model round
	// (#604, deferred from #600 because neither probe took a context.Context).
	probeCtx := ledger.WithCoords(ctx, ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, Round: probeRound})
	// activity replays the worker's session from that node dir - the cwd its
	// relative paths (and the judge's re-read of them) resolve against - then
	// folds in the clone's git state (augmentFromRepo): an external ACP worker
	// commits outside the tool layer, so the session alone under-reports.
	activity := func() workerActivity {
		act := activityFromSessionAt(ctx.Session(), nodeDir)
		augmentFromRepo(probeCtx, &act, cfg)
		return act
	}
	// actFor additionally folds in the reviewer's staged review: first the tool-
	// staged one (augmentFromReviewStage, the #451 review MCP surface), then the
	// answer-tail fallback (augmentFromAnswer) - which no-ops once the tool path
	// has staged, keeping the fallback for a reviewer that never called the tool.
	actFor := func(answer string) workerActivity {
		act := activity()
		augmentFromReviewStage(&act, advisorToken)
		augmentFromAnswer(&act, cfg, answer)
		augmentFromPRStage(&act, advisorToken)
		return act
	}

	cancelled := func() bool { return ctrl != nil && ctrl.Cancelled() }
	paused := func() bool { return ctrl != nil && ctrl.Paused() }
	// The judge runs in its own isolated runner (off the workflow event stream), so
	// its activity can't ride that stream. Forward it to the client as a stage:judge
	// run via the SSE sink injected on ctx (executor.Execute) - SSE-only, never
	// written to the session, so it can't re-poison a downstream node's request.
	sink, _ := stream.YieldFromContext(ctx)

	// promptEmit delivers each worker prompt as a session event, but ONLY for
	// agents that can't take RunNode input natively (remote A2A workers - see
	// Config.DeliverPromptEvent). nil disables emitPrompt for local llmagents,
	// whose single-turn contents a stray user-role event would contaminate.
	promptEmit := emit
	if !cfg.DeliverPromptEvent {
		promptEmit = nil
	}

	// Per-node cancel/pause/queue (M5b, reworked for #265), cooperative at
	// gate-stage boundaries: ADK v2 can't cancel a single model call mid-flight
	// without breaking event streaming, so cancel/pause/a queued message all
	// land between stages. basePrompt is the un-guided task; a delivered queued
	// message re-runs the whole gate with it appended.
	basePrompt := prompt
	queueAttempt := 0
	for {
		if cancelled() {
			return "", GateResult{}, nil // cancelled before drafting → empty (continue-but-warn)
		}
		if paused() {
			return "", GateResult{}, ErrNodePaused // paused before drafting → keep whatever this node has (nothing yet)
		}
		// A re-run after a delivered queued message needs fresh RunNode run IDs:
		// WithRunID replays a completed run (the durable-skip property), so
		// reusing "worker-r0" would replay the pre-queue draft instead of
		// re-invoking the worker with the message folded in.
		sfx := ""
		if queueAttempt > 0 {
			sfx = fmt.Sprintf("-s%d", queueAttempt)
		}
		question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}

		// HITL resume: if this node previously paused to ask the user and THIS turn
		// delivered the answer (ADK re-entered the node with a ResumedInput under the
		// round-stable interrupt ID), skip the normal draft - run the worker once with
		// the Q&A folded into a self-contained prompt.
		var answer string
		var err error
		resumed := false
		if scan := scanNodeAsks(ctx.Session(), ctx.InvocationID(), nodeID); scan.pauses > 0 {
			if reply, ok := ctx.ResumedInput(hitlInterruptID(nodeID, scan.pauses)); ok {
				resumed = true
				// The just-delivered answer may not have landed in session history yet
				// at scan time (it arrives as this turn's inbound message) - fill it in
				// from ctx.ResumedInput so the current round is in the transcript too.
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
				// Log at our boundary before returning: ADK's scheduler can swallow a
				// node error into a silent empty completion, so this ERROR line (with the
				// model's error body) is what makes a failed worker visible in the logs.
				log.Error("worker draft failed", "run", "worker-r0", "err", err)
				return "", GateResult{}, err
			}
		}

		// HITL / guard pause: park the NODE when the worker's turn raised a fresh
		// ask_user question or guard confirmation, so ADK routes the human's answer
		// back here next turn - regardless of draft emptiness (a chatty model asks
		// and writes in the same turn); this turn's draft is discarded, since resume
		// re-runs the worker with the Q&A folded in. Repeated after every revise round.
		if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, emit, log); paused {
			return "", GateResult{}, ierr // ErrNodeInterrupted → park
		}

		// Continuation loop: keep giving the worker TOOL-BEARING turns until the
		// WORK is done (workIncomplete), not until the model emits text - a
		// reasoning-only turn is not "done". Tested against the node's OWN task
		// (cfg.Task), never `prompt` - judged against the whole user request, every
		// node, even read-only explorers, would stay forever incomplete.
		if workIncomplete(answer, cfg.Task, actFor(answer), cfg.ReadOnly, cfg.IsReviewer) {
			_, contSpan := otelobs.Start(nodeCtx, "gate.continuation",
				attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", nodeID))
			contAttempts := 0
			for attempt := 1; attempt <= maxContinueRounds && workIncomplete(answer, cfg.Task, actFor(answer), cfg.ReadOnly, cfg.IsReviewer); attempt++ {
				contAttempts = attempt
				act := actFor(answer)
				log.Warn("work not finished; continuing the worker with its tools",
					"attempt", attempt, "empty", strings.TrimSpace(answer) == "", "committed", act.committed, "pushed", act.pushed)
				answer, err = runWorkerNodeTraced(ctx, nodeCtx, cfg, workerModel, workerNode, buildContinuationPrompt(cfg.Task, act, cfg.Checks, cfg.ReadOnly, cfg.IsReviewer)+markerLine,
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

		// Last resort for a genuinely stuck worker (nothing, on every turn): a
		// TOOL-LESS writer in a FRESH runner writes up whatever the session shows it
		// found. Better than an empty node - never a substitute for the work itself,
		// which is why it now runs only AFTER the worker has been given its
		// continuation budget.
		if strings.TrimSpace(answer) == "" {
			log.Warn("worker still empty after continuation; falling back to the tool-less writer", "rounds", maxContinueRounds)
			answer, err = runWriterFresh(ctx, workerModel, buildFinalizeContent(question, activity()))
			if err != nil {
				log.Error("writer recovery failed", "err", err)
				return "", GateResult{}, err
			}
			if strings.TrimSpace(answer) == "" {
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

		// Judge/revise loop: judge the current answer, fold in the deterministic
		// citation/length criteria, and on a fail revise via a fresh worker call whose
		// prompt inlines the feedback + prior answer (buildRevisionContent is
		// self-contained, so the stateless worker needs no session continuity).
		var res GateResult
		queuedText := ""
		// JudgeRounds counts REVISION attempts: round r judges, and on a fail (with
		// budget left) revises, so N rounds = N revisions / N+1 judgments. The
		// cfg.JudgeRounds > 0 guard keeps 0 meaning "no judge at all" - judge:false
		// sets 0 but the global judge factory is non-nil, so only this bound skips it.
		for round := 1; judge != nil && cfg.JudgeRounds > 0 && round <= cfg.JudgeRounds+1; round++ {
			// Cooperative cancel/pause/queue, checked before each judge round AND
			// before the empty-answer guard below - so an empty node (a reasoning
			// model that produced nothing) can still be cancelled, paused, or
			// re-run with a queued message. Cancel/pause stop refining (keep the
			// current answer); a delivered queued message re-runs the gate.
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
			if strings.TrimSpace(answer) == "" {
				break // still nothing to judge after recovery
			}
			act := actFor(answer)
			runID := fmt.Sprintf("judge-r%d", round)
			judgeCtx, judgeSpan := otelobs.Start(nodeCtx, "gate.judge",
				attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", nodeID),
				attribute.String("run_id", runID), attribute.String("agent", cfg.Agent), attribute.Int("round", round))
			emitJudge(sink, nodeID, stream.SSEEvent{Name: stream.EventAgentStart, Data: stream.AgentStartData{RunID: runID, Agent: "judge", Stage: stream.StageJudge, Round: round}})
			// ledgerCtx carries this round's replay-ledger coordinates
			// (gen_ai.agent.name: judge, per the design) into every model/tool
			// call the judge round makes - same WithCoords seam runWorkerNodeTraced
			// uses for the worker, just via plain context.WithValue here since
			// runJudgeAgent already takes a context.Context, not an adkagent.Context.
			ledgerCtx := ledger.WithCoords(ctx, ledger.Coords{ChatID: cfg.ChatID, Node: nodeID, Agent: "judge", Round: runID})
			// Cheapest-first: compute before the judge runs, so it can be told
			// what's already decided instead of scoring blind.
			det := computeDeterministicCriteria(judgeCtx, answer, act, cfg)
			v, jerr := runJudgeAgent(ledgerCtx, judge, cfg, question, answer, act, det, judgePartEmitter(sink, nodeID, runID))
			if jerr != nil {
				// ERROR, not Warn: a judge failure means the answer is going out
				// UNVETTED - that must be loud in the logs, not buried.
				log.Error("judge failed; surfacing answer unvetted", "round", round, "err", jerr)
				emitJudge(sink, nodeID, stream.SSEEvent{Name: stream.EventAgentComplete, Data: stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Status: "unavailable", Reason: jerr.Error()}})
				otelobs.End(judgeSpan, jerr)
				// No score exists to record here - RecordJudgeVerdict below never
				// runs for this round, which would otherwise leave this agent's
				// judge.score/judge.verdict series silently thin whenever its judge
				// calls error disproportionately (bigger prompts, flakier tool use).
				otelobs.RecordJudgeUnavailable(cfg.Agent)
				// Fail closed but fall through to the SAME deliver-with-caveat
				// path a low score takes (not an early return) - only that path
				// writes a GitHub review's verdict marker, so returning here
				// strands it (#572).
				res = GateResult{Score: 0, Passed: false, Feedback: "quack's judge was unavailable, so this answer could not be scored: " + jerr.Error(), Rounds: round}
				break
			}
			// Adversarial verify (#370): before folding in the deterministic
			// criteria, give load-bearing PASSING judge criteria a chance to be
			// refuted by independent skeptics sharing the SAME repo access - a
			// no-op when cfg.Skeptic is unset (the default).
			v = adversarialVerify(judgeCtx, cfg, question, answer, act, v, judgePartEmitter(sink, nodeID, runID+"-skeptic"))
			v = mergeDeterministic(v, det)
			feedback := composeFeedback(v, cfg.Threshold)
			res = GateResult{Passed: v.Score >= cfg.Threshold, Score: v.Score, Feedback: feedback, Rounds: round}
			emitEvaluationResults(ledgerCtx, runID, v)
			emitJudge(sink, nodeID, stream.SSEEvent{Name: stream.EventAgentComplete, Data: stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Score: res.Score, Passed: res.Passed, Feedback: res.Feedback}})
			log.Info("judge round done", "round", round, "score", v.Score, "passed", res.Passed)
			judgeSpan.SetAttributes(attribute.Float64("score", res.Score), attribute.Bool("passed", res.Passed))
			judgeSpan.End()
			otelobs.RecordJudgeVerdict(cfg.Agent, res.Score, res.Passed)
			// DEBUG: the per-criterion reasoning + feedback behind that score, so a
			// failing gate is diagnosable from logs instead of only the UI.
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
			revisePrompt := contentPlainText(buildRevisionContent(cfg.Constitution, question, answer, feedback, act, citationOnlyFailure(v, cfg.Threshold))) + markerLine
			revised, rerr := runWorkerNodeTraced(ctx, nodeCtx, cfg, workerModel, workerNode, revisePrompt, fmt.Sprintf("worker-r%d%s", round, sfx), "revise", promptEmit)
			if rerr != nil {
				log.Error("revision worker failed; keeping prior answer", "round", round, "err", rerr)
				return answer, res, nil // revision failed; keep the prior answer
			}
			// A revision can ITSELF raise a fresh ask_user or guard-ladder
			// confirmation - a worker commonly proposes its confirm-tiered delivery
			// step (git_commit + git_push) only after the judge flags the task
			// incomplete. Without this check here the unconfirmed operation is
			// silently skipped and the incomplete answer sails to the next judge
			// round with no human ever approving the push. Park exactly as the
			// draft-time check does.
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
		// Fold in whatever the ACP memory MCP surface's stage_memory landed across
		// every round of this node (#344) - the same pass-gated commit path a
		// native agent's stage_memory tool call rides via session replay. Looked
		// up via the node's MemSecret (a SEPARATE, unguessable registry key from
		// advisorToken - see AdvisorTask.MemSecret), and unregistered the moment
		// it's drained: a straggler stage_memory call arriving after this point
		// finds no session and fails outright, rather than writing into a buffer
		// nobody will ever read again.
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
		if res.Passed {
			commitMemoryOnPass(ctx, nodeCtx, cfg, nodeID, answer, act.staged)
		}
		// Deliver even on a judge FAIL - graceful degradation: the work is done
		// (committed + staged), so open the PR / post it, but with the gate's
		// concerns attached as a caveat (see App.Deliver) so a human decides.
		// Memory stays pass-only: never persist tradecraft the gate rejected.
		commitDelivery(nodeCtx, sink, cfg, nodeID, act, res)
		return answer, res, nil
	}
}

// pauseIfWorkerRaisedHITL parks the node when the worker's latest turn
// raised a NEW ask_user/guard confirmation - re-derived from session events
// (len(turns) > pauses), robust to node-ID reuse and resume re-entry. MUST
// run after EVERY worker run: guard-confirmed delivery is often proposed
// only during a late revision. ask_user is checked first (more specific).
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
	// Guard-ladder pause: the worker called a confirm-tiered tool
	// (internal/tools/guard.go) that returned the pending marker. Same park
	// mechanism, a distinct interrupt-ID namespace ("confirm-" vs "hitl-").
	if cscan := scanNodeConfirms(ctx.Session(), ctx.InvocationID(), nodeID); len(cscan.turns) > cscan.pauses {
		t := cscan.turns[len(cscan.turns)-1]
		// Prefer the guard's own hint - it carries call-specific warnings
		// (e.g. "this DIFFERS from the previously approved operation").
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

// stagedCandidate parses a stage_memory tool call's args (content + optional kind
// and bucket) into a memory candidate; ok=false if there's no usable content. The
// bucket (repo|role|user) is the agent's own declaration of WHAT the memory is
// about, and routes the write (memory.Scope.writeBucket); an absent/unknown one
// takes the caller's default bucket.
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

// stagedDeliveryTarget parses one stage_pr/stage_review/stage_comment/unstage
// call into its target key ("pr" | "review" | "comment:<slot>") and, for a
// stage_* call, the item to upsert there. ok=false for a call this scanner
// doesn't recognise or that carries no usable target (a malformed unstage).
// unstage=true means DROP the target rather than upsert item.
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

// commitMemoryOnPass fires staged knowledge (plus answer-mined
// consolidation) into shared memory - only on a gate pass, fire-and-forget,
// never blocking or failing the node. Runs even with empty staged. Bucketed
// by SUBJECT (repo/role/user), never the agent's own name - resolved here
// because the gate is workflow-side and holds the real user id + jail coords.
func commitMemoryOnPass(ctx adkagent.Context, spanCtx context.Context, cfg Config, author, answer string, staged []memory.Candidate) {
	if cfg.Memory == nil || !cfg.CommitMemory || strings.TrimSpace(answer) == "" {
		return
	}
	sc := MemoryScope(ctx, cfg, author)
	// The commit is fire-and-forget (best-effort, never blocks the node), so its
	// span cannot be a normal CHILD of the node span - the node (and its span)
	// may finish well before this goroutine does. Link it to the node span
	// instead: a separate trace, cross-referenced, which is OTel's documented
	// shape for async work triggered by a request that doesn't wait for it.
	parentSC := oteltrace.SpanContextFromContext(spanCtx)
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), memoryCommitTimeout)
		defer cancel()
		cctx, commitSpan := otelobs.StartLinked(cctx, "memory.commit", parentSC, attribute.String("agent", author))
		n, err := cfg.Memory.Commit(cctx, sc, author, staged, answer)
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

// commitDelivery posts the node's FINAL staged delivery set, exactly once,
// regardless of judge verdict (graceful degradation) - a FAIL rides its
// score+feedback along (dc.GatePassed/dc.GateFeedback) so App.Deliver
// attaches a caveat instead of blocking delivery. BLOCKS (no goroutine): a
// delivery failure is user-visible, so it must be attempted before the run
// completes.
func commitDelivery(ctx context.Context, sink func(stream.SSEEvent), cfg Config, nodeID string, act workerActivity, res GateResult) {
	if cfg.Deliver == nil || len(act.stagedDelivery) == 0 {
		recordDeliveryOutcomeMetric(cfg, res, false, false)
		// The phantom-success class this event exists to surface: a delivery-
		// capable node's judge-passed work-request that staged nothing at all.
		if !cfg.ReadOnly && res.Passed {
			emitDeliveryResult(sink, nodeID, stream.DeliveryResult(nodeID, stream.DeliveryOutcomeNone, "", "", "", otelobs.TraceIDOf(ctx)))
		}
		return
	}
	spanCtx, span := otelobs.Start(ctx, "delivery",
		attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", nodeID))
	defer span.End()
	traceID := otelobs.TraceIDOf(spanCtx)

	dc := DeliveryContext{NodeID: nodeID, ChatID: cfg.ChatID, Items: sortedStagedDelivery(act.stagedDelivery), IssueNumber: act.prNumber, GatePassed: res.Passed, GateFeedback: res.Feedback}
	if cfg.Setup != nil {
		// Setup guaranteed this branch exists (or the run errored before any node
		// ran) - deliver on it, never the worker's own git-tracking ledger, which
		// a setup-provisioned worker is told not to touch (internal/github/webhook.go).
		dc.Branch = cfg.Setup.WorkBranch
		dc.CloneURL = cfg.Setup.Repo
		if cfg.Workspace != nil {
			// cfg.NodeID, not the nodeID argument: it's the workspace-directory
			// scope (dag.buildGateNodes stamps it - node.ID normally, or the ONE
			// shared clone dir for a chained repo-touching node - see
			// dag.workspaceNodeID), which is where setup actually cloned to.
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
	// Mermaid validity (#448) is now a deterministic GATE criterion
	// (mermaidCriterion, mermaid.go) checked before this point - a body
	// reaching commitDelivery has already passed that check (or shipped as a
	// gate-failed draft, same as any other unmet deterministic criterion).
	// Nothing left to strip or repair here.

	// The permission boundary (#657, #662): drop any staged item the
	// trigger's grant does not permit BEFORE it reaches cfg.Deliver - the
	// only enforcement point, since this is the only path to GitHub (ACP
	// workers can't git push; native write-side tools were deleted in
	// 0.6.0). A refusal is loud - logged and surfaced as a failed
	// delivery_result - never a silently dropped item.
	if allowed, refused, reasons := partitionByGrant(dc.Items, cfg.Grant); len(refused) > 0 {
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
	itemOutcomes, err := cfg.Deliver(cctx, dc)
	span.SetAttributes(attribute.Bool("delivered", err == nil))
	otelobs.End(span, err)

	// The extension's own record (itemOutcomes) is authoritative - a real
	// PR/review URL or a real per-item error, never the worker's self-report.
	// Fall back to one synthetic outcome per staged item (the aggregate error)
	// only when the extension reported nothing at all (e.g. it failed before
	// reaching any item, such as the branch push).
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

// partitionByGrant splits staged items into what g permits and what it
// refuses, pairing each refused item with why (same index as refused) - the
// gate's actual permission boundary (#662). A nil grant (no GitHub trigger
// governs this run) permits everything.
func partitionByGrant(items []StagedDelivery, g *Grant) (allowed, refused []StagedDelivery, reasons []string) {
	for _, item := range items {
		if ok, reason := g.allows(item.Kind); ok {
			allowed = append(allowed, item)
		} else {
			refused = append(refused, item)
			reasons = append(reasons, reason)
		}
	}
	return allowed, refused, reasons
}

// emitDeliveryResult sends a delivery_result SSE event, if a sink is present
// - SSE-only, like emitJudge (never written to the session). ev already
// carries NodeID (set by stream.DeliveryResult); nodeID is accepted for
// signature symmetry with emitJudge.
func emitDeliveryResult(sink func(stream.SSEEvent), nodeID string, ev stream.SSEEvent) {
	if sink != nil {
		sink(ev)
	}
}

// recordDeliveryOutcomeMetric records quack.delivery.outcome - the plan's
// alertable phantom-success guard. Scoped to delivery-capable agents
// (!cfg.ReadOnly): a reviewer/explorer never delivers, so it has no signal to
// contribute either way. "none" is the phantom-success class: a judge-passed
// work-request that recorded no delivery attempt at all. "draft" mirrors the
// documented gate-fail behaviour (AGENTS.md: "a gate-failed PR opens as a
// draft") - a successful delivery riding a failed verdict.
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

// MemoryScope is the node's memory entitlement: the repo it is working in (derived
// from the chat's jail - "" when there is no repo or more than one, in which case the
// write falls back to the role bucket rather than guessing), its agent's role family,
// the real user, and its agent name as the legacy read key. Exported so the ACP
// memory MCP surface (internal/acp) resolves the SAME scope for load_memory that
// the gate itself recalls with (#344).
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

// runWorkerNode runs the worker as a sub-branched child with a stable per-run
// RunID (so a completed run replays from the event log on resume rather than
// re-executing) and returns its answer with leaked <think> content stripped.
// workerInput builds a worker node's input: a plain string when there are no
// attachments, or a *genai.Content carrying the prompt + media parts (image/audio)
// for a media-capable node. AgentNode's nodeInputToContent accepts either.
// ponytail: media rides only the initial draft; revisions are text (the prior
// answer already captured the media reading) - re-attaching each round is costly.
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
	// WithIsolationScopeFromNodePath: plan nodes share ONE workflow session, and
	// a local llmagent's current-turn pivot scan is NOT branch-filtered (ADK
	// v2.0.0), so a concurrent sibling's tail event steals the pivot and leaves
	// this worker an empty request. The pivot scan does honour isolation scope,
	// so scoping each run to its own node path hides sibling events. Inert for
	// remote (A2A) workers - their sibling leak is fixed read-side in
	// internal/agent/a2a.go. Known quirk: a local llmagent sees its prompt twice
	// (harmless; local workers exist only in tests).
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

// runWorkerNodeTraced wraps runWorkerNode with a "quack.worker.round" span
// and duration metric. Every worker round passes through here, so this is
// the ONE place that stamps this round's replay-ledger coordinates onto ctx
// via WithAgentContext, readable downstream via ledger.CoordsFromContext.
// Duration is the span's own window, not a separate timer.
func runWorkerNodeTraced(ctx adkagent.Context, spanCtx context.Context, cfg Config, workerModel model.LLM, workerNode workflow.Node, input any, runID, stage string, emit func(*session.Event) error) (string, error) {
	_, ts := otelobs.StartTimedSpan(spanCtx, "worker.round",
		attribute.String(otelobs.ChatIDKey, cfg.ChatID),
		attribute.String("node_id", cfg.NodeID),
		attribute.String("run_id", runID),
		attribute.String("agent", cfg.Agent),
		attribute.String("model", modelName(workerModel)),
		attribute.String("stage", stage),
	)
	coords := ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, Round: runID}
	gctx := ctx.WithAgentContext(ledger.WithCoords(ctx, coords))
	// The WithAgentContext stamp above does not survive workflow.RunNode's
	// dynamic-child scheduling (see inference.tracedModel.SetLedgerCoords) -
	// a model built via the inference package implements this and gets
	// stamped directly instead; anything else (a test stub, an ACP worker
	// with no local model.LLM) just doesn't get per-round ledger coords.
	if cs, ok := workerModel.(interface{ SetLedgerCoords(ledger.Coords) }); ok {
		cs.SetLedgerCoords(coords)
	}
	out, err := runWorkerNode(gctx, workerNode, input, runID, emit)
	d := ts.End(err)
	otelobs.RecordRoundDuration(cfg.Agent, modelName(workerModel), stage, d)
	return out, err
}

// checksPassCriterionTraced wraps checksPassCriterion with a "quack.gate.checks"
// span - the deterministic-checks gate stage - and stamps the SAME fixed-round
// replay-ledger coordinates node.go's probeCtx uses, so each check's
// workspace.RunPipeline call records an execute_tool event (checks.go) with
// proper coords (#604, deferred from #600).
func checksPassCriterionTraced(ctx context.Context, cfg Config) (criterionScore, bool) {
	spanCtx, span := otelobs.Start(ctx, "gate.checks",
		attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", cfg.NodeID))
	defer span.End()
	probeCtx := ledger.WithCoords(spanCtx, ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, Round: probeRound})
	c, ok := checksPassCriterion(probeCtx, cfg)
	span.SetAttributes(attribute.Bool("applicable", ok), attribute.Float64("score", c.Score))
	return c, ok
}

// emitJudge sends a judge-stage SSE event scoped to nodeID, if a sink is present.
func emitJudge(sink func(stream.SSEEvent), nodeID string, ev stream.SSEEvent) {
	if sink != nil {
		sink(stream.ScopeToNode(ev, nodeID))
	}
}

// judgePartEmitter forwards the judge agent's streamed parts to the SSE sink as
// stage:judge activity (thinking / tokens / tool calls), scoped to nodeID+runID.
// nil-sink-safe. It never writes to the workflow session, so it cannot re-poison
// a downstream node's model request (unlike the v1 orphan-marker approach).
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

// computeDeterministicCriteria computes the code-owned criteria (citation
// backing, length, retrieval grounding, checks, answer-shape, delivery) on
// their own, with no verdict involved - callable before the judge runs.
func computeDeterministicCriteria(ctx context.Context, answer string, act workerActivity, cfg Config) map[string]criterionScore {
	det := map[string]criterionScore{}
	if ls := lengthScore(answer); ls < 1.0 {
		det["sufficient_length"] = criterionScore{Score: ls, Reason: fmt.Sprintf("deterministic: %d chars", len(strings.TrimSpace(answer)))}
	}
	// A retrieval agent that performed ZERO retrieval cannot have a grounded
	// answer - it either answered from model memory (its citations, if any, are
	// unverifiable) or wrote a question to the user as answer text instead of
	// calling ask_user. Hard weakest-link fail with feedback naming both ways
	// out; citationScore abstains in this case (no activity to grade against),
	// which previously let exactly these answers sail through to the judge.
	// A clone or a local file read is retrieval too (cloned-repo grounding):
	// a coding node that consulted the repo on disk instead of the web is not
	// answering from model memory.
	if cfg.RequireRetrieval && len(act.fetched) == 0 && len(act.seen) == 0 && len(act.clonedRepos) == 0 && len(act.paths) == 0 {
		det["grounded_in_retrieval"] = criterionScore{Score: 0, Reason: "deterministic: no web_search/web_fetch activity this session - " +
			"research the task and cite what you retrieve; if you are blocked on information only the user has, call ask_user (never write a question to the user as your answer)"}
	}
	// A read-only EXTERNAL agent (code-explorer/code-reviewer) that cloned a
	// repo but performed ZERO reads/greps on it did no real exploration - a
	// clone puts every file on disk, which is enough for grounded_in_retrieval
	// above, but not evidence anything was actually opened and read. Without
	// this, a fabricated "survey" of a clone the worker never looked at sails
	// through on the clone alone (#289). Scoped to ExternalWorker+ReadOnly
	// nodes that actually cloned something, so a legitimately read-nothing
	// node (synthesis, a code-implementer working in a pre-provisioned setup
	// clone it never re-clones) is untouched.
	if cfg.ExternalWorker && cfg.ReadOnly && len(act.clonedRepos) > 0 && len(act.paths) == 0 && act.greps == 0 {
		det["exploration_grounded"] = criterionScore{Score: 0, Reason: "deterministic: repo cloned but zero read_file/grep calls - " +
			"the answer's findings are not grounded in anything actually read; explore the clone (read_file/grep) before reporting"}
	}
	if cs, details, hasCites := citationScore(answer, act, resolveCiteCloneRoots(cfg, act)); hasCites {
		det["cites_sources"] = criterionScore{Score: cs, Reason: fmt.Sprintf("deterministic: %d cited link(s), mean backing %.2f", len(details), cs)}
	}
	// §4: deterministic gate checks - the planner's `checks` when it set them,
	// else the ones derived from the repo on disk (checks.go). Untouched for a
	// node that has neither (research, synthesis).
	if c, ok := checksPassCriterionTraced(ctx, cfg); ok {
		det["checks_pass"] = c
	}
	// Mermaid validity (#448): checked against the answer AND every currently
	// staged delivery body - whichever is about to ship to GitHub. Only added
	// on a failure (mirrors sufficient_length above): a clean diagram, or no
	// diagram at all, needs no entry.
	if c, ok := mermaidCriterion(answer, act); ok {
		det["mermaid_valid"] = c
	}
	// Answer-shape check (#565): a leaked or malformed tool-call fragment in a
	// deliverable is broken by construction, same family and fold as
	// mermaid_valid above.
	if c, ok := toolCallSyntaxCriterion(answer, act); ok {
		det["no_tool_call_syntax"] = c
	}
	// Answer-shape check (#569): a pointer to a file this run wrote but never
	// committed is broken by construction - the working directory holding it
	// is discarded when the run ends.
	if c, ok := danglingDeliverablePathCriterion(answer, act); ok {
		det["no_dangling_deliverable_path"] = c
	}
	// Delivery: a node whose task says commit/push/PR cannot pass unless the
	// ledger shows it did, and one told to POST a review on a PR cannot pass
	// unless it submitted one (delivery.go). Untouched for a node whose task
	// demands neither.
	for name, c := range incompleteCriteria(cfg.Task, act, cfg.ReadOnly, cfg.IsReviewer) {
		det[name] = c
	}
	return det
}

// mergeDeterministic folds a computed deterministic criteria map (see
// computeDeterministicCriteria) into the judge's verdict and re-aggregates
// (weakest-link).
func mergeDeterministic(v verdict, det map[string]criterionScore) verdict {
	if v.Criteria == nil {
		v.Criteria = map[string]criterionScore{}
	}
	for name, c := range det {
		v.Criteria[name] = c
	}
	return aggregateVerdict(v)
}

// foldDeterministic computes then merges in one step, for callers that don't
// need the deterministic results before the judge runs.
func foldDeterministic(ctx context.Context, v verdict, answer string, act workerActivity, cfg Config) verdict {
	return mergeDeterministic(v, computeDeterministicCriteria(ctx, answer, act, cfg))
}

// resolveCiteCloneRoots resolves every ABSOLUTE clone-dir root citationScore
// should disk-verify local code citations under: the harness-provisioned
// setup clone (cfg.Setup, resolved exactly as commitDelivery resolves it)
// plus every repo the worker itself git_clone'd this session (act.clonedDirs).
// An entry that can't be resolved (no cfg.Workspace wired, a jail-escape, a
// dir that was never created) is silently dropped - citationScore just has
// one fewer root to check, never an error.
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

// composeFeedback merges the judge's own narrative Feedback with the Reasons
// of any criterion scoring below threshold - a deterministic criterion's
// Reason (grounded_in_retrieval, checks_pass, …) is set by code the judge
// never saw, so without this the revise prompt would carry a numeric fail
// with no explanation of what actually needs to change (see §4: "the revise
// prompt therefore contains the actual compiler/test failure").
func composeFeedback(v verdict, threshold float64) string {
	var extra []string
	for name, c := range v.Criteria {
		if c.Score < threshold && strings.TrimSpace(c.Reason) != "" {
			extra = append(extra, fmt.Sprintf("- %s: %s", name, c.Reason))
		}
	}
	findingsFeedback := composeFindingsFeedback(v.Findings)
	if len(extra) == 0 && findingsFeedback == "" {
		return v.Feedback
	}
	sort.Strings(extra) // stable order across runs (map iteration is random)
	var sb strings.Builder
	if strings.TrimSpace(v.Feedback) != "" {
		sb.WriteString(v.Feedback)
		sb.WriteString("\n\n")
	}
	if len(extra) > 0 {
		sb.WriteString("Deterministic check failures:\n")
		sb.WriteString(strings.Join(extra, "\n"))
	}
	if findingsFeedback != "" {
		if len(extra) > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(findingsFeedback)
	}
	return sb.String()
}

// citationOnlyFailure reports whether the ONLY criterion below threshold is
// cites_sources - i.e. the answer is substantively fine and just needs its
// already-retrieved URLs attached to the claims. When true the revise prompt
// tells the worker to do a formatting pass (attach the URLs it already has),
// NOT to re-research: extra fetches to fix a pure-citation fail waste tokens.
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

// activityFromSession reconstructs the worker's retrieval activity AND its
// workspace-operation ledger (fs/git/run_command calls - ledger.go) from the
// workflow session events, pairing calls to responses by call ID. Consumed
// by the citation check, the judge's workspace section, and the
// revise/finalize prompts.
// joinWritten resolves a path argument against the cwd at call time - the
// read-side mirror of tools.joinCwd (kept here to avoid a vetting→tools
// dependency). A leading "/" ignores the cwd.
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

// activityFromSession replays a session with no node scope (an un-gated/legacy
// worker, and every test that doesn't care about scoping).
func activityFromSession(sess session.Session) workerActivity {
	return activityFromSessionAt(sess, "")
}

// activityFromSessionAt replays the worker's session inside nodeDir - the node's
// OWN working dir (workspace.NodeDir) and its invisible root, which every path the
// worker passed and every cwd its `cd` reported is relative to. The recorded
// act.written paths come back CHAT-relative (writtenRel), which is what
// buildChangedFilesSection hands to Jail.Resolve. Getting this wrong silently
// breaks the judge's changed-file re-read: it would resolve the worker's writes
// against the wrong root, find nothing, and quietly degrade to scoring the answer's
// self-report.
func activityFromSessionAt(sess session.Session, nodeDir string) workerActivity {
	s := &activityScanner{
		act:         workerActivity{fetched: map[string]fetchRecord{}, seen: map[string]string{}, paths: map[string]bool{}},
		nodeDir:     nodeDir,
		writtenSeen: map[string]bool{},
	}
	if sess == nil {
		return s.act
	}
	pending := map[string]string{}           // web_fetch call ID → URL
	pendingWs := map[string]map[string]any{} // workspace-tool call ID → args (see ledger.go)
	pendingWsTool := map[string]string{}     // workspace-tool call ID → tool name
	pendingCd := map[string]bool{}           // cd call ID → awaiting response (to track cwd)
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
					// ALSO route into the workspace ledger (isWorkspaceTool now
					// covers web_fetch): a worker that fetched repo files from the
					// web instead of reading the local clone must leave a visible
					// trace, or the judge has nothing to catch it with.
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
					// ACP grep/glob calls (ToolKindSearch) - counted regardless of
					// success/failure, like recordSearch's web queries: a call that
					// found nothing is still evidence the worker looked.
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
				// Only a completed call/response pair enters the ledger - an
				// operation with no response never happened as far as claims go.
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

// activityScanner accumulates one worker's activity. Every recorder below is
// reached from BOTH paths - a direct tool call's session events, and a call
// replayed out of a run_code record - which is what makes the two
// indistinguishable to the trust gate. There is one implementation of each rule,
// so the two paths cannot diverge.
type activityScanner struct {
	act     workerActivity
	nodeDir string
	// curCwd is the NODE-relative cwd in effect ("" = the node's own root),
	// updated on each successful cd - including a cd made from inside a script,
	// since the tool writes the same session state either way.
	curCwd      string
	writtenSeen map[string]bool // dedup for act.written
}

// recordPRNumber captures a github_* call's pull_number arg as the review/
// comment delivery target - first call wins (a review session targets one PR).
func (s *activityScanner) recordPRNumber(args map[string]any) {
	if s.act.prNumber != 0 {
		return
	}
	if n, ok := args["pull_number"].(float64); ok && n > 0 {
		s.act.prNumber = int(n)
	}
}

// applyDelivery upserts or drops one target in the worker's staged-delivery
// set (see stagedDeliveryTarget) - the keyed-set half of the memory-candidate
// append-log pattern: a later stage_* call for the SAME target replaces the
// earlier one, and unstage removes it outright, so only what's staged at
// judge-pass time (commitDelivery) is ever posted.
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
		s.act.fetched[url] = fetchRecord{sample: strings.TrimSpace(trimToSample(result))}
	}
}

// recordCd tracks the working directory a later write resolves against. cd
// reports the new cwd as a NODE-relative slash path ("." = the node's own root);
// mirror the tool's own storage so writtenRel resolves later writes correctly.
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

// recordWorkspace is the ledger entry plus the structured grounding/delivery
// capture for one completed workspace operation. Failures ARE recorded
// (recordWsOp marks them FAILED): "the tests passed" claimed over a failed run
// must be contradictable.
func (s *activityScanner) recordWorkspace(name string, args, resp map[string]any) {
	s.act.workspace = append(s.act.workspace, recordWsOp(name, args, resp))
	// Structured grounding capture (successful ops only - a FAILED clone/read
	// retrieved nothing and backs no citation, though it stays in the ledger
	// above for claim-checking).
	if _, failed := resp["error"]; failed {
		return
	}
	switch name {
	// Delivery actions (delivery.go): a task that demands the work be
	// committed/pushed cannot pass without these.
	case "git_commit":
		s.act.committed = true
	case "git_push":
		s.act.pushed = true
	case "github_pull_request":
		s.act.prOpened = true
	// The reviewer's delivery (delivery.go): a task that demands a POSTED review
	// cannot pass without a submit. A drafted comment posts nothing on its own -
	// the draft only becomes a review on the PR when github_submit_review
	// succeeds (internal/github).
	case "github_add_review_comment":
		s.act.reviewCommented = true
		s.recordPRNumber(args)
	case "github_submit_review":
		s.act.reviewSubmitted = true
		s.recordPRNumber(args)
	// Execution (delivery.go): a node reviewing a code change cannot pass without
	// having actually RUN something against it. "run_command" here is the ACP
	// ledger label for the worker's own shell/execute tool call (internal/acp/
	// translate.go's mapToolCall), not a quack tool - any successful one counts:
	// the test suite, a build, or a throwaway probe. A non-zero exit_code still
	// ran.
	case "run_command":
		s.act.ranCommand = true
	case "git_clone":
		// A successful clone IS retrieval. citationScore backs the repo URL, URLs
		// under it, and local paths inside the clone dir - without this, a
		// clone-and-read node scores near zero on files of its own clone.
		if u, ok := args["url"].(string); ok && strings.TrimSpace(u) != "" {
			s.act.clonedRepos = append(s.act.clonedRepos, strings.TrimSpace(u))
		}
		dir, _ := resp["dir"].(string)
		if strings.TrimSpace(dir) == "" {
			dir, _ = args["dir"].(string)
		}
		// Resolved against the cwd in effect at clone time (writtenRel), exactly
		// like a write/edit path - so a later delivery step can jail.Resolve this
		// straight to the clone's real location instead of a bare repo-name guess.
		if d := normalizePath(writtenRel(s.nodeDir, s.curCwd, dir)); d != "" {
			s.act.clonedDirs = append(s.act.clonedDirs, d)
		}
	case "git_checkout":
		// The delivery step (delivery.go/commitDelivery) needs to know
		// which branch to push - the worker names it by checking it out, but
		// never pushes it itself.
		if br, ok := resp["branch"].(string); ok && strings.TrimSpace(br) != "" {
			s.act.currentBranch = strings.TrimSpace(br)
		}
	case "git_branch":
		if cur, ok := resp["current"].(string); ok && strings.TrimSpace(cur) != "" {
			s.act.currentBranch = strings.TrimSpace(cur)
		}
	case "read_file", "write_file", "edit_file", "delete_path":
		// Repo-exploration/coding answers cite the files they worked on
		// ([games-repo/app/games.ts](games-repo/app/games.ts)), not web pages -
		// that's correct behavior, and citationScore backs such a path only if it
		// appears here or under a cloned dir.
		pth, ok := args["path"].(string)
		if !ok {
			return
		}
		if np := normalizePath(pth); np != "" {
			s.act.paths[np] = true
		}
		// write/edit only: record the jail-relative path so the judge can re-read
		// the real post-edit source (buildChangedFilesSection).
		if name == "write_file" || name == "edit_file" {
			if jr := writtenRel(s.nodeDir, s.curCwd, pth); jr != "" && !s.writtenSeen[jr] {
				s.writtenSeen[jr] = true
				s.act.written = append(s.act.written, jr)
			}
		}
	}
}
