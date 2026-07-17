package tools

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

func TestNewPlanToolMetadata(t *testing.T) {
	planner := dag.NewPlanner(nil, nil, nil)
	tl, err := NewPlanTool(planner, NewPlanCache(), nil, nil, "")
	if err != nil {
		t.Fatalf("NewPlanTool error: %v", err)
	}
	if tl.Name() != "plan" {
		t.Errorf("Name() = %q, want %q", tl.Name(), "plan")
	}
	if !strings.Contains(tl.Description(), "DAG") {
		t.Errorf("Description() = %q, want mention of DAG", tl.Description())
	}
	// Every plan must declare setup + delivery, and the model must never run
	// git/push/PR itself — see github-delivery-architecture.
	for _, want := range []string{"setup", "delivery", "you never run git, push, or open a PR yourself"} {
		if !strings.Contains(tl.Description(), want) {
			t.Errorf("Description() = %q, want it to mention %q", tl.Description(), want)
		}
	}
}

// summarizePlan is the summary the model sees back after calling plan — it
// must surface the declared setup/delivery so the model can catch its own
// mistake before calling execute.
func TestSummarizePlanIncludesSetupAndDelivery(t *testing.T) {
	plan := &dag.Plan{
		Nodes:    []dag.Node{{ID: "impl", AgentName: "code-implementer"}},
		Setup:    &dag.Setup{BaseRef: "main", WorkBranch: "feat/widget"},
		Delivery: &dag.Delivery{Kind: "pull_request"},
	}
	got := summarizePlan(plan)
	for _, want := range []string{"feat/widget", "pull_request"} {
		if !strings.Contains(got, want) {
			t.Errorf("summarizePlan = %q, want it to contain %q", got, want)
		}
	}
}

// TestPlanEdges verifies the wire edge list is derived from each node's
// DependsOn (From=dep, To=node), and a no-dependency node contributes nothing.
func TestPlanEdges(t *testing.T) {
	nodes := []dag.Node{
		{ID: "a"},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "c", DependsOn: []string{"a", "b"}},
	}
	got := planEdges(nodes)
	want := []stream.DagEdgeDef{
		{From: "a", To: "b"},
		{From: "a", To: "c"},
		{From: "b", To: "c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planEdges = %+v, want %+v", got, want)
	}
}
