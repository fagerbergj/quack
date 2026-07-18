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

// correctReviewFindingArgs is one explicit, attributable correction: a
// maintainer told quack (conversationally) that a specific finding it posted
// on a specific PR was wrong, and why.
type correctReviewFindingArgs struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
	Finding  string `json:"finding"`
	Reason   string `json:"reason"`
}

// falsePositiveCandidate shapes a correction into the SAME scope a code
// review's gate-side recall reads (memory.Scope{Repo, Role: RoleCoding} — see
// internal/vetting/node.go's memoryScope), so it reaches code-reviewer (and
// code-implementer/code-explorer, which share the repo bucket) before their
// next run on this repo. Kept separate from the tool wrapper so the shaping
// and validation are unit-testable without an agent.Context.
func falsePositiveCandidate(a correctReviewFindingArgs) (memory.Scope, memory.Candidate, error) {
	owner, repo := strings.TrimSpace(a.Owner), strings.TrimSpace(a.Repo)
	finding, reason := strings.TrimSpace(a.Finding), strings.TrimSpace(a.Reason)
	if owner == "" || repo == "" || a.PRNumber <= 0 || finding == "" || reason == "" {
		return memory.Scope{}, memory.Candidate{}, fmt.Errorf("correct_review_finding: owner, repo, pr_number, finding and reason are all required")
	}
	content := fmt.Sprintf("False positive on %s/%s PR #%d: %q was flagged in review but is NOT a real issue — %s",
		owner, repo, a.PRNumber, finding, reason)
	sc := memory.Scope{
		Repo: strings.ToLower(fmt.Sprintf("github.com/%s/%s", owner, repo)),
		Role: memory.RoleCoding,
	}
	cand := memory.Candidate{Content: content, Metadata: map[string]string{"kind": "false_positive"}}
	return sc, cand, nil
}

// NewCorrectReviewFindingTool builds the orchestrator's ONE write path from a
// conversational turn into the shared CODING memory bucket: recording that a
// review finding quack posted was a false positive. Unlike commit_memory
// (arbitrary user facts), this tool accepts nothing but a structured,
// attributable correction — which repo, which PR, which finding, why it was
// wrong — so a conversational reply can never stage free-form or unattributed
// memory (#249). The write still runs through Store.Commit's vet +
// consolidation pass, exactly like a worker's gated commit.
func NewCorrectReviewFindingTool(store *memory.Store) (tool.Tool, error) {
	return functiontool.New[correctReviewFindingArgs, string](
		functiontool.Config{
			Name:        "correct_review_finding",
			Description: "Record that a code-review finding quack previously posted on a GitHub PR was a FALSE POSITIVE, so future reviews of similar code don't repeat it. Call this ONLY when a maintainer explicitly says a specific finding was wrong — never for general commentary or unconfirmed doubts. `owner`/`repo`/`pr_number` identify the PR the finding was posted on; `finding` is the specific claim that was wrong, one sentence; `reason` is why it's actually fine, in the maintainer's own words.",
		},
		func(ctx agent.Context, a correctReviewFindingArgs) (string, error) {
			sc, cand, err := falsePositiveCandidate(a)
			if err != nil {
				return "", err
			}
			// Bound the consolidation round-trip so a stalled model can't hang
			// the orchestrator's turn (this write is synchronous — the model
			// awaits it), matching commit_memory's timeout.
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if _, err := store.Commit(cctx, sc, "orchestrator", []memory.Candidate{cand}, ""); err != nil {
				return "", fmt.Errorf("correct_review_finding: %w", err)
			}
			return "Recorded — future reviews of this repo won't repeat that finding.", nil
		},
	)
}
