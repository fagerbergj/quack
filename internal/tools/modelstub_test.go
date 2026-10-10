package tools

import (
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Stub-model helpers mirroring internal/dag's gCall/gText/gSysText; duplicated because
// tools imports dag, so sharing them would be an import cycle.

func atText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func atCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: name, Args: args},
		}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

// atAllText concatenates every non-thought text part in the request's contents, to assert
// what a stub model actually saw.
func atAllText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}
