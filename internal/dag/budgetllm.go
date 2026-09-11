package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// budgetCharsPerToken/budgetOutputReserve mirror internal/agent's
// charsPerToken/compactionBuffer - a local copy rather than an exported
// cross-package constant for one formula, since dag and agent otherwise share
// no dependency in this direction.
const (
	budgetCharsPerToken = 4
	budgetOutputReserve = 20_000
)

// BudgetedLLM is a hard, quack-owned backstop under ADK's own compaction.
// ADK's intra-invocation tail-retention pass is gated on EventRetentionSize -
// an EVENT count, not a token count (google.golang.org/adk/v2/session/
// compaction.Config) - so it declines ("no compactable window") whenever the
// retained tail is still the whole history, which is exactly the shape of
// the orchestrator's create_plan/edit_plan/execute round trips: each round
// is only a handful of events (a plan call, its response, an execute call,
// its rejection), but each one can carry a full plan JSON plus a paragraph
// of judge reasoning, so the EVENT count can stay low while the TOKEN count
// balloons well past the configured context_window, with tail-retention
// never triggering because its window looks empty. BudgetedLLM trims the
// request directly, in quack's own code, so no call ever exceeds budget
// regardless of whether ADK's own compaction also engaged this round.
type BudgetedLLM struct {
	model.LLM
	budget int
}

// NewBudgetedLLM returns inner unwrapped when contextWindow is unset (nothing to enforce).
func NewBudgetedLLM(inner model.LLM, contextWindow int) model.LLM {
	if contextWindow <= 0 {
		return inner
	}
	budget := contextWindow - budgetOutputReserve
	if budget <= 0 {
		// A window smaller than the reserve (a tiny test/rig config) still
		// needs a positive budget to trim toward, or every call would trim to nothing.
		budget = contextWindow / 2
	}
	return &BudgetedLLM{LLM: inner, budget: budget}
}

func (b *BudgetedLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if req != nil {
		trimContentsToBudget(req, b.budget)
	}
	return b.LLM.GenerateContent(ctx, req, stream)
}

// trimContentsToBudget drops the oldest complete tool-call/response rounds
// until the request's estimated size fits budget. req.Contents[0] - the
// invocation's own opening turn - is never dropped. A FunctionCall content is
// always dropped together with its paired response content, never split:
// most providers 400 on an orphaned one or the other.
func trimContentsToBudget(req *model.LLMRequest, budget int) {
	if budget <= 0 || estimateTokens(req.Contents) <= budget {
		return
	}
	dropped := 0
	i := 1
	for i < len(req.Contents) && estimateTokens(req.Contents) > budget {
		end := i + 1
		if hasFunctionCall(req.Contents[i]) && end < len(req.Contents) {
			end++
		}
		req.Contents = append(req.Contents[:i], req.Contents[end:]...)
		dropped += end - i
	}
	if dropped == 0 {
		return
	}
	note := &genai.Content{Role: "user", Parts: []*genai.Part{{
		Text: fmt.Sprintf("[%d earlier message(s) omitted to fit the model's context window]", dropped),
	}}}
	req.Contents = append(req.Contents[:1], append([]*genai.Content{note}, req.Contents[1:]...)...)
}

func hasFunctionCall(c *genai.Content) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Parts {
		if p != nil && p.FunctionCall != nil {
			return true
		}
	}
	return false
}

// estimateTokens is the same char/4 heuristic quack's compaction buffer
// sizing already uses (internal/agent.charsPerToken) - rough by design,
// cheap, and conservative enough for a last-resort guard. FunctionCall/
// FunctionResponse payloads are JSON-marshaled first, since Text alone
// misses the bulk of a plan-judge round (a full plan, a rejection reason).
func estimateTokens(contents []*genai.Content) int {
	chars := 0
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			chars += len(p.Text)
			if p.FunctionCall != nil {
				chars += len(p.FunctionCall.Name)
				if b, err := json.Marshal(p.FunctionCall.Args); err == nil {
					chars += len(b)
				}
			}
			if p.FunctionResponse != nil {
				chars += len(p.FunctionResponse.Name)
				if b, err := json.Marshal(p.FunctionResponse.Response); err == nil {
					chars += len(b)
				}
			}
		}
	}
	return chars / budgetCharsPerToken
}
