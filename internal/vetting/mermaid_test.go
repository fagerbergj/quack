package vetting

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mermaidGlobalsMu guards the package-level mermaidValidatorPath /
// mermaidValidateTimeout. The validator tests run in parallel (t.Parallel)
// and both read those globals - running them serially costs ~1.5s each
// (one node spawn loading mermaid's full parser) and pushed the suite past
// the 60s test budget. The two tests that REASSIGN the globals stay serial
// and hold the write lock for their whole body, so a parallel test can
// never observe a mutated path/timeout mid-validation (which the race
// detector would flag and which would also change that test's verdict).
var mermaidGlobalsMu sync.RWMutex

// requireMermaidValidator provisions scripts/node_modules (npm ci, shared
// with internal/tools via EnsureMermaidValidatorDeps) so the tests just work
// on a fresh clone, and skips rather than fail meaninglessly when node/npm is
// absent or the install can't run (e.g. offline) - mirrors sandbox_test.go's
// posture for a missing bubblewrap.
func requireMermaidValidator(t *testing.T) {
	t.Helper()
	// Hold the read lock until the test (and any subtest that acquires its
	// own) finishes, so the whole body is covered by the guard.
	mermaidGlobalsMu.RLock()
	t.Cleanup(func() { mermaidGlobalsMu.RUnlock() })
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("SKIPPING mermaid validator test: node not on PATH")
	}
	if _, err := os.Stat(mermaidValidatorPath); err != nil {
		t.Skipf("SKIPPING mermaid validator test: %s missing", mermaidValidatorPath)
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("SKIPPING mermaid validator test: npm not on PATH (run `cd scripts && npm ci` some other way)")
	}
	if err := EnsureMermaidValidatorDeps(); err != nil {
		t.Skipf("SKIPPING mermaid validator test: could not provision scripts/node_modules (fix with `cd scripts && npm ci`): %v", err)
	}
}

func TestFindInvalidMermaid_ValidDiagramPasses(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\n    A[line one\\nline two] --> B[x]\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none - a literal backslash-n renders as text, it doesn't break mermaid's parser", issues)
	}
}

func TestFindInvalidMermaid_UnknownSequenceArrowDetected(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
			requireMermaidValidator(t)
			got := mermaidError(c.body) != ""
			if got != c.invalid {
				t.Errorf("mermaidError(%q) invalid = %v, want %v", c.body, got, c.invalid)
			}
		})
	}
}

// TestCheckMermaid_ValidDiagramPasses covers the check_mermaid tool's happy path.
func TestCheckMermaid_ValidDiagramPasses(t *testing.T) {
	t.Parallel()
	requireMermaidValidator(t)
	ok, line, col, msg := CheckMermaid("flowchart TD\n    A[Start] --> B[Finish]")
	if !ok || line != 0 || col != 0 || msg != "" {
		t.Fatalf("CheckMermaid(valid) = (%v, %d, %d, %q), want (true, 0, 0, \"\")", ok, line, col, msg)
	}
}

// TestCheckMermaid_InvalidDiagramReportsLocation covers the tool's failure path:
// ok=false plus a located line/column pulled out of the same message the gate shows.
func TestCheckMermaid_InvalidDiagramReportsLocation(t *testing.T) {
	t.Parallel()
	requireMermaidValidator(t)
	ok, line, col, msg := CheckMermaid(`flowchart TD
    G[Node (parens)] --> H[End]`)
	if ok {
		t.Fatal("want ok=false - unquoted parens inside a label")
	}
	if line == 0 || col == 0 {
		t.Fatalf("line/col = %d/%d, want both located", line, col)
	}
	if !strings.Contains(msg, "parse error") {
		t.Fatalf("msg = %q, want it to carry the parse error", msg)
	}
}

// TestCheckMermaid_SharesValidatorWithGate proves the tool and mermaidCriterion
// (the gate's wiring) agree on the same diagram - one source of truth, not two
// implementations that could drift apart.
func TestCheckMermaid_SharesValidatorWithGate(t *testing.T) {
	t.Parallel()
	requireMermaidValidator(t)
	body := "A[Start] --> B[Finish]" // no diagram-type declaration: invalid
	toolOK, _, _, _ := CheckMermaid(body)
	_, gateFlagged := mermaidCriterion("", workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Body: "```mermaid\n" + body + "\n```"},
	}})
	if toolOK {
		t.Fatal("want the tool to flag this diagram invalid")
	}
	if !gateFlagged {
		t.Fatal("want the gate to also flag this diagram invalid")
	}
}

// mermaidCriterion is the gate wiring: it must find an invalid diagram in
// either the answer text or a staged delivery body, and stay inapplicable
// (ok=false) when nothing invalid is present anywhere.
func TestMermaidCriterion_DetectsInvalidStagedBody(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	requireMermaidValidator(t)
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Body: "See the flow:\n\n```mermaid\nflowchart TD\n    A[Start] --> B[End]\n```"},
	}}
	if _, ok := mermaidCriterion("no diagrams here", act); ok {
		t.Fatal("want ok=false - nothing invalid anywhere")
	}
}

// v0.1.0-era mermaid feature checks, kept as regression coverage against the
// real parser: quoted subgraph titles and class-diagram notes must still
// parse clean.
func TestFindInvalidMermaid_QuotedSubgraphTitlePasses(t *testing.T) {
	t.Parallel()
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\nsubgraph \"My Title\"\nA-->B\nend\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none - quoted subgraph titles are valid mermaid", issues)
	}
}

func TestFindInvalidMermaid_ClassDiagramNotePasses(t *testing.T) {
	t.Parallel()
	requireMermaidValidator(t)
	md := "```mermaid\nclassDiagram\nclass Foo\nnote for Foo \"a note\"\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none - class diagram notes are valid mermaid", issues)
	}
}

// #735: the real jison error for an unquoted paren must translate into a
// message naming the "(" and the quoted fix, keep the diagram's own line 2
// and caret column, and drop the grammar-internal Expecting-list noise.
func TestMermaidError_UnquotedParenTranslated(t *testing.T) {
	t.Parallel()
	requireMermaidValidator(t)
	got := mermaidError("flowchart TD\n  F[x] --> G[filterChats(chats, filterState)]")
	if got == "" {
		t.Fatal("want a parse error")
	}
	if strings.Contains(got, "DOUBLECIRCLEEND") {
		t.Fatalf("err = %q, must not leak jison's Expecting-list noise", got)
	}
	if !strings.Contains(got, `"("`) {
		t.Fatalf("err = %q, want it to name the unquoted \"(\"", got)
	}
	if !strings.Contains(got, "double quotes") {
		t.Fatalf("err = %q, want a quoting fix hint", got)
	}
	if !strings.Contains(got, "diagram line 2") {
		t.Fatalf("err = %q, want the diagram's own line 2 preserved", got)
	}
	if !strings.Contains(got, "^") {
		t.Fatalf("err = %q, want the parser's caret preserved", got)
	}
}

// translateMermaidError is a pure function - exercise the token map and the
// fallback path directly, without shelling out to node.
func TestTranslateMermaidError_KnownTokens(t *testing.T) {
	cases := []struct {
		token string
		want  string
	}{
		{"PS", `"("`},
		{"PE", `")"`},
		{"SQS", `"["`},
		{"DIAMOND_START", `"{"`},
		{"DIAMOND_STOP", `"}"`},
		{"PIPE", `"|"`},
	}
	for _, c := range cases {
		t.Run(c.token, func(t *testing.T) {
			raw := "Parse error on line 2:\n...G[label\n----------^\n" +
				"Expecting 'SQE', 'DOUBLECIRCLEEND', got '" + c.token + "'"
			got := translateMermaidError(raw)
			if strings.Contains(got, "DOUBLECIRCLEEND") {
				t.Fatalf("got %q, must drop the Expecting-list noise", got)
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("got %q, want it to name %s", got, c.want)
			}
			if !strings.Contains(got, "diagram line 2, column 11") {
				t.Fatalf("got %q, want the line/column preserved", got)
			}
		})
	}
}

// An unmapped "got" token (or an error shape that isn't jison's structured
// parse error at all) must still carry line/column, the excerpt, a generic
// quoting hint, and the untranslated raw text - never a bare "invalid".
func TestTranslateMermaidError_UnrecognizedTokenKeepsRawText(t *testing.T) {
	raw := "Parse error on line 2:\n...bad arrow\n----------^\n" +
		"Expecting '+', '-', '()', 'ACTOR', got 'INVALID'"
	got := translateMermaidError(raw)
	if !strings.Contains(got, "diagram line 2, column 11") {
		t.Fatalf("got %q, want line/column preserved even when the token is unmapped", got)
	}
	if !strings.Contains(got, "...bad arrow") {
		t.Fatalf("got %q, want the source excerpt preserved", got)
	}
	if !strings.Contains(got, "double quotes") {
		t.Fatalf("got %q, want a generic quoting hint", got)
	}
	if !strings.Contains(got, raw) {
		t.Fatalf("got %q, want the raw parser text preserved verbatim", got)
	}
}

// A totally different error shape (no "Parse error on line" at all, e.g. a
// missing diagram-type header) has no line/column to extract - it must still
// return the raw text rather than a generic "invalid" with nothing to act on.
func TestTranslateMermaidError_UnstructuredErrorKeepsRawText(t *testing.T) {
	raw := "No diagram type detected matching given configuration for text: AAAA"
	got := translateMermaidError(raw)
	if !strings.Contains(got, raw) {
		t.Fatalf("got %q, want the raw parser text preserved verbatim", got)
	}
	if !strings.Contains(got, "double quotes") {
		t.Fatalf("got %q, want a generic quoting hint even with no structure to parse", got)
	}
}

// A node with no mermaid at all is untouched whether or not the validator is
// available - and a node WITH a diagram derives nothing (no false failure)
// rather than crash when the validator can't be found.
func TestMermaidCriterion_NoOpWhenValidatorUnavailable(t *testing.T) {
	// Serial: this test reassigns the shared validator path, so it takes
	// the write lock for its whole body.
	mermaidGlobalsMu.Lock()
	defer mermaidGlobalsMu.Unlock()
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

// mermaid 11.17.1's expanded shape catalog (person/folder/bucket/etc, unified
// @{ shape: ... } syntax) and collapsible subgraphs (@{ view: collapsed })
// parse clean - since the gate defers to the real mermaid.js parser, this
// just proves the upgraded dependency didn't regress either.
func TestFindInvalidMermaid_NewShapeSyntaxPasses(t *testing.T) {
	t.Parallel()
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\n  A@{ shape: person, label: \"User\" }\n  B@{ shape: folder, label: \"Docs\" }\n  A --> B\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none for the new unified shape syntax", issues)
	}
}

func TestFindInvalidMermaid_CollapsedSubgraphPasses(t *testing.T) {
	t.Parallel()
	requireMermaidValidator(t)
	md := "```mermaid\nflowchart TD\n  subgraph sub1[\"Details\"]\n    X --> Y\n  end\n  sub1@{ view: collapsed }\n  A --> sub1\n```"
	if issues := FindInvalidMermaid(md); len(issues) != 0 {
		t.Fatalf("issues = %v, want none for a collapsed subgraph", issues)
	}
}

// A validator that outruns its deadline must not report a VALID diagram as
// invalid: CommandContext's kill surfaces as an ExitError, which used to fall
// through to "unreadable output" and fail the gate on a slow box. Drives the
// real mermaidError path (not a standalone stdlib probe) so reverting the
// ctx.Err() guard in mermaidError fails this test.
func TestMermaidValidate_TimeoutIsNotAnInvalidDiagram(t *testing.T) {
	// Serial: this test reassigns the shared validator path and timeout,
	// so it takes the write lock for its whole body.
	mermaidGlobalsMu.Lock()
	defer mermaidGlobalsMu.Unlock()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable in this environment")
	}

	script := filepath.Join(t.TempDir(), "hang.mjs")
	if err := os.WriteFile(script, []byte("setTimeout(()=>{}, 10000);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath, oldTimeout := mermaidValidatorPath, mermaidValidateTimeout
	mermaidValidatorPath = script
	mermaidValidateTimeout = 200 * time.Millisecond
	defer func() { mermaidValidatorPath, mermaidValidateTimeout = oldPath, oldTimeout }()

	// Before the fix this produced "unreadable output" (i.e. invalid) because
	// CommandContext's kill arrives as an *exec.ExitError with empty output.
	if got := mermaidError("A --> B"); got != "" {
		t.Fatalf("mermaidError = %q, want \"\" (a timeout must not mark a valid diagram invalid)", got)
	}
}
