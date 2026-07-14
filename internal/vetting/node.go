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

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
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

// RunGatedRefine runs the trust-gate refine loop against an already-built worker
// node: worker draft → deterministic citation/length checks → independent judge →
// revise, until the score clears cfg.Threshold or cfg.JudgeRounds is exhausted.
// It is the reusable core shared by NewGatedWorkerNode and the DAG graph builder
// (dag.BuildWorkflow); callable from inside any dynamic-node body since it drives
// the worker via RunNode. prompt is the fully-assembled worker instruction.
//
// Returns (answer, result, err); result carries the final verdict (score/passed/
// feedback/rounds) so the graph can persist it for node_done + continue-but-warn.
// ErrNodeEmpty is returned by RunGatedRefine when the worker produced no answer
// even after the empty-recovery retry. The node body catches it to pause the run
// for a human steer (re-run with guidance) or cancel, instead of silently
// emitting an empty output that cascades into empty dependents.
var ErrNodeEmpty = errors.New("vetting: node produced no answer")

// NodeControl lets a caller cancel or steer a running gate between its stages.
// nil = no control. Cooperative: checked at gate-stage boundaries (before each
// judge round), not mid-model-call: mid-call per-node cancel isn't possible on
// ADK v2 without breaking event streaming (the nodes of one plan share a runner
// and its event stream; cancelling that context would take out the siblings).
//
// That makes this check the BACKSTOP, not the whole story: it is what actually
// ends the node and keeps its partial answer (continue-but-warn), but a worker
// deep in a tool loop can be minutes from the next stage boundary. The TOOL layer
// closes that window — a cancelled node's next tool call is refused outright
// (tools.Deps.NodeCancelled / cancelguard.go), so the worker gives up its turn
// and arrives here promptly. Steer has no such shortcut: it still lands only at
// the next stage boundary.
type NodeControl interface {
	// Cancelled reports whether this node should stop (keep its current answer).
	Cancelled() bool
	// TakeSteer returns and clears any pending steer guidance ("" if none).
	TakeSteer() string
}

// AskToolName is the mid-node HITL tool a worker calls to ask the user a
// question. The tool itself only records the question (in its call args) and ends
// the worker's turn; the GATE detects the call and pauses the node via
// workflow.ResumeOrRequestInput under a round-stable InterruptID, so ADK routes
// the user's answer back to this node on the next turn.
const AskToolName = "ask_user"

// hitlInterruptID is the STABLE per-node, per-round interrupt key for a mid-node
// HITL pause. Node IDs repeat across plans and rounds repeat within a node, but
// (invocation, node, round) is unique — and ADK scopes resume rehydration by
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
		// node's own path — collect them by interrupt ID so each round's answer
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
// prompt — the ask_user calls live in earlier rounds' branches and are filtered
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

func RunGatedRefine(ctx adkagent.Context, nodeID string, workerNode workflow.Node, workerModel model.LLM, judge JudgeFactory, cfg Config, prompt string, attachments []*genai.Part, ctrl NodeControl, emit func(*session.Event) error) (string, GateResult, error) {
	log := slog.With("component", "vetting", "node", nodeID)

	// Per-NODE workspace scope: a plan's nodes run concurrently in ONE chat, so
	// each gets its own directory under the chat scope (<root>/<user>/<chat>/
	// <node>/) — the default cwd its tools resolve relative paths against (they
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
	// activity replays the worker's session from that node dir — the cwd its
	// relative paths (and the judge's re-read of them) resolve against.
	activity := func() workerActivity { return activityFromSessionAt(ctx.Session(), nodeDir) }

	cancelled := func() bool { return ctrl != nil && ctrl.Cancelled() }
	// The judge runs in its own isolated runner (off the workflow event stream), so
	// its activity can't ride that stream. Forward it to the client as a stage:judge
	// run via the SSE sink injected on ctx (executor.Execute) — SSE-only, never
	// written to the session, so it can't re-poison a downstream node's request.
	sink, _ := stream.YieldFromContext(ctx)

	// promptEmit delivers each worker prompt as a session event, but ONLY for
	// agents that can't take RunNode input natively (remote A2A workers — see
	// Config.DeliverPromptEvent). nil disables emitPrompt for local llmagents,
	// whose single-turn contents a stray user-role event would contaminate.
	promptEmit := emit
	if !cfg.DeliverPromptEvent {
		promptEmit = nil
	}

	// Per-node steer/cancel (M5b), cooperative at gate-stage boundaries: ADK v2
	// can't cancel a single model call mid-flight without breaking event streaming,
	// so cancel/steer land between stages. basePrompt is the un-guided task; a steer
	// re-runs the whole gate with the guidance appended.
	basePrompt := prompt
	steerAttempt := 0
	for {
		if cancelled() {
			return "", GateResult{}, nil // cancelled before drafting → empty (continue-but-warn)
		}
		// A steered re-run needs fresh RunNode run IDs: WithRunID replays a completed
		// run (the durable-skip property), so reusing "worker-r0" would replay the
		// pre-steer draft instead of re-invoking the worker with the guidance.
		sfx := ""
		if steerAttempt > 0 {
			sfx = fmt.Sprintf("-s%d", steerAttempt)
		}
		question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}

		// HITL resume: if this node previously paused to ask the user and THIS turn
		// delivered the answer (ADK re-entered the node with a ResumedInput under the
		// round-stable interrupt ID), skip the normal draft — run the worker once with
		// the Q&A folded into a self-contained prompt.
		var answer string
		var err error
		resumed := false
		if scan := scanNodeAsks(ctx.Session(), ctx.InvocationID(), nodeID); scan.pauses > 0 {
			if reply, ok := ctx.ResumedInput(hitlInterruptID(nodeID, scan.pauses)); ok {
				resumed = true
				// The just-delivered answer may not have landed in session history yet
				// at scan time (it arrives as this turn's inbound message) — fill it in
				// from ctx.ResumedInput so the current round is in the transcript too.
				turns := scan.turns
				if n := len(turns); n > 0 && turns[n-1].answer == "" {
					turns[n-1].answer = replyString(reply)
				}
				log.Info("node resumed with user answer", "round", scan.pauses)
				answer, err = runWorkerNode(ctx, workerNode,
					workerInput(withUserAnswer(prompt, turns), attachments),
					fmt.Sprintf("worker-hitl-r%d%s", scan.pauses, sfx), promptEmit)
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
					answer, err = runWorkerNode(ctx, workerNode,
						workerInput(withConfirmDecision(prompt, turns), attachments),
						fmt.Sprintf("worker-confirm-r%d%s", cscan.pauses, sfx), promptEmit)
					if err != nil {
						log.Error("post-decision worker run failed", "err", err)
						return "", GateResult{}, err
					}
				}
			}
		}
		if !resumed {
			answer, err = runWorkerNode(ctx, workerNode, workerInput(prompt, attachments), "worker-r0"+sfx, promptEmit)
			if err != nil {
				// Log at our boundary before returning: ADK's scheduler can swallow a
				// node error into a silent empty completion, so this ERROR line (with the
				// model's error body) is what makes a failed worker visible in the logs.
				log.Error("worker draft failed", "run", "worker-r0", "err", err)
				return "", GateResult{}, err
			}
		}

		// HITL / guard pause: the worker's just-finished turn may have raised a
		// fresh ask_user question or a guard-ladder confirmation (beyond what the
		// gate already paused for) and ended without a real answer. Park the NODE
		// so ADK routes the human's answer/decision back here on the next turn.
		//
		// Runs REGARDLESS of whether the draft is empty. A chatty model asks (or
		// proposes a guarded op) AND writes draft text in the SAME turn (observed
		// live: code-implementer called ask_user with a real design question yet
		// also emitted a draft). Any draft text from THIS turn is discarded on the
		// pause: it was written WITHOUT the user's answer/decision, and the resume
		// path re-runs the worker with the full Q&A / decision folded in — so
		// nothing of value is lost. The SAME check runs after every revise round
		// below (see the judge loop) — a worker often proposes its guarded delivery
		// step only after the judge flags the draft incomplete.
		if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, emit, log); paused {
			return "", GateResult{}, ierr // ErrNodeInterrupted → park
		}

		// Continuation loop: keep giving the worker TOOL-BEARING turns until the WORK
		// is done — not until the model happens to emit text. An agentic harness
		// (goose, OpenHands) calls the model, runs its tools, feeds the results back
		// and calls it AGAIN, until the model says it is finished; quack instead
		// treated ONE llmagent invocation as "the draft" and its text as the
		// deliverable, so a turn that spent its whole output budget on reasoning (empty
		// content) read as "done" — and the gate replaced the worker's continuation
		// with a TOOL-LESS writer that summarised half-finished work. Live TC2 evidence
		// (2026-07-13): four runs, ZERO git_commit calls ever; run v4's answer contained
		// a markdown code block of a file the worker was supposed to WRITE, and the
		// judge passed it at 0.7. Same model, in goose, drove the same task to a pushed
		// branch — an architecture gap, not a model limit.
		//
		// So the loop condition is the WORK, not the text (workIncomplete): an empty
		// draft, or a task that demanded a commit/push the ledger doesn't show. The
		// continuation prompt rides the same delivery path as a revise round
		// (runWorkerNode → RunNode input for a local llmagent, gate-authored session
		// event for a remote A2A worker — see emitPrompt), which is what makes it land
		// at all. Bounded by maxContinueRounds so a stuck worker can't spin forever.
		//
		// The completion test reads the node's OWN task (cfg.Task) — NEVER `prompt`.
		// `prompt` is the assembled worker input, and it carries the user's verbatim
		// request as background (dag.buildTask). Judged against that, EVERY node in a
		// plan whose request ends in "commit, push a branch, and open the PR" is
		// forever incomplete — including the read-only explorers, which have no commit
		// or push tools and whose job is explicitly not to deliver code. Live
		// (2026-07-13): every code-explorer node ran the continuation loop to its bound
		// on `committed=false pushed=false`, reading for HOURS, and not one ever
		// finished or reached a judge round. The delivery check has always keyed on
		// cfg.Task (see the deterministic fold below); only this loop didn't.
		for attempt := 1; attempt <= maxContinueRounds && workIncomplete(answer, cfg.Task, activity()); attempt++ {
			act := activity()
			log.Warn("work not finished; continuing the worker with its tools",
				"attempt", attempt, "empty", strings.TrimSpace(answer) == "", "committed", act.committed, "pushed", act.pushed)
			answer, err = runWorkerNode(ctx, workerNode, buildContinuationPrompt(cfg.Task, act, cfg.Checks),
				fmt.Sprintf("worker-cont%d%s", attempt, sfx), promptEmit)
			if err != nil {
				log.Error("worker continuation failed", "attempt", attempt, "err", err)
				return "", GateResult{}, err
			}
			// A continuation is where the worker finally proposes its guarded delivery
			// step (git_commit/git_push) — park the node for the human exactly as the
			// draft and revise paths do.
			if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, emit, log); paused {
				return "", GateResult{}, ierr // ErrNodeInterrupted → park
			}
		}

		// Last resort for a genuinely stuck worker (nothing, on every turn): a
		// TOOL-LESS writer in a FRESH runner writes up whatever the session shows it
		// found. Better than an empty node — never a substitute for the work itself,
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

		// Judge/revise loop: judge the current answer, fold in the deterministic
		// citation/length criteria, and on a fail revise via a fresh worker call whose
		// prompt inlines the feedback + prior answer (buildRevisionContent is
		// self-contained, so the stateless worker needs no session continuity).
		var res GateResult
		steered := ""
		for round := 1; judge != nil && round <= cfg.JudgeRounds; round++ {
			// Cooperative cancel/steer, checked before each judge round AND before the
			// empty-answer guard below — so an empty node (a reasoning model that
			// produced nothing) can still be cancelled or steered into a fresh attempt.
			// Cancel stops refining (keep the current answer); a steer re-runs the gate.
			if ctrl != nil {
				if ctrl.Cancelled() {
					return answer, res, nil
				}
				if g := ctrl.TakeSteer(); strings.TrimSpace(g) != "" {
					steered = g
					break
				}
			}
			if strings.TrimSpace(answer) == "" {
				break // still nothing to judge after recovery
			}
			act := activity()
			runID := fmt.Sprintf("judge-r%d", round)
			emitJudge(sink, nodeID, stream.SSEEvent{Name: stream.EventAgentStart, Data: stream.AgentStartData{RunID: runID, Agent: "judge", Stage: stream.StageJudge, Round: round}})
			v, jerr := runJudgeAgent(ctx, judge, cfg, question, answer, act, judgePartEmitter(sink, nodeID, runID))
			if jerr != nil {
				// ERROR, not Warn: a judge failure means the answer is going out
				// UNVETTED — that must be loud in the logs, not buried.
				log.Error("judge failed; surfacing answer unvetted", "round", round, "err", jerr)
				emitJudge(sink, nodeID, stream.SSEEvent{Name: stream.EventAgentComplete, Data: stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Status: "unavailable", Reason: jerr.Error()}})
				return answer, res, nil
			}
			v = foldDeterministic(v, answer, act, cfg)
			feedback := composeFeedback(v, cfg.Threshold)
			res = GateResult{Passed: v.Score >= cfg.Threshold, Score: v.Score, Feedback: feedback, Rounds: round}
			emitJudge(sink, nodeID, stream.SSEEvent{Name: stream.EventAgentComplete, Data: stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Score: res.Score, Passed: res.Passed, Feedback: res.Feedback}})
			log.Info("judge round done", "round", round, "score", v.Score, "passed", res.Passed)
			if res.Passed || round >= cfg.JudgeRounds {
				break
			}
			revisePrompt := contentPlainText(buildRevisionContent(cfg.Constitution, question, answer, feedback, act, citationOnlyFailure(v, cfg.Threshold)))
			revised, rerr := runWorkerNode(ctx, workerNode, revisePrompt, fmt.Sprintf("worker-r%d%s", round, sfx), promptEmit)
			if rerr != nil {
				log.Error("revision worker failed; keeping prior answer", "round", round, "err", rerr)
				return answer, res, nil // revision failed; keep the prior answer
			}
			// A revision can ITSELF raise a fresh ask_user or guard-ladder
			// confirmation — a worker commonly proposes the outward-facing,
			// confirm-tiered delivery step (e.g. git_commit + git_push) only AFTER
			// the judge flags the task incomplete, i.e. during a late revise round.
			// The draft-time pause check above never sees that, so without this the
			// unconfirmed operation is silently skipped and the incomplete answer
			// sails to the next judge round (live safety bug 2026-07-12: a
			// code-implementer proposed git_push in worker-r3, the confirm pause
			// never fired, and the judge passed the unpushed answer at 0.7 — no
			// human ever approved the push). Park the node exactly as the draft-time
			// check does; the empty revise output is discarded, and on resume the
			// worker re-runs with the decision folded in.
			if paused, ierr := pauseIfWorkerRaisedHITL(ctx, nodeID, emit, log); paused {
				return "", GateResult{}, ierr // ErrNodeInterrupted → park
			}
			if strings.TrimSpace(revised) != "" {
				answer = revised
			}
		}
		if steered != "" {
			log.Info("node steered; re-running with guidance", "node", nodeID)
			steerAttempt++
			prompt = basePrompt + "\n\n--- User steering guidance (revise your approach accordingly) ---\n" + steered
			continue // re-run the whole gate with the guidance (fresh run IDs)
		}
		if res.Passed {
			commitMemoryOnPass(ctx, cfg, nodeID, answer, activity().staged)
		}
		return answer, res, nil
	}
}

// pauseIfWorkerRaisedHITL parks the node when the worker's latest turn raised a
// NEW ask_user question or guard-ladder confirmation beyond what the gate has
// already paused for. It re-derives the FULL ask/confirm history from session
// events (scanNodeAsks/scanNodeConfirms), so "new" is len(turns) > pauses —
// robust to node-ID reuse and resume re-entry. Returns paused=true (with the
// ErrNodeInterrupted sentinel to propagate) when it parked, false otherwise.
//
// It MUST run after EVERY worker run — the initial draft, each resume run, AND
// each revise round — because a worker frequently proposes its outward-facing,
// guard-confirmed delivery step (git_commit + git_push) only once the judge has
// pushed back that the task is incomplete, i.e. during a LATE revision. A check
// that ran solely after the initial draft never saw that confirmation, so the
// unconfirmed (and incomplete) answer sailed straight to the judge and no human
// ever approved the operation (live safety bug 2026-07-12).
//
// ask_user is checked before the guard confirmation: an interactive question is
// the more specific signal, and a single turn raising both is not a shape any
// worker prompt produces.
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
		// Prefer the guard's own hint — it carries call-specific warnings
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

// commitMemoryOnPass fires the agent's staged knowledge (plus consolidation from
// the accepted answer) into shared memory — only on a gate pass, so nothing is
// remembered from a failed answer. Fire-and-forget: memory is best-effort and
// never blocks or fails the node. Commit also runs with empty staged (its
// answer-extraction still mines the accepted answer), matching the M6 design.
//
// The write is bucketed by SUBJECT (memory.Scope): the repo the node is working in,
// the agent's role family, or the user — never the agent's own name. The gate is the
// right place to resolve that scope: it runs workflow-side, so it holds the real user
// id and the jail coordinates the node's repo was cloned into.
func commitMemoryOnPass(ctx adkagent.Context, cfg Config, author, answer string, staged []memory.Candidate) {
	if cfg.Memory == nil || !cfg.CommitMemory || strings.TrimSpace(answer) == "" {
		return
	}
	sc := memoryScope(ctx, cfg, author)
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		n, err := cfg.Memory.Commit(cctx, sc, author, staged, answer)
		if err != nil {
			slog.Warn("memory commit failed", "component", "vetting", "node", author, "err", err, "staged", len(staged))
			return
		}
		if n > 0 {
			slog.Info("memory committed", "component", "vetting", "node", author,
				"count", n, "repo", sc.Repo, "role", sc.Role, "user", sc.User)
		}
	}()
}

// memoryScope is the node's memory entitlement: the repo it is working in (derived
// from the chat's jail — "" when there is no repo or more than one, in which case the
// write falls back to the role bucket rather than guessing), its agent's role family,
// the real user, and its agent name as the legacy read key.
func memoryScope(ctx adkagent.Context, cfg Config, author string) memory.Scope {
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
// answer already captured the media reading) — re-attaching each round is costly.
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
// model as user messages — exactly what a prompt should be).
const gatePromptAuthor = "quack-gate"

// emitPrompt writes the worker's prompt into the session as a gate-authored
// event, immediately before the RunNode that consumes it. A LOCAL llmagent
// worker doesn't need it (node-mode llmagents take the RunNode input
// directly), but production workers are A2A REMOTE agents, and those build
// their outbound message from SESSION EVENTS ONLY — remoteagent's newMessage
// reads ctx.Session().Events() and drops RunNode input/UserContent on the
// floor (only llmagent implements the NodeRunner interface AgentNode would
// deliver input through). Without this event a remote worker never sees its
// node's task prompt at all, and a follow-up round whose session tail is
// empty SKIPS the dispatch entirely (an empty outbound message short-circuits
// in remoteagent). The emit → scheduler handshake → runner AppendEvent chain
// completes before emit returns, so the event is durably in the session
// before the dispatch that needs it — no ordering race.
//
// The event is filtered everywhere else by its author/branch: the chat store
// skips non-orchestrator authors, the orchestrator's own history is branch-
// filtered, and the dagStream translates it to (at most) the node's
// node_start.
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
	// WithIsolationScopeFromNodePath: every plan node shares ONE workflow
	// session, and a LOCAL single-turn llmagent worker picks its "current turn"
	// pivot by scanning that session's tail for the latest user/foreign-authored
	// event WITHOUT branch filtering (ADK v2.0.0
	// llminternal.buildContentsCurrentTurnContextOnly — branch is only applied
	// AFTER the pivot). A concurrently-running sibling node's event landing at
	// the tail therefore steals the pivot, and everything before it — including
	// THIS worker's own node-input prompt (seeded as a synthetic user event) —
	// falls out of the request window; the branch filter then removes the
	// sibling's events too, leaving the worker an EMPTY request (CI flake:
	// TestRunPlanAsGraph_HITLPauseResume, n2 "asking n1's question"). The pivot
	// scan DOES honour isolation scope, so scoping each worker run to its own
	// node path makes sibling events invisible to it. Scope is inert for remote
	// (A2A) workers — remoteagent ignores IsolationScope; their sibling leak is
	// fixed read-side in internal/agent/a2a.go's part converter instead. Known
	// ADK quirk of scope+single_turn: the node input is ALSO prepended from
	// UserContent, so a local llmagent sees its prompt twice — harmless, and
	// local llmagent workers exist only in tests (production workers are A2A).
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

// foldDeterministic folds the code-owned criteria (citation backing, answer
// length, retrieval grounding) into the judge's verdict and re-aggregates
// (weakest-link). Mirrors the deterministic fold in gate.run's judge stage.
func foldDeterministic(v verdict, answer string, act workerActivity, cfg Config) verdict {
	if v.Criteria == nil {
		v.Criteria = map[string]criterionScore{}
	}
	if ls := lengthScore(answer); ls < 1.0 {
		v.Criteria["sufficient_length"] = criterionScore{Score: ls, Reason: fmt.Sprintf("deterministic: %d chars", len(strings.TrimSpace(answer)))}
	}
	// A retrieval agent that performed ZERO retrieval cannot have a grounded
	// answer — it either answered from model memory (its citations, if any, are
	// unverifiable) or wrote a question to the user as answer text instead of
	// calling ask_user. Hard weakest-link fail with feedback naming both ways
	// out; citationScore abstains in this case (no activity to grade against),
	// which previously let exactly these answers sail through to the judge.
	// A clone or a local file read is retrieval too (cloned-repo grounding):
	// a coding node that consulted the repo on disk instead of the web is not
	// answering from model memory.
	if cfg.RequireRetrieval && len(act.fetched) == 0 && len(act.seen) == 0 && len(act.clonedRepos) == 0 && len(act.paths) == 0 {
		v.Criteria["grounded_in_retrieval"] = criterionScore{Score: 0, Reason: "deterministic: no web_search/web_fetch activity this session — " +
			"research the task and cite what you retrieve; if you are blocked on information only the user has, call ask_user (never write a question to the user as your answer)"}
	}
	if det, details, hasCites := citationScore(answer, act); hasCites {
		v.Criteria["cites_sources"] = criterionScore{Score: det, Reason: fmt.Sprintf("deterministic: %d cited link(s), mean backing %.2f", len(details), det)}
	}
	// §4: deterministic gate checks — the planner's `checks` when it set them,
	// else the ones derived from the repo on disk (checks.go). Untouched for a
	// node that has neither (research, synthesis).
	if c, ok := checksPassCriterion(cfg); ok {
		v.Criteria["checks_pass"] = c
	}
	// Delivery: a node whose task says commit/push/PR cannot pass unless the
	// ledger shows it did, and one told to POST a review on a PR cannot pass
	// unless it submitted one (delivery.go). Untouched for a node whose task
	// demands neither.
	for name, c := range incompleteCriteria(cfg.Task, act) {
		v.Criteria[name] = c
	}
	return aggregateVerdict(v)
}

// composeFeedback merges the judge's own narrative Feedback with the Reasons
// of any criterion scoring below threshold — a deterministic criterion's
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
	if len(extra) == 0 {
		return v.Feedback
	}
	sort.Strings(extra) // stable order across runs (map iteration is random)
	var sb strings.Builder
	if strings.TrimSpace(v.Feedback) != "" {
		sb.WriteString(v.Feedback)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Deterministic check failures:\n")
	sb.WriteString(strings.Join(extra, "\n"))
	return sb.String()
}

// citationOnlyFailure reports whether the ONLY criterion below threshold is
// cites_sources — i.e. the answer is substantively fine and just needs its
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

// activityFromSession reconstructs the worker's retrieval activity (web_search
// queries, fetched URLs, searched URLs) AND its workspace-operation ledger
// (fs/git/run_command calls — see ledger.go) from the workflow session events.
// It replaces the legacy live-stream capture in gate.runWorker: calls are
// paired to their responses by call ID, and web_search results feed the "seen"
// set. Consumed by the deterministic citation check, the judge prompt's
// workspace section (claims-vs-activity), and the revise/finalize prompts.
// joinWritten resolves a path argument against the cwd in effect at the time of
// the call — the read-side mirror of tools.joinCwd (kept here to avoid a
// vetting→tools dependency): a leading "/" ignores the cwd, everything else is
// relative to it.
func joinWritten(cwd, p string) string {
	if strings.HasPrefix(p, "/") {
		return strings.TrimPrefix(p, "/")
	}
	if cwd == "" || cwd == "." {
		return p
	}
	return filepath.Join(cwd, p)
}

// writtenRel resolves a worker's write/edit path argument to a CHAT-relative path
// for Jail.Resolve — the read-side mirror of tools.jailPath. The worker's own paths
// (and the cwd its `cd` reports) are NODE-relative, because the node dir is an
// invisible root the model never sees; the node dir is re-applied here, at the one
// resolution boundary, exactly as the tools do it. A leading "/" is the chat-root
// escape hatch: cwd AND node dir ignored. The result feeds Jail.Resolve, which
// re-verifies containment.
//
// The two namespaces must match. If the worker records paths in one and the judge
// resolves them in another, buildChangedFilesSection silently reads NOTHING and the
// judge degrades to trusting the answer's self-report — this exact regression has
// bitten twice.
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

// activityFromSessionAt replays the worker's session inside nodeDir — the node's
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
				case "stage_memory":
					if cand, ok := stagedCandidate(p.FunctionCall); ok {
						s.act.staged = append(s.act.staged, cand)
					}
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
				// Only a completed call/response pair enters the ledger — an
				// operation with no response never happened as far as claims go.
				if args, known := pendingWs[p.FunctionResponse.ID]; known && pendingWsTool[p.FunctionResponse.ID] == p.FunctionResponse.Name {
					delete(pendingWs, p.FunctionResponse.ID)
					delete(pendingWsTool, p.FunctionResponse.ID)
					s.recordWorkspace(p.FunctionResponse.Name, args, p.FunctionResponse.Response)
				}
			}
			// CODE MODE. A tool called from inside a script (internal/tools/run_code.go)
			// emits no session event of its own, so without this the gate would be blind
			// to it — a node that really did write and commit code would be failed for
			// claiming work with no ledger evidence, and, far worse, real work would go
			// unverified. run_code's response carries a compact record of every call the
			// script made; each one is replayed HERE, through the very same recorders a
			// direct call goes through, in the order the script made them. The result is
			// that a write from inside a script is indistinguishable, to the trust gate,
			// from a direct write_file.
			if p.FunctionResponse != nil && p.FunctionResponse.Name == RunCodeToolName {
				for _, c := range expandRunCode(p.FunctionResponse.Response) {
					s.replay(c)
				}
			}
		}
	}
	return s.act
}

// activityScanner accumulates one worker's activity. Every recorder below is
// reached from BOTH paths — a direct tool call's session events, and a call
// replayed out of a run_code record — which is what makes the two
// indistinguishable to the trust gate. There is one implementation of each rule,
// so the two paths cannot diverge.
type activityScanner struct {
	act     workerActivity
	nodeDir string
	// curCwd is the NODE-relative cwd in effect ("" = the node's own root),
	// updated on each successful cd — including a cd made from inside a script,
	// since the tool writes the same session state either way.
	curCwd      string
	writtenSeen map[string]bool // dedup for act.written
}

// replay folds one call a script made into the activity, exactly as if the model
// had made it directly. It covers the call-side capture too (a search query, a
// staged memory), which for a direct call is read from the FunctionCall part.
func (s *activityScanner) replay(c innerCall) {
	switch c.name {
	case "web_search":
		s.recordSearch(c.args)
		recordSearchResults(s.act.seen, c.result)
	case "web_fetch":
		if u, ok := c.args["url"].(string); ok && strings.TrimSpace(u) != "" {
			s.recordFetch(strings.TrimSpace(u), c.result)
		}
	case "stage_memory":
		if cand, ok := stagedCandidate(&genai.FunctionCall{Name: c.name, Args: c.args}); ok {
			s.act.staged = append(s.act.staged, cand)
		}
	case "cd":
		s.recordCd(c.result)
	default:
		if isWorkspaceTool(c.name) {
			s.recordWorkspace(c.name, c.args, c.result)
		}
	}
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
	// Structured grounding capture (successful ops only — a FAILED clone/read
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
	// cannot pass without a submit. A drafted comment posts nothing on its own —
	// the draft only becomes a review on the PR when github_submit_review
	// succeeds (internal/github).
	case "github_add_review_comment":
		s.act.reviewCommented = true
	case "github_submit_review":
		s.act.reviewSubmitted = true
	// Execution (delivery.go): a node reviewing a code change cannot pass without
	// having actually RUN something against it. Any successful run_command counts
	// — the test suite, a build, or a throwaway probe. A non-zero exit_code still
	// ran.
	case "run_command":
		s.act.ranCommand = true
	case "git_clone":
		// A successful clone IS retrieval: the whole repository is now local,
		// strictly more consulted than a search-result snippet. citationScore gives
		// full backing to the repo URL, to URLs under it (blob/tree links), and to
		// local paths inside the clone dir — without this, a node following the
		// research-git-repos flow (clone + read instead of web_fetch) scored 0.25
		// backing on files of the very repo it had cloned (live failures
		// 2026-07-10, 2026-07-12).
		if u, ok := args["url"].(string); ok && strings.TrimSpace(u) != "" {
			s.act.clonedRepos = append(s.act.clonedRepos, strings.TrimSpace(u))
		}
		dir, _ := resp["dir"].(string)
		if strings.TrimSpace(dir) == "" {
			dir, _ = args["dir"].(string)
		}
		if d := normalizePath(dir); d != "" {
			s.act.clonedDirs = append(s.act.clonedDirs, d)
		}
	case "read_file", "write_file", "edit_file", "delete_path":
		// Repo-exploration/coding answers cite the files they worked on
		// ([games-repo/app/games.ts](games-repo/app/games.ts)), not web pages —
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
