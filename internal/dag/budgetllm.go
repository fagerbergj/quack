package dag

import (
	"context"
	"encoding/json"
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
	// Capped at contextWindow/4: a large create_plan output must still fit.
	reserve := budgetOutputReserve
	if ceil := contextWindow / 4; ceil < reserve {
		reserve = ceil
	}
	budget := contextWindow - reserve
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
// session's very first turn, not this invocation's own - is never dropped,
// and neither is the last user-role content: on a chat with history,
// Contents[0] is long past this invocation's actual query, so pinning only
// it left the real current turn (or, mid tool-loop, its latest
// FunctionResponse - also role "user") exposed to the same front-to-back
// trim as everything else. Most chat templates require the message list to
// END on a user turn to know where to generate from; losing it 500s with
// "no user query found" rather than degrading gracefully. A FunctionCall
// content is always dropped together with its paired response content,
// never split: most providers 400 on an orphaned one or the other.
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
		// Always drop a complete round (this content plus the very next
		// one), never a single content alone - a chat alternates roles
		// strictly, so dropping just one joins its two neighbors into an
		// adjacent SAME role (#slice3 review), exactly what the note-splice
		// below already treats as unsafe. A FunctionCall's own response is
		// exactly the next content in a well-formed session, so this still
		// keeps that pair together as before.
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
	// Appended as an extra part on the surviving content at index 1, not a
	// new same-role content spliced in - two consecutive user-role contents
	// can 400 on some providers, and a single inserted content can't
	// preserve alternation between two opposite-role neighbors anyway.
	if len(req.Contents) > 1 && req.Contents[1] != nil {
		req.Contents[1].Parts = append([]*genai.Part{{Text: note}}, req.Contents[1].Parts...)
		return
	}
	req.Contents = append(req.Contents, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: note}}})
}

// estimateOverhead accounts for the parts of a request trimContentsToBudget
// never touches - the system instruction and tool-declaration schemas are
// sent on every call regardless of how much of Contents survives trimming,
// so excluding them understates the real prompt size (a ~7KB system prompt
// plus large create_plan/edit_plan/execute schemas is ~2k tokens on its own).
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
