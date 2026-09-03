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
// revise rounds per node for nothing. The prompt must tell a slice-scoped
// reviewer its verdict is scoped to the slice, not the whole PR.
func TestCodeReviewerPromptScopesSliceVerdict(t *testing.T) {
	b, err := os.ReadFile("../../agents/code-reviewer/prompt.md")
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	body := string(b)
	if !strings.Contains(body, "YOUR SLICE") {
		t.Fatal("prompt.md should reference the plan-work slice task phrasing")
	}
	if !strings.Contains(body, "your verdict is scoped") && !strings.Contains(body, "verdict still follows the same one rule, scoped to your slice") {
		t.Fatal("prompt.md must tell a slice-scoped reviewer its verdict covers only its slice, not the whole PR - otherwise it reasons it can't approve and defaults to `comment`")
	}
}

// TestPlanWorkSkillTellsSliceVerdictScope guards the same bug at the planning
// side: the fanned-out reviewer task template must say the verdict is
// slice-scoped, or every node authored from it inherits the same
// comment-over-nits contradiction.
func TestPlanWorkSkillTellsSliceVerdictScope(t *testing.T) {
	b, err := os.ReadFile("../../skills/plan-work/SKILL.md")
	if err != nil {
		t.Fatalf("read plan-work/SKILL.md: %v", err)
	}
	body := string(b)
	if !strings.Contains(body, "Your verdict covers ONLY your slice") {
		t.Fatal("plan-work's fanned reviewer task template must tell the node its verdict is scoped to its slice, not the whole PR")
	}
}
