package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/memory"
)

// GitHubPR: repo/PR the conversation is happening on.
type GitHubPR struct {
	Owner   string
	Repo    string
	Number  int
	HeadRef string
}

type githubPRContextKey struct{}

// WithGitHubPR: attaches authoritative repo/PR to ctx.
func WithGitHubPR(ctx context.Context, owner, repo string, number int, headRef string) context.Context {
	return context.WithValue(ctx, githubPRContextKey{}, GitHubPR{Owner: owner, Repo: repo, Number: number, HeadRef: headRef})
}

// GitHubPRFromContext: reads back the PR attached by WithGitHubPR.
func GitHubPRFromContext(ctx context.Context) (GitHubPR, bool) {
	pr, ok := ctx.Value(githubPRContextKey{}).(GitHubPR)
	return pr, ok
}

// correctReviewFindingArgs: model supplies what to correct; repo/PR from GitHubPR context.
type correctReviewFindingArgs struct {
	Finding string `json:"finding"`
	Reason  string `json:"reason"`
}

// falsePositiveCandidate: shapes correction into coding memory scope.
func falsePositiveCandidate(pr GitHubPR, a correctReviewFindingArgs) (memory.Scope, memory.Candidate, error) {
	finding, reason := strings.TrimSpace(a.Finding), strings.TrimSpace(a.Reason)
	if finding == "" || reason == "" {
		return memory.Scope{}, memory.Candidate{}, fmt.Errorf("correct_review_finding: finding and reason are both required")
	}
	content := fmt.Sprintf("False positive on %s/%s PR #%d: %q was flagged in review but is NOT a real issue - %s",
		pr.Owner, pr.Repo, pr.Number, finding, reason)
	sc := memory.Scope{
		Repo: strings.ToLower(fmt.Sprintf("github.com/%s/%s", pr.Owner, pr.Repo)),
		Role: memory.RoleCoding,
	}
	cand := memory.Candidate{Content: content, Metadata: map[string]string{"kind": "false_positive"}}
	return sc, cand, nil
}

// NewCorrectReviewFindingTool: orchestrator's write path into coding memory.
func NewCorrectReviewFindingTool(store *memory.Store, pr GitHubPR) (tool.Tool, error) {
	return functiontool.New[correctReviewFindingArgs, string](
		functiontool.Config{
			Name:        "correct_review_finding",
			Description: "Record that a code-review finding quack previously posted on THIS pull request was a FALSE POSITIVE, so future reviews of similar code don't repeat it. Call this ONLY when a maintainer explicitly says a specific finding was wrong - never for general commentary or unconfirmed doubts. `finding` is the specific claim that was wrong, one sentence; `reason` is why it's actually fine, in the maintainer's own words.",
		},
		func(ctx agent.Context, a correctReviewFindingArgs) (string, error) {
			sc, cand, err := falsePositiveCandidate(pr, a)
			if err != nil {
				return "", err
			}
			// Timeout so a stalled model can't hang the orchestrator.
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if _, err := store.Commit(cctx, sc, "orchestrator", []memory.Candidate{cand}, ""); err != nil {
				return "", fmt.Errorf("correct_review_finding: %w", err)
			}
			return "Recorded - future reviews of this repo won't repeat that finding.", nil
		},
	)
}
