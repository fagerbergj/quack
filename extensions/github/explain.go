package github

import (
	"context"
	"fmt"
	"strings"
)

// explainSessionID keys an /explain chat per requester, apart from the PR's review
// session; the number stays last because quack reads a trailing -<N> as the PR.
func explainSessionID(owner, repo string, number int, login string) string {
	return fmt.Sprintf("github-%s-%s-explain-%s-%d", owner, repo, strings.ToLower(login), number)
}

// shapeExplain marks p as an /explain run and returns its task. The task
// opens the deliverable so the orchestrator routes it to the PR tutor.
func shapeExplain(p *issueCommentPayload) string {
	task := fmt.Sprintf("Walk me through and quiz me on PR %s/%s#%d", p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number)
	p.explain = true
	p.deliverableHint = task + ": an interactive walkthrough and quiz rendered in quack's chat UI. Post nothing to GitHub - no review, no comment, no commit."
	return task
}

// explainLinkText names where the walkthrough lives, without an @ (posted
// bodies never ping). No public_url means no link, like the run footer.
func explainLinkText(publicURL, login, chatID string) string {
	if publicURL == "" {
		return fmt.Sprintf("Walkthrough and quiz for %s: quack chat `%s`", login, chatID)
	}
	return fmt.Sprintf("Walkthrough and quiz for %s: %s/chat/%s", login, publicURL, chatID)
}

// postExplainLink is the only thing an /explain run posts. Best effort.
func (e *Extension) postExplainLink(ctx context.Context, owner, repo string, number int, login, chatID string) {
	if err := e.app.postIssueComment(ctx, owner, repo, number, explainLinkText(e.app.publicURL, login, chatID)); err != nil {
		e.host.Log.Warn("github: explain link comment failed", "repo", owner+"/"+repo, "pr", number, "err", err)
	}
}

func (e *Extension) postExplainLinkNow(owner, repo string, number int, login, chatID string) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	e.postExplainLink(ctx, owner, repo, number, login, chatID)
}
