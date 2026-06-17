package tools

import (
	"strings"
	"testing"
)

func TestNewGetUserChoiceToolMetadata(t *testing.T) {
	tl, err := NewGetUserChoiceTool()
	if err != nil {
		t.Fatalf("NewGetUserChoiceTool error: %v", err)
	}
	if tl.Name() != ChoiceToolName {
		t.Errorf("Name() = %q, want %q", tl.Name(), ChoiceToolName)
	}
	// Must be long-running: that is what pauses the orchestrator's turn until the
	// user answers (session.IsFinalResponse keys off LongRunningToolIDs).
	if !tl.IsLongRunning() {
		t.Error("IsLongRunning() = false, want true (the clarification must pause the turn)")
	}
	if !strings.Contains(strings.ToLower(tl.Description()), "option") {
		t.Errorf("Description() = %q, want mention of options", tl.Description())
	}
}
