package openaimodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// capturingServer records the last request body's reasoning_effort field.
func capturingServer(t *testing.T, resp string) (*httptest.Server, *string) {
	t.Helper()
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured = body.ReasoningEffort
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	return srv, &captured
}

const stubChatResp = `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`

func TestGenerate_DefaultEffortSetsReasoningEffort(t *testing.T) {
	srv, captured := capturingServer(t, stubChatResp)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "low")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	for _, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}
	if *captured != "low" {
		t.Errorf("reasoning_effort = %q, want %q", *captured, "low")
	}
}

func TestGenerate_NoEffortConfiguredOmitsReasoningEffort(t *testing.T) {
	srv, captured := capturingServer(t, stubChatResp)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "")

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
	for _, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}
	if *captured != "" {
		t.Errorf("reasoning_effort = %q, want empty", *captured)
	}
}

// TestGenerate_ExplicitThinkingConfigWinsOverDefaultEffort covers the judge's
// gates.judge.thinking_level, which must take precedence over models.<name>.effort.
func TestGenerate_ExplicitThinkingConfigWinsOverDefaultEffort(t *testing.T) {
	srv, captured := capturingServer(t, stubChatResp)
	defer srv.Close()
	m := NewOpenAIModel("m", srv.URL, "k", "low")

	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}},
		Config:   &genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelHigh}},
	}
	for _, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}
	if *captured != "high" {
		t.Errorf("reasoning_effort = %q, want %q (explicit ThinkingConfig should win)", *captured, "high")
	}
}
