package vetting

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCheckCodeReviewCaps pins the caps stage_review and write_code_review
// both enforce, and that a violation's error names the cap plus where the
// content belongs instead - never a bare "invalid" (one fixed review format).
func TestCheckCodeReviewCaps(t *testing.T) {
	overLong := strings.Repeat("x", reviewTakeawayMaxLen+1)
	items := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "ok"
		}
		return out
	}

	t.Run("valid passes", func(t *testing.T) {
		if err := CheckCodeReviewCaps("One clean sentence.", items(8), items(8)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("empty takeaway skips takeaway checks", func(t *testing.T) {
		if err := CheckCodeReviewCaps("", nil, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("takeaway too long", func(t *testing.T) {
		err := CheckCodeReviewCaps(overLong, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "takeaway") || !strings.Contains(err.Error(), "240") {
			t.Fatalf("got %v, want an error naming takeaway and the 240 cap", err)
		}
	})
	t.Run("multi-sentence and abbreviations are not rejected", func(t *testing.T) {
		// The period-counting "one sentence" heuristic is gone (it
		// false-positived on e.g./i.e./vs.) - only length and newlines are
		// enforced now.
		for _, tk := range []string{"First sentence. Second sentence.", "Uses e.g. an abbreviation.", "See cfg.Setup for details."} {
			if err := CheckCodeReviewCaps(tk, nil, nil); err != nil {
				t.Fatalf("CheckCodeReviewCaps(%q) = %v, want no error", tk, err)
			}
		}
	})
	t.Run("takeaway with newline", func(t *testing.T) {
		err := CheckCodeReviewCaps("line one\nline two", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "newline") {
			t.Fatalf("got %v, want a newline error", err)
		}
	})
	t.Run("too many verified items names the cap", func(t *testing.T) {
		err := CheckCodeReviewCaps("fine.", items(9), nil)
		if err == nil || !strings.Contains(err.Error(), "verified: 9 items, max 8") {
			t.Fatalf("got %v, want the exact cap message", err)
		}
	})
	t.Run("too many notes points to stage_review_comment", func(t *testing.T) {
		err := CheckCodeReviewCaps("fine.", nil, items(11))
		if err == nil || !strings.Contains(err.Error(), "notes: 11 items, max 8") || !strings.Contains(err.Error(), "stage_review_comment") {
			t.Fatalf("got %v, want the cap plus where content belongs", err)
		}
	})
	t.Run("verified item too long", func(t *testing.T) {
		err := CheckCodeReviewCaps("fine.", []string{strings.Repeat("y", reviewVerifiedMaxLen+1)}, nil)
		if err == nil || !strings.Contains(err.Error(), "verified") || !strings.Contains(err.Error(), "160") {
			t.Fatalf("got %v, want an error naming verified and the 160 cap", err)
		}
	})
}

// TestValidateCodeReview proves the recordstore Validate hook enforces the
// same caps for a native write_code_review call, so it can't bypass what
// stage_review enforces at the tool boundary.
func TestValidateCodeReview(t *testing.T) {
	ok := CodeReviewRecord{Verdict: "approve", Takeaway: "Fine.", Verified: []string{"checked build"}}
	raw, err := json.Marshal(ok)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCodeReview(raw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bad := CodeReviewRecord{Verdict: "approve", Takeaway: strings.Repeat("x", reviewTakeawayMaxLen+1)}
	raw, err = json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCodeReview(raw); err == nil || !strings.Contains(err.Error(), "takeaway") {
		t.Fatalf("got %v, want a takeaway-cap error surfaced through the record validator", err)
	}
}
