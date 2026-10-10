package dag

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// hasFunctionCall reports whether c carries a FunctionCall part.
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

// recordingBudgetLLM records the request it actually received, the way
// scoreMessage's own recordingModel does in internal/agent's tests.
type recordingBudgetLLM struct{ got *model.LLMRequest }

func (r *recordingBudgetLLM) Name() string { return "recording" }

func (r *recordingBudgetLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	r.got = req
	return func(yield func(*model.LLMResponse, error) bool) { yield(&model.LLMResponse{}, nil) }
}

// bigCallResponsePair is one plan-judge round trip: a sizable FunctionCall followed
// by its sizable FunctionResponse.
func bigCallResponsePair(n int) []*genai.Content {
	blob := strings.Repeat("x", n)
	return []*genai.Content{
		{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "execute", Args: map[string]any{"plan": blob}}}}},
		{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "execute", Response: map[string]any{"error": blob}}}}},
	}
}

// A tool loop growing well past the window (ADK's tail-retention declines on this
// shape) must still never be forwarded over budget.
func TestBudgetedLLMTrimsAToolLoopGrowingPastTheWindow(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = 8000 // reserve = contextWindow/4 = 2000, budget = 6000
	llm := NewBudgetedLLM(rec, contextWindow)
	budget := llm.(*BudgetedLLM).budget

	opening := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "review PR #1"}}}
	contents := []*genai.Content{opening}

	// 40 rounds of a 500-char pair (~20000 tokens total) - well past budget.
	for round := 0; round < 40; round++ {
		contents = append(contents, bigCallResponsePair(500)...)
		req := &model.LLMRequest{Contents: append([]*genai.Content(nil), contents...)}
		drainLLM(llm.GenerateContent(context.Background(), req, false))

		if got := estimateTokens(rec.got.Contents); got > budget {
			t.Fatalf("round %d: forwarded request = %d estimated tokens, want <= %d (the configured budget)", round, got, budget)
		}
		if rec.got.Contents[0] != opening {
			t.Fatalf("round %d: Contents[0] = %+v, want the invocation's own opening turn preserved", round, rec.got.Contents[0])
		}
	}
}

// A changed note on a second trim would move the cache divergence point.
func TestBudgetedLLMTrimNoteIsFixedText(t *testing.T) {
	rec := &recordingBudgetLLM{}
	// 800: budget 600, so the pairs below overflow. Two pairs on the first
	// call - one pair alone is the pinned last content and can never be trimmed.
	const contextWindow = 800
	llm := NewBudgetedLLM(rec, contextWindow)

	opening := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "review PR #1"}}}
	contents := []*genai.Content{opening}
	contents = append(contents, bigCallResponsePair(2000)...)
	contents = append(contents, bigCallResponsePair(2000)...)
	req := &model.LLMRequest{Contents: append([]*genai.Content(nil), contents...)}
	drainLLM(llm.GenerateContent(context.Background(), req, false))
	firstNote := rec.got.Contents[1].Parts[0].Text

	contents = append(contents, bigCallResponsePair(2000)...)
	req = &model.LLMRequest{Contents: append([]*genai.Content(nil), contents...)}
	drainLLM(llm.GenerateContent(context.Background(), req, false))
	secondNote := rec.got.Contents[1].Parts[0].Text

	if firstNote != secondNote {
		t.Fatalf("trim note changed between trims: %q vs %q", firstNote, secondNote)
	}
}

// A trim never leaves a FunctionCall without its FunctionResponse or vice versa;
// most providers 400 on that wire shape.
func TestBudgetedLLMNeverOrphansACallOrResponse(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = 1600 // reserve = contextWindow/4 = 400, budget = 1200
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

// Plain text turns alternate user/model; dropping one middle content would join its
// neighbours into adjacent same-role turns, so every survivor must still alternate.
func TestBudgetedLLMPlainTextTurnsNeverAdjacentSameRole(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = 800 // reserve capped at contextWindow/4 = 200, so budget = 600 - tight
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

// The current query is always the LAST content, not Contents[0]; a trim that erases it
// makes the model server 500 with "no user query found".
func TestBudgetedLLMPreservesTheCurrentQueryOnAChatWithHistory(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = 800 // reserve capped at contextWindow/4 = 200, so budget = 600 - tight
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

// Below the flat 20k, the reserve caps at contextWindow/4 instead.
func TestNewBudgetedLLMCapsReserveAtContextWindow4(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = 32768
	llm := NewBudgetedLLM(rec, contextWindow)
	bl := llm.(*BudgetedLLM)
	wantReserve := contextWindow / 4 // 8192, well under the flat 20_000
	if want := contextWindow - wantReserve; bl.budget != want {
		t.Fatalf("budget = %d, want %d (reserve capped at contextWindow/4 = %d instead of the flat %d)", bl.budget, want, wantReserve, budgetOutputReserve)
	}
}

// Above 80k, contextWindow/4 exceeds 20k, so the flat reserve wins instead.
func TestNewBudgetedLLMKeepsFlatReserveOnALargeWindow(t *testing.T) {
	rec := &recordingBudgetLLM{}
	const contextWindow = 200_000 // contextWindow/4 (50_000) exceeds the flat reserve
	llm := NewBudgetedLLM(rec, contextWindow)
	bl := llm.(*BudgetedLLM)
	if want := contextWindow - budgetOutputReserve; bl.budget != want {
		t.Fatalf("budget = %d, want %d (flat reserve %d)", bl.budget, want, budgetOutputReserve)
	}
}

// TestNewBudgetedLLMUnwrapsWhenUnenforced mirrors AdmittingLLM's own no-op contract.
func TestNewBudgetedLLMUnwrapsWhenUnenforced(t *testing.T) {
	rec := &recordingBudgetLLM{}
	if got := NewBudgetedLLM(rec, 0); got != model.LLM(rec) {
		t.Error("contextWindow=0 should return the inner LLM unwrapped")
	}
}
