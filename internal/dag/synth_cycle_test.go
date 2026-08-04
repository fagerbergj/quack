package dag

import "testing"

// A synthesizer is NOT always the terminal fan-in. research → synthesize → implement
// is a perfectly good plan: the implementer depends ON the synthesizer.
//
// The hardening pass used to give every synthesizer an edge to EVERY other node -
// including its own descendants - which manufactures a cycle. quack then rejected the
// plan with "dag plan contains a cycle", blaming the orchestrator for a correct plan
// that quack itself had just corrupted.
//
// Live failure (code-mode dogfood): 5 code-explorer nodes → synthesize-design →
// implement-code-mode. The orchestrator produced exactly this, six times, and quack
// rejected it every time.
func TestSynthesizerHardeningDoesNotCreateACycle(t *testing.T) {
	agents := []AgentInfo{
		{Name: "code-explorer"},
		{Name: "synthesizer"},
		{Name: "code-implementer"},
	}
	raw := []RawNode{
		{ID: "explorer-openhands", Agent: "code-explorer", Task: "read OpenHands"},
		{ID: "explorer-goose", Agent: "code-explorer", Task: "read goose"},
		{ID: "explorer-quack", Agent: "code-explorer", Task: "read quack"},
		{ID: "synthesize-design", Agent: "synthesizer", Task: "synthesize a design",
			DependsOn: []string{"explorer-openhands", "explorer-goose", "explorer-quack"}},
		{ID: "implement-code-mode", Agent: "code-implementer", Task: "implement it",
			DependsOn: []string{"synthesize-design"}},
	}

	plan, err := assemble(raw, agents, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("assemble rejected a valid research → synthesize → implement plan: %v", err)
	}

	// The synthesizer must depend on the research nodes...
	var synth Node
	for _, n := range plan.Nodes {
		if n.ID == "synthesize-design" {
			synth = n
		}
	}
	if len(synth.DependsOn) != 3 {
		t.Fatalf("synthesizer depends on %v; want the 3 explorers", synth.DependsOn)
	}
	// ...and must NOT depend on the implementer, which depends on IT.
	for _, d := range synth.DependsOn {
		if d == "implement-code-mode" {
			t.Fatal("the synthesizer was given an edge to its own descendant (implement-code-mode) - that is a cycle by construction")
		}
	}

	layers, err := topoLayers(*plan)
	if err != nil {
		t.Fatalf("topoLayers: %v", err)
	}
	if len(layers) != 3 {
		t.Fatalf("got %d layers, want 3 (explorers → synthesizer → implementer)", len(layers))
	}
	if len(layers[0]) != 3 || layers[1][0] != "synthesize-design" || layers[2][0] != "implement-code-mode" {
		t.Fatalf("unexpected layering: %v", layers)
	}
}

// The original hardening still holds: a synthesizer that is the terminal fan-in gets
// every research node as a dependency even when the orchestrator omitted some.
func TestSynthesizerHardeningStillFillsMissingPredecessors(t *testing.T) {
	agents := []AgentInfo{{Name: "web-researcher"}, {Name: "synthesizer"}}
	raw := []RawNode{
		{ID: "r1", Agent: "web-researcher", Task: "a"},
		{ID: "r2", Agent: "web-researcher", Task: "b"},
		{ID: "sum", Agent: "synthesizer", Task: "combine", DependsOn: []string{"r1"}}, // r2 omitted
	}
	plan, err := assemble(raw, agents, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	for _, n := range plan.Nodes {
		if n.ID != "sum" {
			continue
		}
		if len(n.DependsOn) != 2 {
			t.Fatalf("synthesizer depends on %v; want both r1 and r2 (the omitted predecessor must be filled in)", n.DependsOn)
		}
	}
}
