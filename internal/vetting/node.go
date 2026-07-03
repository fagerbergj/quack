package vetting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// hitlScan summarizes a node's HITL history within ONE invocation: how many times
// its worker asked the user (asks), how many pauses the gate already emitted for
// it (pauses), and the most recent question. Derived entirely from session events
// (no state keys) so it survives resume re-entry and node-ID reuse across plans.
type hitlScan struct {
	asks   int
	pauses int
	lastQ  string
}

func scanNodeAsks(sess session.Session, invocationID, nodeID string) hitlScan {
	var s hitlScan
	if sess == nil {
		return s
	}
	prefix := "hitl-" + nodeID + "-r"
	for ev := range sess.Events().All() {
		if ev == nil || ev.Content == nil || ev.InvocationID != invocationID || !pathHasNode(ev, nodeID) {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil || p.FunctionCall == nil {
				continue
			}
			switch p.FunctionCall.Name {
			case AskToolName:
				s.asks++
				if q, ok := p.FunctionCall.Args["question"].(string); ok && strings.TrimSpace(q) != "" {
					s.lastQ = strings.TrimSpace(q)
				}
			case workflow.WorkflowInputFunctionCallName:
				if strings.HasPrefix(p.FunctionCall.ID, prefix) {
					s.pauses++
				}
			}
		}
	}
	return s
}

// pathHasNode reports whether the event was emitted under the given graph node
// (NodeInfo.Path segments are "name@run"; worker/advisor child runs nest below
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

// withUserAnswer builds the self-contained prompt for the post-answer worker run.
// The worker runs in a fresh sub-branch, so the Q&A must ride the prompt (the
// ask_user call lives in the previous run's branch and is filtered from this
// run's LLM history).
func withUserAnswer(prompt, question, answer string) string {
	return prompt + "\n\n--- You previously asked the user a question and they have now answered ---\nQ: " + question +
		"\nA: " + answer + "\n\nUse this answer and complete the task now. Do not ask again."
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

func RunGatedRefine(ctx adkagent.Context, nodeID string, workerNode, advisorNode workflow.Node, workerModel model.LLM, judge JudgeFactory, cfg Config, prompt string, attachments []*genai.Part, ctrl NodeControl, emit func(*session.Event) error) (string, GateResult, error) {
	log := slog.With("component", "vetting", "node", nodeID)

	// Advisor consult (formative, once per worker round). Best-effort: on error,
	// proceed WITHOUT advice rather than fail the node. It runs via RunNode so it
	// streams to the UI as a stage:advisor run (dagStream translates advisor-rN).
	// Replaces the dropped self-critique stage — an independent second look at the
	// approach before the worker commits.
	consult := func(runID, task string) string {
		if advisorNode == nil {
			return ""
		}
		advice, aerr := runWorkerNode(ctx, advisorNode, "Advise on this task before it is attempted:\n\n"+task, runID)
		if aerr != nil {
			log.Warn("advisor consult failed; proceeding without advice", "run", runID, "err", aerr)
			return ""
		}
		return advice
	}
	withAdvice := func(base, advice string) string {
		if strings.TrimSpace(advice) == "" {
			return base
		}
		return base + "\n\n--- Advisor guidance (consider before answering) ---\n" + advice
	}
	cancelled := func() bool { return ctrl != nil && ctrl.Cancelled() }
	// The judge runs in its own isolated runner (off the workflow event stream), so
	// its activity can't ride that stream. Forward it to the client as a stage:judge
	// run via the SSE sink injected on ctx (executor.Execute) — SSE-only, never
	// written to the session, so it can't re-poison a downstream node's request.
	sink, _ := stream.YieldFromContext(ctx)

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
				log.Info("node resumed with user answer", "round", scan.pauses)
				answer, err = runWorkerNode(ctx, workerNode,
					workerInput(withUserAnswer(prompt, scan.lastQ, replyString(reply)), attachments),
					fmt.Sprintf("worker-hitl-r%d%s", scan.pauses, sfx))
				if err != nil {
					log.Error("post-answer worker run failed", "err", err)
					return "", GateResult{}, err
				}
			}
		}
		if !resumed {
			answer, err = runWorkerNode(ctx, workerNode, workerInput(withAdvice(prompt, consult("advisor-r0"+sfx, prompt)), attachments), "worker-r0"+sfx)
			if err != nil {
				// Log at our boundary before returning: ADK's scheduler can swallow a
				// node error into a silent empty completion, so this ERROR line (with the
				// model's error body) is what makes a failed worker visible in the logs.
				log.Error("worker draft failed", "run", "worker-r0", "err", err)
				return "", GateResult{}, err
			}
		}

		// HITL pause: the worker just called ask_user (a fresh ask beyond the pauses
		// already requested) and ended its turn without an answer. Park the NODE via
		// ResumeOrRequestInput — first pass emits the request event and returns
		// ErrNodeInterrupted (propagated up; NOT a failure, no empty-recovery); on the
		// answer turn ADK re-enters the node and the resume branch above runs instead.
		if strings.TrimSpace(answer) == "" && emit != nil {
			if scan := scanNodeAsks(ctx.Session(), ctx.InvocationID(), nodeID); scan.asks > scan.pauses {
				log.Info("worker asked the user; pausing node", "question", scan.lastQ, "round", scan.pauses+1)
				_, ierr := workflow.ResumeOrRequestInput(ctx, emit, session.RequestInput{
					InterruptID: hitlInterruptID(nodeID, scan.pauses+1),
					Message:     scan.lastQ,
				})
				return "", GateResult{}, ierr // ErrNodeInterrupted → park
			}
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
			v, jerr := runJudgeAgent(ctx, judge, cfg, question, answer, judgePartEmitter(sink, nodeID, runID))
			if jerr != nil {
				// ERROR, not Warn: a judge failure means the answer is going out
				// UNVETTED — that must be loud in the logs, not buried.
				log.Error("judge failed; surfacing answer unvetted", "round", round, "err", jerr)
				emitJudge(sink, nodeID, stream.SSEEvent{Name: stream.EventAgentComplete, Data: stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Status: "unavailable", Reason: jerr.Error()}})
				return answer, res, nil
			}
			v = foldDeterministic(v, answer, act)
			res = GateResult{Passed: v.Score >= cfg.Threshold, Score: v.Score, Feedback: v.Feedback, Rounds: round}
			emitJudge(sink, nodeID, stream.SSEEvent{Name: stream.EventAgentComplete, Data: stream.AgentCompleteData{RunID: runID, Stage: stream.StageJudge, Round: round, Score: res.Score, Passed: res.Passed, Feedback: res.Feedback}})
			log.Info("judge round done", "round", round, "score", v.Score, "passed", res.Passed)
			if res.Passed || round >= cfg.JudgeRounds {
				break
			}
			revisePrompt := contentPlainText(buildRevisionContent(cfg.Constitution, question, answer, v.Feedback, act))
			advRun := fmt.Sprintf("advisor-r%d%s", round, sfx)
			revised, rerr := runWorkerNode(ctx, workerNode, withAdvice(revisePrompt, consult(advRun, revisePrompt)), fmt.Sprintf("worker-r%d%s", round, sfx))
			if rerr != nil {
				log.Error("revision worker failed; keeping prior answer", "round", round, "err", rerr)
				return answer, res, nil // revision failed; keep the prior answer
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

func runWorkerNode(ctx adkagent.Context, workerNode workflow.Node, input any, runID string) (string, error) {
	t0 := time.Now()
	out, err := workflow.RunNode[string](ctx, workerNode, input,
		workflow.WithUseSubBranch(), workflow.WithRunID(runID))
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
// length) into the judge's verdict and re-aggregates (weakest-link). Mirrors the
// deterministic fold in gate.run's judge stage.
func foldDeterministic(v verdict, answer string, act workerActivity) verdict {
	if v.Criteria == nil {
		v.Criteria = map[string]criterionScore{}
	}
	if ls := lengthScore(answer); ls < 1.0 {
		v.Criteria["sufficient_length"] = criterionScore{Score: ls, Reason: fmt.Sprintf("deterministic: %d chars", len(strings.TrimSpace(answer)))}
	}
	if det, details, hasCites := citationScore(answer, act); hasCites {
		v.Criteria["cites_sources"] = criterionScore{Score: det, Reason: fmt.Sprintf("deterministic: %d cited URL(s), mean backing %.2f", len(details), det)}
	}
	return aggregateVerdict(v)
}

// activityFromSession reconstructs the worker's retrieval activity (web_search
// queries, fetched URLs, searched URLs) from the workflow session events. It
// replaces the legacy live-stream capture in gate.runWorker: web_fetch calls are
// paired to their responses by call ID, and web_search results feed the "seen"
// set. Consumed by the deterministic citation check.
func activityFromSession(sess session.Session) workerActivity {
	act := workerActivity{fetched: map[string]fetchRecord{}, seen: map[string]string{}}
	if sess == nil {
		return act
	}
	pending := map[string]string{} // web_fetch call ID → URL
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
		}
	}
	return act
}
