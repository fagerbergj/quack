package serve

import (
	"os"
	"strings"
	"testing"
)

// TestCodeReviewerPromptScopesSliceVerdict: a slice never stages a verdict (the downstream synthesizer
// owns it), so the prompt must say so - else a slice stages `comment` and fails structured_verdict.
func TestCodeReviewerPromptScopesSliceVerdict(t *testing.T) {
	b, err := os.ReadFile("../../.agents/plugins/github/agents/code-reviewer/prompt.md")
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

// TestPlanWorkSkillTellsSliceVerdictScope: the fanned-out reviewer task template must say a slice
// stages findings only and the terminal synthesizer owns the PR's one verdict.
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
