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

// GitHubPR identifies the real GitHub repo/PR a conversation is happening on.
type GitHubPR struct {
	Owner  string
	Repo   string
	Number int
	// HeadRef is the PR's actual head branch (e.g. "feat/oidc-auth"), used to
	// override a review plan's planner-authored Setup.WorkBranch - see
	// dag.OverrideReviewWorkBranch. Empty for a plain issue (no head branch).
	HeadRef string
}

type githubPRContextKey struct{}

// WithGitHubPR attaches the AUTHORITATIVE repo/PR a conversation is about to
// ctx. It is the ONLY source correct_review_finding trusts for WHERE it may
// write - never owner/repo/pr_number from the model, which a hostile message
// (any chat turn, any MCP call, a comment on an unrelated PR) could forge via
// prompt injection to redirect the write into a repo/PR it has no business
// touching. Call ONLY from the boundary that actually knows this - the GitHub
// webhook, before it dispatches the orchestrator - never from anything fed by
// model output.
func WithGitHubPR(ctx context.Context, owner, repo string, number int, headRef string) context.Context {
	return context.WithValue(ctx, githubPRContextKey{}, GitHubPR{Owner: owner, Repo: repo, Number: number, HeadRef: headRef})
}

// GitHubPRFromContext reads back the PR WithGitHubPR attached, if any. Read
// exactly ONCE, at the top of Orchestrator.Run - before any tool is even
// built - and threaded in as a plain closed-over value (see
// NewCorrectReviewFindingTool), so the tool's write target never depends on a
// context.Value surviving deep inside the agent runtime's tool-call plumbing.
func GitHubPRFromContext(ctx context.Context) (GitHubPR, bool) {
	pr, ok := ctx.Value(githubPRContextKey{}).(GitHubPR)
	return pr, ok
}

// correctReviewFindingArgs is the ONLY part of a correction the model
// supplies. WHICH repo/PR it applies to is never one of them - it comes from
// GitHubPR, the verified conversation context - so nothing the model says can
// redirect where the write lands.
type correctReviewFindingArgs struct {
	Finding string `json:"finding"`
	Reason  string `json:"reason"`
}

// falsePositiveCandidate shapes a correction into the SAME scope a code
// review's gate-side recall reads (memory.Scope{Repo, Role: RoleCoding} - see
// internal/vetting/node.go's memoryScope), keyed on pr (the verified
// conversation context, never model input) so it reaches code-reviewer (and
// code-implementer/code-explorer, which share the repo bucket) before their
// next run on THIS repo. Bucket routing is the Scope ladder's default (Repo
// set, no Metadata["bucket"] override) - Metadata["kind"] below is only a
// label for the consolidator, not the routing key.
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

// NewCorrectReviewFindingTool builds the orchestrator's ONE write path from a
// conversational turn into the shared CODING memory bucket: recording that a
// review finding quack posted on THIS pull request was a false positive. pr
// is resolved ONCE by the caller (Orchestrator.Run, via GitHubPRFromContext)
// and closed over here - the tool never accepts owner/repo/pr_number as
// arguments, so it can only ever write to the repo/PR the conversation is
// actually on, regardless of what a hostile message asks for (#249 hardening:
// this used to trust those fields from the model). Only registered by the
// orchestrator when a verified GitHubPR is present, so a plain REST/MCP turn
// (no PR context) never sees this tool at all.
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
			// Bound the consolidation round-trip so a stalled model can't hang
			// the orchestrator's turn (this write is synchronous - the model
			// awaits it), matching commit_memory's timeout.
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if _, err := store.Commit(cctx, sc, "orchestrator", []memory.Candidate{cand}, ""); err != nil {
				return "", fmt.Errorf("correct_review_finding: %w", err)
			}
			return "Recorded - future reviews of this repo won't repeat that finding.", nil
		},
	)
}
