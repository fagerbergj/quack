package memory

import (
	"testing"

	"google.golang.org/genai"
)

func TestFirstStep(t *testing.T) {
	user := func(text string) *genai.Content {
		return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: text}}}
	}
	modelCall := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "calling web_search"}}}
	// Function responses are role=user but textless — must not count as the user turn.
	funcResp := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "web_search"}}}}
	const q = "research native plants"

	tests := []struct {
		name     string
		contents []*genai.Content
		want     bool
	}{
		{"step 1: only user message", []*genai.Content{user(q)}, true},
		{"step 2: model produced output", []*genai.Content{user(q), modelCall}, false},
		{"step 3: model + func response", []*genai.Content{user(q), modelCall, funcResp}, false},
		// Long-lived session (orchestrator): prior turn then a fresh user message.
		{"new turn after prior turn", []*genai.Content{user("old question"), modelCall, user(q)}, true},
		{"new turn, then model output", []*genai.Content{user("old question"), modelCall, user(q), modelCall}, false},
		// A trailing func response (textless, role=user) means the model already acted.
		{"func response after user is not first", []*genai.Content{user(q), funcResp}, false},
		{"empty contents", nil, true},
	}
	for _, tt := range tests {
		if got := firstStep(tt.contents); got != tt.want {
			t.Errorf("%s: firstStep = %v, want %v", tt.name, got, tt.want)
		}
	}
}
