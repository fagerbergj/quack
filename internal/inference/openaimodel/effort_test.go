package openaimodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

// A 502 burst (OpenRouter with fallbacks off) is retried on one backoff schedule: a recovering upstream is
// reached, and a dead one gets exactly modelCallAttempts tries, never openai-go's retries nested inside httpx's.
func TestGenerate_RetriesGatewayBurst(t *testing.T) {
	defer func(d time.Duration) { modelCallBaseDelay = d }(modelCallBaseDelay)
	modelCallBaseDelay = time.Millisecond
	for _, failFirst := range []int32{3, 1000} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) <= failFirst {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"message":"Provider returned error","code":502}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(stubChatResp))
		}))
		m := NewOpenAIModel("m", srv.URL, "k", "")
		req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
		var gotErr error
		for _, err := range m.GenerateContent(context.Background(), req, false) {
			if err != nil {
				gotErr = err
			}
		}
		srv.Close()
		want, wantErr := failFirst+1, false
		if failFirst >= int32(modelCallAttempts) {
			want, wantErr = int32(modelCallAttempts), true
		}
		if n := calls.Load(); n != want || (gotErr != nil) != wantErr {
			t.Errorf("failFirst %d: calls = %d, err = %v; want %d calls, error %v", failFirst, n, gotErr, want, wantErr)
		}
	}
}
