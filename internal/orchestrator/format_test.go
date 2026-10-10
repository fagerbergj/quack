package orchestrator

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/dag"
)

// A lone non-synthesizer node without delivery would ship raw output, so it needs the pass.
func TestNeedsFormatPass_LoneNonSynthesizerNoDelivery(t *testing.T) {
	plan := dag.Plan{Nodes: []dag.Node{{ID: "a", AgentName: "code-explorer"}}}
	if !needsFormatPass(plan, "a short unstructured answer with no headings or lists") {
		t.Error("a lone non-synthesizer node with no delivery must need a format pass")
	}
}

// TestNeedsFormatPass_TerminalSynthesizerSkipped: a plan whose terminal node
// IS a synthesizer already produced a formatted deliverable - no double pass.
func TestNeedsFormatPass_TerminalSynthesizerSkipped(t *testing.T) {
	plan := dag.Plan{Nodes: []dag.Node{
		{ID: "a", AgentName: "web-researcher"},
		{ID: "b", AgentName: "web-researcher"},
		{ID: "combine", AgentName: "synthesizer", DependsOn: []string{"a", "b"}},
	}}
	if needsFormatPass(plan, "a short unstructured answer") {
		t.Error("a plan already ending in a synthesizer must not get a second format pass")
	}
}

// A pull_request/review delivery ships via commitDelivery; chat text isn't the deliverable.
func TestNeedsFormatPass_GitHubDeliverySkipped(t *testing.T) {
	plan := dag.Plan{
		Nodes:    []dag.Node{{ID: "impl", AgentName: "code-implementer"}},
		Delivery: &dag.Delivery{Kind: "pull_request"},
	}
	if needsFormatPass(plan, "a short unstructured answer") {
		t.Error("a plan with a declared GitHub delivery must not get a format pass")
	}
}

// TestNeedsFormatPass_EmptyPlan: no nodes ⇒ no terminal ⇒ nothing to format.
func TestNeedsFormatPass_EmptyPlan(t *testing.T) {
	if needsFormatPass(dag.Plan{}, "anything") {
		t.Error("an empty plan must not need a format pass")
	}
}

// eligiblePlan: a lone non-synthesizer node with no delivery - the shape
// that reaches needsFormatPass's structure/length check at all.
func eligiblePlan() dag.Plan {
	return dag.Plan{Nodes: []dag.Node{{ID: "a", AgentName: "code-explorer"}}}
}

// A short, already-structured answer skips the near-identity format pass.
func TestNeedsFormatPass_StructuredShortAnswerSkipsPass(t *testing.T) {
	tests := []struct {
		name   string
		answer string
	}{
		{"markdown heading", "# Plan\n\nDo the thing."},
		{"heading further down", "Some intro.\n\n## Steps\n\n1. First\n2. Second"},
		{"two-plus bullet items", "- First finding\n- Second finding\n- Third finding"},
		{"two-plus numbered items", "1. First step\n2. Second step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if needsFormatPass(eligiblePlan(), tt.answer) {
				t.Errorf("needsFormatPass(%q) = true, want false (already structured and short)", tt.answer)
			}
		})
	}
}

// An unstructured answer needs the pass at any length; a long one needs it even with structure.
func TestNeedsFormatPass_UnstructuredLongAnswerNeedsPass(t *testing.T) {
	longUnstructured := strings.Repeat("word ", formatPassLengthCeiling/4)
	if !needsFormatPass(eligiblePlan(), longUnstructured) {
		t.Error("a long unstructured answer must still get a format pass")
	}

	longStructured := "# Heading\n\n" + strings.Repeat("word ", formatPassLengthCeiling/4)
	if len(longStructured) < formatPassLengthCeiling {
		t.Fatalf("test fixture too short: %d bytes", len(longStructured))
	}
	if !needsFormatPass(eligiblePlan(), longStructured) {
		t.Error("a structured answer at or above the length ceiling must still get a format pass")
	}
}

// TestNeedsFormatPass_SingleListItemNeedsPass: one list item isn't "structure" yet.
func TestNeedsFormatPass_SingleListItemNeedsPass(t *testing.T) {
	if !needsFormatPass(eligiblePlan(), "- just one item, no real structure") {
		t.Error("a single list item must not count as already structured")
	}
}

// TestFormatAnswer_ReturnsModelOutput: the happy path - the formatted text
// comes back from the tool-less writer.
func TestFormatAnswer_ReturnsModelOutput(t *testing.T) {
	stub := replyModel("# Plan\n\n1. Do the thing.")
	got := formatAnswer(context.Background(), stub, "plan the thing", "raw exploration notes", "chat-1")
	if got != "# Plan\n\n1. Do the thing." {
		t.Errorf("formatAnswer = %q, want the model's formatted text", got)
	}
}

// TestFormatAnswer_FailsOpenOnModelError: a broken format pass must never
// block delivery - it falls back to the raw answer unchanged.
func TestFormatAnswer_FailsOpenOnModelError(t *testing.T) {
	stub := funcModel(func(context.Context, *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) { yield(nil, fmt.Errorf("model unavailable")) }
	})
	got := formatAnswer(context.Background(), stub, "plan the thing", "raw exploration notes", "chat-1")
	if got != "raw exploration notes" {
		t.Errorf("formatAnswer = %q, want the raw answer unchanged on model error", got)
	}
}

// TestFormatAnswer_NilModelReturnsRaw: no model configured ⇒ no pass attempted.
func TestFormatAnswer_NilModelReturnsRaw(t *testing.T) {
	got := formatAnswer(context.Background(), nil, "plan the thing", "raw exploration notes", "chat-1")
	if got != "raw exploration notes" {
		t.Errorf("formatAnswer = %q, want the raw answer unchanged with a nil model", got)
	}
}

// TestFormatAnswer_EmptyAnswerShortCircuits: nothing to format.
func TestFormatAnswer_EmptyAnswerShortCircuits(t *testing.T) {
	stub := replyModel("should never be seen")
	if got := formatAnswer(context.Background(), stub, "plan the thing", "  ", "chat-1"); got != "" {
		t.Errorf("formatAnswer = %q, want empty for an empty raw answer", got)
	}
}
