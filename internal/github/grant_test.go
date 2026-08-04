package github

import (
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

func testLabels() config.GitHubLabels {
	return config.GitHubLabels{
		Plan: "quack:plan", Implement: "quack:implement",
		Review: "quack:review", Fix: "quack:fix",
	}
}

// A fork PR must never receive push_commits_to_pr - quack cannot push to a
// fork's head (cifix.go) - regardless of which label would otherwise grant
// it.
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
			g := computeGrant(testLabels(), tc.labels, true /* prScoped */, tc.authoredByQuack, true /* forkHead */)
			if g.PushCommitsToPR {
				t.Fatalf("PushCommitsToPR = true for a fork PR, want false: %+v", g)
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
			g := computeGrant(testLabels(), tc.labels, true /* prScoped */, tc.authoredByQuack, false /* forkHead */)
			if !g.PushCommitsToPR {
				t.Fatalf("PushCommitsToPR = false for a same-repo PR, want true: %+v", g)
			}
		})
	}
}

// A quack-authored PR gets push + PR-conversation grants even with NO label
// applied at all - "authorship IS the flag" (#656).
func TestComputeGrant_AuthoredPRGrantsPushAndConversationWithNoLabel(t *testing.T) {
	g := computeGrant(testLabels(), nil /* no labels */, true /* prScoped */, true /* authoredByQuack */, false /* forkHead */)
	if !g.PushCommitsToPR || !g.JoinPRConversation {
		t.Fatalf("authored PR grant = %+v, want PushCommitsToPR and JoinPRConversation both true", g)
	}
	if g.OpenPR || g.PostReview {
		t.Fatalf("authored PR grant = %+v, want OpenPR and PostReview false - no label was applied", g)
	}
}

// An issue with no labels and no authorship (there is no PR yet) grants
// nothing - permission comes only from labels/authorship/fork state, never
// from message text.
func TestComputeGrant_PlainIssueNoLabelsGrantsNothing(t *testing.T) {
	g := computeGrant(testLabels(), nil, false /* prScoped */, false, false)
	if g.JoinIssueConversation || g.OpenPR || g.PostReview || g.JoinPRConversation || g.PushCommitsToPR {
		t.Fatalf("grant = %+v, want everything false with no labels and no authorship", g)
	}
}

func TestComputeGrant_LabelsMapToTheirDocumentedPermissions(t *testing.T) {
	plan := computeGrant(testLabels(), []string{"quack:plan"}, false, false, false)
	if !plan.JoinIssueConversation {
		t.Fatalf("quack:plan grant = %+v, want JoinIssueConversation", plan)
	}

	implement := computeGrant(testLabels(), []string{"quack:implement"}, false, false, false)
	if !implement.OpenPR {
		t.Fatalf("quack:implement grant = %+v, want OpenPR", implement)
	}

	review := computeGrant(testLabels(), []string{"quack:review"}, true, false, false)
	if !review.PostReview || !review.JoinPRConversation {
		t.Fatalf("quack:review grant = %+v, want PostReview and JoinPRConversation", review)
	}
}
