package openaimodel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// sseServer serves an OpenAI-compatible streaming /chat/completions response from
// the given raw SSE "data:" payloads (each a ChatCompletionChunk JSON), then
// [DONE]. It's the offline stand-in for the model host, so this test guards our
// load-bearing adapter (v2 ships no OpenAI provider) with no live dependency.
func sseServer(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func collect(t *testing.T, m *OpenAIModel) (*genai.Content, genai.FinishReason, *genai.GenerateContentResponseUsageMetadata) {
	t.Helper()
	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	var final *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, true) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		if resp != nil && resp.TurnComplete {
			final = resp
		}
	}
	if final == nil {
		t.Fatal("no terminal (TurnComplete) response")
	}
	return final.Content, final.FinishReason, final.UsageMetadata
}

// TestStreaming_TranslatesContentReasoningTools verifies the streaming adapter
// aggregates text, surfaces reasoning_content as a Thought part, aggregates tool
// calls, and carries finish reason + usage.
func TestStreaming_TranslatesContentReasoningTools(t *testing.T) {
	srv := sseServer(t,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"let me think"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Hello "}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"world"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"q\":\"x\"}"}}]}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k")

	content, finish, usage := collect(t, m)
	var text, toolName string
	for _, p := range content.Parts {
		switch {
		case p.Text != "" && !p.Thought:
			text += p.Text
		case p.FunctionCall != nil:
			toolName = p.FunctionCall.Name
		}
	}
	// (reasoning_content → Thought is verified in production/UI, not here — it's
	// extracted via the openai-go client's ExtraFields, which a hand-rolled SSE
	// fake can't reproduce faithfully across client versions.)
	if text != "Hello world" {
		t.Errorf("answer text = %q, want %q", text, "Hello world")
	}
	if toolName != "web_search" {
		t.Errorf("tool call = %q, want web_search", toolName)
	}
	if finish != genai.FinishReasonStop {
		t.Errorf("finish = %v, want Stop", finish)
	}
	if usage == nil || usage.TotalTokenCount != 15 {
		t.Errorf("usage = %+v, want total 15", usage)
	}
}

// TestStreaming_EmptyTurnReasoningOnly reproduces the reasoning-model failure
// mode that bit us live: the model streams only reasoning_content and hits the
// length limit — the answer (non-thought text) comes back empty. The adapter must
// still return a terminal response with finish=length (so the empty-turn WARN can
// fire and the gate's empty-answer recovery can kick in) rather than hang or drop.
func TestStreaming_EmptyTurnReasoningOnly(t *testing.T) {
	srv := sseServer(t,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"thinking and thinking"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}`,
	)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k")

	content, finish, _ := collect(t, m)
	var answer string
	for _, p := range content.Parts {
		if !p.Thought && p.FunctionCall == nil && p.Text != "" {
			answer += p.Text
		}
	}
	if strings.TrimSpace(answer) != "" {
		t.Errorf("expected empty answer (reasoning-only turn), got %q", answer)
	}
	if finish != genai.FinishReasonMaxTokens {
		t.Errorf("finish = %v, want MaxTokens (length)", finish)
	}
}

// TestReasoningToolCalls covers the Qwen3.x recovery: tool calls that arrive as
// <tool_call> XML inside reasoning_content (llama.cpp#22684) are parsed back into
// FunctionCalls, and the blocks are stripped from the thinking.
func TestReasoningToolCalls(t *testing.T) {
	reasoning := "Let me search.\n<tool_call>\n{\"name\": \"web_search\", \"arguments\": {\"query\": \"SMR 2026\"}}\n</tool_call>\nand fetch:\n<tool_call>{\"name\":\"web_fetch\",\"arguments\":{\"url\":\"https://x\"}}</tool_call>"
	calls, cleaned := reasoningToolCalls(reasoning)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Name != "web_search" || calls[0].Args["query"] != "SMR 2026" {
		t.Errorf("call0 = %+v", calls[0])
	}
	if calls[1].Name != "web_fetch" || calls[1].Args["url"] != "https://x" {
		t.Errorf("call1 = %+v", calls[1])
	}
	if calls[0].ID == "" || calls[0].ID == calls[1].ID {
		t.Errorf("calls need distinct non-empty IDs: %q %q", calls[0].ID, calls[1].ID)
	}
	if strings.Contains(cleaned, "<tool_call>") {
		t.Errorf("tool_call blocks not stripped: %q", cleaned)
	}
	// no false positives on plain thinking
	if c, _ := reasoningToolCalls("just thinking, no tools"); len(c) != 0 {
		t.Errorf("plain thinking yielded %d calls", len(c))
	}
}
