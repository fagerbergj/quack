package tools

import (
	"context"

	"github.com/fagerbergj/quack/internal/dag"
)

type githubSetupContextKey struct{}

// WithGitHubSetup attaches the deterministic dag.Setup facts for a
// GitHub-originated run: Repo and BaseRef are ground truth off the webhook
// event for every such run; WorkBranch is a DEFAULT (quack/issue-<n>) that
// dag.OverrideExistingPRHead (via GitHubPRFromContext's HeadRef) replaces
// with the PR's real head when the run is bound to one. Call ONLY from the
// GitHub webhook dispatch boundary - never from anything fed by model output
// (same rule as WithGitHubPR).
func WithGitHubSetup(ctx context.Context, s dag.Setup) context.Context {
	return context.WithValue(ctx, githubSetupContextKey{}, s)
}

// GitHubSetupFromContext reads back the Setup WithGitHubSetup attached, if
// any. Read once, at the top of Orchestrator.Run, mirroring
// GitHubPRFromContext's contract.
func GitHubSetupFromContext(ctx context.Context) (dag.Setup, bool) {
	s, ok := ctx.Value(githubSetupContextKey{}).(dag.Setup)
	return s, ok
}
