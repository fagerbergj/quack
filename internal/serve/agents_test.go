package serve

import (
	"context"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestAgentBundlesLoad: a malformed agent-card.json or missing prompt.md in any shipped bundle
// fails here instead of at startup.
func TestAgentBundlesLoad(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"}, {"QUACK_DATABASE_URL", "postgres://localhost/db"},
		{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"}, {"QUACK_MEDIA_MODEL", "qwen3-omni-30b"}, {"QUACK_IMAGE_MODEL", "qwen3-vl-32b"},
		{"QUACK_JUDGE_MODEL", "gemma4-26b-a4b"}, {"QUACK_EMBED_MODEL", "qwen3-embed"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c, err := config.LoadDeferringAgentCompleteness("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	bundles := []string{"agents/orchestrator"}
	for _, a := range c.Agents {
		if a.Bundle == "" {
			continue // plugin-override entry; the github plugin's own tests cover its bundle load
		}
		bundles = append(bundles, a.Bundle)
	}
	for _, b := range bundles {
		if _, err := agent.LoadBundle(context.Background(), nil, "../../"+b); err != nil {
			t.Errorf("bundle %q failed to load: %v", b, err)
		}
	}
}

// TestCodeImplementerBundle: the card name matches its config key (buildAgents keys gate configs by it),
// and the rubric override loads non-empty via vetting.LoadBundleRubric.
func TestCodeImplementerBundle(t *testing.T) {
	b, err := agent.LoadBundle(context.Background(), nil, "../../.agents/plugins/github/agents/code-implementer")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if b.Card.Name != "code-implementer" {
		t.Errorf("card name = %q, want %q", b.Card.Name, "code-implementer")
	}
	rubric, err := vetting.LoadBundleRubric(context.Background(), nil, "../../.agents/plugins/github/agents/code-implementer")
	if err != nil {
		t.Fatalf("LoadBundleRubric: %v", err)
	}
	if rubric == "" {
		t.Fatal("rubric override is empty - buildAgents would silently fall back to the default rubric")
	}
	// "weakest-link" is judge-prompt content, not rubric content: judge.go states the aggregation once.
	for _, marker := range []string{"checks_pass", "complexity_proportionate", "module_shape", "coupling",
		"claims_match_activity", "Workspace activity", "ledger"} {
		if !strings.Contains(rubric, marker) {
			t.Errorf("rubric missing expected marker %q", marker)
		}
	}
	// The anti-fabrication rule pairs with the judge's claims_match_activity criterion
	// (ACP: the gate reads the clone, so claims are checked against git itself).
	for _, marker := range []string{"Report only what actually happened", "you never push or open the PR"} {
		if !strings.Contains(b.Prompt, marker) {
			t.Errorf("prompt missing expected hard-rule marker %q", marker)
		}
	}
}

// TestCodeReviewerBundlePrefersStaging: staging the review must be the imperative and the structured
// tail a conditional fallback, or the reviewer writes every finding twice.
func TestCodeReviewerBundlePrefersStaging(t *testing.T) {
	b, err := agent.LoadBundle(context.Background(), nil, "../../.agents/plugins/github/agents/code-reviewer")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	prompt := b.Prompt

	// Staging is commanded, not merely described.
	for _, marker := range []string{"Stage the review as you go", "call `stage_review_comment`"} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing staging imperative %q", marker)
		}
	}

	// The tail is gated on the tools being absent from the round's generated MCP list.
	if !strings.Contains(prompt, "has no `stage_review_comment`/`stage_review`") {
		t.Error("prompt does not condition the structured tail on the staging tools being unavailable")
	}
	if strings.Contains(prompt, "the two never conflict") {
		t.Error("prompt still invites writing both the staged review and the tail")
	}

	// The summary must not restate findings already staged inline, and must
	// not open with process narration.
	if !strings.Contains(prompt, "restatement of findings already staged inline") {
		t.Error("prompt does not forbid restating staged findings in the summary")
	}
	for _, narration := range []string{"Now I have a complete picture", "Let me compile my findings"} {
		if !strings.Contains(prompt, narration) {
			t.Errorf("prompt does not name and forbid the process-narration lead %q", narration)
		}
	}
}
