package serve

import (
	"os"
	"strings"
	"testing"
)

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
