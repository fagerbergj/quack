package github

import (
	"github.com/fagerbergj/quack/internal/config"
)

// computeGrant derives a run's allowed delivery kinds from labels,
// authorship, and fork state (#657, #662). prScoped is known once, here, and
// resolves the "pull_request"/"comment" ambiguity locally (open a NEW PR vs.
// push to an EXISTING one; join an issue vs. a PR conversation) - the caller
// never needs to re-derive scope from the flattened result. Always returns a
// non-nil slice: this is an actually-computed, trigger-scoped grant, never
// the "no trigger governs this run" sentinel (nil) vetting.Config reserves
// for a non-GitHub run.
func computeGrant(labelCfg config.GitHubLabels, labels []string, prScoped, authoredByQuack, forkHead bool) []string {
	var joinIssueConversation, openPR, postReview, joinPRConversation, pushCommitsToPR bool

	if hasLabel(labels, labelCfg.Plan) {
		joinIssueConversation = true
	}
	if hasLabel(labels, labelCfg.Implement) {
		openPR = true
	}
	if hasLabel(labels, labelCfg.Review) {
		postReview = true
		joinPRConversation = true
	}
	// quack:fix and authorship share one fork-gated path.
	if hasLabel(labels, labelCfg.Implement) || hasLabel(labels, labelCfg.Fix) || authoredByQuack {
		joinPRConversation = true
		if !forkHead {
			pushCommitsToPR = true
		}
	}

	kinds := make([]string, 0, 3)
	if postReview {
		kinds = append(kinds, "review")
	}
	if prScoped {
		if pushCommitsToPR {
			kinds = append(kinds, "pull_request")
		}
		if joinPRConversation {
			kinds = append(kinds, "comment")
		}
	} else {
		if openPR {
			kinds = append(kinds, "pull_request")
		}
		if joinIssueConversation {
			kinds = append(kinds, "comment")
		}
	}
	return kinds
}
