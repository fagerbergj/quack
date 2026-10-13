package vetting

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestCheckCodeReviewCaps: a violation's error names the cap and where the content
// belongs instead, never a bare "invalid".
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
		// Only length and newlines are enforced: sentence counting false-positives on e.g./i.e./vs.
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
	t.Run("every violation reported in one error", func(t *testing.T) {
		long := func(n int) string { return strings.Repeat("z", n+1) }
		err := CheckCodeReviewCaps(long(reviewTakeawayMaxLen), []string{"ok", long(reviewVerifiedMaxLen)},
			[]string{long(reviewNotesMaxLen), long(reviewNotesMaxLen)})
		for _, want := range []string{"takeaway: 241 characters", "verified: item 2 is 161", "notes: item 1 is 201", "notes: item 2 is 201"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("got %v, want it to include %q", err, want)
			}
		}
	})
}

// TestValidateCodeReview: the Validate hook enforces the same caps for a native
// write_code_review call, so it can't bypass stage_review's.
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

// TestCodeReviewSchemaDeclaresCaps: write_code_review's schema states each enforced cap, so the model
// sees it before the call instead of in a rejection.
func TestCodeReviewSchemaDeclaresCaps(t *testing.T) {
	var s struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(codeReviewJSONSchema), &s); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"takeaway": fmt.Sprintf("max %d characters", reviewTakeawayMaxLen),
		"verified": fmt.Sprintf("max %d items, each max %d characters", reviewVerifiedMaxItems, reviewVerifiedMaxLen),
		"notes":    fmt.Sprintf("max %d items, each max %d characters", reviewNotesMaxItems, reviewNotesMaxLen),
	}
	for field, limit := range want {
		if got := s.Properties[field].Description; !strings.Contains(got, limit) {
			t.Errorf("%s description = %q, want it to state %q", field, got, limit)
		}
	}
}
