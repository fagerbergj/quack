package github

import "slices"

// computeGrant derives allowed delivery kinds from labels, authorship, and fork state; prScoped resolves
// "pull_request"/"comment" (new PR vs push, issue vs PR thread). Never nil: nil means unrestricted to quack.
func computeGrant(labelCfg Labels, labels []string, prScoped, authoredByQuack, forkHead bool) []string {
	joinIssueConversation := slices.Contains(labels, labelCfg.Plan)
	openPR := slices.Contains(labels, labelCfg.Implement)
	postReview := slices.Contains(labels, labelCfg.Review)
	// quack:fix and authorship share one fork-gated path.
	writer := openPR || slices.Contains(labels, labelCfg.Fix) || authoredByQuack
	joinPRConversation := postReview || writer
	pushCommitsToPR := writer && !forkHead

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
