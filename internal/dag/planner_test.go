package dag

import (
	"slices"
	"testing"
)

func testPlanner() *Planner {
	return NewPlanner([]AgentInfo{
		{Name: "web-researcher"}, {Name: "synthesizer"},
	})
}

func TestBuildValidatesAndStamps(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build([]RawNode{
		{ID: "n1", Agent: "web-researcher", Task: "a"},
		{ID: "n2", Agent: "web-researcher", Task: "b"},
		{ID: "n3", Agent: "synthesizer", Task: "combine"},
	}, []HistoryTurn{{Role: "user", Text: "hi"}}, "do it", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.UserMessage != "do it" || len(plan.History) != 1 {
		t.Errorf("turn context not stamped: %+v", plan)
	}
	// Synthesizer hardening: n3 depends on all non-synth nodes even though we
	// supplied none.
	var synth Node
	for _, n := range plan.Nodes {
		if n.AgentName == "synthesizer" {
			synth = n
		}
	}
	if !slices.Equal(synth.DependsOn, []string{"n1", "n2"}) {
		t.Errorf("synthesizer depends_on = %v, want [n1 n2]", synth.DependsOn)
	}
}

func TestBuildRejectsBadPlans(t *testing.T) {
	p := testPlanner()
	cases := map[string][]RawNode{
		"empty":         {},
		"missing id":    {{Agent: "web-researcher", Task: "x"}},
		"unknown agent": {{ID: "n1", Agent: "nope", Task: "x"}},
		"duplicate id":  {{ID: "n1", Agent: "web-researcher"}, {ID: "n1", Agent: "web-researcher"}},
		"cycle":         {{ID: "n1", Agent: "web-researcher", DependsOn: []string{"n2"}}, {ID: "n2", Agent: "web-researcher", DependsOn: []string{"n1"}}},
	}
	for name, nodes := range cases {
		if _, err := p.Build(nodes, nil, "m", nil); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestBuildAppendsMissingSynthesizer: a multi-terminal plan with no synthesizer
// (the model forgot the fan-in) gets one appended depending on every node —
// otherwise the native graph build rejects the plan outright ("2 terminal
// nodes (want 1)") and the whole run fails. Regression: live e2e 2026-07-05.
func TestBuildAppendsMissingSynthesizer(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build([]RawNode{
		{ID: "a", Agent: "web-researcher", Task: "research A"},
		{ID: "b", Agent: "web-researcher", Task: "research B"},
	}, nil, "compare", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (auto-appended synthesizer)", len(plan.Nodes))
	}
	synth := plan.Nodes[2]
	if synth.AgentName != "synthesizer" || !slices.Equal(synth.DependsOn, []string{"a", "b"}) {
		t.Errorf("appended node = %+v, want synthesizer depending on [a b]", synth)
	}
	if got := terminalIDs(plan.Nodes); len(got) != 1 {
		t.Errorf("terminals = %v, want exactly one", got)
	}
}

// TestBuildNoSynthesizerAppendedForChain: a linear chain has one terminal —
// nothing to fan in, no synthesizer appended.
func TestBuildNoSynthesizerAppendedForChain(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build([]RawNode{
		{ID: "a", Agent: "web-researcher", Task: "research"},
		{ID: "b", Agent: "web-researcher", Task: "refine", DependsOn: []string{"a"}},
	}, nil, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2 (no synthesizer needed)", len(plan.Nodes))
	}
}
