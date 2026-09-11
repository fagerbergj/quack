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

// TestBudgetedLLMPlainTextTurnsNeverAdjacentSameRole pins the reviewer's
// finding (#slice3 review): the three tests above only exercise tool-loop
// layouts (back-to-back FunctionCall/FunctionResponse pairs), where the
// pairing rule happens to already drop two at a time. A plain multi-turn
// TEXT chat (no tool calls at all) alternates strictly user/model/user/...;
// dropping a single content from the middle - as the old code did for
// anything that wasn't a FunctionCall - joins its two neighbors into an
// adjacent SAME role, exactly the shape the note-splice comment already
// calls unsafe. Every surviving content here must still alternate role with
// its neighbor.
func TestBudgetedLLMPlainTextTurnsNeverAdjacentSameRole(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = budgetOutputReserve + 200 // usable budget = 200 tokens - tight
	llm := NewBudgetedLLM(rec, contextWindow)

	blob := strings.Repeat("x", 300)
	contents := []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "start: " + blob}}}}
	for round := 0; round < 10; round++ {
		contents = append(contents,
			&genai.Content{Role: "model", Parts: []*genai.Part{{Text: blob}}},
			&genai.Content{Role: "user", Parts: []*genai.Part{{Text: blob}}},
		)
	}
	req := &model.LLMRequest{Contents: contents}
	drainLLM(llm.GenerateContent(context.Background(), req, false))

	got := rec.got.Contents
	if len(got) < 2 {
		t.Fatalf("Contents = %v, want at least 2 survivors (Contents[0] plus the current query)", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] == nil || got[i] == nil {
			continue
		}
		if got[i-1].Role == got[i].Role {
			t.Fatalf("Contents[%d] and [%d] are both role %q after trimming a plain text chat - alternation broken", i-1, i, got[i].Role)
		}
	}
}

// TestBudgetedLLMPreservesTheCurrentQueryOnAChatWithHistory pins the rig
// regression (#slice3 review): on a chat with history, Contents[0] is the
// SESSION's first-ever turn, not this invocation's own query - which is
// always the LAST content (whatever the model must respond to next, plain
// text or a mid-loop FunctionResponse). A front-to-back trim that protects
// only index 0 can erase that current query while older, larger history
// survives, and the model server then 500s with "no user query found".
func TestBudgetedLLMPreservesTheCurrentQueryOnAChatWithHistory(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = budgetOutputReserve + 200 // usable budget = 200 tokens - tight
	llm := NewBudgetedLLM(rec, contextWindow)

	opening := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "session start: review this repo"}}}
	contents := []*genai.Content{opening}
	for round := 0; round < 10; round++ { // long tool rounds, from turns well before this one
		contents = append(contents, bigCallResponsePair(400)...)
	}
	currentQuery := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "CURRENT_QUERY: what about the follow-up?"}}}
	contents = append(contents, currentQuery)

	req := &model.LLMRequest{Contents: contents}
	drainLLM(llm.GenerateContent(context.Background(), req, false))

	got := rec.got.Contents
	if got[0] != opening {
		t.Fatalf("Contents[0] = %+v, want the session's opening turn preserved", got[0])
	}
	if last := got[len(got)-1]; last != currentQuery {
		t.Fatalf("last content = %+v, want the current invocation's own query preserved - "+
			"the request must end on a user turn or the model server has nothing to answer", last)
	}
	for i, c := range got {
		if hasFunctionCall(c) {
			if i+1 >= len(got) {
				t.Fatalf("Contents[%d] is a FunctionCall with no following content", i)
			}
			resp := false
			for _, p := range got[i+1].Parts {
				if p != nil && p.FunctionResponse != nil {
					resp = true
				}
			}
			if !resp {
				t.Fatalf("Contents[%d] is a FunctionCall whose next content (%d) is not its FunctionResponse - pairing broken", i, i+1)
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
