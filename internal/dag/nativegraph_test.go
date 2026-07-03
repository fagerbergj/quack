package dag

import (
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/workflow"
)

// dummyNode builds a no-op first-class node so buildPlanGraph has something to wire.
func dummyNode(name string) workflow.Node {
	return workflow.NewFunctionNode[any, any](name,
		func(adkagent.Context, any) (any, error) { return name, nil }, workflow.NodeConfig{})
}

func nodesFor(plan Plan) map[string]workflow.Node {
	m := map[string]workflow.Node{}
	for _, n := range plan.Nodes {
		m[n.ID] = dummyNode(n.ID)
	}
	return m
}

// edgeSet renders edges as "from->to" strings for order-independent comparison.
func edgeSet(edges []workflow.Edge) map[string]bool {
	s := map[string]bool{}
	for _, e := range edges {
		s[e.From.Name()+"->"+e.To.Name()] = true
	}
	return s
}

func TestBuildPlanGraph(t *testing.T) {
	t.Run("fan-out researchers, synthesizer excluded", func(t *testing.T) {
		plan := Plan{Nodes: []Node{
			{ID: "r1", AgentName: "web-researcher"},
			{ID: "r2", AgentName: "web-researcher"},
			{ID: "s", AgentName: "synthesizer", DependsOn: []string{"r1", "r2"}},
		}}
		edges, ids, err := buildPlanGraph(plan, nodesFor(plan))
		if err != nil {
			t.Fatal(err)
		}
		if got := edgeSet(edges); !got["START->r1"] || !got["START->r2"] || len(got) != 2 {
			t.Errorf("fan-out edges = %v, want START->r1, START->r2 only", got)
		}
		if strings.Join(ids, ",") != "r1,r2" {
			t.Errorf("worker ids = %v, want [r1 r2] (synth excluded)", ids)
		}
	})

	t.Run("single-dep chain", func(t *testing.T) {
		plan := Plan{Nodes: []Node{
			{ID: "a", AgentName: "web-researcher"},
			{ID: "b", AgentName: "web-researcher", DependsOn: []string{"a"}},
			{ID: "s", AgentName: "synthesizer", DependsOn: []string{"a", "b"}},
		}}
		edges, _, err := buildPlanGraph(plan, nodesFor(plan))
		if err != nil {
			t.Fatal(err)
		}
		got := edgeSet(edges)
		if !got["START->a"] || !got["a->b"] || len(got) != 2 {
			t.Errorf("chain edges = %v, want START->a, a->b", got)
		}
	})

	t.Run("mid-DAG fan-in is rejected", func(t *testing.T) {
		plan := Plan{Nodes: []Node{
			{ID: "a", AgentName: "web-researcher"},
			{ID: "b", AgentName: "web-researcher"},
			{ID: "c", AgentName: "web-researcher", DependsOn: []string{"a", "b"}}, // non-synth fan-in
			{ID: "s", AgentName: "synthesizer", DependsOn: []string{"a", "b", "c"}},
		}}
		_, _, err := buildPlanGraph(plan, nodesFor(plan))
		if err == nil || !strings.Contains(err.Error(), "fan-in") {
			t.Fatalf("want mid-DAG fan-in error, got %v", err)
		}
	})

	t.Run("dependency on synthesizer is ignored (treated as leaf)", func(t *testing.T) {
		// Defensive: the synthesizer is terminal so this shouldn't happen, but a
		// worker whose only dep is the synth must not be starved — it fans from Start.
		plan := Plan{Nodes: []Node{
			{ID: "a", AgentName: "web-researcher", DependsOn: []string{"s"}},
			{ID: "s", AgentName: "synthesizer", DependsOn: []string{"a"}},
		}}
		edges, ids, err := buildPlanGraph(plan, nodesFor(plan))
		if err != nil {
			t.Fatal(err)
		}
		if got := edgeSet(edges); !got["START->a"] || len(got) != 1 {
			t.Errorf("edges = %v, want START->a", got)
		}
		if strings.Join(ids, ",") != "a" {
			t.Errorf("ids = %v, want [a]", ids)
		}
	})
}
