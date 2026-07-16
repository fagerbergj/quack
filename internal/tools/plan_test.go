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
