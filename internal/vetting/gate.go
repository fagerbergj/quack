// Package vetting implements Quack's trust gate: a custom ADK agent that wraps a
// worker agent and makes its output trustworthy before returning it. It runs the
// worker, optionally lets the worker self-refine its own draft, then submits the
// answer to an independent judge model that scores it against a standing rubric,
// revising up to MaxRounds until the score clears Threshold.
//
// The gate is platform code, not part of an agent bundle — bundle authors still
// write only a card + prompt. The gate is applied by wrapping the worker before
// it is served over A2A (see cmd/server), so the orchestrator dispatches to the
// gated agent unchanged. The gated agent echoes the worker's name/description so
// A2A dispatch still resolves it.
//
// Streaming model: the worker's thinking and tool activity stream through live,
// but the gate buffers the worker's answer text and surfaces only the final,
// vetted answer — so the chat never shows an un-vetted draft. Self-refine and
// judge activity surface as dedicated wire events (see internal/stream).
package vetting

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// Config is the resolved vetting policy: per-stage round budgets plus the loaded
// constitution and rubric text. Build it with FromConfig. The three stages run
// cheapest-first: deterministic checks → self-critique → judge.
type Config struct {
	DeterministicRounds int     // cheap citation/length check + targeted revise cycles
	SelfCritiqueRounds  int     // worker self-improvement passes before the judge
	JudgeRounds         int     // expensive model-judge/revise rounds
	Threshold           float64 // judge pass score in (0,1]
	JudgeMaxIterations  int     // cap on the agentic judge's model turns per round (0 ⇒ default)
	Constitution        string  // global principles; used for self-critique + prefixed in judge prompt
	Rubric              string  // scoring guide; global default or per-agent override
}

// judgeAuthor is the ADK author the gate stamps on the judge's streamed display
// events (thinking + tool activity). A distinct author keeps them out of the
// worker's session view during agentic revision — filteredSession drops them,
// and ADK's contents processor would otherwise convert/replay foreign events —
// so the worker is driven only by the judge's feedback, not its raw transcript.
const judgeAuthor = "judge"

type gate struct {
	worker   adkagent.Agent
	writer   adkagent.Agent // tool-less variant of the worker for the finalize write-up
	newJudge JudgeFactory
	cfg      Config
	name     string
}

// GatedAgent is the public handle for a trust-gated worker. It embeds the ADK
// agent (Name/Description/Run pass through) so it satisfies adkagent.Agent, and
// exposes the inner worker so a2a.Serve can pull its tool-derived skills for the
// published AgentCard.
type GatedAgent struct {
	adkagent.Agent
	worker adkagent.Agent
}

// Worker returns the wrapped worker (consumed by agent.Serve via the
// agentWithWorker interface to propagate skill metadata to the AgentCard).
func (g *GatedAgent) Worker() adkagent.Agent { return g.worker }

// NewGatedAgent wraps worker in the trust gate. judge is a factory for the
// independent agentic judge (built per round so each round's submit_verdict
// binds a clean sink). The worker continues its own session for the agentic
// self-refine and revision passes, so no separate worker model is needed.
//
// writer is a TOOL-LESS variant of the worker (same model + prompt, no tools)
// used only for the finalize write-up: when the worker ends a turn having called
// tools but written no answer, a tool-having re-invoke just keeps researching
// (it ignores "stop and write"), so the write-up pass uses a tool-less agent
// that physically cannot call tools and must produce text from context. Pass nil
// to fall back to the worker (finalize then behaves as before).
func NewGatedAgent(worker, writer adkagent.Agent, judge JudgeFactory, cfg Config) (*GatedAgent, error) {
	if writer == nil {
		writer = worker
	}
	g := &gate{worker: worker, writer: writer, newJudge: judge, cfg: cfg, name: worker.Name()}
	// The worker is invoked directly (g.worker.Run), not registered as a SubAgent:
	// the gate echoes the worker's name so A2A dispatch resolves it, and a SubAgent
	// of the same name would collide in the runner's agent-tree uniqueness check.
	ag, err := adkagent.New(adkagent.Config{
		Name:        worker.Name(),
		Description: worker.Description(),
		Run:         g.run,
	})
	if err != nil {
		return nil, err
	}
	return &GatedAgent{Agent: ag, worker: worker}, nil
}

func (g *gate) run(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		t0 := time.Now()
		// Run IDs are unique per node already (each node is its own invocation with
		// its own Translator, and events are scoped by node_id), but prefix with the
		// invocation ID so they're globally unique too — clearer in logs and immune
		// to any consumer that keys runs without the node_id.
		runN := 0
		nextRun := func() string { runN++; return fmt.Sprintf("%s-r%d", ctx.InvocationID(), runN) }

		// ── Worker stage: round-0 draft + an optional finalize write-up, under one
		// run. Thinking/tool events stream through (attributed to this run by the
		// Translator); the answer text is buffered so only the vetted answer surfaces.
		workerRun := nextRun()
		if !g.emit(ctx, yield, stream.AgentStartPart(workerRun, g.name, stream.StageWorker, 0)) {
			return
		}
		log.Printf("vetting[%s]: worker start (run=%s)", g.name, workerRun)
		// Run the worker against a gate-marker-filtered session view: the agent_start
		// marker just emitted is an orphan FunctionResponse that ADK would otherwise
		// choke on when the worker's llmagent rebuilds its request from the session.
		answer, act, ok := g.runWorker(newCritiqueContext(ctx, ctx.UserContent()), yield, g.worker, false)
		if !ok {
			return
		}
		log.Printf("vetting[%s]: worker done in %s answer_len=%d", g.name, time.Since(t0).Round(time.Second), len(answer))

		// Empty-answer recovery: the worker sometimes ends a turn with no answer
		// content — it tries to call another tool, or (on a long context) thinks into
		// the void and stops. Re-invoke it — continuing its session, all tool results
		// in context — to write up what it found, retrying until we get a real answer
		// or the retry budget is exhausted.
		if strings.TrimSpace(answer) == "" {
			finalized, mergedAct, ok := g.finalizeUntilNonEmpty(ctx, yield, act)
			if !ok {
				return
			}
			act = mergedAct
			if strings.TrimSpace(finalized) != "" {
				answer = finalized
			}
		}
		if !g.emit(ctx, yield, stream.AgentCompletePart(stream.AgentCompleteData{RunID: workerRun, Stage: stream.StageWorker})) {
			return
		}

		// Retrieval summary — so "the agent cited URLs it didn't fetch" can be
		// checked against what the harness actually recorded. If this shows real
		// fetched URLs but the judge still says "not fetched", it's a citation/URL
		// mismatch, not a missing fetch; if it shows 0 fetched, the worker (model)
		// genuinely didn't fetch.
		log.Printf("vetting[%s]: retrieval: fetched=%d %v · seen-via-search=%d", g.name, len(act.fetched), fetchedKeys(act.fetched), len(act.seen))

		// If the worker produced nothing even after the finalize pass, don't burn a
		// (slow, agentic) judge round scoring an empty answer 0 — surface it directly
		// so the node reads as "no answer" rather than a confusing failed verdict.
		if strings.TrimSpace(answer) == "" {
			log.Printf("vetting[%s]: still no answer after finalize; skipping judge", g.name)
			g.emitAnswer(ctx, yield, answer)
			return
		}

		// Refinement stages (self-critique → deterministic → judge), mutating
		// `answer`/`act`. An early `return` here means a worker stream ended — i.e.
		// the consumer has gone and `yield` is dead — so we must NOT emit again
		// (doing so panics the range), and a gone consumer can't receive an answer
		// anyway. The judge-error path is the exception: the consumer is alive, so
		// it surfaces the unvetted answer explicitly.
		{
			// ══ Stage 1 — Self-critique: the worker critiques and revises its own
			// draft against the rubric (agentic — it may fetch sources for weak
			// claims), up to SelfCritiqueRounds passes, stopping early once a pass
			// changes nothing. Runs before the deterministic check so the citation
			// check below grades against the post-critique retrieval (a source the
			// critique just fetched no longer reads as unbacked) — and so a still-
			// empty answer is caught and recovered by the deterministic stage.
			for round := 1; round <= g.cfg.SelfCritiqueRounds; round++ {
				tsr := time.Now()
				srRun := nextRun()
				if !g.emit(ctx, yield, stream.AgentStartPart(srRun, g.name, stream.StageSelfRefine, round)) {
					return
				}
				refined, mergedAct, ok := g.runSelfCritique(ctx, yield, answer, act)
				if !ok {
					log.Printf("vetting[%s]: self-critique %d stopped early (worker stream ended); keeping current answer (len=%d)", g.name, round, len(answer))
					return
				}
				act = mergedAct
				changed := refined != "" && strings.TrimSpace(refined) != strings.TrimSpace(answer)
				if changed {
					answer = refined
				}
				log.Printf("vetting[%s]: self-critique %d done in %s changed=%v", g.name, round, time.Since(tsr).Round(time.Millisecond), changed)
				if !g.emit(ctx, yield, stream.AgentCompletePart(stream.AgentCompleteData{RunID: srRun, Stage: stream.StageSelfRefine, Round: round, Changed: changed})) {
					return
				}
				if !changed {
					break
				}
			}

			// Empty-answer safety net: if self-critique somehow left the answer empty
			// (e.g. it overwrote the draft with an empty agentic turn), recover it with
			// the tool-less finalize write-up before the deterministic/judge stages —
			// catches the 0-length issue at the latest point we still can.
			if strings.TrimSpace(answer) == "" {
				finalized, mergedAct, ok := g.finalizeUntilNonEmpty(ctx, yield, act)
				if !ok {
					return
				}
				act = mergedAct
				if strings.TrimSpace(finalized) != "" {
					answer = finalized
				}
			}

			// ══ Stage 2 — Deterministic checks. The citation + length checks are free
			// and run here, after self-critique, so they grade the final retrieval and
			// drive targeted worker revisions to drop any still-fabricated citations
			// before the expensive judge round. The loop short-circuits the moment the
			// answer is clean; each fix is a worker pass, bounded by DeterministicRounds.
			prevUnbacked := -1
			for round := 1; round <= g.cfg.DeterministicRounds; round++ {
				_, details, hasCites := citationScore(answer, act)
				unbacked := unbackedCitations(details)
				if !hasCites || len(unbacked) == 0 {
					break // nothing the deterministic check can fix
				}
				// Stop if the last revise didn't reduce the unbacked count — the worker
				// won't drop these citations, so more passes just thrash and balloon the
				// session (which then overflows and fails a later stage). The remaining
				// unbacked citations still fold into the judge's verdict.
				if prevUnbacked >= 0 && len(unbacked) >= prevUnbacked {
					log.Printf("vetting[%s]: deterministic revise made no progress (%d unbacked); stopping", g.name, len(unbacked))
					break
				}
				prevUnbacked = len(unbacked)
				td := time.Now()
				detRun := nextRun()
				if !g.emit(ctx, yield, stream.AgentStartPart(detRun, g.name, stream.StageRevise, round)) {
					return
				}
				fb := "These cited URLs were never retrieved this session — drop them (and any claim relying only on them) or cite a page you actually fetched: " + strings.Join(unbacked, ", ")
				revised, mergedAct, ok := g.runAgenticRevision(ctx, yield, answer, fb, act)
				if !ok {
					log.Printf("vetting[%s]: deterministic revise %d stopped early (worker stream ended); keeping current answer (len=%d)", g.name, round, len(answer))
					return
				}
				act = mergedAct
				if strings.TrimSpace(revised) != "" {
					answer = revised
				}
				log.Printf("vetting[%s]: deterministic revise %d done in %s (%d unbacked citation(s))", g.name, round, time.Since(td).Round(time.Millisecond), len(unbacked))
				if !g.emit(ctx, yield, stream.AgentCompletePart(stream.AgentCompleteData{RunID: detRun, Stage: stream.StageRevise, Round: round})) {
					return
				}
			}

			// ══ Stage 3 — Judge (most expensive, last). Each round is a FULL round —
			// a judge run, then (on a fail) a revise run — repeated until the score
			// clears the threshold or we run out of rounds. Deterministic citation/
			// length scores are folded into the verdict. Skipped when no judge is
			// configured (g.newJudge nil) or JudgeRounds is 0. A judge error degrades
			// gracefully: complete the run with status=unavailable, then surface.
			for round := 1; g.newJudge != nil && round <= g.cfg.JudgeRounds; round++ {
				tj := time.Now()
				judgeRun := nextRun()
				if !g.emit(ctx, yield, stream.AgentStartPart(judgeRun, judgeAuthor, stream.StageJudge, round)) {
					return
				}
				// The judge runs in its own isolated runner+session; its thinking and tool
				// activity stream back as display copies authored as judgeAuthor (so the
				// worker's revision context filters them out). emitJudge returning false
				// means the consumer disconnected — runJudgeAgent turns that into
				// context.Canceled and aborts the round.
				emitJudge := func(part *genai.Part) bool {
					return g.emitAuthored(ctx, yield, judgeAuthor, part)
				}
				v, err := runJudgeAgent(ctx, g.newJudge, g.cfg, ctx.UserContent(), answer, emitJudge)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return // consumer stopped mid-judge; exit cleanly
					}
					log.Printf("vetting[%s]: judge round %d error after %s: %v (surfacing answer unvetted)", g.name, round, time.Since(tj).Round(time.Millisecond), err)
					if !g.emit(ctx, yield, stream.AgentCompletePart(stream.AgentCompleteData{RunID: judgeRun, Stage: stream.StageJudge, Round: round, Status: "unavailable", Reason: err.Error()})) {
						return
					}
					g.emitAnswer(ctx, yield, answer) // consumer alive — surface the unvetted answer
					return
				}
				// Deterministic gates own what a one-shot small model can't reliably
				// judge — citation backing and answer length — computed in code and
				// folded into the verdict as code-owned criteria, then re-aggregated once.
				//   • cites_sources: each cited URL graded against what was actually
				//     fetched/searched (exact-fetch 1.0 → same-host-search 0.25 → 0.0).
				//     Overrides the model's value; left to the model only when the answer
				//     cites nothing (that's a "did it cite at all" call, not backing).
				//   • sufficient_length: an absolute length floor (incomplete answers).
				if v.Criteria == nil {
					v.Criteria = map[string]criterionScore{}
				}
				trimmedLen := len(strings.TrimSpace(answer))
				ls := lengthScore(answer)
				// Only fold length in when it docks (empty), so a constant 1.0 doesn't
				// inflate the criterion mean for every normal answer.
				if ls < 1.0 {
					v.Criteria["sufficient_length"] = criterionScore{Score: ls, Reason: fmt.Sprintf("deterministic: %d chars", trimmedLen)}
				}
				det, details, hasCites := citationScore(answer, act)
				if hasCites {
					v.Criteria["cites_sources"] = criterionScore{Score: det, Reason: fmt.Sprintf("deterministic: %d cited URL(s), mean backing %.2f", len(details), det)}
				}
				v = aggregateVerdict(v)
				log.Printf("vetting[%s]: deterministic gates: length=%.2f (%d chars) citations=%.2f hasCites=%v %v", g.name, ls, trimmedLen, det, hasCites, details)
				var notes []string
				if ls < 1.0 {
					notes = append(notes, "the answer is empty — produce a complete answer")
				}
				if weak := weakCitations(details); weak != "" {
					notes = append(notes, "weakly-sourced citations (cite a page you fetched, or drop the claim): "+weak)
				}
				if len(notes) > 0 {
					v.Feedback = strings.TrimSpace(v.Feedback + "\n\n" + strings.Join(notes, "; "))
				}
				passed := v.Score >= g.cfg.Threshold
				log.Printf("vetting[%s]: judge round %d done in %s score=%.2f passed=%v", g.name, round, time.Since(tj).Round(time.Millisecond), v.Score, passed)
				if !g.emit(ctx, yield, stream.AgentCompletePart(stream.AgentCompleteData{RunID: judgeRun, Stage: stream.StageJudge, Round: round, Score: v.Score, Passed: passed, Feedback: v.Feedback})) {
					return
				}
				if passed {
					break
				}
				// Revise stage (runs on every failed round, including the last — so one
				// round = judge + refine): re-invoke the worker so it continues its own
				// session with
				// full tool access to address the judge's feedback. A sibling run of the
				// judge — never nested under it. New retrieval merges into act for the next
				// judge round.
				tr := time.Now()
				reviseRun := nextRun()
				if !g.emit(ctx, yield, stream.AgentStartPart(reviseRun, g.name, stream.StageRevise, round)) {
					return
				}
				revised, mergedAct, ok := g.runAgenticRevision(ctx, yield, answer, v.Feedback, act)
				if !ok {
					return // consumer stopped mid-revision
				}
				act = mergedAct
				if strings.TrimSpace(revised) != "" {
					answer = revised
				}
				log.Printf("vetting[%s]: revise round %d done in %s", g.name, round, time.Since(tr).Round(time.Millisecond))
				if !g.emit(ctx, yield, stream.AgentCompletePart(stream.AgentCompleteData{RunID: reviseRun, Stage: stream.StageRevise, Round: round})) {
					return
				}
			}
		}

		// Surface the trusted answer as the node's final output (node-level token —
		// no run is open, so the Translator emits it with an empty run_id).
		log.Printf("vetting[%s]: vetted answer ready total=%s len=%d", g.name, time.Since(t0).Round(time.Second), len(answer))
		g.emitAnswer(ctx, yield, answer)
	}
}

// critiqueContext wraps an InvocationContext, substituting a new UserContent
// and a gate-marker-filtered Session so the worker can be re-invoked for
// agentic self-refine without ADK erroring on orphan FunctionResponse events.
//
// ADK v1.4.0 errors if the last session event is a FunctionResponse with no
// matching FunctionCall. Gate marker events are exactly that — they have no
// FunctionCall counterpart — so we hide them from the session view the worker
// sees during its re-invocation.
type critiqueContext struct {
	adkagent.InvocationContext
	content *genai.Content
	sess    session.Session
}

func newCritiqueContext(ctx adkagent.InvocationContext, content *genai.Content) *critiqueContext {
	return &critiqueContext{
		InvocationContext: ctx,
		content:           content,
		sess:              &filteredSession{Session: ctx.Session()},
	}
}

func (c *critiqueContext) UserContent() *genai.Content { return c.content }
func (c *critiqueContext) Session() session.Session    { return c.sess }

func (c *critiqueContext) WithContext(goCtx context.Context) adkagent.InvocationContext {
	return &critiqueContext{
		InvocationContext: c.InvocationContext.WithContext(goCtx),
		content:           c.content,
		sess:              c.sess,
	}
}

// filteredSession wraps session.Session, presenting a live-filtered Events()
// that hides gate marker events so the worker agent sees no orphan
// FunctionResponses in its session history during agentic self-refine.
type filteredSession struct {
	session.Session
}

func (f *filteredSession) Events() session.Events {
	// Materialise the filtered list once per call so Len/At are O(1). Each
	// call to Events() re-reads from the underlying session, so events added
	// by the worker's tool loop are visible to the next LLM call.
	var events []*session.Event
	for ev := range f.Session.Events().All() {
		// Drop gate markers (orphan FunctionResponses) and the judge's streamed
		// display events so neither pollutes the worker's re-invocation context.
		if isGateMarkerEvent(ev) || (ev != nil && ev.Author == judgeAuthor) {
			continue
		}
		events = append(events, ev)
	}
	return &materializedEvents{events: events}
}

// materializedEvents is a snapshot of the filtered session used within one
// LLM context-build step. Len/At are O(1); All iterates the pre-built slice.
type materializedEvents struct {
	events []*session.Event
}

func (m *materializedEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, ev := range m.events {
			if !yield(ev) {
				return
			}
		}
	}
}

func (m *materializedEvents) Len() int { return len(m.events) }

func (m *materializedEvents) At(i int) *session.Event {
	if i < 0 || i >= len(m.events) {
		return nil
	}
	return m.events[i]
}

// isGateMarkerEvent reports whether ev consists entirely of gate-internal
// marker FunctionResponse parts that must be hidden from the worker.
func isGateMarkerEvent(ev *session.Event) bool {
	if ev == nil || ev.Content == nil || len(ev.Content.Parts) == 0 {
		return false
	}
	hasMarker := false
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if p.FunctionResponse == nil || !stream.IsGateMarkerName(p.FunctionResponse.Name) {
			return false
		}
		hasMarker = true
	}
	return hasMarker
}

// runSelfCritique re-invokes the worker (agentic, WITH tools) with a critique
// prompt so it can actually fix what the draft got wrong — fetching missing
// sources, verifying claims. A tool-less pass doesn't work here: the critique
// prompt asks the model to use its tools, so without them qwen emits <tool_call>
// XML as plain text instead of fetching. Returns the refined answer, merged
// activity (original + new fetches), and false on early stop.
func (g *gate) runSelfCritique(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, answer string, act workerActivity) (string, workerActivity, bool) {
	content := buildCritiqueContent(g.cfg.Constitution, g.cfg.Rubric, ctx.UserContent(), answer, act)
	cctx := newCritiqueContext(ctx, content)
	// textAsThinking=true: plain text from the model streams as thinking events so
	// the user sees activity inside the self-critique container. The local model
	// outputs reasoning as plain text (not Thought parts), so without this the
	// self-critique phase is a silent blank for the user.
	refined, refinedAct, ok := g.runWorker(cctx, yield, g.worker, true)
	if !ok {
		return "", workerActivity{}, false
	}
	return refined, mergeActivity(act, refinedAct), true
}

// runAgenticRevision re-invokes the worker so it continues its own session and
// tools to address the judge's feedback. Like self-refine it is genuinely
// agentic — the worker runs its full tool loop (re-fetching sources, verifying
// claims), not a single model call. Returns the revised answer, merged activity
// (prior + new fetches), and false on early stop.
func (g *gate) runAgenticRevision(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, answer, feedback string, act workerActivity) (string, workerActivity, bool) {
	content := buildRevisionContent(g.cfg.Constitution, ctx.UserContent(), answer, feedback, act)
	cctx := newCritiqueContext(ctx, content)
	revised, revisedAct, ok := g.runWorker(cctx, yield, g.worker, true)
	if !ok {
		return "", workerActivity{}, false
	}
	merged := mergeActivity(act, revisedAct)

	// Empty-answer recovery (same as round 0): the revision often comes back empty
	// (the worker tries another tool, or works the answer out in reasoning and never
	// writes content). Coax the written answer out, retrying until non-empty.
	if strings.TrimSpace(revised) == "" {
		finalized, fact, ok := g.finalizeUntilNonEmpty(ctx, yield, merged)
		if !ok {
			return "", workerActivity{}, false
		}
		merged = fact
		if strings.TrimSpace(finalized) != "" {
			revised = finalized
		}
	}
	return revised, merged, true
}

// maxEmptyRetries bounds how many times the gate re-invokes the worker to coax a
// non-empty answer out before giving up — a genuinely broken model (or an
// inference-config bug) must not spin forever.
const maxEmptyRetries = 4

// finalizeUntilNonEmpty re-invokes the worker (a finalize write-up pass)
// repeatedly until it returns a non-empty answer or maxEmptyRetries is hit. Each
// attempt continues the worker's session (all tool results in context) and asks
// only for the write-up. Returns the answer (may be "" if every attempt came back
// empty), the merged activity, and false on early stop (consumer disconnect).
func (g *gate) finalizeUntilNonEmpty(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, act workerActivity) (string, workerActivity, bool) {
	for attempt := 1; attempt <= maxEmptyRetries; attempt++ {
		tf := time.Now()
		log.Printf("vetting[%s]: empty answer; finalize retry %d/%d start", g.name, attempt, maxEmptyRetries)
		finalized, mergedAct, ok := g.runFinalize(ctx, yield, act)
		if !ok {
			return "", workerActivity{}, false
		}
		act = mergedAct
		log.Printf("vetting[%s]: finalize retry %d/%d done in %s answer_len=%d", g.name, attempt, maxEmptyRetries, time.Since(tf).Round(time.Millisecond), len(finalized))
		if strings.TrimSpace(finalized) != "" {
			return finalized, act, true
		}
	}
	log.Printf("vetting[%s]: finalize exhausted %d retries, still empty", g.name, maxEmptyRetries)
	return "", act, true
}

// runFinalize re-invokes the worker to write its final answer when a pass ended
// without one. It continues the worker's session — all its tool results are in
// context — and asks only for the write-up. Returns the answer, merged activity,
// and false on early stop.
func (g *gate) runFinalize(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, act workerActivity) (string, workerActivity, bool) {
	content := buildFinalizeContent(ctx.UserContent(), act)
	cctx := newCritiqueContext(ctx, content)
	// textAsThinking=false: this pass produces the primary answer (like round 0),
	// so its text is buffered as the answer rather than streamed as thinking.
	finalized, finalAct, ok := g.runWorker(cctx, yield, g.writer, false)
	if !ok {
		return "", workerActivity{}, false
	}
	return finalized, mergeActivity(act, finalAct), true
}

// recordSearchResults extracts {url: snippet} pairs from a web_search tool
// response (shape {results: [{title, url, snippet}]}) into seen. Each URL the
// worker surfaced via search is a genuinely-retrieved lead — a valid source if
// later cited. The snippet is kept (trimmed) as advisory context for spot-
// checking whether a specific claim is actually supported. The first snippet
// seen for a URL wins; a later fetch of the same URL is recorded separately in
// act.fetched (the stronger tier).
func recordSearchResults(seen map[string]string, resp map[string]any) {
	if resp == nil {
		return
	}
	var items []any
	switch r := resp["results"].(type) {
	case []any:
		items = r
	case []map[string]any:
		items = make([]any, len(r))
		for i, m := range r {
			items[i] = m
		}
	default:
		return
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		u, _ := m["url"].(string)
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, exists := seen[u]; exists {
			continue
		}
		snippet, _ := m["snippet"].(string)
		seen[u] = strings.TrimSpace(trimToSample(snippet))
	}
}

// fetchedKeys returns the successfully-fetched URLs in stable order, for the
// diagnostic retrieval-summary log.
func fetchedKeys(m map[string]fetchRecord) []string {
	keys := make([]string, 0, len(m))
	for u := range m {
		keys = append(keys, u)
	}
	sort.Strings(keys)
	return keys
}

// mergeActivity unions two workerActivity records. Entries in b (the
// self-refine pass) override same-URL entries in a so fresh content wins.
func mergeActivity(a, b workerActivity) workerActivity {
	merged := workerActivity{
		searches: append(append([]string(nil), a.searches...), b.searches...),
		fetched:  make(map[string]fetchRecord, len(a.fetched)+len(b.fetched)),
		seen:     make(map[string]string, len(a.seen)+len(b.seen)),
	}
	for u, r := range a.fetched {
		merged.fetched[u] = r
	}
	for u, r := range b.fetched {
		merged.fetched[u] = r
	}
	for u, s := range a.seen {
		merged.seen[u] = s
	}
	for u, s := range b.seen {
		merged.seen[u] = s
	}
	return merged
}

// fetchRecord holds a successfully fetched URL and a short content sample.
type fetchRecord struct {
	// sample is the first fetchSampleBytes of the page's readable text,
	// passed to the judge for spot-checking cited claims.
	sample string
}

// fetchSampleBytes is how many bytes of fetched content we keep per URL.
// Enough for the judge to spot-check a claim; small enough not to flood the
// judge's context when an answer cites many sources.
const fetchSampleBytes = 300

// runWorker runs the worker, streaming its non-answer events (thinking, tool
// calls/results) and accumulating its answer text. It also tracks successful
// web_fetch calls — pairing FunctionCall (URL arg) with FunctionResponse
// (page content) by call ID — so the judge can verify cited links against
// the pages the worker actually read.
// workerActivity summarises what retrieval the worker performed. It is passed
// to self-refine and the judge so neither can falsely claim no retrieval happened.
type workerActivity struct {
	// searches holds every query the worker passed to web_search.
	searches []string
	// fetched maps URL → fetchRecord for web_fetch calls that returned content.
	fetched map[string]fetchRecord
	// seen maps URL → search snippet for URLs the worker surfaced via web_search
	// but did not (or could not) fetch. A cited URL that appears here was
	// genuinely retrieved this session — a real lead from search, NOT a
	// fabrication — so the source check treats it as a weaker-but-valid source
	// rather than lumping it in with hallucinated/guessed URLs.
	seen map[string]string
}

// Returns the buffered answer, retrieval activity, and false on early stop.
// When textAsThinking is true, plain text parts are converted to thought parts
// so they stream as thinking events — used during agentic self-refine so the
// user sees the model working instead of a silent blank.
func (g *gate) runWorker(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, ag adkagent.Agent, textAsThinking bool) (string, workerActivity, bool) {
	var answer strings.Builder
	var act workerActivity
	act.fetched = make(map[string]fetchRecord)
	act.seen = make(map[string]string)
	// pendingCalls maps call-ID → URL for in-flight web_fetch calls.
	pendingCalls := make(map[string]string)

	// Diagnostics for empty-answer post-mortem: the last finish reason and whether
	// the final streamed event was partial (ADK marks the last chunk Partial when
	// a streaming turn is truncated at the token limit).
	var lastFinish genai.FinishReason
	var lastPartial bool
	var steps int

	for ev, err := range ag.Run(ctx) {
		if err != nil {
			if !yield(nil, err) {
				return "", workerActivity{}, false
			}
			continue
		}
		if ev == nil {
			continue
		}
		if ev.FinishReason != "" && ev.FinishReason != genai.FinishReasonUnspecified {
			lastFinish = ev.FinishReason
		}
		lastPartial = ev.Partial
		evHasTool := false
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p == nil {
					continue
				}
				if p.FunctionCall != nil || p.FunctionResponse != nil {
					evHasTool = true
				}
				if p.FunctionCall != nil {
					switch p.FunctionCall.Name {
					case "web_search":
						// Record the query so self-refine knows searches happened.
						if q, ok := p.FunctionCall.Args["query"].(string); ok && q != "" {
							act.searches = append(act.searches, strings.TrimSpace(q))
						}
					case "web_fetch":
						// Record the URL when the call is made so we can look it up when
						// the response arrives (different event, matched by call ID).
						if u, ok := p.FunctionCall.Args["url"].(string); ok && u != "" {
							pendingCalls[p.FunctionCall.ID] = strings.TrimSpace(u)
						}
					}
				}
				// When the web_fetch response arrives, pair it with its call's URL and
				// check whether it returned real content (non-error "result" key).
				if p.FunctionResponse != nil && p.FunctionResponse.Name == "web_fetch" {
					if url, known := pendingCalls[p.FunctionResponse.ID]; known {
						delete(pendingCalls, p.FunctionResponse.ID)
						if result, ok := p.FunctionResponse.Response["result"].(string); ok && strings.TrimSpace(result) != "" {
							act.fetched[url] = fetchRecord{sample: strings.TrimSpace(trimToSample(result))}
						}
					}
				}
				// Record the URLs (and snippets) web_search surfaced. These are
				// genuinely-retrieved leads: a cited URL appearing here is a valid
				// (if weaker than a full fetch) source, not a fabrication. (Per-call
				// fetch/search logging is intentionally omitted — too noisy; the
				// per-node "retrieval:" summary captures what was retrieved.)
				if p.FunctionResponse != nil && p.FunctionResponse.Name == "web_search" {
					recordSearchResults(act.seen, p.FunctionResponse.Response)
				}
			}
		}
		// The answer is the text that follows the worker's LAST tool activity.
		// Text-only narration events between tool calls ("Now I have everything
		// I need, let me compile…") would otherwise accumulate into the answer
		// buffer and leak ahead of the real answer.
		if evHasTool {
			answer.Reset()
			steps++
		}
		passthrough, ans := splitAnswer(ev, textAsThinking)
		answer.WriteString(ans)
		if passthrough != nil {
			if !yield(passthrough, nil) {
				return "", workerActivity{}, false
			}
		}
	}
	// Strip leaked thinking: --jinja primes Qwen3 with <think> in the prompt, so
	// the model outputs reasoning directly in content (not reasoning_content) when
	// llama-server's parser never sees the opening tag. StripThinking handles both
	// a closed <think>…</think> block and an UNCLOSED one (model hit its token /
	// reasoning budget before </think>) — the latter is the leak our old
	// closing-tag-only strip missed (hermes-webui#2152, zed#30498).
	raw := answer.String()
	rawLen := len(raw)
	ans := stream.StripThinking(raw)
	stripped := ans != strings.TrimSpace(raw)
	// Empty-answer post-mortem: distinguish the causes so we know WHY the worker
	// ended without an answer. finish_reason=MAX_TOKENS or partial=true ⇒ the turn
	// was truncated at the token limit; think_stripped with raw_len>0 ⇒ the answer
	// arrived as <think> content that got stripped; raw_len=0 ⇒ the final turn
	// carried no plain text at all (e.g. answer emitted as reasoning parts).
	if strings.TrimSpace(ans) == "" {
		log.Printf("vetting[%s]: EMPTY answer after %d tool-steps (finish_reason=%q partial=%v think_stripped=%v raw_len=%d) — worker ended its turn with no answer text",
			g.name, steps, string(lastFinish), lastPartial, stripped, rawLen)
	}
	return ans, act, true
}

// splitAnswer separates a worker event's answer text (plain non-thought text)
// from the rest. It returns the buffered answer text plus an event carrying only
// the non-answer parts to stream through (nil if there are none). The worker's
// turn-completion is stripped so it doesn't close the agent's group before the
// gate has vetted and emitted the final answer.
//
// When asThinking is true (agentic self-refine pass), plain text parts are
// converted to Thought=true so they stream as thinking events — the local model
// outputs reasoning as plain text, so without this the self-refine is silent.
func splitAnswer(ev *session.Event, asThinking bool) (*session.Event, string) {
	if ev.Content == nil {
		if ev.TurnComplete {
			clone := *ev
			clone.TurnComplete = false
			return &clone, ""
		}
		return ev, ""
	}
	// Text that appears in the same event as a function call is planning
	// narration ("let me call tool X…"), not the final answer. Treat it as
	// thinking so it streams through as activity but never leaks into the
	// buffered answer the gate surfaces to the user.
	hasFuncCall := false
	for _, p := range ev.Content.Parts {
		if p != nil && p.FunctionCall != nil {
			hasFuncCall = true
			break
		}
	}
	var answer strings.Builder
	var keep []*genai.Part
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil && p.Text != "" {
			if !hasFuncCall {
				answer.WriteString(p.Text)
			}
			if asThinking || hasFuncCall {
				keep = append(keep, &genai.Part{Thought: true, Text: p.Text})
			}
			continue
		}
		keep = append(keep, p)
	}
	if len(keep) == 0 {
		return nil, answer.String()
	}
	clone := *ev
	clone.TurnComplete = false
	clone.Content = &genai.Content{Role: ev.Content.Role, Parts: keep}
	return &clone, answer.String()
}

// trimToSample returns the first fetchSampleBytes of s, trimmed back to a valid
// UTF-8 rune boundary so the judge never receives a string ending mid-codepoint.
func trimToSample(s string) string {
	if len(s) <= fetchSampleBytes {
		return s
	}
	s = s[:fetchSampleBytes]
	// Back up at most utf8.UTFMax-1 bytes to reach a valid boundary.
	for i := 0; i < utf8.UTFMax && len(s) > 0 && !utf8.ValidString(s); i++ {
		s = s[:len(s)-1]
	}
	return s
}

// emit yields a gate-authored event carrying a single marker part (self-refine or
// judge verdict), authored as the worker so it nests under the worker's group.
func (g *gate) emit(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, part *genai.Part) bool {
	return g.emitAuthored(ctx, yield, g.name, part)
}

// emitAuthored yields a gate-authored event carrying a single part, stamped with
// the given author. Judge display events use judgeAuthor so they can be filtered
// from the worker's session view; everything else uses the worker's name.
func (g *gate) emitAuthored(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, author string, part *genai.Part) bool {
	ev := session.NewEvent(ctx.InvocationID())
	ev.Author = author
	ev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{part}}
	return yield(ev, nil)
}

// emitAnswer yields the final, vetted answer as the agent's turn-completing
// output, so it streams to the chat and persists as the assistant message.
func (g *gate) emitAnswer(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, answer string) bool {
	ev := session.NewEvent(ctx.InvocationID())
	ev.Author = g.name
	ev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: answer}}}
	ev.TurnComplete = true
	return yield(ev, nil)
}
