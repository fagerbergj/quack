package serve

import (
	"os"
	"strings"
	"testing"
)

// TestCodeReviewerPromptScopesSliceVerdict guards a live bug (PR #1102): a
// slice-fanned code-reviewer reasoned it couldn't approve a PR it hadn't
// reviewed in full, staged `comment` over an otherwise clean slice, and the
// structured_verdict rubric criterion failed it every round (a `comment`
// verdict over non-blocking findings is a self-contradiction), burning three
// revise rounds per node for nothing. #1092 fixes this class of bug at the
// root instead: a slice never stages a verdict at all (a downstream
// synthesizer owns it, and structured_verdict is dropped from a slice's own
// scoring), so the prompt must say so explicitly.
func TestCodeReviewerPromptScopesSliceVerdict(t *testing.T) {
	b, err := os.ReadFile("../../agents/code-reviewer/prompt.md")
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	body := string(b)
	if !strings.Contains(body, "YOUR SLICE") {
		t.Fatal("prompt.md should reference the plan-work slice task phrasing")
	}
	if !strings.Contains(body, "do NOT call `stage_review`") && !strings.Contains(body, "do not stage an overall verdict") {
		t.Fatal("prompt.md must tell a slice-scoped reviewer to stage findings only, never a verdict - a downstream synthesizer owns it")
	}
}

// TestPlanWorkSkillTellsSliceVerdictScope guards the same bug at the planning
// side: the fanned-out reviewer task template must say a slice stages
// findings only, and that a terminal synthesizer node owns the PR's one
// verdict - or every node authored from it inherits the same
// comment-over-nits contradiction.
func TestPlanWorkSkillTellsSliceVerdictScope(t *testing.T) {
	b, err := os.ReadFile("../../skills/plan-work/SKILL.md")
	if err != nil {
		t.Fatalf("read plan-work/SKILL.md: %v", err)
	}
	body := string(b)
	if !strings.Contains(body, "Do not stage an overall verdict") {
		t.Fatal("plan-work's fanned reviewer task template must tell the node not to stage a verdict itself")
	}
	if !strings.Contains(body, "synthesizer") {
		t.Fatal("plan-work must route a fanned review through a terminal synthesizer node that owns the PR's one verdict")
	}
}
