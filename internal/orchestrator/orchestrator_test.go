package orchestrator

import (
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

// TestLastOutput verifies that lastOutput picks the terminal node's result.
func TestLastOutput(t *testing.T) {
	plan := &dag.Plan{
		Nodes: []dag.Node{
			{ID: "n1", AgentName: "web-researcher", DependsOn: nil},
			{ID: "n2", AgentName: "synthesizer", DependsOn: []string{"n1"}},
		},
	}
	outputs := map[string]string{"n1": "research result", "n2": "final answer"}
	got := lastOutput(plan, outputs)
	if got != "final answer" {
		t.Errorf("lastOutput = %q, want %q", got, "final answer")
	}
}

func TestLastOutputSingleNode(t *testing.T) {
	plan := &dag.Plan{
		Nodes: []dag.Node{
			{ID: "n1", AgentName: "web-researcher"},
		},
	}
	outputs := map[string]string{"n1": "only answer"}
	got := lastOutput(plan, outputs)
	if got != "only answer" {
		t.Errorf("lastOutput = %q, want %q", got, "only answer")
	}
}

// TestOrchestratorReturnType is a compile-time check that Run returns SSEEvent,
// not *session.Event. A later integration test in internal/agent covers the full
// A2A round trip; this package test only covers the orchestrator's own logic.
func TestOrchestratorReturnType(t *testing.T) {
	var orch *Orchestrator
	if orch != nil {
		// This line confirms the return type compiles correctly.
		for ev, err := range orch.Run(nil, "", "", "") { //nolint:all
			_ = ev.Name
			var _ string = ev.Name
			// stream.SSEEvent has a Name field of type string.
			switch ev.Data.(type) {
			case stream.AgentTokenData, stream.DagPlanData, stream.NodeStartData:
			}
			if err != nil {
				break
			}
		}
	}
	t.Log("return type is stream.SSEEvent")
}
