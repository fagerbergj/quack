package tools

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

func TestNewExecuteToolMetadata(t *testing.T) {
	tl, err := NewExecuteTool(nil, NewPlanCache(), "user1")
	if err != nil {
		t.Fatalf("NewExecuteTool error: %v", err)
	}
	if tl.Name() != "execute" {
		t.Errorf("Name() = %q, want %q", tl.Name(), "execute")
	}
	if !strings.Contains(tl.Description(), "plan") {
		t.Errorf("Description() = %q, want mention of plan", tl.Description())
	}
}

func TestTerminalOutput(t *testing.T) {
	// Single node — returns its output.
	single := dag.Plan{
		Nodes: []dag.Node{{ID: "n1"}},
	}
	if got := terminalOutput(single, map[string]string{"n1": "answer"}); got != "answer" {
		t.Errorf("single node: got %q, want %q", got, "answer")
	}

	// Two nodes in sequence: n2 depends on n1, so n1 has a successor and n2 is terminal.
	seq := dag.Plan{
		Nodes: []dag.Node{
			{ID: "n1"},
			{ID: "n2", DependsOn: []string{"n1"}},
		},
	}
	if got := terminalOutput(seq, map[string]string{"n1": "intermediate", "n2": "final"}); got != "final" {
		t.Errorf("sequential: got %q, want %q", got, "final")
	}

	// Empty outputs — returns empty string (callers check for this).
	if got := terminalOutput(single, map[string]string{}); got != "" {
		t.Errorf("empty outputs: got %q, want empty", got)
	}
}
