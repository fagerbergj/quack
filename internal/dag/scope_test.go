package dag

import (
	"context"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

// A node is told the verbatim user request is background and that the rest belongs to its
// siblings, so it doesn't do a sibling's job.
func TestBuildTaskMarksTheRequestAsBackgroundAndNamesTheSiblings(t *testing.T) {
	plan := Plan{
		UserMessage: "Research OpenHands, goose and quack. PHASE 2 - synthesize a plan for quack. PHASE 3 - implement it.",
		Nodes: []Node{
			{ID: "goose", AgentName: "code-explorer", Task: "Clone goose and read how it exposes tools."},
			{ID: "quack-repo", AgentName: "code-explorer", Task: "Clone quack and read internal/tools."},
			{ID: "implement", AgentName: "code-implementer", Task: "Implement it."},
		},
	}
	got := buildTask(context.Background(), plan, plan.Nodes[0], nil, nil, vetting.Config{})

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
	got := buildTask(context.Background(), plan, plan.Nodes[0], nil, nil, vetting.Config{})
	if strings.Contains(got, "ALREADY ASSIGNED") {
		t.Error("a lone node was warned off work that no sibling is doing - it may now refuse part of its own task")
	}
	if !strings.Contains(got, "Do the whole thing.") {
		t.Error("the lone node lost its task")
	}
}

// A GitHub run's scoped ask (WorkerBackground) is the node's BACKGROUND, not the
// orchestrator's full envelope.
func TestBuildTaskWorkerBackgroundOverridesUserMessage(t *testing.T) {
	plan := Plan{
		UserMessage:      "<changed_files count=\"40\" additions=\"900\" deletions=\"200\">[... 40 files ...]</changed_files>",
		WorkerBackground: "<permissions>push_commits_to_pr</permissions>\n<deliverable>a commit</deliverable>",
		Nodes:            []Node{{ID: "solo", AgentName: "code-implementer", Task: "Fix the failing build check."}},
	}
	got := buildTask(context.Background(), plan, plan.Nodes[0], nil, nil, vetting.Config{})
	if strings.Contains(got, "changed_files count") {
		t.Errorf("node background leaked the orchestrator's evidence instead of using WorkerBackground:\n%s", got)
	}
	if !strings.Contains(got, "push_commits_to_pr") {
		t.Errorf("node background missing WorkerBackground's content:\n%s", got)
	}
}

// Without WorkerBackground the node's BACKGROUND falls back to UserMessage.
func TestBuildTaskWorkerBackgroundFallsBackToUserMessage(t *testing.T) {
	plan := Plan{
		UserMessage: "Add a feature and open a PR.",
		Nodes:       []Node{{ID: "solo", AgentName: "code-implementer", Task: "Do the whole thing."}},
	}
	got := buildTask(context.Background(), plan, plan.Nodes[0], nil, nil, vetting.Config{})
	if !strings.Contains(got, "Add a feature and open a PR.") {
		t.Errorf("an empty WorkerBackground should fall back to UserMessage:\n%s", got)
	}
}

// A fix node's prompt carries only the annotation detail for the check its own task names.
func TestBuildTaskContextItemsScopedToTheNodeThatNamesThem(t *testing.T) {
	plan := Plan{
		ContextItems: []ContextItem{
			{Name: "build", Detail: "internal/foo.go:12 [failure] undefined: Bar"},
			{Name: "lint", Detail: "internal/baz.go:3 [warning] unused import"},
			{Name: "test", Detail: "internal/qux_test.go:9 [failure] TestQux failed"},
		},
		Nodes: []Node{
			{ID: "fix-build", AgentName: "code-implementer", Task: "Fix the failing `build` check."},
			{ID: "fix-lint", AgentName: "code-implementer", Task: "Fix the failing `lint` check.", DependsOn: nil},
		},
	}
	got := buildTask(context.Background(), plan, plan.Nodes[0], nil, nil, vetting.Config{})
	if !strings.Contains(got, "undefined: Bar") {
		t.Errorf("fix-build node prompt missing its OWN check's annotation detail:\n%s", got)
	}
	if strings.Contains(got, "unused import") || strings.Contains(got, "TestQux failed") {
		t.Errorf("fix-build node prompt leaked another check's annotation detail:\n%s", got)
	}
}

// Upstream answers rendered for the judge keep the gate-failed/no-answer warnings.
func TestRenderUpstreamForJudgeCarriesGateFailedAndNoAnswerMarkers(t *testing.T) {
	upstream := map[string]string{"explore": "the bug is in graph.go:262"}
	got := renderUpstreamForJudge(upstream, []string{"explore", "missing"}, map[string]bool{"explore": true})
	if !strings.Contains(got, "FAILED independent quality vetting") {
		t.Errorf("judge upstream section missing the gate-failed warning for a flagged dependency:\n%s", got)
	}
	if !strings.Contains(got, "produced NO answer") {
		t.Errorf("judge upstream section missing the no-answer note for a dependency with no output:\n%s", got)
	}
}
