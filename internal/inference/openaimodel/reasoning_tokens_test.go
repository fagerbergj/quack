package openaimodel

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// TestGenerate_EstimatesReasoningTokensWhenUsageOmitsThem covers #968:
// llama-server's usage has no completion_tokens_details.reasoning_tokens, so
// the adapter must estimate reasoning tokens from reasoning_content (chars/4)
// and subtract them from CandidatesTokenCount so output means answer.
func TestGenerate_EstimatesReasoningTokensWhenUsageOmitsThem(t *testing.T) {
	// 40-char reasoning_content -> chars/4 = 10 estimated reasoning tokens.
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer","reasoning_content":"0123456789012345678901234567890123456789"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	var final *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		final = resp
	}
	if final == nil || final.UsageMetadata == nil {
		t.Fatal("no usage metadata")
	}
	u := final.UsageMetadata
	if u.ThoughtsTokenCount != 10 {
		t.Errorf("ThoughtsTokenCount = %d, want 10 (estimated from 40 chars)", u.ThoughtsTokenCount)
	}
	if u.CandidatesTokenCount != 40 {
		t.Errorf("CandidatesTokenCount = %d, want 40 (50 completion - 10 reasoning)", u.CandidatesTokenCount)
	}
}

// TestGenerate_UntouchedWhenProviderReportsReasoningTokens covers the case
// where usage already carries an exact reasoning_tokens count: the adapter
// must pass it through unmodified, never re-estimating over an exact value.
func TestGenerate_UntouchedWhenProviderReportsReasoningTokens(t *testing.T) {
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer","reasoning_content":"some reasoning text here"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"completion_tokens_details":{"reasoning_tokens":30}}}`)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	var final *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		final = resp
	}
	if final == nil || final.UsageMetadata == nil {
		t.Fatal("no usage metadata")
	}
	u := final.UsageMetadata
	if u.ThoughtsTokenCount != 30 {
		t.Errorf("ThoughtsTokenCount = %d, want 30 (provider-supplied, untouched)", u.ThoughtsTokenCount)
	}
	if u.CandidatesTokenCount != 50 {
		t.Errorf("CandidatesTokenCount = %d, want 50 (unmodified - provider count already exact)", u.CandidatesTokenCount)
	}
}

// TestGenerate_NoReasoningLeavesUsageUnchanged covers a plain turn with no
// reasoning_content at all - usage must pass through unchanged.
func TestGenerate_NoReasoningLeavesUsageUnchanged(t *testing.T) {
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	var final *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		final = resp
	}
	if final == nil || final.UsageMetadata == nil {
		t.Fatal("no usage metadata")
	}
	u := final.UsageMetadata
	if u.ThoughtsTokenCount != 0 {
		t.Errorf("ThoughtsTokenCount = %d, want 0 (no reasoning_content)", u.ThoughtsTokenCount)
	}
	if u.CandidatesTokenCount != 50 {
		t.Errorf("CandidatesTokenCount = %d, want 50 (unmodified)", u.CandidatesTokenCount)
	}
}

// TestStreaming_EstimatesReasoningTokensWhenUsageOmitsThem is the streaming
// counterpart: the final usage-only chunk carries completion_tokens with no
// reasoning split, and the aggregated reasoning_content deltas must be used
// to estimate and subtract reasoning tokens the same way the non-streaming
// path does.
func TestStreaming_EstimatesReasoningTokensWhenUsageOmitsThem(t *testing.T) {
	srv := sseServer(t,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"0123456789012345678901234567890123456789"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"the answer"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`,
	)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k")

	_, _, usage := collect(t, m)
	if usage == nil {
		t.Fatal("no usage metadata")
	}
	if usage.ThoughtsTokenCount != 10 {
		t.Errorf("ThoughtsTokenCount = %d, want 10 (estimated from 40 chars)", usage.ThoughtsTokenCount)
	}
	if usage.CandidatesTokenCount != 40 {
		t.Errorf("CandidatesTokenCount = %d, want 40 (50 completion - 10 reasoning)", usage.CandidatesTokenCount)
	}
}
