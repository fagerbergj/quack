package serve

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// TestReviewCodeSkillPreloadedIntoPreamble proves code-reviewer's review-code
// skill rides the static ACP preamble (cached across rounds) instead of a
// per-round load_skill tool result (never cacheable).
func TestReviewCodeSkillPreloadedIntoPreamble(t *testing.T) {
	bundle, err := agent.LoadBundle("../../agents/code-reviewer")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if len(bundle.Card.PreloadSkills) == 0 {
		t.Fatal("code-reviewer agent-card.json must declare preloadSkills")
	}

	ctx := context.Background()
	src := newSkillSource(nil)
	skillFms, err := src.ListFrontmatters(ctx)
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}
	body, err := src.LoadInstructions(ctx, "review-code")
	if err != nil {
		t.Fatalf("LoadInstructions(review-code): %v", err)
	}
	const marker = "Do not write a single finding until step 2"
	if !strings.Contains(body, marker) {
		t.Fatalf("review-code skill body missing its known text - fixture assumption broke")
	}

	// "Before": the roster-only shape (name + description, load_skill hint)
	// every skill got prior to this fix - the 10KB body is never in the preamble.
	before := promptbuilder.Agent(bundle.Card.Name, bundle.Card.Description, nil, skillFms, nil, bundle.Prompt, "", "")
	if strings.Contains(before, marker) {
		t.Fatal("roster-only preamble should not already contain the skill body")
	}

	// "After": filter the roster entry (mirrors buildAgents) and inline the body.
	var filtered []*skill.Frontmatter
	for _, fm := range skillFms {
		if fm.Name != "review-code" {
			filtered = append(filtered, fm)
		}
	}
	preloaded := []promptbuilder.PreloadedSkill{{Name: "review-code", Body: body}}
	after := promptbuilder.Agent(bundle.Card.Name, bundle.Card.Description, nil, filtered, preloaded, bundle.Prompt, "", "")

	if !strings.Contains(after, marker) {
		t.Fatal("preloaded preamble must contain the review-code skill body verbatim")
	}
	if strings.Contains(after, "`review-code` -") {
		t.Error("a preloaded skill must not also appear as a load_skill roster bullet")
	}
	t.Logf("before=%d bytes (no body) after=%d bytes (body inlined, +%d bytes moved from a per-round tool result into the once-per-session preamble)",
		len(before), len(after), len(after)-len(before))
}
