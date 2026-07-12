package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// maxWebhookBody caps the raw webhook payload we read (GitHub payloads are well
// under this; the cap bounds a hostile/oversized request).
const maxWebhookBody = 5 << 20 // 5 MiB

// issueCommentPayload is the subset of GitHub's issue_comment webhook we use.
type issueCommentPayload struct {
	Action  string `json:"action"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number int `json:"number"`
		// PullRequest is present only when the issue is a PR (GitHub sends PR
		// conversation comments as issue_comment events).
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// handleWebhook is the inbound trust boundary: it verifies the HMAC signature
// over the RAW body before doing anything else, then dispatches by event type
// and returns FAST — the orchestrator run happens in a goroutine (GitHub
// enforces a ~10s webhook timeout).
func (e *Extension) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if !verifySignature(e.secret, body, r.Header.Get("X-Hub-Signature-256")) {
		slog.Warn("github webhook: signature verification failed", "component", "github")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event != "issue_comment" {
		w.WriteHeader(http.StatusOK) // unhandled event type: no-op ack
		return
	}

	var p issueCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	task, ok := e.triggerTask(p)
	if !ok {
		w.WriteHeader(http.StatusOK) // not a mention we act on: no-op ack
		return
	}

	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number,
		"user", p.Comment.User.Login, "installation", p.Installation.ID)
	go e.ackReaction(p) // instant 👀 "quack saw it", independent of the model run
	go e.dispatch(p, task)
	w.WriteHeader(http.StatusAccepted)
}

// ackReaction posts a deterministic 👀 (eyes) reaction on the mentioning comment
// as an instant, code-level acknowledgment that quack saw the mention — it does
// not wait on the model. Best effort: a failure is logged at WARN and never
// blocks the run dispatch (the reaction is a nicety, not a gate).
func (e *Extension) ackReaction(p issueCommentPayload) {
	if p.Comment.ID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	if _, err := e.app.reactToComment(ctx, owner, repo, "issues", p.Comment.ID, "eyes"); err != nil {
		slog.Warn("github ack reaction failed", "component", "github",
			"repo", owner+"/"+repo, "comment", p.Comment.ID, "err", err)
	}
}

// triggerTask decides whether a comment triggers a run and extracts the task
// text after the mention. The trigger is: a created issue_comment whose body
// contains the configured mention. Returns ("", false) otherwise.
func (e *Extension) triggerTask(p issueCommentPayload) (string, bool) {
	if p.Action != "created" {
		return "", false
	}
	body := p.Comment.Body
	i := strings.Index(strings.ToLower(body), strings.ToLower(e.mention))
	if i < 0 {
		return "", false
	}
	task := strings.TrimSpace(body[i+len(e.mention):])
	if task == "" {
		return "", false
	}
	return task, true
}

// verifySignature checks GitHub's X-Hub-Signature-256 (HMAC-SHA256 of the raw
// body, hex, prefixed "sha256=") against the configured secret using a
// CONSTANT-TIME compare. A missing/malformed header or any mismatch is false —
// this is the trust boundary and there is no bypass.
func verifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	// hmac.Equal is constant time; comparing the full "sha256=" strings is fine.
	return hmac.Equal([]byte(header), []byte(expected))
}

// dispatch runs the orchestrator on the task and posts the final answer back as
// a comment. Runs in its own goroutine with a detached, bounded context so a
// slow run never blocks the webhook ack.
func (e *Extension) dispatch(p issueCommentPayload, task string) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	message := e.runMessage(p, task)

	slog.Info("github run dispatched", "component", "github", "repo", owner+"/"+repo, "issue", number)
	// Drain the stream to drive the run to completion; the answer is read from
	// the persisted session afterwards.
	for _, err := range e.runner.Run(ctx, runUserID, sessionID, message, nil) {
		if err != nil {
			slog.Warn("github run error", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		}
	}
	answer := strings.TrimSpace(e.runner.LatestAnswer(ctx, runUserID, sessionID))
	if answer == "" {
		answer = "quack finished but produced no answer."
	}
	if err := e.app.postIssueComment(ctx, owner, repo, number, answer); err != nil {
		slog.Error("github comment post failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		return
	}
	slog.Info("github comment posted", "component", "github", "repo", owner+"/"+repo, "issue", number)
}

// runMessage frames the task for the orchestrator: the user's verbatim request
// (kept front-and-center — it carries their focus), where the repo is, and how to
// act. It gives BOTH paths — review and implement — since the orchestrator routes
// review-vs-change from the request; when the mention is on a PR it also surfaces
// the pull_number the review tools need (a PR shares its issue number).
func (e *Extension) runMessage(p issueCommentPayload, task string) string {
	isPR := p.Issue.PullRequest != nil
	kind := "issue"
	if isPR {
		kind = "pull request"
	}
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	base := p.Repository.DefaultBranch
	if base == "" {
		base = "main"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are handling a request from GitHub user @%s, who mentioned you on %s/%s %s #%d.\n\n",
		p.Comment.User.Login, owner, repo, kind, p.Issue.Number)
	fmt.Fprintf(&b, "Their request:\n%s\n\n", task)
	fmt.Fprintf(&b, "The repository is %s/%s (default branch %q); clone it from %s using git_clone (authentication is handled for you). ",
		owner, repo, base, p.Repository.CloneURL)
	if isPR {
		fmt.Fprintf(&b, "This is pull request #%d (pull_number=%d). ", p.Issue.Number, p.Issue.Number)
		fmt.Fprintf(&b, "If the request is to REVIEW this PR: read its changes (git_diff after cloning) and its existing discussion (github_list_pr_comments — inline comments, conversation, prior reviews) so you don't repeat what's been said, then deliver your review with the review tools — record each finding the moment you spot it with github_add_review_comment (owner=%s, repo=%s, pull_number=%d, path, line — validated against the diff), and finish with github_submit_review (pull_number=%d) carrying a summary body and an event verdict (APPROVE / REQUEST_CHANGES / COMMENT). ",
			owner, repo, p.Issue.Number, p.Issue.Number)
	}
	b.WriteString("If the task needs code changes, create a branch, commit your work, push it with git_push, then open a pull request with github_pull_request ")
	fmt.Fprintf(&b, "(owner=%s, repo=%s, base=%q). ", owner, repo, base)
	fmt.Fprintf(&b, "You may post progress with github_comment (owner=%s, repo=%s, issue_number=%d); your final answer is posted back automatically. ",
		owner, repo, p.Issue.Number)
	b.WriteString("Answer concisely and reference any branch, PR, or review you created.")
	return b.String()
}
