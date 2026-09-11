package agent

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// The volatile artifact text arrives first, quack's own instruction last.
func TestHoistInstructionCallback_MovesOwnInstructionToFront(t *testing.T) {
	own := "You are quack's orchestrator. Route requests to specialists."
	cb := HoistInstructionCallback(func() string { return own })

	assembled := "You have a list of artifacts: [report-draft-7]\n\n" + own
	req := &model.LLMRequest{
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{
				Parts: []*genai.Part{{Text: assembled}},
			},
		},
	}
	if _, err := cb(nil, req); err != nil {
		t.Fatalf("callback error: %v", err)
	}
	got := req.Config.SystemInstruction.Parts[0].Text
	if !strings.HasPrefix(got, own) {
		t.Fatalf("assembled instruction does not start with the agent prompt: %q", got)
	}
	if !strings.Contains(got, "report-draft-7") {
		t.Fatalf("hoist dropped the volatile artifact text: %q", got)
	}
}

func TestHoistInstructionCallback_NoOpWhenAlreadyLeading(t *testing.T) {
	own := "quack's stable prompt"
	cb := HoistInstructionCallback(func() string { return own })

	req := &model.LLMRequest{
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{
				Parts: []*genai.Part{{Text: own + "\n\nlater static text"}},
			},
		},
	}
	if _, err := cb(nil, req); err != nil {
		t.Fatalf("callback error: %v", err)
	}
	want := own + "\n\nlater static text"
	if got := req.Config.SystemInstruction.Parts[0].Text; got != want {
		t.Fatalf("no-op case changed the text: got %q, want %q", got, want)
	}
}

func TestHoistInstructionCallback_NilConfigIsSafe(t *testing.T) {
	cb := HoistInstructionCallback(func() string { return "own prompt" })
	if _, err := cb(nil, &model.LLMRequest{}); err != nil {
		t.Fatalf("callback error on nil Config: %v", err)
	}
}
