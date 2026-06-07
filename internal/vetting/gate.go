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
	"iter"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// Config is the resolved vetting policy: the parsed adversarial settings plus the
// loaded constitution and rubric text. Build it with FromConfig.
type Config struct {
	MaxRounds    int
	Threshold    float64
	SelfRefine   bool
	Constitution string // global principles; used for self-refine critique + prefixed in judge prompt
	Rubric       string // scoring guide; global default or per-agent override
}

type gate struct {
	worker      adkagent.Agent
	workerModel model.LLM
	judge       model.LLM
	cfg         Config
	name        string
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

// NewGatedAgent wraps worker in the trust gate. workerModel is the worker's own
// model (used for the free self-refine + revision passes); judge is the
// independent judge model.
func NewGatedAgent(worker adkagent.Agent, workerModel, judge model.LLM, cfg Config) (*GatedAgent, error) {
	g := &gate{worker: worker, workerModel: workerModel, judge: judge, cfg: cfg, name: worker.Name()}
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
		log.Printf("vetting[%s]: worker start", g.name)

		// Round 0: run the worker. Its thinking/tool events stream through; its
		// answer text is buffered so only the vetted answer is surfaced.
		// fetchedURLs records every URL the worker called web_fetch on, used by
		// the judge to verify that cited links were actually retrieved.
		answer, act, ok := g.runWorker(ctx, yield, false)
		if !ok {
			return
		}
		log.Printf("vetting[%s]: worker done in %s answer_len=%d", g.name, time.Since(t0).Round(time.Second), len(answer))

		// Self-refine pre-pass: the worker critiques and revises its own draft
		// (a free same-model round-trip) before the judge sees it.
		if g.cfg.SelfRefine && strings.TrimSpace(answer) != "" {
			tsr := time.Now()
			log.Printf("vetting[%s]: self-refine start", g.name)
			if !g.emit(ctx, yield, stream.SelfRefineStartPart()) {
				return
			}
			refined, mergedAct, ok := g.runAgenticSelfRefine(ctx, yield, answer, act)
			if !ok {
				return
			}
			act = mergedAct
			changed := refined != "" && strings.TrimSpace(refined) != strings.TrimSpace(answer)
			if changed {
				answer = refined
			}
			log.Printf("vetting[%s]: self-refine done in %s changed=%v", g.name, time.Since(tsr).Round(time.Millisecond), changed)
			if !g.emit(ctx, yield, stream.SelfRefinePart(changed)) {
				return
			}
		}

		// Judge loop: score against the rubric, revise on a fail until the score
		// clears the threshold or we run out of rounds.
		// A judge error degrades gracefully: emit a judge_unavailable event then
		// surface the answer with a quality-cannot-be-guaranteed flag rather than
		// withholding it from the user entirely.
		for round := 1; round <= g.cfg.MaxRounds; round++ {
			tj := time.Now()
			log.Printf("vetting[%s]: judge round %d/%d start", g.name, round, g.cfg.MaxRounds)
			if !g.emit(ctx, yield, stream.JudgeStartPart(round)) {
				return
			}
			// judgeCtx is cancelled when the consumer stops mid-thinking so
			// generateStream aborts promptly rather than running to completion for a
			// disconnected client.
			judgeCtx, cancelJudge := context.WithCancel(ctx)
			onThinking := func(text string) {
				if !g.emit(ctx, yield, stream.ThinkingPart(text)) {
					cancelJudge()
				}
			}
			v, err := runJudge(judgeCtx, g.judge, g.cfg.Constitution, g.cfg.Rubric, ctx.UserContent(), answer, act.fetched, onThinking)
			cancelJudge()
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return // consumer stopped mid-judge; exit cleanly
				}
				log.Printf("vetting[%s]: judge round %d error after %s: %v (surfacing answer unvetted)", g.name, round, time.Since(tj).Round(time.Millisecond), err)
				if !g.emit(ctx, yield, stream.JudgeUnavailablePart(round, err.Error())) {
					return
				}
				g.emitAnswer(ctx, yield, answer)
				return
			}
			passed := v.Score >= g.cfg.Threshold
			log.Printf("vetting[%s]: judge round %d done in %s score=%.2f passed=%v", g.name, round, time.Since(tj).Round(time.Millisecond), v.Score, passed)
			if !g.emit(ctx, yield, stream.JudgeVerdictPart(round, v.Score, passed, v.Feedback)) {
				return
			}
			if passed || round == g.cfg.MaxRounds {
				break
			}
			tr := time.Now()
			log.Printf("vetting[%s]: revise round %d start", g.name, round)
			reviseCtx, cancelRevise := context.WithCancel(ctx)
			onThinkingRevise := func(text string) {
				if !g.emit(ctx, yield, stream.ThinkingPart(text)) {
					cancelRevise()
				}
			}
			revised, err := revise(reviseCtx, g.workerModel, ctx.UserContent(), answer, v.Feedback, onThinkingRevise)
			cancelRevise()
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return // consumer stopped mid-revision
				}
				log.Printf("vetting[%s]: revise round %d error after %s: %v (surfacing pre-revision answer)", g.name, round, time.Since(tr).Round(time.Millisecond), err)
				if !g.emit(ctx, yield, stream.JudgeUnavailablePart(round, "revision failed: "+err.Error())) {
					return
				}
				g.emitAnswer(ctx, yield, answer)
				return
			}
			log.Printf("vetting[%s]: revise round %d done in %s", g.name, round, time.Since(tr).Round(time.Millisecond))
			if strings.TrimSpace(revised) != "" {
				answer = revised
			}
			if !g.emit(ctx, yield, stream.RevisePart(round)) {
				return
			}
		}

		// Surface the trusted answer as the agent's final output. (PR2 commits it
		// to memory here, only after a passing verdict.)
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
func (c *critiqueContext) Session() session.Session     { return c.sess }

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
		if !isGateMarkerEvent(ev) {
			events = append(events, ev)
		}
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

// runAgenticSelfRefine re-invokes the worker agent with a critique prompt so
// it can use its tools to fix what the draft got wrong — fetching missing
// sources, verifying claims, retrieving URLs cited but not read. This is
// genuinely agentic: the worker runs its full tool loop, not a single model
// call. Returns the refined answer, merged activity (original + new fetches),
// and false on early stop.
func (g *gate) runAgenticSelfRefine(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, answer string, act workerActivity) (string, workerActivity, bool) {
	content := buildCritiqueContent(g.cfg.Constitution, g.cfg.Rubric, ctx.UserContent(), answer, act)
	cctx := newCritiqueContext(ctx, content)
	// textAsThinking=true: plain text from the model streams as thinking events so
	// the user sees activity inside the self-refine container. The local model
	// outputs reasoning as plain text (not Thought parts), so without this the
	// self-refine phase is a silent blank for the user.
	refined, refinedAct, ok := g.runWorker(cctx, yield, true)
	if !ok {
		return "", workerActivity{}, false
	}
	return refined, mergeActivity(act, refinedAct), true
}

// mergeActivity unions two workerActivity records. Entries in b (the
// self-refine pass) override same-URL entries in a so fresh content wins.
func mergeActivity(a, b workerActivity) workerActivity {
	merged := workerActivity{
		searches: append(append([]string(nil), a.searches...), b.searches...),
		fetched:  make(map[string]fetchRecord, len(a.fetched)+len(b.fetched)),
	}
	for u, r := range a.fetched {
		merged.fetched[u] = r
	}
	for u, r := range b.fetched {
		merged.fetched[u] = r
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
}

// Returns the buffered answer, retrieval activity, and false on early stop.
// When textAsThinking is true, plain text parts are converted to thought parts
// so they stream as thinking events — used during agentic self-refine so the
// user sees the model working instead of a silent blank.
func (g *gate) runWorker(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, textAsThinking bool) (string, workerActivity, bool) {
	var answer strings.Builder
	var act workerActivity
	act.fetched = make(map[string]fetchRecord)
	// pendingCalls maps call-ID → URL for in-flight web_fetch calls.
	pendingCalls := make(map[string]string)

	for ev, err := range g.worker.Run(ctx) {
		if err != nil {
			if !yield(nil, err) {
				return "", workerActivity{}, false
			}
			continue
		}
		if ev == nil {
			continue
		}
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p == nil {
					continue
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
			}
		}
		passthrough, ans := splitAnswer(ev, textAsThinking)
		answer.WriteString(ans)
		if passthrough != nil {
			if !yield(passthrough, nil) {
				return "", workerActivity{}, false
			}
		}
	}
	return answer.String(), act, true
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
	var answer strings.Builder
	var keep []*genai.Part
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil && p.Text != "" {
			answer.WriteString(p.Text)
			if asThinking {
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
// judge verdict).
func (g *gate) emit(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool, part *genai.Part) bool {
	ev := session.NewEvent(ctx.InvocationID())
	ev.Author = g.name
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

