package openaimodel

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// TestToOpenAI_ModelTurnStaysAssistant: a model turn with a thought and an
// answer (no tool call) was re-sent as ONE user message, putting the model's own words in the user's mouth.
func TestToOpenAI_ModelTurnStaysAssistant(t *testing.T) {
	req := &model.LLMRequest{Contents: []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "question"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "private reasoning", Thought: true}, {Text: "the answer"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{Text: "more reasoning", Thought: true},
			{Text: "let me check"},
			{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "web_search", Args: map[string]any{"q": "x"}}},
		}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "only a thought", Thought: true}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "part one"}, {Text: "part two"}}},
	}}
	got, err := toOpenAIChatCompletionRequest(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (a thought-only turn sends nothing)", len(got.Messages))
	}
	if split := got.Messages[3].OfAssistant; split == nil || split.Content.OfString.Value != "part one\npart two" {
		t.Fatalf("multi-part model answer = %+v, want one assistant message with both parts", got.Messages[3])
	}
	answer := got.Messages[1].OfAssistant
	if answer == nil || answer.Content.OfString.Value != "the answer" {
		t.Fatalf("model answer turn = %+v, want an assistant message carrying only the answer", got.Messages[1])
	}
	call := got.Messages[2].OfAssistant
	if call == nil || len(call.ToolCalls) != 1 || call.Content.OfString.Value != "let me check" {
		t.Fatalf("tool-call turn = %+v, want assistant with its call and answer text, thought dropped", got.Messages[2])
	}
	for i, m := range got.Messages {
		if m.OfUser != nil && i > 0 {
			t.Errorf("message %d is user-role; only the first is the user's", i)
		}
	}
}

// TestDropThoughts_CopiesOnlyWhatChanges: the session's own contents are
// never mutated, and a request without thoughts is returned as-is.
func TestDropThoughts_CopiesOnlyWhatChanges(t *testing.T) {
	plain := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}}}
	if got := DropThoughts(plain); &got[0] != &plain[0] {
		t.Error("contents without thoughts should come back unchanged, not copied")
	}
	mixed := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "r", Thought: true}, {Text: "a"}}}
	got := DropThoughts([]*genai.Content{plain[0], mixed})
	if len(got) != 2 || len(got[1].Parts) != 1 || got[1].Parts[0].Text != "a" {
		t.Fatalf("DropThoughts = %+v, want the answer part only", got[1])
	}
	if len(mixed.Parts) != 2 || !strings.Contains(mixed.Parts[0].Text, "r") {
		t.Error("DropThoughts mutated the caller's content")
	}
}

// TestReasoningToolCalls_UniqueIDs: a recovered call's id must not repeat across
// turns, or a later result is mistaken for an earlier one keyed on the same id.
func TestReasoningToolCalls_UniqueIDs(t *testing.T) {
	leaked := `<tool_call>{"name":"web_fetch","arguments":{"urls":["u"]}}</tool_call>`
	a, _ := reasoningToolCalls(leaked)
	b, _ := reasoningToolCalls(leaked)
	if len(a) != 1 || len(b) != 1 || a[0].ID == b[0].ID {
		t.Fatalf("recovered ids %v and %v, want one call each with distinct ids", a, b)
	}
}

// TestToOpenAI_ToolMessageFollowsItsCall: dropping a thought must never orphan a tool
// message - it has to follow the assistant message carrying its tool_call.
func TestToOpenAI_ToolMessageFollowsItsCall(t *testing.T) {
	req := &model.LLMRequest{Contents: []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "q"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "thinking", Thought: true},
			{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "web_search", Args: map[string]any{"q": "x"}}}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "web_search", Response: map[string]any{"r": 1}}}}},
	}}
	got, err := toOpenAIChatCompletionRequest(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (thought dropped, FR becomes the tool message)", len(got.Messages))
	}
	assistant := got.Messages[1].OfAssistant
	if assistant == nil || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].OfFunction.ID != "c1" {
		t.Fatalf("message 1 = %+v, want the assistant carrying the c1 tool_call", got.Messages[1])
	}
	if got.Messages[2].OfTool == nil || got.Messages[2].OfTool.ToolCallID != "c1" {
		t.Fatalf("message 2 = %+v, want the c1 tool message directly after its call", got.Messages[2])
	}
}
