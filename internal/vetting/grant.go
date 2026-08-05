package vetting

import "fmt"

// Grant: permissions from GitHub trigger (webhook dispatch). nil = unrestricted (no GitHub trigger).
type Grant struct {
	PRScoped              bool // true for PR-scoped trigger; pull_request = push to existing PR, not open new
	JoinIssueConversation bool
	OpenPR                bool
	PostReview            bool
	JoinPRConversation    bool
	PushCommitsToPR       bool
}

// allows: is a staged delivery kind permitted? nil receiver permits everything.
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
