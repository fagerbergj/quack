package dag

import (
	"strings"
	"testing"
)

// A node must be told that the verbatim user request is BACKGROUND, and that
// the rest of it belongs to its siblings. Regression: an explorer read the
// full multi-phase request "for context," saw a later phase's instructions,
// and went off and did a sibling node's job. Nothing marked the request as
// background, and nothing told the node its siblings existed.
func TestBuildTaskMarksTheRequestAsBackgroundAndNamesTheSiblings(t *testing.T) {
	plan := Plan{
		UserMessage: "Research OpenHands, goose and quack. PHASE 2 - synthesize a plan for quack. PHASE 3 - implement it.",
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

// A lone node has no siblings to warn about - it must not be told to avoid work that
// nobody else is doing.
func TestBuildTaskSingleNodeHasNoSiblingWarning(t *testing.T) {
	plan := Plan{
		UserMessage: "Add a feature and open a PR.",
		Nodes:       []Node{{ID: "solo", AgentName: "code-implementer", Task: "Do the whole thing."}},
	}
	got := buildTask(plan, plan.Nodes[0], nil, nil)
	if strings.Contains(got, "ALREADY ASSIGNED") {
		t.Error("a lone node was warned off work that no sibling is doing - it may now refuse part of its own task")
	}
	if !strings.Contains(got, "Do the whole thing.") {
		t.Error("the lone node lost its task")
	}
}

// TestBuildTaskWorkerBackgroundOverridesUserMessage pins #664: a GitHub run's
// scoped ask (WorkerBackground) is what a node's BACKGROUND carries, not the
// orchestrator's own full envelope (UserMessage) - which for a GitHub run
// carries evidence (e.g. the full changed-files list) no node needs.
func TestBuildTaskWorkerBackgroundOverridesUserMessage(t *testing.T) {
	plan := Plan{
		UserMessage:      "<changed_files count=\"40\" additions=\"900\" deletions=\"200\">[... 40 files ...]</changed_files>",
		WorkerBackground: "<permissions>push_commits_to_pr</permissions>\n<deliverable>a commit</deliverable>",
		Nodes:            []Node{{ID: "solo", AgentName: "code-implementer", Task: "Fix the failing build check."}},
	}
	got := buildTask(plan, plan.Nodes[0], nil, nil)
	if strings.Contains(got, "changed_files count") {
		t.Errorf("node background leaked the orchestrator's evidence instead of using WorkerBackground:\n%s", got)
	}
	if !strings.Contains(got, "push_commits_to_pr") {
		t.Errorf("node background missing WorkerBackground's content:\n%s", got)
	}
}

// TestBuildTaskWorkerBackgroundFallsBackToUserMessage pins the degrade path:
// a Plan built without WorkerBackground (every non-GitHub caller, and every
// test that constructs a Plan directly) behaves exactly as before #664.
func TestBuildTaskWorkerBackgroundFallsBackToUserMessage(t *testing.T) {
	plan := Plan{
		UserMessage: "Add a feature and open a PR.",
		Nodes:       []Node{{ID: "solo", AgentName: "code-implementer", Task: "Do the whole thing."}},
	}
	got := buildTask(plan, plan.Nodes[0], nil, nil)
	if !strings.Contains(got, "Add a feature and open a PR.") {
		t.Errorf("an empty WorkerBackground should fall back to UserMessage:\n%s", got)
	}
}

// TestBuildTaskCIChecksScopedToTheNodeThatNamesThem pins #664's test case 2: a
// fix worker's node prompt carries the annotation detail for the check ITS OWN
// task names, and not the other failing checks' detail - a sibling fix node
// working a different check must never see this one's annotations.
func TestBuildTaskCIChecksScopedToTheNodeThatNamesThem(t *testing.T) {
	plan := Plan{
		CIChecks: []CICheck{
			{Name: "build", Detail: "internal/foo.go:12 [failure] undefined: Bar"},
			{Name: "lint", Detail: "internal/baz.go:3 [warning] unused import"},
			{Name: "test", Detail: "internal/qux_test.go:9 [failure] TestQux failed"},
		},
		Nodes: []Node{
			{ID: "fix-build", AgentName: "code-implementer", Task: "Fix the failing `build` check."},
			{ID: "fix-lint", AgentName: "code-implementer", Task: "Fix the failing `lint` check.", DependsOn: nil},
		},
	}
	got := buildTask(plan, plan.Nodes[0], nil, nil)
	if !strings.Contains(got, "undefined: Bar") {
		t.Errorf("fix-build node prompt missing its OWN check's annotation detail:\n%s", got)
	}
	if strings.Contains(got, "unused import") || strings.Contains(got, "TestQux failed") {
		t.Errorf("fix-build node prompt leaked another check's annotation detail:\n%s", got)
	}
}
