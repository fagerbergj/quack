package tools

import (
	"context"

	"github.com/fagerbergj/quack/internal/dag"
)

type githubSetupContextKey struct{}

// WithGitHubSetup attaches the deterministic dag.Setup facts for a
// dispatch-originated run: Repo and BaseRef are ground truth off the dispatch
// for every such run; WorkBranch is a DEFAULT (quack/issue-<n>) unless
// CheckoutExistingHead marks it as a real existing head (sdk
// Setup.ExistingHeadRef). Call ONLY from the dispatch boundary - never from
// anything fed by model output.
func WithGitHubSetup(ctx context.Context, s dag.Setup) context.Context {
	return context.WithValue(ctx, githubSetupContextKey{}, s)
}

// GitHubSetupFromContext reads back the Setup WithGitHubSetup attached, if
// any. Read once, at the top of Orchestrator.Run.
func GitHubSetupFromContext(ctx context.Context) (dag.Setup, bool) {
	s, ok := ctx.Value(githubSetupContextKey{}).(dag.Setup)
	return s, ok
}
