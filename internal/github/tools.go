package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/tools"
)

// gitHost is the only host this extension supplies credentials for.
const gitHost = "github.com"

// gitUsername is GitHub's recommended placeholder username for token auth
// (the token itself is the password) — mirrors the git-credential default.
const gitUsername = "x-access-token"

// GitCredential implements tools.GitTokenSource: for a github.com clone/remote
// URL it resolves the repo's installation and mints a fresh installation token,
// injected as the git credential. Returns (nil, nil) for any other host so the
// git op proceeds unauthenticated / falls back to a static credential.
func (a *App) GitCredential(ctx context.Context, rawURL string) (*tools.GitCredential, error) {
	owner, repo, ok := ownerRepoFromURL(rawURL)
	if !ok {
		return nil, nil
	}
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("github: mint git credential for %s/%s: %w", owner, repo, err)
	}
	return &tools.GitCredential{Host: gitHost, Username: gitUsername, Token: tok}, nil
}

// ownerRepoFromURL extracts owner/repo from a github.com https URL, e.g.
// https://github.com/acme/widgets(.git) → ("acme","widgets"). ok is false for
// any non-github.com host or a path without both segments.
func ownerRepoFromURL(rawURL string) (owner, repo string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(u.Hostname(), gitHost) {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

// Tools are the extension's outbound capabilities, authed via the App's
// installation token. Kept minimal for the MVP; richer tools (create issue,
// request review, labels, check-runs) are documented follow-ups.
func (a *App) Tools() []tool.Tool {
	return []tool.Tool{a.commentTool(), a.pullRequestTool()}
}

type commentArgs struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	IssueNumber int    `json:"issue_number"`
	Body        string `json:"body"`
}

type commentResult struct {
	Posted bool `json:"posted"`
}

func (a *App) commentTool() tool.Tool {
	t, _ := functiontool.New[commentArgs, commentResult](
		functiontool.Config{
			Name: "github_comment",
			Description: "Post a comment on a GitHub issue or pull request (PR conversation comments are " +
				"issue comments). `owner`/`repo` identify the repository, `issue_number` the issue/PR number, " +
				"`body` the markdown comment text. Authenticated as the app installation.",
		},
		func(ctx adkagent.Context, args commentArgs) (commentResult, error) {
			if args.Owner == "" || args.Repo == "" || args.IssueNumber == 0 || strings.TrimSpace(args.Body) == "" {
				return commentResult{}, fmt.Errorf("github_comment: owner, repo, issue_number and body are all required")
			}
			if err := a.postIssueComment(ctx, args.Owner, args.Repo, args.IssueNumber, args.Body); err != nil {
				return commentResult{}, err
			}
			return commentResult{Posted: true}, nil
		},
	)
	return t
}

type prArgs struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base,omitempty"` // default "main"
	Body  string `json:"body,omitempty"`
}

type prResult struct {
	URL string `json:"url"`
}

func (a *App) pullRequestTool() tool.Tool {
	t, _ := functiontool.New[prArgs, prResult](
		functiontool.Config{
			Name: "github_pull_request",
			Description: "Open a pull request. `head` is the branch you pushed (must already exist on the remote — " +
				"push it with git_push first); `base` is the target branch (default `main`). `title`/`body` are the " +
				"PR text. Returns the PR URL. Authenticated as the app installation.",
		},
		func(ctx adkagent.Context, args prArgs) (prResult, error) {
			if args.Owner == "" || args.Repo == "" || strings.TrimSpace(args.Title) == "" || args.Head == "" {
				return prResult{}, fmt.Errorf("github_pull_request: owner, repo, title and head are all required")
			}
			base := args.Base
			if base == "" {
				base = "main"
			}
			u, err := a.createPullRequest(ctx, args.Owner, args.Repo, args.Title, args.Head, base, args.Body)
			if err != nil {
				return prResult{}, err
			}
			return prResult{URL: u}, nil
		},
	)
	return t
}
