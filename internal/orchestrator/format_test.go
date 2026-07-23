package orchestrator

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
)

// TestNeedsFormatPass_LoneNonSynthesizerNoDelivery: a single-node plan with no
// declared GitHub delivery is exactly the #430 case - its raw output would
// otherwise ship verbatim, so it needs the fallback format pass.
func TestNeedsFormatPass_LoneNonSynthesizerNoDelivery(t *testing.T) {
	plan := dag.Plan{Nodes: []dag.Node{{ID: "a", AgentName: "code-explorer"}}}
	if !needsFormatPass(plan) {
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
	if needsFormatPass(plan) {
		t.Error("a plan already ending in a synthesizer must not get a second format pass")
	}
}

// TestNeedsFormatPass_GitHubDeliverySkipped: a plan declaring a pull_request/
// review delivery ships its deliverable via commitDelivery (per-node, gate-
// owned) - this chat text is not the deliverable, so no format pass.
func TestNeedsFormatPass_GitHubDeliverySkipped(t *testing.T) {
	plan := dag.Plan{
		Nodes:    []dag.Node{{ID: "impl", AgentName: "code-implementer"}},
		Delivery: &dag.Delivery{Kind: "pull_request"},
	}
	if needsFormatPass(plan) {
		t.Error("a plan with a declared GitHub delivery must not get a format pass")
	}
}

// TestNeedsFormatPass_EmptyPlan: no nodes ⇒ no terminal ⇒ nothing to format.
func TestNeedsFormatPass_EmptyPlan(t *testing.T) {
	if needsFormatPass(dag.Plan{}) {
		t.Error("an empty plan must not need a format pass")
	}
}

// formatStub is a minimal model.LLM for formatAnswer: it always replies with a
// fixed text, or errors if configured to.
type formatStub struct {
	reply string
	err   error
}

func (*formatStub) Name() string { return "formatStub" }

func (s *formatStub) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if s.err != nil {
			yield(nil, s.err)
			return
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s.reply}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// TestFormatAnswer_ReturnsModelOutput: the happy path - the formatted text
// comes back from the tool-less writer.
func TestFormatAnswer_ReturnsModelOutput(t *testing.T) {
	stub := &formatStub{reply: "# Plan\n\n1. Do the thing."}
	got := formatAnswer(context.Background(), stub, "plan the thing", "raw exploration notes")
	if got != "# Plan\n\n1. Do the thing." {
		t.Errorf("formatAnswer = %q, want the model's formatted text", got)
	}
}

// TestFormatAnswer_FailsOpenOnModelError: a broken format pass must never
// block delivery - it falls back to the raw answer unchanged.
func TestFormatAnswer_FailsOpenOnModelError(t *testing.T) {
	stub := &formatStub{err: fmt.Errorf("model unavailable")}
	got := formatAnswer(context.Background(), stub, "plan the thing", "raw exploration notes")
	if got != "raw exploration notes" {
		t.Errorf("formatAnswer = %q, want the raw answer unchanged on model error", got)
	}
}

// TestFormatAnswer_NilModelReturnsRaw: no model configured ⇒ no pass attempted.
func TestFormatAnswer_NilModelReturnsRaw(t *testing.T) {
	got := formatAnswer(context.Background(), nil, "plan the thing", "raw exploration notes")
	if got != "raw exploration notes" {
		t.Errorf("formatAnswer = %q, want the raw answer unchanged with a nil model", got)
	}
}

// TestFormatAnswer_EmptyAnswerShortCircuits: nothing to format.
func TestFormatAnswer_EmptyAnswerShortCircuits(t *testing.T) {
	stub := &formatStub{reply: "should never be seen"}
	if got := formatAnswer(context.Background(), stub, "plan the thing", "  "); got != "" {
		t.Errorf("formatAnswer = %q, want empty for an empty raw answer", got)
	}
}
