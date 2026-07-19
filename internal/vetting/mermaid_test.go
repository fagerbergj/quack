package vetting

import (
	"context"
	"strings"
	"testing"
)

func TestValidateAndRepairMermaid_ValidDiagramUntouched(t *testing.T) {
	md := "Here's the plan:\n\n```mermaid\nflowchart TD\n    A[Start] --> B[Finish]\n```\n\nDone."
	out, changed := ValidateAndRepairMermaid(md)
	if changed {
		t.Fatalf("changed = true for an already-valid diagram, want untouched; out=%s", out)
	}
	if out != md {
		t.Fatalf("out = %q, want unchanged input", out)
	}
}

func TestValidateAndRepairMermaid_NoMermaidBlockUntouched(t *testing.T) {
	md := "No diagrams here, just prose and a ```go\nfmt.Println(1)\n``` block."
	out, changed := ValidateAndRepairMermaid(md)
	if changed || out != md {
		t.Fatalf("markdown with no mermaid fence must be left alone, got changed=%v out=%q", changed, out)
	}
}

// A missing diagram-type header is a real parse error from mermaid-check
// ("unknown or unsupported diagram type") — a known-bad diagram the library
// actually rejects, not a guess. It must be stripped, not shipped.
func TestValidateAndRepairMermaid_MissingHeaderStripped(t *testing.T) {
	md := "Before.\n\n```mermaid\nA[Start] --> B[Finish]\n```\n\nAfter."
	out, changed := ValidateAndRepairMermaid(md)
	if !changed {
		t.Fatal("want changed=true — a diagram with no recognized header must be stripped")
	}
	if strings.Contains(out, "```mermaid") {
		t.Fatalf("out still contains a mermaid fence, want it stripped: %q", out)
	}
	if !strings.Contains(out, "Before.") || !strings.Contains(out, "After.") {
		t.Fatalf("out = %q, want surrounding prose preserved", out)
	}
	if !strings.Contains(out, "A[Start] --> B[Finish]") {
		t.Fatalf("out = %q, want the raw source kept as a plain-text fallback", out)
	}
}

// sequenceDiagram with an arrow token mermaid-check's parser doesn't
// recognize — another real, library-caught parse error (not a heuristic).
func TestValidateAndRepairMermaid_UnknownSequenceArrowStripped(t *testing.T) {
	md := "```mermaid\nsequenceDiagram\n    Alice ->>> Bob: bad arrow\n```"
	out, changed := ValidateAndRepairMermaid(md)
	if !changed {
		t.Fatal("want changed=true — an unrecognized sequence arrow must be stripped")
	}
	if strings.Contains(out, "```mermaid") {
		t.Fatalf("out still contains a mermaid fence, want it stripped: %q", out)
	}
}

// The issue's own named example — a bracket label with an unquoted paren —
// parses fine (mermaid-check is lenient at the AST level) but fails STRICT
// validation (NoParenthesesInLabels), which is exactly why validateAndRepair
// validates strict: a diagram that parses but renders wrong must still strip.
func TestValidateAndRepairMermaid_UnquotedParenLabelStripped(t *testing.T) {
	md := "```mermaid\nflowchart TD\n    A[Login (OAuth)] --> B[Done]\n```"
	out, changed := ValidateAndRepairMermaid(md)
	if !changed {
		t.Fatal("want changed=true — an unquoted paren label fails strict validation")
	}
	if strings.Contains(out, "```mermaid") {
		t.Fatalf("out still contains a mermaid fence, want it stripped: %q", out)
	}
	if !strings.Contains(out, "Login (OAuth)") {
		t.Fatalf("out = %q, want the raw source kept as a plain-text fallback", out)
	}
}

// BLOCKING regression: a ```mermaid-looking fence quoted INSIDE an unrelated
// fence's body (a ```go block whose content merely mentions "```mermaid",
// e.g. in a comment demonstrating markdown) must be left byte-for-byte
// untouched — not treated as a real mermaid opener, not stripped, not
// mangled. Reachable in practice: delivered PR/review/comment bodies
// routinely quote code or examples.
func TestValidateAndRepairMermaid_NestedFenceLeftUntouched(t *testing.T) {
	md := "Example:\n\n```go\n" +
		"// Here's how you'd write a bad diagram:\n" +
		"// ```mermaid\n" +
		"// A[Start --> B[[Finish\n" +
		"// ```\n" +
		"fmt.Println(\"done\")\n" +
		"```\n\nEnd."
	out, changed := ValidateAndRepairMermaid(md)
	if changed || out != md {
		t.Fatalf("nested ```mermaid quoted inside a ```go block must be untouched:\ngot changed=%v out=%q\nwant unchanged", changed, out)
	}
}

// The same regression, but proving a REAL top-level bad mermaid block right
// next to the nested false-positive is still caught and stripped — the fix
// must not overcorrect into ignoring every ```mermaid fence.
func TestValidateAndRepairMermaid_NestedFenceUntouchedRealBlockStillStripped(t *testing.T) {
	md := "```go\n// ```mermaid\n// not a real diagram\n// ```\nfmt.Println(1)\n```\n\n" +
		"```mermaid\nA[Start] --> B[Finish]\n```"
	out, changed := ValidateAndRepairMermaid(md)
	if !changed {
		t.Fatal("want changed=true — the real top-level bad diagram must still be stripped")
	}
	if !strings.Contains(out, "// ```mermaid") {
		t.Fatalf("out = %q, want the nested false-positive inside the go block preserved verbatim", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "```mermaid" {
			t.Fatalf("out = %q, want the real invalid top-level mermaid fence stripped (no top-level ```mermaid opener left)", out)
		}
	}
}

// GitHub renders ```Mermaid / ```MERMAID the same as ```mermaid — the fence
// match must be case-insensitive so a bad diagram can't ship unvalidated
// just by differently-cased fence info.
func TestValidateAndRepairMermaid_CaseInsensitiveFence(t *testing.T) {
	md := "```MERMAID\nA[Start] --> B[Finish]\n```"
	out, changed := ValidateAndRepairMermaid(md)
	if !changed {
		t.Fatal("want changed=true — an invalid diagram in a ```MERMAID fence must still be validated and stripped")
	}
	if strings.Contains(out, "MERMAID") && strings.Contains(out, "```MERMAID") {
		t.Fatalf("out = %q, want the invalid diagram stripped regardless of fence case", out)
	}
}

// mermaidValid must degrade to "invalid" rather than panic through — a young
// third-party parser on the single-shot, no-retry delivery path.
func TestMermaidValid_RecoversFromPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("mermaidValid must recover internally, panicked instead: %v", r)
		}
	}()
	// Not expected to panic in practice — this just proves the call is safe
	// to make on arbitrary/adversarial input without a defer at the call site.
	if mermaidValid(strings.Repeat("A", 1<<20)) {
		t.Fatal("garbage input must not validate as a diagram")
	}
}

func TestMermaidValid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"valid flowchart", "flowchart TD\nA[Start] --> B[Finish]", true},
		{"valid sequence", "sequenceDiagram\nAlice->>Bob: Hello", true},
		{"no header", "A[Start] --> B[Finish]", false},
		{"unquoted paren label", "flowchart TD\nA[Login (OAuth)] --> B[Done]", false},
		{"unknown sequence arrow", "sequenceDiagram\nAlice ->>> Bob: bad", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mermaidValid(c.body); got != c.want {
				t.Errorf("mermaidValid(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// commitDelivery must apply the mermaid pass to every staged item's Body
// before handing it to Deliver — end-to-end wiring, not just the pure func.
func TestCommitDelivery_StripsInvalidMermaidInStagedBody(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "stage_pr", map[string]any{
			"title": "Add feature",
			"body":  "See the flow:\n\n```mermaid\nA[Start] --> B[End]\n```",
		}),
	))
	var gotBody string
	commitDelivery(context.Background(), nil, Config{Deliver: func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		if len(dc.Items) != 1 {
			t.Fatalf("Items = %d, want 1", len(dc.Items))
		}
		gotBody = dc.Items[0].Body
		return nil, nil
	}}, "n1", act, GateResult{Passed: true})
	if strings.Contains(gotBody, "```mermaid") {
		t.Fatalf("body still has the invalid diagram fenced as mermaid: %q", gotBody)
	}
	if !strings.Contains(gotBody, "A[Start] --> B[End]") {
		t.Fatalf("body = %q, want the raw source preserved as a plain-text fallback", gotBody)
	}
}

// A valid diagram staged for delivery must reach Deliver byte-for-byte.
func TestCommitDelivery_LeavesValidMermaidUntouched(t *testing.T) {
	body := "See the flow:\n\n```mermaid\nflowchart TD\n    A[Start] --> B[End]\n```"
	act := activityFromSession(newTestSession(t,
		fnCall("1", "stage_pr", map[string]any{"title": "Add feature", "body": body}),
	))
	var gotBody string
	commitDelivery(context.Background(), nil, Config{Deliver: func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		gotBody = dc.Items[0].Body
		return nil, nil
	}}, "n1", act, GateResult{Passed: true})
	if gotBody != body {
		t.Fatalf("body = %q, want unchanged %q", gotBody, body)
	}
}
