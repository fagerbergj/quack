package serve

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestAgentBundlesLoad guards that every agent bundle referenced by the shipped
// config (plus the orchestrator) loads — a malformed agent-card.json or missing
// prompt.md fails here instead of at startup.
func TestAgentBundlesLoad(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"}, {"QUACK_DATABASE_URL", "postgres://localhost/db"},
		{"QUACK_ORCH_MODEL", "m"}, {"QUACK_RESEARCHER_MODEL", "r"}, {"QUACK_MEDIA_MODEL", "md"}, {"QUACK_IMAGE_MODEL", "im"},
		{"QUACK_JUDGE_MODEL", "j"}, {"QUACK_EMBED_MODEL", "e"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c, err := config.Load("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	bundles := []string{"agents/orchestrator"}
	for _, a := range c.Agents {
		bundles = append(bundles, a.Bundle)
	}
	for _, b := range bundles {
		if _, err := agent.LoadBundle("../../" + b); err != nil {
			t.Errorf("bundle %q failed to load: %v", b, err)
		}
	}
}

// TestCodeImplementerBundle pins the new bundle's specifics beyond the generic
// sweep above: the card's name matches its config key (buildAgents keys gate
// configs by that name), and its rubric.md override loads non-empty — the
// exact path buildAgents takes (vetting.LoadBundleRubric) to replace the
// default config/rubric.md with the code-quality one for this agent.
func TestCodeImplementerBundle(t *testing.T) {
	b, err := agent.LoadBundle("../../agents/code-implementer")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if b.Card.Name != "code-implementer" {
		t.Errorf("card name = %q, want %q", b.Card.Name, "code-implementer")
	}
	rubric, err := vetting.LoadBundleRubric("../../agents/code-implementer")
	if err != nil {
		t.Fatalf("LoadBundleRubric: %v", err)
	}
	if rubric == "" {
		t.Fatal("rubric override is empty — buildAgents would silently fall back to the default rubric")
	}
	// Spot-check the rubric carries all three parts of its contract: the
	// research criteria, the first-class ponytail section, and the
	// claims-vs-ledger fabrication criterion (live e2e 2026-07-10).
	for _, marker := range []string{"checks_pass", "yagni_speculative_generality", "diff_minimality", "deletion_over_addition", "native_first", "weakest-link",
		"claims_match_activity", "Workspace activity", "ledger"} {
		if !strings.Contains(rubric, marker) {
			t.Errorf("rubric missing expected marker %q", marker)
		}
	}
	// The prompt carries the anti-fabrication hard rule that pairs with the
	// judge's claims_match_activity criterion (ACP phrasing: the gate reads the
	// clone, so claims are checked against git itself).
	for _, marker := range []string{"Report only what actually happened", "You commit locally. You never push"} {
		if !strings.Contains(b.Prompt, marker) {
			t.Errorf("prompt missing expected hard-rule marker %q", marker)
		}
	}
}
