package openaimodel

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// captureLogs redirects the default slog logger to a buffer for the duration
// of the test, restoring the previous default on cleanup.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestStreaming_EmptyTurnLogsFinishReason pins the empty-turn diagnostic log
// (no answer, no thinking) that the streaming path emits - the only signal
// available downstream when a model returns nothing at all.
func TestStreaming_EmptyTurnLogsFinishReason(t *testing.T) {
	buf := captureLogs(t)
	srv := sseServer(t,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10}}`,
	)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")

	content, finish, _ := collect(t, m)
	if len(content.Parts) != 0 {
		t.Errorf("parts = %+v, want none (no answer, no thinking)", content.Parts)
	}
	if finish != genai.FinishReasonMaxTokens {
		t.Errorf("finish = %v, want MaxTokens", finish)
	}
	if !strings.Contains(buf.String(), "model returned no answer content (empty turn)") {
		t.Errorf("expected empty-turn log, got: %s", buf.String())
	}
}

// TestGenerate_EmptyTurnDoesNotLog pins the CURRENT asymmetry: unlike the
// streaming path, the non-streaming path does not log anything for a turn
// with neither answer nor thinking. This is called out in the PR as a
// pre-existing divergence the refactor deliberately preserves.
func TestGenerate_EmptyTurnDoesNotLog(t *testing.T) {
	buf := captureLogs(t)
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":""}}]}`)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	var final *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		final = resp
	}
	if final == nil || len(final.Content.Parts) != 0 {
		t.Fatalf("content = %+v, want no parts", final)
	}
	if strings.Contains(buf.String(), "empty turn") {
		t.Errorf("non-streaming path logged an empty-turn message, expected none: %s", buf.String())
	}
}

// TestStreaming_ToolCallsFinishReason pins the finish_reason mapping for a
// turn that ends with tool_calls - both "tool_calls" and "function_call"
// collapse to genai.FinishReasonStop (convertFinishReason).
func TestStreaming_ToolCallsFinishReason(t *testing.T) {
	srv := sseServer(t,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"web_search","arguments":"{}"}}]}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")

	_, finish, _ := collect(t, m)
	if finish != genai.FinishReasonStop {
		t.Errorf("finish = %v, want Stop (tool_calls maps to Stop)", finish)
	}
}

// TestGenerate_ToolCallsFinishReason is the non-streaming counterpart.
func TestGenerate_ToolCallsFinishReason(t *testing.T) {
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{}"}}]}}]}`)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	var final *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		final = resp
	}
	if final == nil {
		t.Fatal("no response")
	}
	if final.FinishReason != genai.FinishReasonStop {
		t.Errorf("finish = %v, want Stop (tool_calls maps to Stop)", final.FinishReason)
	}
}

// TestStreaming_PromotedReasoningLogMessage and
// TestGenerate_PromotedReasoningLogMessage pin the exact (currently
// DIFFERENT) log message text each path emits when promoting reasoning to
// the answer - called out in the PR as a divergence the refactor unifies.
func TestStreaming_PromotedReasoningLogMessage(t *testing.T) {
	buf := captureLogs(t)
	srv := sseServer(t,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")
	collect(t, m)
	if !strings.Contains(buf.String(), "promoted reasoning to answer (empty content, unclosed </think>)") {
		t.Errorf("expected streaming's promoted-reasoning message, got: %s", buf.String())
	}
}

func TestGenerate_PromotedReasoningLogMessage(t *testing.T) {
	buf := captureLogs(t)
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"","reasoning_content":"thinking"}}]}`)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		_ = resp
	}
	if !strings.Contains(buf.String(), "promoted reasoning to answer (empty content, reasoning_content held the answer)") {
		t.Errorf("expected non-streaming's promoted-reasoning message, got: %s", buf.String())
	}
}

// TestGenerate_ToolCallsSuppressPromotion is a regression test for PR #1243
// review: the non-streaming path must never promote reasoning_content to the
// answer on a turn that already has a real tool call, even when the answer text is empty. Before the fix, real tool-call parts were appended AFTER the fallback ladder ran, so the ladder saw no answer yet and promoted.
func TestGenerate_ToolCallsSuppressPromotion(t *testing.T) {
	buf := captureLogs(t)
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","reasoning_content":"deciding which tool to call","tool_calls":[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{}"}}]}}]}`)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	var final *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		final = resp
	}
	if final == nil {
		t.Fatal("no response")
	}
	var answer string
	var calls []*genai.FunctionCall
	for _, p := range final.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			calls = append(calls, p.FunctionCall)
		case !p.Thought && p.Text != "":
			answer += p.Text
		}
	}
	if len(calls) != 1 || calls[0].Name != "web_search" {
		t.Fatalf("calls = %+v, want one web_search", calls)
	}
	if answer != "" {
		t.Errorf("answer = %q, want empty - reasoning must not be promoted on a tool-call turn", answer)
	}
	if strings.Contains(buf.String(), "promoted reasoning to answer") {
		t.Errorf("promotion fired on a tool-call turn: %s", buf.String())
	}
}

// TestGenerate_PromotionTrimsReasoning pins that the non-streaming path now
// trims the promoted answer (and the logged char count) the same way the
// streaming path always has - a deliberate harmonization, not a regression:
// the two paths previously disagreed (streaming trimmed, non-streaming did
// not) and now both trim.
func TestGenerate_PromotionTrimsReasoning(t *testing.T) {
	buf := captureLogs(t)
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"","reasoning_content":"  the answer  "}}]}`)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	var final *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		final = resp
	}
	var answer string
	for _, p := range final.Content.Parts {
		if !p.Thought && p.FunctionCall == nil {
			answer += p.Text
		}
	}
	if answer != "the answer" {
		t.Errorf("answer = %q, want trimmed %q", answer, "the answer")
	}
	if !strings.Contains(buf.String(), `"chars"=10`) && !strings.Contains(buf.String(), "chars=10") {
		t.Errorf("expected logged chars=10 (trimmed length), got: %s", buf.String())
	}
}

// TestGenerate_LeakedReasoningUsageMatchesPreRefactor pins the reasoning-token
// estimate for a turn where a tool call leaked inside reasoning_content: the
// estimate is based on the CLEANED (post-recovery) thinking text, same as
// before the fallback-ladder extraction (confirmed unchanged, not a
// divergence - see PR body).
func TestGenerate_LeakedReasoningUsageMatchesPreRefactor(t *testing.T) {
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer","reasoning_content":"Let me search.\n<tool_call>\n<function=web_search>\n<parameter=query>\nSMR 2026\n</parameter>\n</function>\n</tool_call>"}}],"usage":{"prompt_tokens":10,"completion_tokens":50,"total_tokens":60}}`)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")

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
	// "Let me search.\n" is the only text left after the <tool_call> block is
	// stripped - 16 chars -> chars/4 = 4 estimated reasoning tokens.
	if final.UsageMetadata.ThoughtsTokenCount != 4 {
		t.Errorf("ThoughtsTokenCount = %d, want 4 (estimated from cleaned reasoning text)", final.UsageMetadata.ThoughtsTokenCount)
	}
	if final.UsageMetadata.CandidatesTokenCount != 46 {
		t.Errorf("CandidatesTokenCount = %d, want 46 (50 completion - 4 reasoning)", final.UsageMetadata.CandidatesTokenCount)
	}
}
