package vetting

import "fmt"

// Grant is the permission set a GitHub trigger computed once at webhook
// dispatch, from labels, authorship, and the fork check (#657, #662) -
// carried through the plan as INFORMATION (Config.Grant) so commitDelivery,
// the one place that actually mutates GitHub, can refuse an ungranted
// delivery. A nil *Grant means no GitHub trigger governs this run (a plain
// REST/MCP conversation) - nothing is refused.
type Grant struct {
	// PRScoped is true once the triggering thread already names an existing
	// PR, false for a plain issue. It resolves the one ambiguity in
	// Delivery.Kind: "pull_request" means OPEN a new PR on an issue-scoped
	// run (gated on OpenPR) but PUSH a commit to the existing PR on a
	// PR-scoped run (gated on PushCommitsToPR) - the kind alone can't tell
	// them apart.
	PRScoped bool

	JoinIssueConversation bool
	OpenPR                bool
	PostReview            bool
	JoinPRConversation    bool
	PushCommitsToPR       bool
}

// allows reports whether a staged delivery of the given kind
// ("pull_request" | "review" | "comment") is permitted, and why not when it
// isn't. A nil receiver (no GitHub trigger governs this run) permits
// everything.
func (g *Grant) allows(kind string) (ok bool, reason string) {
	if g == nil {
		return true, ""
	}
	switch kind {
	case "pull_request":
		if g.PRScoped {
			if g.PushCommitsToPR {
				return true, ""
			}
			return false, "push_commits_to_pr not granted"
		}
		if g.OpenPR {
			return true, ""
		}
		return false, "open_pr not granted"
	case "review":
		if g.PostReview {
			return true, ""
		}
		return false, "post_review not granted"
	case "comment":
		if g.PRScoped {
			if g.JoinPRConversation {
				return true, ""
			}
			return false, "join_pr_conversation not granted"
		}
		if g.JoinIssueConversation {
			return true, ""
		}
		return false, "join_issue_conversation not granted"
	default:
		return false, fmt.Sprintf("unknown delivery kind %q", kind)
	}
}
