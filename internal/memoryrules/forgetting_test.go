package memoryrules

import (
	"strings"
	"testing"
)

func TestEvaluate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		expr string
		f    Fields
		want bool
	}{
		{"eq int", "upvotes == 3", Fields{Upvotes: 3}, true},
		{"ne int", "upvotes != 3", Fields{Upvotes: 4}, true},
		{"lt", "score < 0", Fields{Score: -1}, true},
		{"lte boundary", "score <= -2", Fields{Score: -2}, true},
		{"gt false", "recalls > 5", Fields{Recalls: 5}, false},
		{"string eq", `tier == "verified"`, Fields{Tier: "verified"}, true},
		{"string ne", `scope != "repo:foo"`, Fields{Scope: "repo:bar"}, true},
		{"and precedence over or", `tier == "verified" || tier == "unverified" && score < 0`, Fields{Tier: "unverified", Score: -1}, true},
		{"and short of or when first true", `tier == "verified" || tier == "unverified" && score < 0`, Fields{Tier: "verified", Score: 5}, true},
		{"and binds tighter", `tier == "verified" && score < 0 || recalls > 0`, Fields{Tier: "verified", Score: 5, Recalls: 1}, true},
		{"not binds to next", `!(tier == "verified") && score < 0`, Fields{Tier: "unverified", Score: -1}, true},
		{"not false", `!(score < 0)`, Fields{Score: 5}, true},
		{"parens override", `(score < 0 || recalls > 0) && tier == "verified"`, Fields{Score: -1, Recalls: 0, Tier: "verified"}, true},
		{"never upvoted equals age", "days_since_upvote > 90", Fields{AgeDays: 100, DaysSinceUpvote: 100}, true},
		{"default rule 1", `tier == "unverified" && days_since_upvote > 90`, Fields{Tier: "unverified", DaysSinceUpvote: 91}, true},
		{"default rule 2", "score <= -2", Fields{Score: -3}, true},
		{"default rule 3", `tier == "verified"`, Fields{Tier: "verified"}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := Evaluate(c.expr, c.f)
			if err != nil {
				t.Fatalf("Evaluate(%q) error: %v", c.expr, err)
			}
			if got != c.want {
				t.Errorf("Evaluate(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestEvaluateErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		expr    string
		wantSub string
	}{
		{"unknown field", "bogus == 1", "unknown field"},
		{"unterminated string", `tier == "verified`, "unterminated string"},
		{"bad char", "score $ 1", "unexpected character"},
		{"missing paren", "(score < 1", "expected closing"},
		{"trailing tokens", "score < 1 score", "unexpected token"},
		{"type mismatch numeric op on string", `tier < "x"`, "requires numeric"},
		{"type mismatch eq", `tier == 1`, "same type"},
		{"non-bool result", "score", "does not evaluate to a boolean"},
		{"empty expr", "", "when must not be empty"},
		{"blank expr", " ", "when must not be empty"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Evaluate(c.expr, Fields{})
			if err == nil {
				t.Fatalf("Evaluate(%q) expected error, got nil", c.expr)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("Evaluate(%q) error = %q, want substring %q", c.expr, err.Error(), c.wantSub)
			}
		})
	}
}

func TestValidateRules(t *testing.T) {
	t.Parallel()
	if err := ValidateRules(DefaultRules()); err != nil {
		t.Fatalf("DefaultRules() must validate: %v", err)
	}
	if err := ValidateRules([]Rule{{When: "score <= -2", Then: "bogus"}}); err == nil {
		t.Fatal("expected error for unknown then")
	} else if !strings.Contains(err.Error(), "rule 0") {
		t.Errorf("error should name rule index: %v", err)
	}
	if err := ValidateRules([]Rule{{When: "bogus == 1", Then: ThenKeep}}); err == nil {
		t.Fatal("expected error for bad expression")
	} else if !strings.Contains(err.Error(), "rule 0") {
		t.Errorf("error should name rule index: %v", err)
	}
	// A contradictory rule (can never match) parses fine - it's a config
	// smell, not an error; the validator only rejects syntax it can't run.
	if err := ValidateRules([]Rule{{When: `tier == "verified" && tier == "unverified"`, Then: ThenKeep}}); err != nil {
		t.Errorf("a rule that never matches should still validate: %v", err)
	}
}
