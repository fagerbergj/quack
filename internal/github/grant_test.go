package github

import (
	"slices"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

func testLabels() config.GitHubLabels {
	return config.GitHubLabels{
		Plan: "quack:plan", Implement: "quack:implement",
		Review: "quack:review", Fix: "quack:fix",
	}
}

// A fork PR must never receive the pull_request kind (push to an existing
// PR) - quack cannot push to a fork's head (cifix.go) - regardless of which
// label would otherwise grant it.
func TestComputeGrant_ForkPRNeverGetsPushCommits(t *testing.T) {
	for _, tc := range []struct {
		name            string
		labels          []string
		authoredByQuack bool
	}{
		{"quack:implement on a fork PR", []string{"quack:implement"}, false},
		{"quack:fix on a fork PR", []string{"quack:fix"}, false},
		{"quack authored a fork PR", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kinds := computeGrant(testLabels(), tc.labels, true /* prScoped */, tc.authoredByQuack, true /* forkHead */)
			if slices.Contains(kinds, "pull_request") {
				t.Fatalf("kinds = %v, want no pull_request for a fork PR", kinds)
			}
		})
	}
}

// The same three cases on a SAME-repo PR (not a fork) must grant push - the
// fork check is the only thing withholding it above.
func TestComputeGrant_SameRepoPRGetsPushCommits(t *testing.T) {
	for _, tc := range []struct {
		name            string
		labels          []string
		authoredByQuack bool
	}{
		{"quack:implement on a same-repo PR", []string{"quack:implement"}, false},
		{"quack:fix on a same-repo PR", []string{"quack:fix"}, false},
		{"quack authored a same-repo PR", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kinds := computeGrant(testLabels(), tc.labels, true /* prScoped */, tc.authoredByQuack, false /* forkHead */)
			if !slices.Contains(kinds, "pull_request") {
				t.Fatalf("kinds = %v, want pull_request for a same-repo PR", kinds)
			}
		})
	}
}

// A quack-authored PR gets push + PR-conversation grants even with NO label
// applied at all - "authorship IS the flag" (#656).
func TestComputeGrant_AuthoredPRGrantsPushAndConversationWithNoLabel(t *testing.T) {
	kinds := computeGrant(testLabels(), nil /* no labels */, true /* prScoped */, true /* authoredByQuack */, false /* forkHead */)
	if !slices.Contains(kinds, "pull_request") || !slices.Contains(kinds, "comment") {
		t.Fatalf("kinds = %v, want pull_request and comment both granted", kinds)
	}
	if slices.Contains(kinds, "review") {
		t.Fatalf("kinds = %v, want review false - no label was applied", kinds)
	}
}

// An issue with no labels and no authorship (there is no PR yet) grants
// nothing - permission comes only from labels/authorship/fork state, never
// from message text.
func TestComputeGrant_PlainIssueNoLabelsGrantsNothing(t *testing.T) {
	kinds := computeGrant(testLabels(), nil, false /* prScoped */, false, false)
	if len(kinds) != 0 {
		t.Fatalf("kinds = %v, want empty with no labels and no authorship", kinds)
	}
}

func TestComputeGrant_LabelsMapToTheirDocumentedPermissions(t *testing.T) {
	plan := computeGrant(testLabels(), []string{"quack:plan"}, false, false, false)
	if !slices.Contains(plan, "comment") {
		t.Fatalf("quack:plan kinds = %v, want comment (join_issue_conversation)", plan)
	}

	implement := computeGrant(testLabels(), []string{"quack:implement"}, false, false, false)
	if !slices.Contains(implement, "pull_request") {
		t.Fatalf("quack:implement kinds = %v, want pull_request (open_pr)", implement)
	}

	review := computeGrant(testLabels(), []string{"quack:review"}, true, false, false)
	if !slices.Contains(review, "review") || !slices.Contains(review, "comment") {
		t.Fatalf("quack:review kinds = %v, want review and comment (post_review, join_pr_conversation)", review)
	}
}
