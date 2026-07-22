package vetting

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// boundExcerpt leaves within-cap input untouched and, over the cap, returns a
// head+tail excerpt with the truncation marker - both ends of the original are
// preserved so nothing salient (opening framing, closing detail) is lost wholesale.
func TestBoundExcerpt(t *testing.T) {
	small := "a short section"
	if got := boundExcerpt(small, 1_000); got != small {
		t.Fatalf("under-cap input was modified: %q", got)
	}

	head := strings.Repeat("H", 5_000)
	tail := strings.Repeat("T", 5_000)
	big := head + strings.Repeat("M", 40_000) + tail
	got := boundExcerpt(big, 8_000)
	if len(got) > 8_000 {
		t.Fatalf("boundExcerpt returned %d chars, over the 8000 cap", len(got))
	}
	if !strings.Contains(got, "truncated to fit the context window") {
		t.Fatalf("truncation marker missing: %q", got[:200])
	}
	if !strings.HasPrefix(got, "HHHH") {
		t.Fatalf("head not preserved")
	}
	if !strings.HasSuffix(got, "TTTT") {
		t.Fatalf("tail not preserved")
	}
	if strings.Contains(got, strings.Repeat("M", 20_000)) {
		t.Fatalf("the bulk middle was not elided")
	}
}

// A pathological revise input (huge original prompt embedding upstream outputs,
// huge previous answer, huge activity ledger, huge feedback) must not produce an
// unbounded contents[0]. The composed prompt stays within a documented cap and
// carries truncation markers; small inputs pass through verbatim.
func TestBuildRevisionContentBounded(t *testing.T) {
	huge := func(c byte, n int) string { return strings.Repeat(string(c), n) }
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: huge('Q', 200_000)}}}
	answer := huge('A', 200_000)
	feedback := huge('F', 200_000)
	act := workerActivity{}
	for i := 0; i < 500; i++ {
		act.workspace = append(act.workspace, wsOp{tool: "read_file", detail: huge('L', 400), sample: huge('S', 300)})
	}

	got := contentPlainText(buildRevisionContent("principles", question, answer, feedback, act, false))

	// Fixed scaffolding (directive + principles + labels) plus the four capped
	// sections plus the markers - comfortably under a documented ceiling.
	const ceiling = maxOriginalQuestionChars + maxPreviousAnswerChars + maxActivitySectionChars + maxFeedbackChars + 8_000
	if len(got) > ceiling {
		t.Fatalf("revise prompt is %d chars, over the %d ceiling - contents[0] would risk overflow", len(got), ceiling)
	}
	if n := strings.Count(got, "truncated to fit the context window"); n < 4 {
		t.Fatalf("expected all four oversized sections truncated, saw %d markers", n)
	}

	// Small inputs pass through unmodified.
	smallQ := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "what is 2+2?"}}}
	small := contentPlainText(buildRevisionContent("", smallQ, "4", "be precise", workerActivity{}, false))
	if strings.Contains(small, "truncated to fit the context window") {
		t.Fatalf("small revise inputs were needlessly truncated: %q", small)
	}
	if !strings.Contains(small, "what is 2+2?") || !strings.Contains(small, "be precise") {
		t.Fatalf("small revise prompt dropped its content: %q", small)
	}
}
