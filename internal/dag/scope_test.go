package dag

import (
	"strings"
	"testing"
)

// A node must be told that the verbatim user request is BACKGROUND, and that the rest
// of it belongs to its siblings.
//
// The live failure (code-mode dogfood, 2026-07-13): every node is handed the user's
// full request as context. The request spelled out three phases — research OpenHands,
// goose and quack; synthesize a design for quack; implement it. The `goose` explorer
// finished reading goose, then read the brief it had been given "for context", saw
// "PHASE 2 — SYNTHESIZE A PLAN FOR QUACK … quack is Go, its tools are in internal/tools",
// and went off and cloned quack — which was the concurrently-running `quack-repo` node's
// entire job. Two nodes, one job; the duplicate work is discarded.
//
// Nothing in the prompt marked the request as background, and nothing told the node its
// siblings existed. It was being helpful.
func TestBuildTaskMarksTheRequestAsBackgroundAndNamesTheSiblings(t *testing.T) {
	plan := Plan{
		UserMessage: "Research OpenHands, goose and quack. PHASE 2 — synthesize a plan for quack. PHASE 3 — implement it.",
		Nodes: []Node{
			{ID: "goose", AgentName: "code-explorer", Task: "Clone goose and read how it exposes tools."},
			{ID: "quack-repo", AgentName: "code-explorer", Task: "Clone quack and read internal/tools."},
			{ID: "implement", AgentName: "code-implementer", Task: "Implement it."},
		},
	}
	got := buildTask(plan, plan.Nodes[0], nil, nil)

	if !strings.Contains(got, "CONTEXT ONLY") {
		t.Error("the verbatim request is handed over unframed; a node reads the whole brief as its own to-do list")
	}
	// The siblings must be named, so the node knows the rest of the request is taken.
	for _, sib := range []string{"quack-repo", "implement"} {
		if !strings.Contains(got, sib) {
			t.Errorf("sibling %q is not named; the node cannot know that part of the request is already assigned", sib)
		}
	}
	if sibs := siblingIDs(plan, "goose"); strings.Contains(sibs, "goose") {
		t.Errorf("the node listed ITSELF as a sibling: %q", sibs)
	}
	// Its own task must still be unmistakably its own.
	if !strings.Contains(got, "ONLY this") || !strings.Contains(got, "Clone goose") {
		t.Error("the node's own task is no longer clearly delimited as the thing to do")
	}
}

// A lone node has no siblings to warn about — it must not be told to avoid work that
// nobody else is doing.
func TestBuildTaskSingleNodeHasNoSiblingWarning(t *testing.T) {
	plan := Plan{
		UserMessage: "Add a feature and open a PR.",
		Nodes:       []Node{{ID: "solo", AgentName: "code-implementer", Task: "Do the whole thing."}},
	}
	got := buildTask(plan, plan.Nodes[0], nil, nil)
	if strings.Contains(got, "ALREADY ASSIGNED") {
		t.Error("a lone node was warned off work that no sibling is doing — it may now refuse part of its own task")
	}
	if !strings.Contains(got, "Do the whole thing.") {
		t.Error("the lone node lost its task")
	}
}
