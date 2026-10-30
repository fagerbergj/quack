package vetting

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference/openaimodel"
)

// TestSubmitVerdictWire_StringifiedArgsDecode: submit_verdict's real schema drives the adapter's decoder,
// so a criteria string missing its final brace and a stringified findings array arrive as object and array.
func TestSubmitVerdictWire_StringifiedArgsDecode(t *testing.T) {
	submit, err := newSubmitVerdictTool(&verdict{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"score": 3, "criteria": `{"grounded": {"score": 3, "reason": "ok"}`, "findings": `[]`})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": submitVerdictTool, "arguments": string(args)}}},
			}}},
		})
	}))
	defer srv.Close()
	req := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("go", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{
			submit.(interface {
				Declaration() *genai.FunctionDeclaration
			}).Declaration(),
		}}}},
	}
	for resp, err := range openaimodel.NewOpenAIModel("m", srv.URL, "k", "").GenerateContent(t.Context(), req, false) {
		if err != nil {
			t.Fatal(err)
		}
		got := resp.Content.Parts[0].FunctionCall.Args
		if _, ok := got["criteria"].(map[string]any); !ok {
			t.Errorf("criteria = %#v, want a decoded object", got["criteria"])
		}
		if _, ok := got["findings"].([]any); !ok {
			t.Errorf("findings = %#v, want a decoded array", got["findings"])
		}
	}
}
