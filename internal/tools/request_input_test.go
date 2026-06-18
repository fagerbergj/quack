package tools

import (
	"strings"
	"testing"
)

func TestRequestInputToolMetadata(t *testing.T) {
	tl, err := newRequestInput(Deps{})
	if err != nil {
		t.Fatalf("newRequestInput error: %v", err)
	}
	if tl.Name() != RequestInputToolName {
		t.Errorf("Name() = %q, want %q", tl.Name(), RequestInputToolName)
	}
	// Must be long-running: that is what pauses the node's turn until the user
	// answers (ADK populates LongRunningToolIDs; the gate detects it to suspend).
	if !tl.IsLongRunning() {
		t.Error("IsLongRunning() = false, want true (request_input must pause the node)")
	}
	if !strings.Contains(strings.ToLower(tl.Description()), "question") {
		t.Errorf("Description() = %q, want mention of a question", tl.Description())
	}
}

func TestRequestInputResolvesFromRegistry(t *testing.T) {
	got, err := Build([]string{RequestInputToolName}, Deps{})
	if err != nil {
		t.Fatalf("Build(%q) error: %v", RequestInputToolName, err)
	}
	if len(got) != 1 || got[0].Name() != RequestInputToolName {
		t.Fatalf("Build returned %v, want a single %q tool", got, RequestInputToolName)
	}
}
