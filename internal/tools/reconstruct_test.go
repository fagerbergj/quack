package tools

import (
	"encoding/json"
	"testing"

	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

func TestReconstructPlan(t *testing.T) {
	wire := stream.DagPlanData{
		PlanID: "p1",
		Nodes:  []stream.DagNodeDef{{ID: "n1", Agent: "web-researcher", Task: "do it", DependsOn: []string{"n0"}}},
	}
	js, _ := json.Marshal(wire)
	tc := &store.TurnContent{UserText: "hi", Plan: &store.DagPlan{ID: "p1", PlanJSON: string(js)}}

	// Valid: node present → executable plan with user text carried through.
	plan, err := reconstructPlan("p1", "n1", tc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Nodes) != 1 || plan.Nodes[0].AgentName != "web-researcher" || plan.UserMessage != "hi" {
		t.Errorf("reconstructed plan = %+v, want one web-researcher node + user text", plan)
	}
	if len(plan.Nodes[0].DependsOn) != 1 || plan.Nodes[0].DependsOn[0] != "n0" {
		t.Errorf("deps not carried through: %+v", plan.Nodes[0])
	}

	// Node not in the stored plan → error (rather than a silent no-op resume).
	if _, err := reconstructPlan("p1", "nX", tc); err == nil {
		t.Error("want an error when the resumed node is absent from the stored plan")
	}

	// Corrupt plan JSON → error.
	bad := &store.TurnContent{Plan: &store.DagPlan{ID: "p1", PlanJSON: "{not json"}}
	if _, err := reconstructPlan("p1", "n1", bad); err == nil {
		t.Error("want an error when the stored plan JSON can't be parsed")
	}
}
