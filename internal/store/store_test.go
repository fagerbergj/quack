package store

import (
	"slices"
	"testing"

	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

func userEvent(text string) *session.Event {
	ev := session.NewEvent("test")
	ev.Author = "user"
	ev.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: text}}}
	return ev
}

func asstEvent(parts ...*genai.Part) *session.Event {
	ev := session.NewEvent("test")
	ev.Author = "orchestrator"
	ev.Content = &genai.Content{Role: "model", Parts: parts}
	return ev
}

// answerEvent is how a resumed clarification answer is persisted: a user-authored
// event whose only part is a get_user_choice FunctionResponse carrying the choice.
func answerEvent(choice string) *session.Event {
	ev := session.NewEvent("test")
	ev.Author = "user"
	ev.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "get_user_choice", Response: map[string]any{"choice": choice}},
	}}}
	return ev
}

// TestGroupSessionEvents verifies per-turn bucketing, text/thinking split, and
// tool-call extraction with result pairing and transfer_to_agent exclusion.
func TestGroupSessionEvents(t *testing.T) {
	events := []*session.Event{
		userEvent("which Springfield?"),
		asstEvent(&genai.Part{Text: "deciding…", Thought: true}),
		asstEvent(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "get_user_choice", Args: map[string]any{"options": []any{"IL", "MO"}}}}),
		asstEvent(&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "get_user_choice", Response: map[string]any{"status": "pending"}}}),
		// transfer_to_agent is noise and must be dropped.
		asstEvent(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "t1", Name: transferTool, Args: map[string]any{}}}),
		// The answer arrives as a get_user_choice FunctionResponse on a user event.
		answerEvent("IL"),
		asstEvent(&genai.Part{Text: "Springfield, Illinois has…"}),
	}

	groups := groupSessionEvents(slices.Values(events))

	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}

	g0 := groups[0]
	if g0.userText != "which Springfield?" {
		t.Errorf("turn 0 userText = %q", g0.userText)
	}
	if g0.asstThink != "deciding…" {
		t.Errorf("turn 0 asstThink = %q", g0.asstThink)
	}
	if len(g0.toolCalls) != 1 {
		t.Fatalf("turn 0 toolCalls = %d, want 1 (transfer_to_agent excluded)", len(g0.toolCalls))
	}
	tc := g0.toolCalls[0]
	if tc.CallID != "c1" || tc.Name != "get_user_choice" {
		t.Errorf("toolCall = %+v", tc)
	}
	if tc.Result == nil || tc.Result["status"] != "pending" {
		t.Errorf("toolCall result not paired: %+v", tc.Result)
	}

	g1 := groups[1]
	if g1.userText != "IL" || g1.asstText != "Springfield, Illinois has…" {
		t.Errorf("turn 1 = %+v", g1)
	}
	if len(g1.toolCalls) != 0 {
		t.Errorf("turn 1 toolCalls = %d, want 0", len(g1.toolCalls))
	}
}
