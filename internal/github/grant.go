package github

import (
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/vetting"
)

// computeGrant derives a run's permission set from the labels CURRENTLY on
// the issue/PR, whether quack itself authored the PR, and whether the PR's
// head is a fork (#657, #662) - the deterministic facts the gate's
// commitDelivery enforces, never re-derived by the planner or the model.
// prScoped is true once the triggering thread already names a PR (an issue
// has none yet); authoredByQuack and forkHead are meaningless (ignored) when
// it's false.
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
	// quack:fix and PR authorship both grant the same thing - the ability to
	// push a fix/commit to the PR - so they share one fork-gated branch
	// rather than risking the two drifting apart.
	if hasLabel(labels, labelCfg.Implement) || hasLabel(labels, labelCfg.Fix) || authoredByQuack {
		g.JoinPRConversation = true
		if !forkHead {
			g.PushCommitsToPR = true
		}
	}
	return g
}
