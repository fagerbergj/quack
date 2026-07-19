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
	var text, toolName, thought string
	for _, p := range content.Parts {
		switch {
		case p.Thought && p.Text != "":
			thought += p.Text
		case p.Text != "" && !p.Thought:
			text += p.Text
		case p.FunctionCall != nil:
			toolName = p.FunctionCall.Name
		}
	}
	if text != "Hello world" {
		t.Errorf("answer text = %q, want %q", text, "Hello world")
	}
	if thought != "let me think" {
		t.Errorf("thought = %q, want %q", thought, "let me think")
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
// length limit, so content (the non-thought text) comes back empty. Per #295,
// the adapter promotes the reasoning to the answer rather than dropping it —
// and still returns a terminal response with finish=length.
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
	if answer != "thinking and thinking" {
		t.Errorf("answer = %q, want reasoning promoted to answer", answer)
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

// TestReasoningToolCalls_XMLFunctionStyle covers the second qwen leak format:
// tool calls in reasoning_content as <function=name><parameter=k>v</parameter>
// XML inside <tool_call> (observed live from qwen3.x — the Hermes-JSON regex
// never matches it, so the calls were lost and the raw XML ended up promoted
// to the answer). Verbatim shape from the incident, multi-line values included.
func TestReasoningToolCalls_XMLFunctionStyle(t *testing.T) {
	reasoning := "Good progress. Let me now fetch more detailed pages.\n\n" +
		"<tool_call>\n<function=web_fetch>\n<parameter=url>\nhttps://butchartgardens.com/\n</parameter>\n</function>\n</tool_call>\n" +
		"<tool_call>\n<function=web_search>\n<parameter=query>\nCraigdarroch Castle admission price 2026 hours tour\n</parameter>\n</function>\n</tool_call>"
	calls, cleaned := reasoningToolCalls(reasoning)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Name != "web_fetch" || calls[0].Args["url"] != "https://butchartgardens.com/" {
		t.Errorf("call0 = %+v", calls[0])
	}
	if calls[1].Name != "web_search" || calls[1].Args["query"] != "Craigdarroch Castle admission price 2026 hours tour" {
		t.Errorf("call1 = %+v", calls[1])
	}
	if calls[0].ID == "" || calls[0].ID == calls[1].ID {
		t.Errorf("calls need distinct non-empty IDs: %q %q", calls[0].ID, calls[1].ID)
	}
	if !strings.Contains(cleaned, "Good progress") {
		t.Errorf("cleaned reasoning lost the real thinking: %q", cleaned)
	}
	for _, residue := range []string{"<tool_call>", "<function=", "<parameter="} {
		if strings.Contains(cleaned, residue) {
			t.Errorf("cleaned reasoning still contains %q: %q", residue, cleaned)
		}
	}

	// JSON-typed parameter values keep their type; anything else stays a string.
	typed, _ := reasoningToolCalls("<tool_call><function=read_file><parameter=offset>10</parameter><parameter=all>true</parameter><parameter=path>README.md</parameter></function></tool_call>")
	if len(typed) != 1 {
		t.Fatalf("typed-params: got %d calls, want 1", len(typed))
	}
	if typed[0].Args["offset"] != float64(10) {
		t.Errorf("offset = %#v, want float64(10)", typed[0].Args["offset"])
	}
	if typed[0].Args["all"] != true {
		t.Errorf("all = %#v, want true", typed[0].Args["all"])
	}
	if typed[0].Args["path"] != "README.md" {
		t.Errorf("path = %#v, want string", typed[0].Args["path"])
	}
}

// TestStreaming_RecoversXMLToolCallsFromReasoning runs the leak through the
// streaming adapter end to end, with the XML block split across chunks the
// way a live stream delivers it. The recovered calls must surface as
// FunctionCall parts and no XML residue may remain in the thinking.
func TestStreaming_RecoversXMLToolCallsFromReasoning(t *testing.T) {
	srv := sseServer(t,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"Let me fetch details.\n<tool_call>\n<function=web_fetch>\n"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"<parameter=url>\nhttps://example.com/\n</parameter>\n</function>\n</tool_call>\n"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"<tool_call>\n<function=web_search>\n<parameter=query>\nVictoria BC Inner Harbour\n</parameter>\n</function>\n</tool_call>"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k")

	content, _, _ := collect(t, m)
	var thought string
	var calls []*genai.FunctionCall
	for _, p := range content.Parts {
		switch {
		case p.FunctionCall != nil:
			calls = append(calls, p.FunctionCall)
		case p.Thought && p.Text != "":
			thought += p.Text
		}
	}
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 (parts: %+v)", len(calls), content.Parts)
	}
	if calls[0].Name != "web_fetch" || calls[0].Args["url"] != "https://example.com/" {
		t.Errorf("call0 = %+v", calls[0])
	}
	if calls[1].Name != "web_search" || calls[1].Args["query"] != "Victoria BC Inner Harbour" {
		t.Errorf("call1 = %+v", calls[1])
	}
	for _, residue := range []string{"<tool_call>", "<function=", "<parameter="} {
		if strings.Contains(thought, residue) {
			t.Errorf("thinking still contains %q: %q", residue, thought)
		}
	}
}

// TestGenerate_RecoversXMLToolCallsFromReasoning covers the non-streaming path
// (the one worker rounds use): a tool call leaked into reasoning_content must
// become a FunctionCall, and the empty-content fallback must NOT promote the
// raw XML thinking to the answer.
func TestGenerate_RecoversXMLToolCallsFromReasoning(t *testing.T) {
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"","reasoning_content":"Let me search.\n<tool_call>\n<function=web_search>\n<parameter=query>\nSMR 2026\n</parameter>\n</function>\n</tool_call>"}}]}`)
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
	if final == nil {
		t.Fatal("no response")
	}
	var answer, thought string
	var calls []*genai.FunctionCall
	for _, p := range final.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			calls = append(calls, p.FunctionCall)
		case p.Thought:
			thought += p.Text
		default:
			answer += p.Text
		}
	}
	if len(calls) != 1 || calls[0].Name != "web_search" || calls[0].Args["query"] != "SMR 2026" {
		t.Fatalf("calls = %+v, want one web_search(SMR 2026)", calls)
	}
	if strings.Contains(answer, "<tool_call>") || strings.Contains(answer, "<function=") {
		t.Errorf("raw XML promoted to answer: %q", answer)
	}
	if strings.Contains(thought, "<tool_call>") || strings.Contains(thought, "<function=") {
		t.Errorf("raw XML left in thinking: %q", thought)
	}
}

// jsonServer serves a single non-streaming OpenAI-compatible /chat/completions
// response body (a raw ChatCompletion JSON object).
func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// TestGenerate_PromotesReasoningWhenContentEmpty covers issue #295: the
// non-streaming path (used for actual worker rounds — RunConfig.StreamingMode
// defaults to "none") dropped a synthesized answer that landed entirely in
// reasoning_content, leaving content empty. The answer must be recovered.
func TestGenerate_PromotesReasoningWhenContentEmpty(t *testing.T) {
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"","reasoning_content":"Sources read and synthesized: the answer is 42."}}]}`)
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
	if final == nil {
		t.Fatal("no response")
	}
	var answer string
	for _, p := range final.Content.Parts {
		if !p.Thought && p.FunctionCall == nil {
			answer += p.Text
		}
	}
	if answer != "Sources read and synthesized: the answer is 42." {
		t.Errorf("answer = %q, want reasoning text promoted", answer)
	}
}

// TestGenerate_ContentTakesPriorityOverReasoning guards the fallback: a
// genuinely non-empty content field must never be overwritten by reasoning.
func TestGenerate_ContentTakesPriorityOverReasoning(t *testing.T) {
	srv := jsonServer(t, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the real answer","reasoning_content":"unrelated thinking"}}]}`)
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
	if final == nil {
		t.Fatal("no response")
	}
	var answer string
	for _, p := range final.Content.Parts {
		if !p.Thought && p.FunctionCall == nil {
			answer += p.Text
		}
	}
	if answer != "the real answer" {
		t.Errorf("answer = %q, want %q", answer, "the real answer")
	}
}
