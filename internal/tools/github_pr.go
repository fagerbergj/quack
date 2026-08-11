package tools

import "context"

// GitHubPR is authoritative repo/PR for a run.
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

// GitHubPRFromContext reads back the PR attached by WithGitHubPR.
func GitHubPRFromContext(ctx context.Context) (GitHubPR, bool) {
	pr, ok := ctx.Value(githubPRContextKey{}).(GitHubPR)
	return pr, ok
}
