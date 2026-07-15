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
	"regexp"
	"strings"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
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

// pullRequestPayload is the subset of GitHub's pull_request webhook we use
// (opened / labeled actions, for the pr_opened and label triggers).
type pullRequestPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Label struct {
		Name string `json:"name"` // present on the "labeled" action
	} `json:"label"`
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

// autoReviewTask is the synthesized request for a pr_opened/label-triggered
// auto-review — there is no human comment to extract a task from.
const autoReviewTask = "Review this pull request."

// autoReviewUser is the synthetic "commenter" attributed to an auto-review run
// (no human triggered it).
const autoReviewUser = "quack-auto-review"

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

	switch r.Header.Get("X-GitHub-Event") {
	case "issue_comment":
		e.handleIssueComment(w, body)
	case "pull_request":
		e.handlePullRequest(w, body)
	default:
		w.WriteHeader(http.StatusOK) // unhandled event type: no-op ack
	}
}

func (e *Extension) handleIssueComment(w http.ResponseWriter, body []byte) {
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

// handlePullRequest fires an auto-review on "opened" (pr_opened trigger) or
// "labeled" with the configured auto_review_label (label trigger). There is no
// comment to react to, so no ackReaction here.
func (e *Extension) handlePullRequest(w http.ResponseWriter, body []byte) {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	fires := (p.Action == "opened" && e.triggers["pr_opened"]) ||
		(p.Action == "labeled" && e.triggers["label"] && p.Label.Name == e.autoReviewLabel)
	if !fires {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Reuse the mention path's dispatch/runMessage by shaping the PR event as an
	// issueCommentPayload — same session key, same review-tool guidance, no
	// duplicated prompt.
	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = p.Number
	synthetic.Issue.PullRequest = &struct{}{}
	synthetic.Comment.User.Login = autoReviewUser
	synthetic.Repository.Name = p.Repository.Name
	synthetic.Repository.Owner.Login = p.Repository.Owner.Login
	synthetic.Repository.CloneURL = p.Repository.CloneURL
	synthetic.Repository.DefaultBranch = p.Repository.DefaultBranch
	synthetic.Installation.ID = p.Installation.ID

	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
		"action", p.Action, "installation", p.Installation.ID)
	go e.dispatch(synthetic, autoReviewTask)
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
	if !e.triggers["mention"] {
		return "", false
	}
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
// slow run never blocks the webhook ack. headSHA is the PR's current head
// commit when known (the pr_opened/label auto-review path carries it from the
// PR event) — dispatch fetches the PR's head/base refs authoritatively anyway.
func (e *Extension) dispatch(p issueCommentPayload, task string) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	// Serialise runs on one PR: a follow-up that lands while a review is still
	// running must WAIT, not run concurrently on the same session (concurrent runs
	// corrupt each other — the answer skip and cross-run tool events seen in
	// dogfooding). The webhook already 202'd, so blocking this goroutine is fine.
	lock := e.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	var rc reviewContext
	if p.Issue.PullRequest != nil {
		rc = e.gatherReviewContext(ctx, owner, repo, number)
	}
	message := e.runMessage(p, task, rc)

	slog.Info("github run dispatched", "component", "github", "repo", owner+"/"+repo, "issue", number)
	// A WORK request (review/implement) always runs as a plan. A mid-tier
	// orchestrator model sometimes answers in prose without calling plan — "Let me
	// start by cloning the repo…" — and that preamble would be posted as if it were
	// the review. So if a work request ran no plan, nudge once to actually run it.
	// A purely conversational follow-up ("what did you mean by that finding?")
	// legitimately runs no plan and must be answered directly — never nudged.
	planRan, delivered := e.drive(ctx, sessionID, message, owner, repo, number)
	if !planRan && isWorkRequest(task) {
		slog.Warn("github: work request produced no plan; nudging it to run the work once",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		_, d2 := e.drive(ctx, sessionID, runNudge, owner, repo, number)
		delivered = delivered || d2
	}
	// The review (github_submit_review) or PR (github_pull_request) IS the
	// deliverable and is already on the PR — posting the run's text summary too
	// would duplicate it. Only fall back to a summary comment when nothing was
	// delivered (a conversational answer, or a run that produced only text).
	if delivered {
		slog.Info("github: work delivered on the PR; skipping the duplicate summary comment",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		return
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

// runNudge is delivered when a webhook run answered without running a plan — a
// firm instruction to actually do the work rather than narrate intent.
const runNudge = "You answered without running anything. Do NOT reply in prose: use the plan and execute tools NOW to actually clone the repo, read the change, and carry out the review (or the requested change). Nothing has run yet and the user is waiting."

// drive runs one orchestrator turn to completion and reports whether it EXECUTED
// a plan (a dag_plan event) and whether it DELIVERED to GitHub — submitted a
// review or opened a PR. A run with no plan produced only a direct-text answer
// (the work never happened); a run that delivered has already posted its output,
// so dispatch skips the redundant summary comment.
func (e *Extension) drive(ctx context.Context, sessionID, message, owner, repo string, number int) (planRan, delivered bool) {
	for ev, err := range e.runner.Run(ctx, runUserID, sessionID, message, nil) {
		if err != nil {
			slog.Warn("github run error", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
			continue
		}
		switch ev.Name {
		case stream.EventDagPlan:
			planRan = true
		case stream.EventAgentToolCall:
			if d, ok := ev.Data.(stream.AgentToolCallData); ok && (d.Name == "github_submit_review" || d.Name == "github_pull_request") {
				delivered = true
			}
		}
	}
	return planRan, delivered
}

// reviewContext is everything the orchestrator needs to PLAN a PR review without
// a node cloning first — the webhook payload carries none of it. Every field is
// best-effort: a failed fetch just omits that slice of context.
type reviewContext struct {
	meta          prMeta
	files         []changedFile
	discussion    prDiscussion
	prevReviewSHA string
}

// gatherReviewContext assembles a PR's plan-time context from the GitHub API.
// Each fetch is independent and best-effort — a failure logs and omits that part
// rather than sinking the whole run.
func (e *Extension) gatherReviewContext(ctx context.Context, owner, repo string, number int) reviewContext {
	var rc reviewContext
	if m, err := e.app.pullMeta(ctx, owner, repo, number); err != nil {
		slog.Warn("github: pullMeta failed; planner lacks the PR's intent and refs",
			"component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
	} else {
		rc.meta = m
	}
	if f, err := e.app.pullFiles(ctx, owner, repo, number); err != nil {
		slog.Warn("github: pullFiles failed; planner cannot slice the review by file",
			"component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
	} else {
		rc.files = f
	}
	if d, err := e.app.listPRDiscussion(ctx, owner, repo, number); err != nil {
		slog.Warn("github: listPRDiscussion failed; planner lacks prior discussion",
			"component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
	} else {
		rc.discussion = d
	}
	if sha, err := e.app.lastReviewedSHA(ctx, owner, repo, number); err != nil {
		slog.Warn("github: lastReviewedSHA failed; first-time review framing",
			"component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
	} else {
		rc.prevReviewSHA = sha
	}
	return rc
}

// workVerbRe matches an imperative that asks quack to DO something — review or
// change code — as opposed to discussing it. CLAUSE-ANCHORED (start of text, after
// sentence punctuation, or after please/and/then/also/to) so a verb buried
// mid-sentence does not trip it: "No need to re-review" must NOT read as a review
// request. An ALLOWLIST on purpose — a request matching nothing here is treated as
// conversational and NOT nudged, so "what did you mean by that finding?" is
// answered directly instead of being shoved into a DAG.
var workVerbRe = regexp.MustCompile(`(?i)(?:^|[.;:!?\n]\s*|\b(?:please|and|then|also|to)\s+)(review|audit|critique|assess|implement|fix|add|create|refactor|rewrite|write|update|change|remove|delete|rename|migrate|investigate|analy[sz]e|build|check)\b`)

// isWorkRequest reports whether a webhook task asks quack to DO work (so a run
// that produced no plan should be nudged) versus a purely conversational
// follow-up (answered directly, never nudged).
func isWorkRequest(task string) bool {
	return vetting.ImplementationIntent(task) || workVerbRe.MatchString(task)
}

// changedFilesSummary renders a compact, capped changed-files list for the plan
// prompt — paths + churn so the planner can slice the review by area. Capped so a
// huge PR doesn't blow the prompt; the total count still conveys the scale.
func changedFilesSummary(files []changedFile) string {
	if len(files) == 0 {
		return ""
	}
	const cap = 60
	var b strings.Builder
	fmt.Fprintf(&b, "Changed files (%d):\n", len(files))
	for i, f := range files {
		if i == cap {
			fmt.Fprintf(&b, "  … and %d more\n", len(files)-cap)
			break
		}
		fmt.Fprintf(&b, "  %s (+%d/-%d)\n", f.Filename, f.Additions, f.Deletions)
	}
	return b.String()
}

// discussionSummary renders the PR's existing conversation, inline review
// comments and prior reviews so the reviewer does not repeat what's been said.
// Capped per section to bound the prompt.
func discussionSummary(d prDiscussion) string {
	const perSection = 40
	var b strings.Builder
	if len(d.Reviews) > 0 {
		b.WriteString("Prior reviews:\n")
		for i, r := range d.Reviews {
			if i == perSection {
				fmt.Fprintf(&b, "  … and %d more\n", len(d.Reviews)-perSection)
				break
			}
			fmt.Fprintf(&b, "  @%s [%s]: %s\n", r.User, r.State, truncate(r.Body, 300))
		}
	}
	if len(d.ReviewComments) > 0 {
		b.WriteString("Inline comments:\n")
		for i, c := range d.ReviewComments {
			if i == perSection {
				fmt.Fprintf(&b, "  … and %d more\n", len(d.ReviewComments)-perSection)
				break
			}
			fmt.Fprintf(&b, "  @%s %s:%d: %s\n", c.User, c.Path, c.Line, truncate(c.Body, 200))
		}
	}
	if len(d.Comments) > 0 {
		b.WriteString("Conversation:\n")
		for i, c := range d.Comments {
			if i == perSection {
				fmt.Fprintf(&b, "  … and %d more\n", len(d.Comments)-perSection)
				break
			}
			fmt.Fprintf(&b, "  @%s: %s\n", c.User, truncate(c.Body, 300))
		}
	}
	return b.String()
}

// truncate shortens s to at most n runes, marking any cut with an ellipsis.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// runMessage frames the task for the orchestrator: the user's verbatim request
// (kept front-and-center — it carries their focus), where the repo is, and how to
// act. A PR request with no implement-and-deliver intent is a REVIEW and MUST
// carry no commit/push/PR language: otherwise the planner echoes it into the node
// task and the vetting completion gate reads a phantom delivery demand off it
// (delivery.go's demandedDelivery), looping the worker — re-cloning, re-reviewing
// — until maxContinueRounds. vetting.ImplementationIntent is the SAME
// discriminator the planner backstop uses, so the two can't drift.
//
// rc carries the plan-time PR context the webhook payload lacks — head/base refs
// (a shallow clone's `git diff base...HEAD` is empty until the head is checked
// out), the PR's title/description (intent), the changed-files list (so the
// planner can slice the review by area), the existing discussion (so the reviewer
// doesn't repeat it), and quack's last-reviewed commit (change-aware follow-ups).
func (e *Extension) runMessage(p issueCommentPayload, task string, rc reviewContext) string {
	isPR := p.Issue.PullRequest != nil
	reviewOnly := isPR && !vetting.ImplementationIntent(task)
	kind := "issue"
	if isPR {
		kind = "pull request"
	}
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	base := p.Repository.DefaultBranch
	if base == "" {
		base = "main"
	}
	diffBase := rc.meta.BaseRef
	if diffBase == "" {
		diffBase = base
	}
	headSHA := rc.meta.HeadSHA
	if headSHA == "" {
		headSHA = "HEAD"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are handling a request from GitHub user @%s, who mentioned you on %s/%s %s #%d.\n\n",
		p.Comment.User.Login, owner, repo, kind, p.Issue.Number)
	fmt.Fprintf(&b, "Their request:\n%s\n\n", task)
	fmt.Fprintf(&b, "The repository is %s/%s (default branch %q); clone it from %s using git_clone (authentication is handled for you). ",
		owner, repo, base, p.Repository.CloneURL)
	if isPR {
		fmt.Fprintf(&b, "This is pull request #%d (pull_number=%d).\n\n", p.Issue.Number, p.Issue.Number)
		if t := strings.TrimSpace(rc.meta.Title); t != "" {
			fmt.Fprintf(&b, "PR title: %s\n", t)
		}
		if body := strings.TrimSpace(rc.meta.Body); body != "" {
			fmt.Fprintf(&b, "PR description:\n%s\n", truncate(body, 1500))
		}
		if s := changedFilesSummary(rc.files); s != "" {
			b.WriteString("\n" + s)
		}
		if s := discussionSummary(rc.discussion); s != "" {
			b.WriteString("\nExisting discussion — take it into account, do NOT repeat it:\n" + s)
		}
		b.WriteString("\n")
		if rc.meta.HeadRef != "" {
			// git_clone gives a shallow BASE branch, where `git diff base...HEAD` is
			// EMPTY. The reviewer MUST check out the head branch to see the changes.
			fmt.Fprintf(&b, "The PR's changes are on branch `%s` (head commit `%s`), based on `%s`. A git_clone gives only the base branch, so `git diff %s...HEAD` is EMPTY until you check out the head: run git_checkout `%s` FIRST (fetch/unshallow if needed), then `git_diff %s...%s` is exactly this PR's diff. Do this before reviewing — the base branch alone shows no changes. ",
				rc.meta.HeadRef, headSHA, diffBase, diffBase, rc.meta.HeadRef, diffBase, rc.meta.HeadRef)
		}
		if rc.prevReviewSHA != "" {
			fmt.Fprintf(&b, "You previously reviewed this pull request at commit `%s`. The current head is `%s`. Focus your review on what changed since then — use git_diff %s..%s (or git log %s..%s) — and take the existing review discussion into account; do NOT repeat findings you already made. ",
				rc.prevReviewSHA, headSHA, rc.prevReviewSHA, headSHA, rc.prevReviewSHA, headSHA)
		}
		lead := "If the request is to REVIEW this PR: read its changes"
		if reviewOnly {
			lead = "Review it: read its changes"
		}
		fmt.Fprintf(&b, "%s (git_diff after cloning) and its existing discussion (github_list_pr_comments — inline comments, conversation, prior reviews) so you don't repeat what's been said, then deliver your review with the review tools — record each finding the moment you spot it with github_add_review_comment (owner=%s, repo=%s, pull_number=%d, path, line — validated against the diff), and finish with github_submit_review (pull_number=%d) carrying a summary body and an event verdict (APPROVE / REQUEST_CHANGES / COMMENT). ",
			lead, owner, repo, p.Issue.Number, p.Issue.Number)
	}
	if reviewOnly {
		// No commit/push/PR words: a review posts findings, it does not deliver code.
		b.WriteString("This is a REVIEW-ONLY task: do NOT create a branch, commit, push, or open a pull request — deliver your findings with the review tools. ")
		fmt.Fprintf(&b, "You may post progress with github_comment (owner=%s, repo=%s, issue_number=%d); your final answer is posted back automatically. ",
			owner, repo, p.Issue.Number)
		b.WriteString("Answer concisely and reference the review you posted.")
		return b.String()
	}
	b.WriteString("If the task needs code changes, create a branch, commit your work, push it with git_push, then open a pull request with github_pull_request ")
	fmt.Fprintf(&b, "(owner=%s, repo=%s, base=%q). ", owner, repo, base)
	fmt.Fprintf(&b, "You may post progress with github_comment (owner=%s, repo=%s, issue_number=%d); your final answer is posted back automatically. ",
		owner, repo, p.Issue.Number)
	b.WriteString("Answer concisely and reference any branch, PR, or review you created.")
	return b.String()
}
