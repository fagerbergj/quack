package memory

import (
	"context"
	"strings"
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

// Recall is the gate-side preload twin for external workers: formatted block
// on a hit, "" on empty/nil — and a nil store must be safe (memory disabled).
func TestStoreRecall(t *testing.T) {
	consolidator := fakeModel{reply: `{"ops":[{"action":"ADD","content":"build with make dev, not npm run build","kind":"convention"}]}`}
	s := newSQLiteStore(t, "task", consolidator)
	sc := Scope{Role: RoleCoding}
	if _, err := s.Commit(context.Background(), sc, "explorer", []Candidate{{Content: "build with make dev"}}, "report"); err != nil {
		t.Fatal(err)
	}
	got := s.Recall(context.Background(), sc, "how do I build this repo")
	if !strings.Contains(got, "<MEMORY>") || !strings.Contains(got, "make dev") {
		t.Fatalf("recall block wrong: %q", got)
	}
	if got := s.Recall(context.Background(), Scope{Role: "other"}, "anything"); got != "" {
		t.Fatalf("foreign-scope recall must be empty, got %q", got)
	}
	var nilStore *Store
	if got := nilStore.Recall(context.Background(), sc, "q"); got != "" {
		t.Fatal("nil store must recall nothing")
	}
}
