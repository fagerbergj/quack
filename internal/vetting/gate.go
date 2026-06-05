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
	"strings"

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

// NewGatedAgent wraps worker in the trust gate. workerModel is the worker's own
// model (used for the free self-refine + revision passes); judge is the
// independent judge model.
func NewGatedAgent(worker adkagent.Agent, workerModel, judge model.LLM, cfg Config) (adkagent.Agent, error) {
	g := &gate{worker: worker, workerModel: workerModel, judge: judge, cfg: cfg, name: worker.Name()}
	// The worker is invoked directly (g.worker.Run), not registered as a SubAgent:
	// the gate echoes the worker's name so A2A dispatch resolves it, and a SubAgent
	// of the same name would collide in the runner's agent-tree uniqueness check.
	return adkagent.New(adkagent.Config{
		Name:        worker.Name(),
		Description: worker.Description(),
		Run:         g.run,
	})
}

func (g *gate) run(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		// Round 0: run the worker. Its thinking/tool events stream through; its
		// answer text is buffered so only the vetted answer is surfaced.
		answer, ok := g.runWorker(ctx, yield)
		if !ok {
			return
		}

		// Self-refine pre-pass: the worker critiques and revises its own draft
		// (a free same-model round-trip) before the judge sees it.
		if g.cfg.SelfRefine && strings.TrimSpace(answer) != "" {
			refined, err := selfRefine(ctx, g.workerModel, ctx.UserContent(), answer)
			if err != nil {
				if !yield(nil, err) {
					return
				}
			} else {
				changed := strings.TrimSpace(refined) != "" && refined != answer
				if changed {
					answer = refined
				}
				if !g.emit(ctx, yield, stream.SelfRefinePart(changed)) {
					return
				}
			}
		}

		// Judge loop: score against the rubric, revise on a fail until the score
		// clears the threshold or we run out of rounds.
		for round := 1; round <= g.cfg.MaxRounds; round++ {
			v, err := runJudge(ctx, g.judge, g.cfg.Rubric, ctx.UserContent(), answer)
			if err != nil {
				if !yield(nil, err) {
					return
				}
				break
			}
			passed := v.Score >= g.cfg.Threshold
			if !g.emit(ctx, yield, stream.JudgeVerdictPart(round, v.Score, passed, v.Feedback)) {
				return
			}
			if passed || round == g.cfg.MaxRounds {
				break
			}
			revised, err := revise(ctx, g.workerModel, ctx.UserContent(), answer, v.Feedback)
			if err != nil {
				if !yield(nil, err) {
					return
				}
				break
			}
			if strings.TrimSpace(revised) != "" {
				answer = revised
			}
		}

		// Surface the trusted answer as the agent's final output. (PR2 commits it
		// to memory here, only after a passing verdict.)
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
