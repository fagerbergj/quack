package github

import (
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/vetting"
)

// computeGrant derives a run's permission set from labels, authorship, and fork state (#657, #662).
func computeGrant(labelCfg config.GitHubLabels, labels []string, prScoped, authoredByQuack, forkHead bool) vetting.Grant {
	g := vetting.Grant{PRScoped: prScoped}

	if hasLabel(labels, labelCfg.Plan) {
		g.JoinIssueConversation = true
	}
	if hasLabel(labels, labelCfg.Implement) {
		g.OpenPR = true
	}
	if hasLabel(labels, labelCfg.Review) {
		g.PostReview = true
		g.JoinPRConversation = true
	}
	// quack:fix and authorship share one fork-gated path.
	if hasLabel(labels, labelCfg.Implement) || hasLabel(labels, labelCfg.Fix) || authoredByQuack {
		g.JoinPRConversation = true
		if !forkHead {
			g.PushCommitsToPR = true
		}
	}
	return g
}
