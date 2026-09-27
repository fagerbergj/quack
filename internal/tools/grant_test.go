package tools

import (
	"strings"
	"testing"
)

func TestOriginGrant(t *testing.T) {
	base := `{"extension":"github","label":"o/r#1"}`
	if _, ok := OriginGrant(base); ok {
		t.Fatal("no grant recorded, want ok=false")
	}
	withKinds := WithOriginGrant(base, []string{"comment"})
	if kinds, ok := OriginGrant(withKinds); !ok || strings.Join(kinds, ",") != "comment" || !strings.Contains(withKinds, `"label":"o/r#1"`) {
		t.Fatalf("grant = %v %v in %s", kinds, ok, withKinds)
	}
	if kinds, ok := OriginGrant(WithOriginGrant(withKinds, []string{})); !ok || kinds == nil || len(kinds) != 0 {
		t.Fatalf("deny-all grant = %#v %v", kinds, ok)
	}
	if kinds, ok := OriginGrant(WithOriginGrant(withKinds, nil)); !ok || strings.Join(kinds, ",") != "comment" {
		t.Fatalf("nil kinds (a nudge) must keep the recorded grant, got %v %v", kinds, ok)
	}
	if got := WithOriginGrant("", nil); got != "" {
		t.Fatalf("empty origin, no grant = %q", got)
	}
}

func TestValidateAllowedDeliveryKind_EmptyDeniesAll(t *testing.T) {
	const agent = "code-reviewer" // coupled to "review" (dag.RequiredDeliveryKind)
	if err := validateAllowedDeliveryKind(agent, nil); err != nil {
		t.Fatalf("nil (unrestricted): %v", err)
	}
	if err := validateAllowedDeliveryKind(agent, []string{"review"}); err != nil {
		t.Fatalf("allowed: %v", err)
	}
	if err := validateAllowedDeliveryKind(agent, []string{}); err == nil {
		t.Fatal("non-nil empty grant must deny every kind")
	}
}
