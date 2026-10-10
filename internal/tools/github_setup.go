package tools

import (
	"context"

	"github.com/fagerbergj/quack/internal/dag"
)

type githubSetupContextKey struct{}

// WithGitHubSetup: Repo/BaseRef are ground truth; WorkBranch is a default unless CheckoutExistingHead.
// Call only from the dispatch boundary, never with anything fed by model output.
func WithGitHubSetup(ctx context.Context, s dag.Setup) context.Context {
	return context.WithValue(ctx, githubSetupContextKey{}, s)
}

func GitHubSetupFromContext(ctx context.Context) (dag.Setup, bool) {
	s, ok := ctx.Value(githubSetupContextKey{}).(dag.Setup)
	return s, ok
}
