package vetting

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireMermaidValidator skips a test rather than let it pass or fail
// meaninglessly when Node isn't on PATH or scripts/node_modules hasn't been
// installed (`cd scripts && npm ci`) - mirrors sandbox_test.go's posture for
// a missing bubblewrap.
func requireMermaidValidator(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("SKIPPING mermaid validator test: node not on PATH")
	}
	if _, err := os.Stat(mermaidValidatorPath); err != nil {
		t.Skip("SKIPPING mermaid validator test: scripts/node_modules missing (run `cd scripts && npm ci`)")
	}
}

func TestFindInvalidMermaid_ValidDiagramPasses(t *testing.T) {
	requireMermaidValidator(t)
	md := "Here's the plan:\n\n```mermaid\nflowchart TD\n    A[Start] --> B[Finish]\n```\n\nDone."
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none for a valid diagram", issues)
	}
}

func TestFindInvalidMermaid_NoMermaidBlockPasses(t *testing.T) {
	md := "No diagrams here, just prose and a ```go\nfmt.Println(1)\n``` block."
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none - no mermaid fence at all", issues)
	}
}

// A missing diagram-type header is a real parse error from mermaid.js itself
// ("No diagram type detected") - one of the five known-bad shapes from #574.
func TestFindInvalidMermaid_MissingHeaderDetected(t *testing.T) {
	requireMermaidValidator(t)
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

// #574's own named case: a bracket label containing a bare double-quote,
// unquoted - GitHub's real parser rejects this outright (a genuine parse
// error), which is what retired the old quotedLabelIssue supplement (that
// existed only because mermaid-check, the Go reimplementation, parsed and
// strictly validated this clean).
func TestFindInvalidMermaid_QuoteInUnquotedLabelDetected(t *testing.T) {
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\n  A[bundle name<br/>e.g. \"code-reviewer\"] --> B[x]\n```"
	issues := FindInvalidMermaid(md)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 for the quote-in-unquoted-label case", issues)
	}
	if !strings.Contains(issues[0].err, "parse error") {
		t.Fatalf("err = %q, want a parse error", issues[0].err)
	}
}

// A fully-quoted label using mermaid's own escape form is valid.
func TestFindInvalidMermaid_FullyQuotedLabelWithEscapedQuotesPasses(t *testing.T) {
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\n  A[\"bundle name, e.g. \\\"code-reviewer\\\"\"] --> B[Done]\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none - the whole label is quoted", issues)
	}
}

// #574's own named example: an unquoted paren inside a bracket label is a
// real parse error under mermaid.js (it opens a "round" node shape), not
// merely a strict-validation warning.
func TestFindInvalidMermaid_UnquotedParenLabelDetected(t *testing.T) {
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\n    A[Login (OAuth)] --> B[Done]\n```"
	issues := FindInvalidMermaid(md)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 for an unquoted paren label", issues)
	}
}

// #574's other named pattern: an unquoted brace inside a bracket label
// (`CV[ComposeView<br/>setContent { NavHost }]`) - this is what broke two of
// the five real plan diagrams behind #574.
func TestFindInvalidMermaid_UnquotedBraceLabelDetected(t *testing.T) {
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\n    CV[ComposeView<br/>setContent { NavHost }] --> B[Done]\n```"
	issues := FindInvalidMermaid(md)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 for an unquoted brace label", issues)
	}
}

// The same label, fully quoted, is valid - braces inside a QUOTED label are
// just text.
func TestFindInvalidMermaid_QuotedBraceLabelPasses(t *testing.T) {
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\n    CV[\"ComposeView setContent { NavHost }\"] --> B[Done]\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none - the whole label is quoted", issues)
	}
}

// A literal `\n` (two characters, not a real line break) inside an unquoted
// label is NOT rejected by the real parser - it renders the literal text
// rather than breaking the line. A natural rule to hand-code wrongly; pinned
// here so nobody "fixes" this into a false positive.
func TestFindInvalidMermaid_LiteralBackslashNPasses(t *testing.T) {
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\n    A[line one\\nline two] --> B[x]\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none - a literal backslash-n renders as text, it doesn't break mermaid's parser", issues)
	}
}

func TestFindInvalidMermaid_UnknownSequenceArrowDetected(t *testing.T) {
	requireMermaidValidator(t)
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
		t.Fatalf("issues = %v, want none - the ```mermaid text is nested inside a ```go block, not a real fence", issues)
	}
}

// The same regression, but proving a REAL top-level bad mermaid block right
// next to the nested false-positive is still caught.
func TestFindInvalidMermaid_NestedFenceIgnoredRealBlockStillDetected(t *testing.T) {
	requireMermaidValidator(t)
	md := "```go\n// ```mermaid\n// not a real diagram\n// ```\nfmt.Println(1)\n```\n\n" +
		"```mermaid\nA[Start] --> B[Finish]\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 - the real top-level bad diagram, not the nested false-positive", issues)
	}
}

// GitHub renders ```Mermaid / ```MERMAID the same as ```mermaid - the fence
// match must be case-insensitive.
func TestFindInvalidMermaid_CaseInsensitiveFence(t *testing.T) {
	requireMermaidValidator(t)
	md := "```MERMAID\nA[Start] --> B[Finish]\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1 regardless of fence case", issues)
	}
}

// mermaidError must degrade to a reason rather than panic through - this
// process invokes a subprocess, and a bug in that invocation path must not
// take the gate round down with it.
func TestMermaidError_RecoversFromPanic(t *testing.T) {
	requireMermaidValidator(t)
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
	requireMermaidValidator(t)
	cases := []struct {
		name    string
		body    string
		invalid bool
	}{
		{"valid flowchart", "flowchart TD\nA[Start] --> B[Finish]", false},
		{"valid sequence", "sequenceDiagram\nAlice->>Bob: Hello", false},
		{"no header", "A[Start] --> B[Finish]", true},
		{"unquoted paren label", "flowchart TD\nA[Login (OAuth)] --> B[Done]", true},
		{"unquoted brace label", "flowchart TD\nA[setContent { NavHost }] --> B[Done]", true},
		{"quote in unquoted label", "flowchart TD\nA[bundle name, e.g. \"code-reviewer\"] --> B[Done]", true},
		{"literal backslash-n", "flowchart TD\nA[line one\\nline two] --> B[x]", false},
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
func TestMermaidCriterion_DetectsInvalidStagedBody(t *testing.T) {
	requireMermaidValidator(t)
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Body: "See the flow:\n\n```mermaid\nA[Start] --> B[End]\n```"},
	}}
	c, ok := mermaidCriterion("", act)
	if !ok {
		t.Fatal("want ok=true - the staged PR body has an invalid diagram")
	}
	if c.Score != 0 {
		t.Fatalf("Score = %v, want 0", c.Score)
	}
	if !strings.Contains(c.Reason, "parse error") {
		t.Fatalf("Reason = %q, want it to carry the concrete parse error", c.Reason)
	}
}

func TestMermaidCriterion_ValidEverywherePasses(t *testing.T) {
	requireMermaidValidator(t)
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Body: "See the flow:\n\n```mermaid\nflowchart TD\n    A[Start] --> B[End]\n```"},
	}}
	if _, ok := mermaidCriterion("no diagrams here", act); ok {
		t.Fatal("want ok=false - nothing invalid anywhere")
	}
}

// TestDegradeInvalidMermaid_TwoBlocks verifies that with two invalid blocks in
// one answer, both are degraded and the prose BETWEEN them is preserved (not
// swallowed by a fence) - the `last` cursor advances correctly per block.
func TestDegradeInvalidMermaid_TwoBlocks(t *testing.T) {
	requireMermaidValidator(t)
	// Neither block declares a diagram type, so both fail validation (as in the
	// single-block test above).
	md := "```mermaid\nA[Start] --> B[Finish]\n```\n\nMiddle prose.\n\n```mermaid\nX --> Y\n```"
	got, issues := DegradeInvalidMermaid(md)
	if len(issues) != 2 {
		t.Fatalf("issues = %d, want 2 (both blocks invalid)", len(issues))
	}
	if !strings.Contains(got, "Middle prose.") {
		t.Fatalf("prose between the two degraded blocks was swallowed:\n%s", got)
	}
	if n := strings.Count(got, "```text"); n != 2 {
		t.Fatalf("```text fences = %d, want 2", n)
	}
	if n := strings.Count(got, "> ⚠️ invalid mermaid diagram"); n != 2 {
		t.Fatalf("warning callouts = %d, want 2", n)
	}
}

func TestDegradeInvalidMermaid_WarningOutsideFence(t *testing.T) {
	requireMermaidValidator(t)
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
				t.Fatal("warning appeared after the ```text fence - it must come before the fence opener")
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

// v0.1.0-era mermaid feature checks, kept as regression coverage against the
// real parser: quoted subgraph titles and class-diagram notes must still
// parse clean.
func TestFindInvalidMermaid_QuotedSubgraphTitlePasses(t *testing.T) {
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\nsubgraph \"My Title\"\nA-->B\nend\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none - quoted subgraph titles are valid mermaid", issues)
	}
}

func TestFindInvalidMermaid_ClassDiagramNotePasses(t *testing.T) {
	requireMermaidValidator(t)
	md := "```mermaid\nclassDiagram\nclass Foo\nnote for Foo \"a note\"\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none - class diagram notes are valid mermaid", issues)
	}
}

// A node with no mermaid at all is untouched whether or not the validator is
// available - and a node WITH a diagram derives nothing (no false failure)
// rather than crash when the validator can't be found.
func TestMermaidCriterion_NoOpWhenValidatorUnavailable(t *testing.T) {
	old := mermaidValidatorPath
	mermaidValidatorPath = "/nonexistent/mermaid-validate.mjs"
	defer func() { mermaidValidatorPath = old }()

	md := "```mermaid\nA[Start --> B[[Finish\n```" // would be invalid if checked
	if _, ok := mermaidCriterion(md, workerActivity{}); ok {
		t.Fatal("want ok=false - the validator is unavailable, so nothing can be found invalid")
	}
	if _, ok := mermaidCriterion("no diagrams here", workerActivity{}); ok {
		t.Fatal("want ok=false - nothing to validate")
	}
}
