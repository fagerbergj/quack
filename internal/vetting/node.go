package vetting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// ADK v2 without breaking event streaming.
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

		// Empty-answer recovery: the worker sometimes ends a turn with no answer text
		// (it called tools but never wrote up, or thought into the void). Re-invoke it
		// with a finalize prompt asking it to write up what it found, up to the retry
		// budget. ponytail: the legacy gate used a tool-LESS writer clone here (a
		// tool-having re-invoke can keep researching); re-invoking the worker with a
		// finalize prompt is the graph-native port — swap in a tool-less writer node if
		// empty answers prove sticky.
		if strings.TrimSpace(answer) == "" {
			// Empty (no error) is the OTHER silent failure mode besides a 400 — a
			// reasoning model can spend its whole output budget on thinking (or trap its
			// tool calls in reasoning) and return empty content. Recover via a TOOL-LESS
			// writer run in a FRESH runner with the findings: re-invoking the worker in
			// its own session drops the finalize prompt (llmagent rebuilds from session
			// events, which end in the empty reply), so that write-up never happened.
			log.Warn("worker draft empty; recovering via tool-less writer", "retries", maxEmptyRetries)
			fin := buildFinalizeContent(question, activityFromSession(ctx.Session()))
			for attempt := 1; attempt <= maxEmptyRetries && strings.TrimSpace(answer) == ""; attempt++ {
				answer, err = runWriterFresh(ctx, workerModel, fin)
				if err != nil {
					log.Error("writer recovery failed", "attempt", attempt, "err", err)
					return "", GateResult{}, err
				}
			}
			if strings.TrimSpace(answer) == "" {
				log.Error("worker produced NO answer; writer recovery also empty", "retries", maxEmptyRetries)
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
			act := activityFromSession(ctx.Session())
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
			revisePrompt := contentPlainText(buildRevisionContent(cfg.Constitution, question, answer, feedback, act))
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
			commitMemoryOnPass(ctx, cfg, nodeID, answer, activityFromSession(ctx.Session()).staged)
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

// stagedCandidate parses a stage_memory tool call's args (content + optional
// kind) into a memory candidate; ok=false if there's no usable content.
func stagedCandidate(fc *genai.FunctionCall) (memory.Candidate, bool) {
	c, ok := fc.Args["content"].(string)
	if !ok || strings.TrimSpace(c) == "" {
		return memory.Candidate{}, false
	}
	cand := memory.Candidate{Content: strings.TrimSpace(c)}
	if k, ok := fc.Args["kind"].(string); ok && k != "" {
		cand.Metadata = map[string]string{"kind": k}
	}
	return cand, true
}

// commitMemoryOnPass fires the agent's staged tradecraft (plus consolidation from
// the accepted answer) into task memory — only on a gate pass, so nothing is
// remembered from a failed answer. Fire-and-forget: memory is best-effort and
// never blocks or fails the node. Commit also runs with empty staged (its
// answer-extraction still mines the accepted answer), matching the M6 design.
func commitMemoryOnPass(ctx adkagent.Context, cfg Config, author, answer string, staged []memory.Candidate) {
	if cfg.Memory == nil || !cfg.CommitMemory || strings.TrimSpace(answer) == "" {
		return
	}
	userID := ""
	if s := ctx.Session(); s != nil {
		userID = s.UserID()
	}
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		n, err := cfg.Memory.Commit(cctx, userID, author, staged, answer)
		if err != nil {
			slog.Warn("memory commit failed", "component", "vetting", "node", author, "err", err, "staged", len(staged))
			return
		}
		if n > 0 {
			slog.Info("memory committed", "component", "vetting", "node", author, "count", n, "user", userID)
		}
	}()
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
	// §4: orchestrator-set deterministic gate checks (a code-implementer
	// node's `go build`/`go test`/… commands) — untouched for a node with no
	// Checks configured (research, synthesis).
	if len(cfg.Checks) > 0 {
		v.Criteria["checks_pass"] = checksPassCriterion(cfg)
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

// activityFromSession reconstructs the worker's retrieval activity (web_search
// queries, fetched URLs, searched URLs) AND its workspace-operation ledger
// (fs/git/run_command calls — see ledger.go) from the workflow session events.
// It replaces the legacy live-stream capture in gate.runWorker: calls are
// paired to their responses by call ID, and web_search results feed the "seen"
// set. Consumed by the deterministic citation check, the judge prompt's
// workspace section (claims-vs-activity), and the revise/finalize prompts.
func activityFromSession(sess session.Session) workerActivity {
	act := workerActivity{fetched: map[string]fetchRecord{}, seen: map[string]string{}, paths: map[string]bool{}}
	if sess == nil {
		return act
	}
	pending := map[string]string{}           // web_fetch call ID → URL
	pendingWs := map[string]map[string]any{} // workspace-tool call ID → args (see ledger.go)
	pendingWsTool := map[string]string{}     // workspace-tool call ID → tool name
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
					if q, ok := p.FunctionCall.Args["query"].(string); ok && strings.TrimSpace(q) != "" {
						act.searches = append(act.searches, strings.TrimSpace(q))
					}
				case "web_fetch":
					if u, ok := p.FunctionCall.Args["url"].(string); ok && strings.TrimSpace(u) != "" {
						pending[p.FunctionCall.ID] = strings.TrimSpace(u)
					}
				case "stage_memory":
					if cand, ok := stagedCandidate(p.FunctionCall); ok {
						act.staged = append(act.staged, cand)
					}
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
					if result, ok := p.FunctionResponse.Response["result"].(string); ok && strings.TrimSpace(result) != "" {
						act.fetched[url] = fetchRecord{sample: strings.TrimSpace(trimToSample(result))}
					}
				}
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "web_search" {
				recordSearchResults(act.seen, p.FunctionResponse.Response)
			}
			if p.FunctionResponse != nil && isWorkspaceTool(p.FunctionResponse.Name) {
				// Only a completed call/response pair enters the ledger — an
				// operation with no response never happened as far as claims go.
				// Failures ARE recorded (recordWsOp marks them FAILED): "the tests
				// passed" claimed over a failed run must be contradictable.
				if args, known := pendingWs[p.FunctionResponse.ID]; known && pendingWsTool[p.FunctionResponse.ID] == p.FunctionResponse.Name {
					delete(pendingWs, p.FunctionResponse.ID)
					delete(pendingWsTool, p.FunctionResponse.ID)
					act.workspace = append(act.workspace, recordWsOp(p.FunctionResponse.Name, args, p.FunctionResponse.Response))
					// Structured grounding capture (successful ops only — a FAILED
					// clone/read retrieved nothing and backs no citation, though it
					// stays in the ledger above for claim-checking).
					if _, failed := p.FunctionResponse.Response["error"]; !failed {
						switch p.FunctionResponse.Name {
						case "git_clone":
							// A successful clone IS retrieval: the whole repository is
							// now local, strictly more consulted than a search-result
							// snippet. citationScore gives full backing to the repo URL,
							// to URLs under it (blob/tree links), and to local paths
							// inside the clone dir — without this, a node following the
							// research-git-repos flow (clone + read instead of
							// web_fetch) scored 0.25 backing on files of the very repo
							// it had cloned (live failures 2026-07-10, 2026-07-12).
							if u, ok := args["url"].(string); ok && strings.TrimSpace(u) != "" {
								act.clonedRepos = append(act.clonedRepos, strings.TrimSpace(u))
							}
							dir, _ := p.FunctionResponse.Response["dir"].(string)
							if strings.TrimSpace(dir) == "" {
								dir, _ = args["dir"].(string)
							}
							if d := normalizePath(dir); d != "" {
								act.clonedDirs = append(act.clonedDirs, d)
							}
						case "read_file", "write_file", "edit_file", "delete_path":
							// Repo-exploration/coding answers cite the files they worked
							// on ([games-repo/app/games.ts](games-repo/app/games.ts)),
							// not web pages — that's correct behavior, and citationScore
							// backs such a path only if it appears here or under a
							// cloned dir.
							if pth, ok := args["path"].(string); ok {
								if np := normalizePath(pth); np != "" {
									act.paths[np] = true
								}
							}
						}
					}
				}
			}
		}
	}
	return act
}
