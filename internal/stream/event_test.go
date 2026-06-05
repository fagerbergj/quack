package stream

import (
	"testing"

	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

func eventWith(parts ...*genai.Part) *session.Event {
	e := &session.Event{}
	e.Content = &genai.Content{Parts: parts}
	return e
}

func TestTranslateLabelsPartsByKind(t *testing.T) {
	e := eventWith(
		&genai.Part{Text: "reasoning", Thought: true},
		&genai.Part{FunctionCall: &genai.FunctionCall{Name: "current_time", Args: map[string]any{}}},
		&genai.Part{FunctionResponse: &genai.FunctionResponse{Name: "current_time", Response: map[string]any{"result": "now"}}},
		&genai.Part{Text: "the answer"},
	)
	got := Translate(e)
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(got), got)
	}
	want := []string{EventThinking, EventToolCall, EventToolResult, EventToken}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("event[%d] = %q, want %q", i, got[i].Name, w)
		}
	}
	if td, ok := got[0].Data.(ThinkingData); !ok || td.Text != "reasoning" {
		t.Errorf("thinking data = %+v", got[0].Data)
	}
	if tc, ok := got[1].Data.(ToolCallData); !ok || tc.Name != "current_time" {
		t.Errorf("tool_call data = %+v", got[1].Data)
	}
}

func TestTranslateNilSafe(t *testing.T) {
	if got := Translate(nil); got != nil {
		t.Errorf("Translate(nil) = %+v, want nil", got)
	}
	if got := Translate(&session.Event{}); got != nil {
		t.Errorf("Translate(no content) = %+v, want nil", got)
	}
}

func TestTranslateSingleFinalText(t *testing.T) {
	// The common adk-go-openai case: one aggregated text part.
	got := Translate(eventWith(&genai.Part{Text: "hello"}))
	if len(got) != 1 || got[0].Name != EventToken {
		t.Fatalf("got %+v, want one token event", got)
	}
}
