package dag

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// recordingBudgetLLM records the request it actually received, the way
// scoreMessage's own recordingModel does in internal/agent's tests.
type recordingBudgetLLM struct{ got *model.LLMRequest }

func (r *recordingBudgetLLM) Name() string { return "recording" }

func (r *recordingBudgetLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	r.got = req
	return func(yield func(*model.LLMResponse, error) bool) { yield(&model.LLMResponse{}, nil) }
}

// bigCallResponsePair simulates one plan-judge round trip: a FunctionCall
// content (a create_plan/execute call with a sizable args blob, standing in
// for a full plan JSON) immediately followed by its FunctionResponse content
// (a sizable rejection reason).
func bigCallResponsePair(n int) []*genai.Content {
	blob := strings.Repeat("x", n)
	return []*genai.Content{
		{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "execute", Args: map[string]any{"plan": blob}}}}},
		{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "execute", Response: map[string]any{"error": blob}}}}},
	}
}

// TestBudgetedLLMTrimsAToolLoopGrowingPastTheWindow is the regression test
// for the QA rig's context-overflow finding: a fake model's tool loop grows
// well past the configured window (ADK's own EventRetentionSize-gated
// tail-retention declines on a loop this shape - see BudgetedLLM's doc), and
// BudgetedLLM must still never forward a request over budget.
func TestBudgetedLLMTrimsAToolLoopGrowingPastTheWindow(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = budgetOutputReserve + 1000 // usable budget = 1000 tokens (budget = contextWindow - reserve)
	llm := NewBudgetedLLM(rec, contextWindow)

	opening := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "review PR #1"}}}
	contents := []*genai.Content{opening}

	// 20 rounds of a 500-char pair (~2500 tokens total) - past the 1000-token budget.
	for round := 0; round < 20; round++ {
		contents = append(contents, bigCallResponsePair(500)...)
		req := &model.LLMRequest{Contents: append([]*genai.Content(nil), contents...)}
		drainLLM(llm.GenerateContent(context.Background(), req, false))

		if got := estimateTokens(rec.got.Contents); got > 1000 {
			t.Fatalf("round %d: forwarded request = %d estimated tokens, want <= 1000 (the configured budget)", round, got)
		}
		if rec.got.Contents[0] != opening {
			t.Fatalf("round %d: Contents[0] = %+v, want the invocation's own opening turn preserved", round, rec.got.Contents[0])
		}
	}
}

// TestBudgetedLLMNeverOrphansACallOrResponse pins the pairing invariant: a
// trim never leaves a FunctionCall without its FunctionResponse, or vice
// versa - most providers 400 on that wire shape, which would trade one
// failure mode for another.
func TestBudgetedLLMNeverOrphansACallOrResponse(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = budgetOutputReserve + 200 // usable budget = 200 tokens
	llm := NewBudgetedLLM(rec, contextWindow)

	contents := []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "ask"}}}}
	for round := 0; round < 10; round++ {
		contents = append(contents, bigCallResponsePair(300)...)
	}
	req := &model.LLMRequest{Contents: contents}
	drainLLM(llm.GenerateContent(context.Background(), req, false))

	for i, c := range rec.got.Contents {
		if hasFunctionCall(c) {
			if i+1 >= len(rec.got.Contents) {
				t.Fatalf("Contents[%d] is a FunctionCall with no following content at all", i)
			}
			next := rec.got.Contents[i+1]
			hasResponse := false
			for _, p := range next.Parts {
				if p != nil && p.FunctionResponse != nil {
					hasResponse = true
				}
			}
			if !hasResponse {
				t.Fatalf("Contents[%d] is a FunctionCall whose next content (%d) is not its FunctionResponse", i, i+1)
			}
		}
	}
}

// TestNewBudgetedLLMUnwrapsWhenUnenforced mirrors AdmittingLLM's own no-op contract.
func TestNewBudgetedLLMUnwrapsWhenUnenforced(t *testing.T) {
	rec := &recordingBudgetLLM{}
	if got := NewBudgetedLLM(rec, 0); got != model.LLM(rec) {
		t.Error("contextWindow=0 should return the inner LLM unwrapped")
	}
}
