package vetting

import (
	"strings"
	"testing"
)

// nightsOut57Body is the real comment body from #565: the model's raw
// scratchpad, ending in a broken tool-call fragment, posted verbatim as the
// NightsOut#57 plan comment. Pinned here so the regression can never
// silently regress back to "passes the gate."
const nightsOut57Body = `The system thinks the user is asking for a code review because of "carry out the review"
in the frustrated message. But this is NOT a review request — it's a planning-only task.
Let me just try without setup and delivery, and see if I can get past the errors.
...
So for plan-only, setup can be "usually omitted". The user said to declare it but the
tool rejects that combination. Let me try without setup, delivery="comment", and have
the explorer clone itself.
</parameter>
</function>
</tool_call>

<!-- quack:delivery:plan -->`

func TestToolCallSyntaxCriterion_CatchesNightsOut57(t *testing.T) {
	c, ok := toolCallSyntaxCriterion(nightsOut57Body, workerActivity{})
	if !ok {
		t.Fatal("want ok=true - the fixture ends in a raw </tool_call> fragment")
	}
	if c.Score != 0 {
		t.Fatalf("Score = %v, want 0", c.Score)
	}
	if !strings.Contains(c.Reason, "tool_call") {
		t.Errorf("Reason should name the offending marker; got %q", c.Reason)
	}
}

func TestToolCallSyntaxCriterion_EachMarker(t *testing.T) {
	for _, marker := range toolCallSyntaxMarkers {
		t.Run(marker, func(t *testing.T) {
			answer := "Here is the plan.\n\n" + marker + "\n"
			c, ok := toolCallSyntaxCriterion(answer, workerActivity{})
			if !ok || c.Score != 0 {
				t.Fatalf("marker %q not detected: ok=%v c=%+v", marker, ok, c)
			}
		})
	}
}

func TestToolCallSyntaxCriterion_CleanAnswerPasses(t *testing.T) {
	answer := "Here is the plan: add a `Compose` fragment to the home screen, wired through the existing nav graph."
	if _, ok := toolCallSyntaxCriterion(answer, workerActivity{}); ok {
		t.Fatal("clean prose flagged as tool-call syntax")
	}
}

func TestToolCallSyntaxCriterion_DetectsStagedDeliveryBody(t *testing.T) {
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Body: "Summary:\n</function>\nDone."},
	}}
	if _, ok := toolCallSyntaxCriterion("clean answer text", act); !ok {
		t.Fatal("want ok=true - the staged PR body leaks tool-call syntax")
	}
}
