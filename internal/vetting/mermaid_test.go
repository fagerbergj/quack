package vetting

import (
	"context"
	"strings"
	"testing"
)

func TestValidateAndRepairMermaid_ValidDiagramUntouched(t *testing.T) {
	md := "Here's the plan:\n\n```mermaid\nflowchart TD\n    A[Start] --> B[Finish]\n```\n\nDone."
	out, changed := validateAndRepairMermaid(md)
	if changed {
		t.Fatalf("changed = true for an already-valid diagram, want untouched; out=%s", out)
	}
	if out != md {
		t.Fatalf("out = %q, want unchanged input", out)
	}
}

func TestValidateAndRepairMermaid_NoMermaidBlockUntouched(t *testing.T) {
	md := "No diagrams here, just prose and a ```go\nfmt.Println(1)\n``` block."
	out, changed := validateAndRepairMermaid(md)
	if changed || out != md {
		t.Fatalf("markdown with no mermaid fence must be left alone, got changed=%v out=%q", changed, out)
	}
}

func TestValidateAndRepairMermaid_MissingHeaderRepaired(t *testing.T) {
	md := "```mermaid\nA[Start] --> B[Finish]\n```"
	out, changed := validateAndRepairMermaid(md)
	if !changed {
		t.Fatal("want changed=true — a missing flowchart header should be repaired")
	}
	if !strings.Contains(out, "flowchart TD") {
		t.Fatalf("out = %q, want a prepended flowchart header", out)
	}
	if !mermaidStructurallyValid(mermaidFenceRe.FindStringSubmatch(out)[1]) {
		t.Fatalf("repaired block still invalid: %q", out)
	}
}

func TestValidateAndRepairMermaid_UnquotedLabelRepaired(t *testing.T) {
	md := "```mermaid\nflowchart TD\n    A[Login (OAuth)] --> B[Done]\n```"
	out, changed := validateAndRepairMermaid(md)
	if !changed {
		t.Fatal("want changed=true — unquoted paren label should be repaired")
	}
	if !strings.Contains(out, `A["Login (OAuth)"]`) {
		t.Fatalf("out = %q, want the label wrapped in quotes", out)
	}
}

func TestValidateAndRepairMermaid_SingleDashArrowRepaired(t *testing.T) {
	md := "```mermaid\nflowchart TD\n    A[Start] -> B[Finish]\n```"
	out, changed := validateAndRepairMermaid(md)
	if !changed {
		t.Fatal("want changed=true — a bare `->` should become `-->`")
	}
	if !strings.Contains(out, "A[Start] --> B[Finish]") {
		t.Fatalf("out = %q, want the arrow repaired to -->", out)
	}
}

// A known-bad diagram — unbalanced brackets that repair cannot mechanically
// fix — must be stripped to a plain-text fallback rather than shipped broken.
func TestValidateAndRepairMermaid_UnrepairableStripped(t *testing.T) {
	md := "Before.\n\n```mermaid\nflowchart TD\n    A[Start --> B[[Finish\n```\n\nAfter."
	out, changed := validateAndRepairMermaid(md)
	if !changed {
		t.Fatal("want changed=true — an unrepairable diagram must be stripped")
	}
	if strings.Contains(out, "```mermaid") {
		t.Fatalf("out still contains a mermaid fence, want it stripped: %q", out)
	}
	if !strings.Contains(out, "Before.") || !strings.Contains(out, "After.") {
		t.Fatalf("out = %q, want surrounding prose preserved", out)
	}
	if !strings.Contains(out, "A[Start --> B[[Finish") {
		t.Fatalf("out = %q, want the raw source kept as a plain-text fallback", out)
	}
}

func TestMermaidStructurallyValid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"valid flowchart", "flowchart TD\nA[Start] --> B[Finish]", true},
		{"valid sequence", "sequenceDiagram\nAlice->>Bob: Hello", true},
		{"no header", "A[Start] --> B[Finish]", false},
		{"unbalanced brackets", "flowchart TD\nA[Start --> B[[Finish", false},
		{"nested fence", "flowchart TD\n```\nA-->B\n```", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mermaidStructurallyValid(c.body); got != c.want {
				t.Errorf("mermaidStructurallyValid(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// commitDelivery must apply the mermaid pass to every staged item's Body
// before handing it to Deliver — end-to-end wiring, not just the pure func.
func TestCommitDelivery_RepairsMermaidInStagedBody(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "stage_pr", map[string]any{
			"title": "Add feature",
			"body":  "See the flow:\n\n```mermaid\nA[Start] -> B[End]\n```",
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
	if strings.Contains(gotBody, "A[Start] -> B[End]") {
		t.Fatalf("body still has the unrepaired diagram: %q", gotBody)
	}
	if !strings.Contains(gotBody, "flowchart TD") || !strings.Contains(gotBody, "-->") {
		t.Fatalf("body = %q, want a repaired flowchart with a header and --> arrow", gotBody)
	}
}
