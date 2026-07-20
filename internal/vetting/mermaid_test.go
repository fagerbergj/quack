package vetting

import (
	"strings"
	"testing"
)

func TestFindInvalidMermaid_ValidDiagramPasses(t *testing.T) {
	md := "Here's the plan:\n\n```mermaid\nflowchart TD\n    A[Start] --> B[Finish]\n```\n\nDone."
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none for a valid diagram", issues)
	}
}

func TestFindInvalidMermaid_NoMermaidBlockPasses(t *testing.T) {
	md := "No diagrams here, just prose and a ```go\nfmt.Println(1)\n``` block."
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none — no mermaid fence at all", issues)
	}
}

// A missing diagram-type header is a real parse error from mermaid-check
// ("unknown or unsupported diagram type") — a known-bad diagram the library
// actually rejects, not a heuristic.
func TestFindInvalidMermaid_MissingHeaderDetected(t *testing.T) {
	md := "Before.\n\n```mermaid\nA[Start] --> B[Finish]\n```\n\nAfter."
	issues := FindInvalidMermaid(md)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 for a missing diagram-type header", issues)
	}
	if issues[0].line != 3 {
		t.Fatalf("line = %d, want 3 (the fence-open line)", issues[0].line)
	}
	if !strings.Contains(issues[0].err, "parse error") {
		t.Fatalf("err = %q, want a parse error", issues[0].err)
	}
}

// The issue's own named quote-in-unquoted-label case (#448): mermaid-check
// v0.0.4 parses AND strictly validates this clean (verified empirically),
// but GitHub's real parser errors on it — the supplementary quotedLabelIssue
// check exists exactly to close this false-negative.
func TestFindInvalidMermaid_QuoteInUnquotedLabelDetected(t *testing.T) {
	md := "```mermaid\ngraph TD\n  A[bundle name<br/>e.g. \"code-reviewer\"] --> B[x]\n```"
	issues := FindInvalidMermaid(md)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 for the quote-in-unquoted-label case", issues)
	}
	if !strings.Contains(issues[0].err, "double-quote") {
		t.Fatalf("err = %q, want it to name the double-quote problem", issues[0].err)
	}
}

// A fully-quoted label using mermaid's own escape form must NOT trip the
// supplementary quote check — only a BARE quote inside an unquoted label is
// invalid.
func TestFindInvalidMermaid_FullyQuotedLabelWithEscapedQuotesPasses(t *testing.T) {
	md := "```mermaid\nflowchart TD\n  A[\"bundle name, e.g. \\\"code-reviewer\\\"\"] --> B[Done]\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none — the whole label is quoted", issues)
	}
}

// The issue's own named example — a bracket label with an unquoted paren —
// parses fine (mermaid-check is lenient at the AST level) but fails STRICT
// validation (NoParenthesesInLabels).
func TestFindInvalidMermaid_UnquotedParenLabelDetected(t *testing.T) {
	md := "```mermaid\nflowchart TD\n    A[Login (OAuth)] --> B[Done]\n```"
	issues := FindInvalidMermaid(md)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 for an unquoted paren label", issues)
	}
}

func TestFindInvalidMermaid_UnknownSequenceArrowDetected(t *testing.T) {
	md := "```mermaid\nsequenceDiagram\n    Alice ->>> Bob: bad arrow\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 for an unrecognized sequence arrow", issues)
	}
}

// BLOCKING regression: a ```mermaid-looking fence quoted INSIDE an unrelated
// fence's body (a ```go block whose content merely mentions "```mermaid",
// e.g. in a comment demonstrating markdown) must never be treated as a real
// mermaid opener.
func TestFindInvalidMermaid_NestedFenceIgnored(t *testing.T) {
	md := "Example:\n\n```go\n" +
		"// Here's how you'd write a bad diagram:\n" +
		"// ```mermaid\n" +
		"// A[Start --> B[[Finish\n" +
		"// ```\n" +
		"fmt.Println(\"done\")\n" +
		"```\n\nEnd."
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none — the ```mermaid text is nested inside a ```go block, not a real fence", issues)
	}
}

// The same regression, but proving a REAL top-level bad mermaid block right
// next to the nested false-positive is still caught.
func TestFindInvalidMermaid_NestedFenceIgnoredRealBlockStillDetected(t *testing.T) {
	md := "```go\n// ```mermaid\n// not a real diagram\n// ```\nfmt.Println(1)\n```\n\n" +
		"```mermaid\nA[Start] --> B[Finish]\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 — the real top-level bad diagram, not the nested false-positive", issues)
	}
}

// GitHub renders ```Mermaid / ```MERMAID the same as ```mermaid — the fence
// match must be case-insensitive.
func TestFindInvalidMermaid_CaseInsensitiveFence(t *testing.T) {
	md := "```MERMAID\nA[Start] --> B[Finish]\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 regardless of fence case", issues)
	}
}

// mermaidError must degrade to a reason rather than panic through — a young
// third-party parser on the gate's checks path.
func TestMermaidError_RecoversFromPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("mermaidError must recover internally, panicked instead: %v", r)
		}
	}()
	if mermaidError(strings.Repeat("A", 1<<20)) == "" {
		t.Fatal("garbage input must not validate as a diagram")
	}
}

func TestMermaidError(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		invalid bool
	}{
		{"valid flowchart", "flowchart TD\nA[Start] --> B[Finish]", false},
		{"valid sequence", "sequenceDiagram\nAlice->>Bob: Hello", false},
		{"no header", "A[Start] --> B[Finish]", true},
		{"unquoted paren label", "flowchart TD\nA[Login (OAuth)] --> B[Done]", true},
		{"quote in unquoted label", "flowchart TD\nA[bundle name, e.g. \"code-reviewer\"] --> B[Done]", true},
		{"unknown sequence arrow", "sequenceDiagram\nAlice ->>> Bob: bad", true},
		{"empty", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mermaidError(c.body) != ""
			if got != c.invalid {
				t.Errorf("mermaidError(%q) invalid = %v, want %v", c.body, got, c.invalid)
			}
		})
	}
}

// mermaidCriterion is the gate wiring: it must find an invalid diagram in
// either the answer text or a staged delivery body, and stay inapplicable
// (ok=false) when nothing invalid is present anywhere.
func TestDegradeInvalidMermaid_WarningOutsideFence(t *testing.T) {
	md := "Before.\n\n```mermaid\nA[Start] --> B[Finish]\n```\n\nAfter."
	got, issues := DegradeInvalidMermaid(md)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1", issues)
	}
	wantLine := "> ⚠️ invalid mermaid diagram (parse error"
	lines := strings.Split(got, "\n")
	foundWarning, foundTextFence := false, false
	for _, line := range lines {
		if strings.HasPrefix(line, wantLine) {
			if foundTextFence {
				t.Fatal("warning appeared after the ```text fence — it must come before the fence opener")
			}
			foundWarning = true
		}
		if line == "```text" {
			foundTextFence = true
		}
	}
	if !foundWarning {
		t.Fatal("expected a warning line starting with '> ⚠️ invalid mermaid diagram'")
	}
	if !foundTextFence {
		t.Fatal("expected ```text fence in degraded output")
	}
}

func TestMermaidCriterion_DetectsInvalidStagedBody(t *testing.T) {
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Body: "See the flow:\n\n```mermaid\nA[Start] --> B[End]\n```"},
	}}
	c, ok := mermaidCriterion("", act)
	if !ok {
		t.Fatal("want ok=true — the staged PR body has an invalid diagram")
	}
	if c.Score != 0 {
		t.Fatalf("Score = %v, want 0", c.Score)
	}
	if !strings.Contains(c.Reason, "parse error") {
		t.Fatalf("Reason = %q, want it to carry the concrete parse error", c.Reason)
	}
}

func TestMermaidCriterion_ValidEverywherePasses(t *testing.T) {
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Body: "See the flow:\n\n```mermaid\nflowchart TD\n    A[Start] --> B[End]\n```"},
	}}
	if _, ok := mermaidCriterion("no diagrams here", act); ok {
		t.Fatal("want ok=false — nothing invalid anywhere")
	}
}
