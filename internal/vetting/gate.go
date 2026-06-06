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
	"iter"
	"log"
	"strings"
	"time"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// Config is the resolved vetting policy: the parsed adversarial settings plus the
// loaded rubric text. Build it with FromConfig.
type Config struct {
	MaxRounds  int
	Threshold  float64
	SelfRefine bool
	Rubric     string
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
		answer, ok := g.runWorker(ctx, yield)
		if !ok {
			return
		}
		log.Printf("vetting[%s]: worker done in %s answer_len=%d", g.name, time.Since(t0).Round(time.Second), len(answer))

		// Self-refine pre-pass: the worker critiques and revises its own draft
		// (a free same-model round-trip) before the judge sees it.
		if g.cfg.SelfRefine && strings.TrimSpace(answer) != "" {
			tsr := time.Now()
			log.Printf("vetting[%s]: self-refine start", g.name)
			refined, err := selfRefine(ctx, g.workerModel, ctx.UserContent(), answer)
			if err != nil {
				log.Printf("vetting[%s]: self-refine error after %s: %v", g.name, time.Since(tsr).Round(time.Millisecond), err)
				if !yield(nil, err) {
					return
				}
			} else {
				// refined is already trimmed by generate(); compare against the
				// trimmed answer so trailing whitespace alone doesn't read as a change.
				changed := refined != "" && refined != strings.TrimSpace(answer)
				if changed {
					answer = refined
				}
				log.Printf("vetting[%s]: self-refine done in %s changed=%v", g.name, time.Since(tsr).Round(time.Millisecond), changed)
				if !g.emit(ctx, yield, stream.SelfRefinePart(changed)) {
					return
				}
			}
		}

		// Judge loop: score against the rubric, revise on a fail until the score
		// clears the threshold or we run out of rounds.
		// A judge or revise error fails CLOSED: surface the error and abort without
		// emitting the answer, so an un-vetted draft is never returned or persisted
		// when the judge can't run.
		for round := 1; round <= g.cfg.MaxRounds; round++ {
			tj := time.Now()
			log.Printf("vetting[%s]: judge round %d/%d start", g.name, round, g.cfg.MaxRounds)
			v, err := g.runJudgeWithKeepalive(ctx, yield, answer)
			if err != nil {
				log.Printf("vetting[%s]: judge round %d error after %s: %v", g.name, round, time.Since(tj).Round(time.Millisecond), err)
				yield(nil, err)
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
			revised, err := revise(ctx, g.workerModel, ctx.UserContent(), answer, v.Feedback)
			if err != nil {
				log.Printf("vetting[%s]: revise round %d error after %s: %v", g.name, round, time.Since(tr).Round(time.Millisecond), err)
				yield(nil, err)
				return
			}
			log.Printf("vetting[%s]: revise round %d done in %s", g.name, round, time.Since(tr).Round(time.Millisecond))
			if strings.TrimSpace(revised) != "" {
				answer = revised
			}
		}

		// Surface the trusted answer as the agent's final output. (PR2 commits it
		// to memory here, only after a passing verdict.)
		log.Printf("vetting[%s]: vetted answer ready total=%s len=%d", g.name, time.Since(t0).Round(time.Second), len(answer))
		g.emitAnswer(ctx, yield, answer)
	}
}

// runWorker runs the worker, streaming its non-answer events (thinking, tool
// calls/results) and accumulating its answer text. Returns the buffered answer
// and false if the consumer stopped early.
func (g *gate) runWorker(ctx adkagent.InvocationContext, yield func(*session.Event, error) bool) (string, bool) {
	var answer strings.Builder
	for ev, err := range g.worker.Run(ctx) {
		if err != nil {
			if !yield(nil, err) {
				return "", false
			}
			continue
		}
		if ev == nil {
			continue
		}
		passthrough, ans := splitAnswer(ev)
		answer.WriteString(ans)
		if passthrough != nil {
			if !yield(passthrough, nil) {
				return "", false
			}
		}
	}
	return answer.String(), true
}

// splitAnswer separates a worker event's answer text (plain non-thought text)
// from the rest. It returns the buffered answer text plus an event carrying only
// the non-answer parts to stream through (nil if there are none). The worker's
// turn-completion is stripped so it doesn't close the agent's group before the
// gate has vetted and emitted the final answer.
func splitAnswer(ev *session.Event) (*session.Event, string) {
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

// keepaliveInterval is how often the gate heartbeats the A2A connection during
// slow judge calls. Must be shorter than the A2A client's idle read timeout
// (~60 s in the ADK remoteagent HTTP client).
const keepaliveInterval = 30 * time.Second

// runJudgeWithKeepalive runs the judge in a goroutine while emitting keepalive
// marker events via yield every keepaliveInterval. This prevents the A2A SSE
// connection from timing out during long model-load + generation cycles.
// The keepalive events are silently dropped by stream.Translate (no wire event).
func (g *gate) runJudgeWithKeepalive(
	ctx adkagent.InvocationContext,
	yield func(*session.Event, error) bool,
	answer string,
) (verdict, error) {
	type result struct {
		v   verdict
		err error
	}
	ch := make(chan result, 1) // buffered: goroutine never blocks on send
	go func() {
		v, err := runJudge(ctx, g.judge, g.cfg.Rubric, ctx.UserContent(), answer)
		ch <- result{v, err}
	}()
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case r := <-ch:
			return r.v, r.err
		case <-ticker.C:
			if !g.emit(ctx, yield, stream.KeepAlivePart()) {
				<-ch // drain so goroutine can exit cleanly
				return verdict{}, ctx.Err()
			}
		case <-ctx.Done():
			<-ch // drain so goroutine can exit cleanly
			return verdict{}, ctx.Err()
		}
	}
}
