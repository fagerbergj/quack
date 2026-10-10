package dag

import (
	"context"
	"encoding/json"
	"iter"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// budgetCharsPerToken/budgetOutputReserve mirror internal/agent's charsPerToken/compactionBuffer;
// a local copy rather than a cross-package export for one formula.
const (
	budgetCharsPerToken = 4
	budgetOutputReserve = 20_000
)

// BudgetedLLM is a hard backstop under ADK compaction, whose tail retention counts events, not tokens:
// a few plan/execute rounds can blow past context_window while ADK sees nothing to compact.
type BudgetedLLM struct {
	model.LLM
	budget int
}

// NewBudgetedLLM returns inner unwrapped when contextWindow is unset (nothing to enforce).
func NewBudgetedLLM(inner model.LLM, contextWindow int) model.LLM {
	if contextWindow <= 0 {
		return inner
	}
	// Capped at contextWindow/4: a large create_plan output must still fit.
	reserve := budgetOutputReserve
	if ceil := contextWindow / 4; ceil < reserve {
		reserve = ceil
	}
	return &BudgetedLLM{LLM: inner, budget: contextWindow - reserve}
}

func (b *BudgetedLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if req != nil {
		trimContentsToBudget(req, b.budget)
	}
	return b.LLM.GenerateContent(ctx, req, stream)
}

// trimContentsToBudget drops the oldest complete tool-call/response rounds until the request fits.
// Contents[0] and the last user-role content are pinned: templates 500 without a final user turn.
func trimContentsToBudget(req *model.LLMRequest, budget int) {
	overhead := estimateOverhead(req)
	if budget <= 0 || estimateTokens(req.Contents)+overhead <= budget {
		return
	}
	lastUser := len(req.Contents) - 1
	for lastUser > 0 && (req.Contents[lastUser] == nil || req.Contents[lastUser].Role != "user") {
		lastUser--
	}
	dropped := 0
	i := 1
	for i < lastUser && estimateTokens(req.Contents)+overhead > budget {
		// Drop a complete round (two contents), never one: dropping one joins two same-role neighbours,
		// and it keeps a FunctionCall with its response (providers 400 on an orphan).
		end := i + 2
		if end > lastUser {
			break // would consume or split across the pinned content
		}
		req.Contents = append(req.Contents[:i], req.Contents[end:]...)
		dropped += end - i
		lastUser -= end - i
	}
	if dropped == 0 {
		return
	}
	// Fixed text: a changing count would move the cache divergence point.
	const note = "[earlier messages omitted to fit the model's context window]"
	// Appended as a part on index 1, not a new content: consecutive same-role contents can 400 on
	// some providers.
	if len(req.Contents) > 1 && req.Contents[1] != nil {
		req.Contents[1].Parts = append([]*genai.Part{{Text: note}}, req.Contents[1].Parts...)
		return
	}
	req.Contents = append(req.Contents, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: note}}})
}

// estimateOverhead counts what trimming never touches - system instruction and tool schemas are
// sent every call (~2k tokens for the orchestrator alone).
func estimateOverhead(req *model.LLMRequest) int {
	chars := 0
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			if p != nil {
				chars += len(p.Text)
			}
		}
	}
	if len(req.Tools) > 0 {
		if b, err := json.Marshal(req.Tools); err == nil {
			chars += len(b)
		}
	}
	return chars / budgetCharsPerToken
}

// estimateTokens: the char/4 heuristic internal/agent uses, rough but conservative. Function
// call/response payloads are JSON-marshaled, since Text alone misses most of a plan-judge round.
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
