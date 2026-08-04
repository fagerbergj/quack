package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"

	"github.com/google/uuid"
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
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
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

	// planOnly marks a synthetic payload for a label-driven planning run: the
	// task produces a plan comment and must not touch code. Not part of the
	// GitHub payload.
	planOnly bool
	// isLabelTrigger marks a synthetic payload built from a label/pr_opened
	// event (auto-review, quack:plan, quack:implement) - as opposed to a real
	// @mention comment. dispatch resets the session for a label-triggered work
	// request (T4 session hygiene); a conversational @mention never does. Not
	// part of the GitHub payload.
	isLabelTrigger bool
	// deliverableHint, when set, is the exact <deliverable> text the envelope
	// states - used by a synthetic trigger (CI auto-heal, own-PR review
	// response) whose deliverable is fixed by WHICH webhook dispatched it, not
	// by classifying task text. "" falls back to buildEnvelope's own
	// classification (mention, quack:plan, quack:implement, review). Not part
	// of the GitHub payload.
	deliverableHint string
	// rawEvent + eventName carry the ORIGINATING webhook's own JSON body and
	// its dotted name ("issues.labeled", "pull_request.opened", …) into the
	// trigger envelope's <event> block (#659) - filtered but never reshaped or
	// renamed. Not part of the GitHub payload itself (this struct's OTHER
	// fields already aren't); this is quack's own bookkeeping of which real
	// webhook is being carried through a synthetic issueCommentPayload.
	rawEvent  json.RawMessage
	eventName string
	// checkSHA is the commit whose check runs the sibling context directory
	// should dump (#660) - set only by a workflow_run-triggered fix run; ""
	// skips check-runs.json and every annotations-*.json (a plan/review/
	// mention run has no single check run it's reacting to). Not part of the
	// GitHub payload.
	checkSHA string
}

// issuesPayload is the subset of GitHub's issues webhook we use ("labeled",
// for the label-driven issue workflow).
type issuesPayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		// PullRequest is present when the "issue" is actually a PR.
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Label struct {
		Name string `json:"name"`
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
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// pullRequestPayload is the subset of GitHub's pull_request webhook we use
// (opened / labeled actions, for the pr_opened and label triggers).
type pullRequestPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Title string `json:"title"`
		Head  struct {
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
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// autoReviewTask is the synthesized request for a pr_opened/label-triggered
// auto-review - there is no human comment to extract a task from.
const autoReviewTask = "Review this pull request and post your findings as inline review comments and a verdict."

// autoReviewUser is the synthetic "commenter" attributed to an auto-review run
// (no human triggered it).
const autoReviewUser = "quack-auto-review"

// handleWebhook is the inbound trust boundary: it verifies the HMAC signature
// over the RAW body before doing anything else, then dispatches by event type
// and returns FAST - the orchestrator run happens in a goroutine (GitHub
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
	case "pull_request_review":
		e.handlePullRequestReview(w, body)
	case "issues":
		e.handleIssues(w, body)
	case "workflow_run":
		e.handleWorkflowRun(w, body)
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
	// Never act on another bot's comments - quack's own posted plans/summaries
	// and other integrations must not re-trigger runs.
	// ponytail: rejects all bots, not just self - bots don't mention quack legitimately.
	if strings.HasSuffix(p.Comment.User.Login, "[bot]") {
		w.WriteHeader(http.StatusOK)
		return
	}
	task, ok := e.triggerTask(p)
	if !ok {
		w.WriteHeader(http.StatusOK) // not a mention we act on: no-op ack
		return
	}
	p.rawEvent = json.RawMessage(body)
	p.eventName = "issue_comment." + p.Action
	if !e.isInvokerAllowed(p.Comment.User.Login) {
		slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number, "user", p.Comment.User.Login)
		w.WriteHeader(http.StatusOK)
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
	// The merge label is a human authorization, not an agent run: a deterministic
	// handler checks quack's own review verdict and merges (or explains why not).
	// A bot sender can never authorize a merge.
	if p.Action == "labeled" && e.triggers["merge"] && p.Label.Name == e.labels.Merge &&
		!strings.HasSuffix(p.Sender.Login, "[bot]") {
		if !e.isInvokerAllowed(p.Sender.Login) {
			slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
				"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
				"label", p.Label.Name, "user", p.Sender.Login)
			w.WriteHeader(http.StatusOK)
			return
		}
		slog.Info("github webhook received", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
			"label", p.Label.Name, "user", p.Sender.Login, "installation", p.Installation.ID)
		go e.mergeIfApproved(p, body)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// quack:fix is a PERSISTENT capability flag (#656), not a one-shot trigger:
	// (Re-)applying it re-arms auto-heal and, if CI is CURRENTLY failing,
	// fixes it right away - otherwise it just stays armed for the next CI
	// failure (see handleWorkflowRun/autoHeal, which needs no "labeled" event
	// at all). Bot senders never chain label workflows.
	if p.Action == "labeled" && e.triggers["ci_fix"] && p.Label.Name == e.labels.Fix &&
		!strings.HasSuffix(p.Sender.Login, "[bot]") && e.store != nil {
		if !e.isInvokerAllowed(p.Sender.Login) {
			slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
				"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
				"label", p.Label.Name, "user", p.Sender.Login)
			w.WriteHeader(http.StatusOK)
			return
		}
		slog.Info("github webhook received", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
			"label", p.Label.Name, "user", p.Sender.Login, "installation", p.Installation.ID)
		go e.fixLabelApplied(p, body)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	fires := (p.Action == "opened" && e.triggers["pr_opened"]) ||
		(p.Action == "labeled" && e.triggers["label"] && p.Label.Name == e.labels.Review)
	if !fires {
		w.WriteHeader(http.StatusOK)
		return
	}

	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
		"action", p.Action, "installation", p.Installation.ID)
	go e.dispatch(autoReviewPayload(p, body), autoReviewTask)
	w.WriteHeader(http.StatusAccepted)
}

// autoReviewPayload reuses the mention path's dispatch/envelope builder by
// shaping a PR event as an issueCommentPayload - same session key, same
// review-tool guidance, no duplicated prompt. Shared by the pr_opened/label
// auto-review trigger and the merge label's own "no review yet" auto-dispatch
// (mergeIfApproved). rawBody is the ORIGINATING pull_request webhook's own
// JSON - carried through verbatim (filtered, never reshaped) into the
// envelope's <event> block, even though the trigger itself is re-shaped into
// an issueCommentPayload for dispatch's own bookkeeping.
func autoReviewPayload(p pullRequestPayload, rawBody []byte) issueCommentPayload {
	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = p.Number
	synthetic.Issue.Title = p.PullRequest.Title
	synthetic.Issue.PullRequest = &struct{}{}
	synthetic.Comment.User.Login = autoReviewUser
	synthetic.Repository.Name = p.Repository.Name
	synthetic.Repository.Owner.Login = p.Repository.Owner.Login
	synthetic.Repository.CloneURL = p.Repository.CloneURL
	synthetic.Repository.DefaultBranch = p.Repository.DefaultBranch
	synthetic.Installation.ID = p.Installation.ID
	synthetic.isLabelTrigger = true // auto-review, never a mention (T4)
	synthetic.rawEvent = json.RawMessage(rawBody)
	synthetic.eventName = "pull_request." + p.Action
	return synthetic
}

// pullRequestReviewPayload is the subset of GitHub's pull_request_review
// webhook we use: a submitted request_changes review engages quack on a PR IT
// AUTHORED to address the findings (#656, closes #655) - "authorship IS the
// flag", no label needed.
type pullRequestReviewPayload struct {
	Action string `json:"action"`
	Review struct {
		State string `json:"state"` // "approved" | "changes_requested" | "commented"
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"review"`
	PullRequest struct {
		Number int `json:"number"`
	} `json:"pull_request"`
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

// handlePullRequestReview engages quack on a request_changes review, but ONLY
// on a PR it authored itself - a review on anyone else's PR is left to the
// label/mention triggers, which already cover it. Gated on the ci_fix
// trigger: this and CI auto-heal are the same "quack maintains what it's
// responsible for, autonomously" capability.
func (e *Extension) handlePullRequestReview(w http.ResponseWriter, body []byte) {
	var p pullRequestReviewPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if p.Action != "submitted" || p.Review.State != "changes_requested" || !e.triggers["ci_fix"] {
		w.WriteHeader(http.StatusOK)
		return
	}
	if strings.HasSuffix(p.Review.User.Login, "[bot]") {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !e.isInvokerAllowed(p.Review.User.Login) {
		slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.PullRequest.Number, "user", p.Review.User.Login)
		w.WriteHeader(http.StatusOK)
		return
	}
	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.PullRequest.Number,
		"user", p.Review.User.Login, "installation", p.Installation.ID)
	go e.engageOwnPRReview(p, body)
	w.WriteHeader(http.StatusAccepted)
}

// engageOwnPRReview dispatches a fix-the-findings run on the PR's existing
// session, continuing rather than resetting it - the review itself, its
// inline comments, and everything else on the thread are already what
// loadGithubContext injects every dispatch (#459), so nothing needs to be
// copied out of the review payload here.
func (e *Extension) engageOwnPRReview(p pullRequestReviewPayload, rawBody []byte) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.PullRequest.Number
	ctx, cancel := context.WithTimeout(context.Background(), fixContextTimeout)
	defer cancel()

	authored, err := e.authoredByQuack(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: own-PR authorship check failed; not engaging", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	if !authored {
		return // not quack's PR - the label/mention triggers already cover it
	}

	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	login := p.Review.User.Login
	if e.store != nil {
		login = e.store.SessionUserForChat(ctx, sessionID)
	}

	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = number
	synthetic.Issue.PullRequest = &struct{}{}
	synthetic.Comment.User.Login = login
	synthetic.Repository.Name = repo
	synthetic.Repository.Owner.Login = owner
	synthetic.Repository.CloneURL = p.Repository.CloneURL
	synthetic.Repository.DefaultBranch = p.Repository.DefaultBranch
	synthetic.Installation.ID = p.Installation.ID
	// isLabelTrigger stays false: this continues the PR's existing session.
	synthetic.rawEvent = json.RawMessage(rawBody)
	synthetic.eventName = "pull_request_review." + p.Action
	synthetic.deliverableHint = "a commit addressing every finding in the review that requested changes"

	slog.Info("github: engaging own PR after requested changes", "component", "github", "repo", owner+"/"+repo, "pr", number)
	e.dispatch(synthetic, fmt.Sprintf(
		"@%s requested changes on this pull request, which you authored. Address every finding: read the review comments and the current diff, make the fix, run the repo's own checks to verify, and commit the change on this PR's existing head branch.",
		p.Review.User.Login))
}

// handleIssues drives the label-driven issue workflow: applying the configured
// plan label to an issue dispatches a planning run whose answer (the plan) is
// posted back as an issue comment. Labels double as the permission model -
// only repo-write users can apply them.
func (e *Extension) handleIssues(w http.ResponseWriter, body []byte) {
	var p issuesPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	// Only human-applied labels on REAL issues act (PRs surface labels via the
	// pull_request event; a bot sender must never chain label workflows).
	if p.Action != "labeled" || p.Issue.PullRequest != nil ||
		strings.HasSuffix(p.Sender.Login, "[bot]") {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !e.isInvokerAllowed(p.Sender.Login) {
		slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number,
			"label", p.Label.Name, "user", p.Sender.Login)
		w.WriteHeader(http.StatusOK)
		return
	}
	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = p.Issue.Number
	synthetic.Issue.Title = p.Issue.Title
	synthetic.Comment.User.Login = p.Sender.Login
	synthetic.Repository.Name = p.Repository.Name
	synthetic.Repository.Owner.Login = p.Repository.Owner.Login
	synthetic.Repository.CloneURL = p.Repository.CloneURL
	synthetic.Repository.DefaultBranch = p.Repository.DefaultBranch
	synthetic.Installation.ID = p.Installation.ID
	synthetic.isLabelTrigger = true // quack:plan/quack:implement, never a mention (T4)
	synthetic.rawEvent = json.RawMessage(body)
	synthetic.eventName = "issues.labeled"

	switch {
	case e.triggers["issue_plan"] && p.Label.Name == e.labels.Plan:
		synthetic.planOnly = true
		go e.ackLabelReaction(p) // instant 👀 on the issue - the label path's equivalent of ackReaction
		go e.dispatch(synthetic, planTask(p))
	case e.triggers["issue_implement"] && p.Label.Name == e.labels.Implement:
		go e.ackLabelReaction(p)
		go e.runImplement(p, synthetic)
	default:
		w.WriteHeader(http.StatusOK)
		return
	}
	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number,
		"label", p.Label.Name, "user", p.Sender.Login, "installation", p.Installation.ID)
	w.WriteHeader(http.StatusAccepted)
}

// runImplement dispatches the implementation run on the issue's session - the
// same session the planning run used, so the plan is also in the model's own
// history. The orchestrator's initial response IS the ack (no canned Go-side
// comment). Fetches current labels to wire a contextual closing signal into the
// task prompt: if the issue carries quack:partial-fix the implementer skips the
// Closes keyword; otherwise it's instructed to close the issue.
func (e *Extension) runImplement(p issuesPayload, synthetic issueCommentPayload) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number

	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()

	_, _, _, labels, _, err := e.app.issueMeta(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: fetch issue labels failed; running without partial-fix signal", "component", "github",
			"repo", owner+"/"+repo, "issue", number, "err", err)
	}
	e.dispatch(synthetic, implementTask(p, labels, e.labels.PartialFix))
}

// hasPartialFix reports whether names includes the configured partial-fix label.
func hasPartialFix(partialFixLabel string, names []string) bool {
	return hasLabel(names, partialFixLabel)
}

// implementTask synthesizes the implement classification signal (fed to
// vetting.ImplementationIntent, never rendered - the envelope's hoisted
// <issue> block and deliverable text carry the ask itself, #659). The labels
// param carries every label currently on the issue (fetched at dispatch
// start), used to conditionally suppress unconditional Closes #N.
func implementTask(p issuesPayload, labels []string, partialFixLabel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Implement issue #%d: %s\n", p.Issue.Number, strings.TrimSpace(p.Issue.Title))

	if body := strings.TrimSpace(p.Issue.Body); body != "" {
		fmt.Fprintf(&b, "\nIssue description (may be incomplete - see discussion below):\n%s\n", truncate(body, 4000))
	}

	isPartial := hasPartialFix(partialFixLabel, labels)
	if isPartial {
		b.WriteString("\nA maintainer approved this for implementation (see the approved plan in the discussion below). This is a partial fix: implement the changes, commit locally, and call stage_pr. Do NOT use a Closes keyword - the issue will not be fully closed by this PR.")
	} else {
		b.WriteString("\nA maintainer approved this for implementation (see the approved plan in the discussion below). Implement it per the plan, commit your work locally, " +
			"then call stage_pr with a title and a body that includes `Closes #" + fmt.Sprintf("%d", p.Issue.Number) + "` - you do not push or open the pull request yourself; " +
			"the pull request is opened for you once your work passes review")
	}
	b.WriteString("\nNever merge anything - merging is a human decision.")
	return b.String()
}

// planTask synthesizes the planning request for a plan-labeled issue - there is
// no human comment to extract a task from, only the issue itself. The issue
// BODY is deliberately not embedded here: the envelope's hoisted <issue> block
// already carries it verbatim (#659), and embedding it again would duplicate
// the same text twice in the same prompt (#619). task is used only for
// internal classification (vetting.ImplementationIntent) and the chat-title
// fallback (ensureTitle); it is not rendered into the envelope itself.
func planTask(p issuesPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Produce an implementation plan for issue #%d: %s\n", p.Issue.Number, strings.TrimSpace(p.Issue.Title))
	b.WriteString("\nInvestigate the repository first, then lay out a concrete plan: the approach, the files to change, and how to verify it. A maintainer will review the plan before any implementation happens. Load and follow the `present-coding-plan` skill (load_skill) for how to structure and format the plan comment.")
	return b.String()
}

// mergeTimeout bounds the deterministic merge-label handler (a few API calls).
const mergeTimeout = 2 * time.Minute

// mergeIfApproved is the merge-label handler: merge ONLY at the intersection
// of a human's label (repo write access) and quack's own approving review
// verdict - a formal APPROVED review, or (on a PR quack authored, where
// GitHub forbids a formal self-review) the verdict marker in quack's own-PR
// comment-review.
//
// An approving review already on the PR merges immediately (unchanged). Any
// other verdict ("", "comment", "request_changes") turns the label into a
// STANDING INTENT (store.GithubMergeIntent, durable across a restart):
// tryMergeStandingIntent consumes it and merges the moment a quack review
// later lands approving, so the human never has to come back and re-apply the
// label. request_changes deliberately leaves the intent standing rather than
// clearing it - a later approving review after fixes should still authorize
// the merge; the label already said "merge once you approve", and a request
// for changes doesn't withdraw that, it just isn't satisfied yet. An unseen
// PR (verdict == "") also triggers a review run itself, unless one is already
// in flight - "apply merge" alone is a useless no-op otherwise, since nothing
// else would ever ask quack to look at it.
func (e *Extension) mergeIfApproved(p pullRequestPayload, rawBody []byte) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Number
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	ctx, cancel := context.WithTimeout(context.Background(), mergeTimeout)
	defer cancel()

	comment := func(text string) {
		if err := e.app.postIssueComment(ctx, owner, repo, number, text); err != nil {
			slog.Error("github: merge-label comment failed", "component", "github",
				"repo", owner+"/"+repo, "pr", number, "err", err)
		}
	}

	verdict, err := e.latestQuackVerdict(ctx, owner, repo, number)
	if err != nil {
		slog.Error("github: merge-label review lookup failed", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
		comment(fmt.Sprintf("Not merging: I could not read this PR's reviews (%v). Re-apply the `%s` label to retry.", err, e.labels.Merge))
		return
	}
	if verdict == "approve" {
		if err := e.app.mergePR(ctx, owner, repo, number, ""); err != nil {
			slog.Error("github merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
			comment(fmt.Sprintf("Merge failed: %v", err))
			return
		}
		slog.Info("github pr merged", "component", "github", "repo", owner+"/"+repo, "pr", number, "user", p.Sender.Login)
		if e.store != nil {
			if derr := e.store.DeleteGithubMergeIntent(ctx, sessionID); derr != nil {
				slog.Warn("github: stale merge-intent cleanup failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", derr)
			}
		}
		comment(fmt.Sprintf("Merged - my review approved this PR and @%s authorized the merge via the `%s` label.", p.Sender.Login, e.labels.Merge))
		return
	}

	// Not approved yet: record the standing intent BEFORE saying anything is
	// queued - fail CLOSED, an unrecorded intent must never be reported as one.
	if e.store == nil {
		comment(fmt.Sprintf("Not merging: I have not approved this PR yet, and I have no durable store to queue the merge for later. Re-apply `%s` once I approve.", e.labels.Merge))
		return
	}
	if err := e.store.SetGithubMergeIntent(ctx, sessionID, p.Sender.Login); err != nil {
		slog.Error("github: merge-intent persist failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		comment(fmt.Sprintf("Not merging: I could not record your merge request (%v) - not queued. Re-apply `%s` to retry.", err, e.labels.Merge))
		return
	}

	switch verdict {
	case "":
		msg := fmt.Sprintf("Queued: I have not reviewed this PR yet. @%s's `%s` label authorizes the merge once I approve.", p.Sender.Login, e.labels.Merge)
		if _, inflight := e.inflight.Load(sessionID); inflight {
			msg += " A review is already in progress - I'll merge automatically once it lands, if it approves."
		} else {
			msg += " Reviewing it now."
			go e.dispatch(autoReviewPayload(p, rawBody), autoReviewTask)
		}
		comment(msg)
	default: // "request_changes" or "comment": already reviewed, just not approving
		comment(fmt.Sprintf("Standing by: my latest review is %s, not an approval, so I'm not merging yet. @%s's `%s` label stands as authorization - I'll merge automatically the next time a review from me approves.",
			verdict, p.Sender.Login, e.labels.Merge))
	}
}

// reviewVerdictMarkerRe extracts quack's verdict from the hidden marker
// embedded in an own-PR review comment (deliverOne's own-PR branch,
// internal/github/tools.go): GitHub forbids a formal review on a PR quack
// authored (422), so that verdict has no formal review record - the marker is
// the only place latestQuackVerdict can read it from.
var reviewVerdictMarkerRe = regexp.MustCompile(`<!-- quack:delivery:review:(approve|request_changes|comment) -->`)

// formalReviewVerdicts maps a formal GitHub review state to the same verdict
// vocabulary as reviewVerdictMarkerRe ("approve" / "request_changes" /
// "comment"), so both sources merge into one timeline.
var formalReviewVerdicts = map[string]string{
	"APPROVED":          "approve",
	"CHANGES_REQUESTED": "request_changes",
	"COMMENTED":         "comment",
}

// latestQuackVerdict returns quack's most recent review verdict on the PR -
// "approve", "request_changes", or "comment" - reading BOTH formal reviews and
// own-PR comment-reviews (verdict marker), since a PR quack authored can only
// ever carry the latter. "" means quack has not reviewed this PR yet.
func (e *Extension) latestQuackVerdict(ctx context.Context, owner, repo string, number int) (string, error) {
	bot, err := e.app.botLogin(ctx)
	if err != nil {
		return "", err
	}
	type dated struct {
		at      time.Time
		verdict string
	}
	var verdicts []dated

	reviews, err := e.app.listReviews(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	for _, r := range reviews {
		if r.User.Login != bot {
			continue
		}
		at, _ := time.Parse(time.RFC3339, r.SubmittedAt)
		// Marker first: an own-PR review always submits as state COMMENTED
		// (GitHub disallows approve/request_changes on your own PR) but carries
		// the REAL verdict in the marker - the state alone would read as "comment".
		if m := reviewVerdictMarkerRe.FindStringSubmatch(r.Body); m != nil {
			verdicts = append(verdicts, dated{at, m[1]})
			continue
		}
		if v := formalReviewVerdicts[r.State]; v != "" {
			verdicts = append(verdicts, dated{at, v})
		}
	}

	comments, err := e.app.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	for _, c := range comments {
		if c.User != bot {
			continue
		}
		m := reviewVerdictMarkerRe.FindStringSubmatch(c.Body)
		if m == nil {
			continue
		}
		at, _ := time.Parse(time.RFC3339, c.CreatedAt)
		verdicts = append(verdicts, dated{at, m[1]})
	}

	if len(verdicts) == 0 {
		return "", nil
	}
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].at.Before(verdicts[j].at) })
	return verdicts[len(verdicts)-1].verdict, nil
}

// tryMergeStandingIntent consumes a merge intent recorded by mergeIfApproved
// (quack:merge applied before an approving review existed) once a review has
// actually been POSTED to GitHub this dispatch - called from dispatch only
// when the delivery outcome's reviewDelivered is true, never on a merely
// staged review. Re-reads the verdict fresh (latestQuackVerdict, the same
// source mergeIfApproved uses) rather than trusting the caller's own
// judgment of what it just posted, so this and the immediate-merge path can
// never disagree about what "approved" means.
//
// A request_changes/comment verdict leaves the intent standing for a later
// review, same as mergeIfApproved's own decision. headSHA pins the merge to
// the commit this review was actually against (the dispatch's own snapshot,
// taken before the run - a review never pushes, so the head cannot have
// moved during it): GitHub 409s the merge if the PR's head has since moved
// past it, rather than merging commits nobody's approval covers under
// someone else's standing authorization.
func (e *Extension) tryMergeStandingIntent(ctx context.Context, owner, repo string, number int, sessionID, headSHA string) {
	if e.store == nil {
		return
	}
	intent, err := e.store.GetGithubMergeIntent(ctx, sessionID)
	if err != nil {
		slog.Warn("github: merge-intent lookup failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	if intent == nil {
		return // no standing authorization on this PR
	}
	verdict, err := e.latestQuackVerdict(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: merge-intent verdict lookup failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	if verdict != "approve" {
		return // still not approved; the intent stands for a later review
	}

	comment := func(text string) {
		if err := e.app.postIssueComment(ctx, owner, repo, number, text); err != nil {
			slog.Error("github: merge-intent comment failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		}
	}
	if err := e.app.mergePR(ctx, owner, repo, number, headSHA); err != nil {
		slog.Error("github: standing-intent merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		comment(fmt.Sprintf("Merge failed: %v. @%s's standing `%s` authorization still stands - I'll retry the next time a review from me approves.", err, intent.RequestedBy, e.labels.Merge))
		return
	}
	slog.Info("github pr merged", "component", "github", "repo", owner+"/"+repo, "pr", number, "user", intent.RequestedBy)
	// Clear the intent BEFORE announcing the merge: once the comment is visible
	// the intent must already be gone, not racing whoever reads it next.
	if derr := e.store.DeleteGithubMergeIntent(ctx, sessionID); derr != nil {
		slog.Warn("github: merge-intent cleanup failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", derr)
	}
	comment(fmt.Sprintf("Merged - my review approved this PR, on the standing authorization @%s gave via the `%s` label.", intent.RequestedBy, e.labels.Merge))
}

// ackReaction posts a deterministic 👀 (eyes) reaction on the mentioning comment
// as an instant, code-level acknowledgment that quack saw the mention - it does
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

// ackLabelReaction posts a deterministic 👀 reaction on the ISSUE as an instant
// acknowledgment that a label-triggered run (quack:plan/quack:implement) started
// - the label path's equivalent of ackReaction. It can't reuse ackReaction: a
// label event carries no comment ID, so it reacts to the issue itself. Best
// effort: a failure is logged at WARN and never blocks dispatch.
func (e *Extension) ackLabelReaction(p issuesPayload) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	if _, err := e.app.reactToIssue(ctx, owner, repo, p.Issue.Number, "eyes"); err != nil {
		slog.Warn("github label ack reaction failed", "component", "github",
			"repo", owner+"/"+repo, "issue", p.Issue.Number, "err", err)
	}
}

// ackDedup fires a best-effort 👀 reaction on the issue/PR that triggered a
// deduplicated dispatch ("a run is already in-flight on this thread"). It is
// fire-and-forget: a failure is logged at WARN and never panics. The method
// picks the right reaction target (comment vs issue) because a dedup can arrive
// from either an @mention comment or a label event.
func (e *Extension) ackDedup(owner, repo string, number int) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	// reactToIssue works for both plain issues and PRs: reactions land on the
	// /repos/{owner}/{repo}/issues/{number}/reactions endpoint regardless.
	if _, err := e.app.reactToIssue(ctx, owner, repo, number, "eyes"); err != nil {
		slog.Warn("github dedup ack reaction failed", "component", "github",
			"repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// triggerTask decides whether a comment triggers a run and extracts the task
// text. The trigger is a created issue_comment carrying the configured
// mention token at the START OF A LINE (leading spaces/tabs only) - not
// anywhere in the body. This is what makes a quote-reply safe: GitHub quotes
// an earlier line as "> /quack …", and the leading "> " means that line does
// NOT start with the token, so it never re-fires. It also means ordinary
// prose that happens to contain the word ("quack's gate did not pass") never
// dispatches - only a line that OPENS with it does. Returns ("", false) when
// no line qualifies.
func (e *Extension) triggerTask(p issueCommentPayload) (string, bool) {
	if !e.triggers["mention"] {
		return "", false
	}
	if p.Action != "created" {
		return "", false
	}
	lines := strings.Split(p.Comment.Body, "\n")
	mentionLower := strings.ToLower(e.mention)
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if len(trimmed) < len(e.mention) || !strings.HasPrefix(strings.ToLower(trimmed), mentionLower) {
			continue
		}
		// A word boundary right after the token: "/quackers" is not "/quack".
		if len(trimmed) > len(e.mention) && isTokenRune(trimmed[len(e.mention)]) {
			continue
		}
		task := strings.TrimSpace(trimmed[len(e.mention):])
		if rest := strings.TrimSpace(strings.Join(lines[i+1:], "\n")); rest != "" {
			if task != "" {
				task += "\n" + rest
			} else {
				task = rest
			}
		}
		if task == "" {
			return "", false
		}
		return task, true
	}
	return "", false
}

// isTokenRune reports whether b could continue an identifier - used to reject
// a mention token match that's actually a prefix of a longer word.
func isTokenRune(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// verifySignature checks GitHub's X-Hub-Signature-256 (HMAC-SHA256 of the raw
// body, hex, prefixed "sha256=") against the configured secret using a
// CONSTANT-TIME compare. A missing/malformed header or any mismatch is false -
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
// PR event) - dispatch fetches the PR's head/base refs authoritatively anyway.
func (e *Extension) dispatch(p issueCommentPayload, task string) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number

	// Key this run by the commenter's login so memories and ADK sessions are
	// partitioned per-person (#262: one maintainer's preferences must not leak
	// into another's). Falls back to "github" when there's no comment author
	// (no human invoker present — should not happen on live events, but safe).
	login := p.Comment.User.Login
	if login == "" {
		login = runUserID
	}

	// Dedup: one active run per issue/PR thread. A second trigger that arrives
	// while a run is in-flight (same sessionID) is DROPPED - not queued - with a
	// best-effort 👀 reaction on the triggering event - it never panics even
	// if the GitHub API call fails. Queueing was considered and rejected: two
	// runs on one session would either corrupt each other if run concurrently,
	// or, if serialised, the second would consume a conversation-watermark delta
	// the first run's context already captured (#665, #668) - dropping and
	// waiting for the next trigger avoids both.
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	if _, inflight := e.inflight.LoadOrStore(sessionID, struct{}{}); inflight {
		slog.Info("deduplicated trigger", "sessionID", sessionID)
		go e.ackDedup(owner, repo, number)
		return
	}

	// No deadline here. The run deadline is the ORCHESTRATOR's (SetRunDeadline,
	// applied once a run slot is held) so that queueing is not charged against
	// it: this context covers the server-wide run queue, where a run can
	// legitimately wait hours on a serial deployment. Starting the clock here
	// killed three implement runs at the 4h wall having delivered nothing -
	// they spent the budget waiting. Post-run GitHub calls use their own short
	// contexts (see tailCtx, reactionTimeout).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer e.inflight.Delete(sessionID)

	e.persistGithubLink(ctx, sessionID, login, owner, repo, number, p.Issue.PullRequest != nil)
	e.ensureTitle(ctx, sessionID, p, task)

	// A LABEL-driven work request starts a FRESH session: unlike a conversational
	// @mention (kept for continuity), a new attempt must not inherit a prior
	// attempt's events, which can make the run conclude the work is "already
	// done" instead of doing it. The stored GitHub snapshot must be forgotten
	// too - otherwise the fresh session would still only get the DELTA since
	// the stale snapshot (near-empty), instead of the full context a reset
	// session needs (see loadGithubContext's forceReseed).
	resetSession := p.isLabelTrigger
	if resetSession {
		if err := e.runner.ResetSession(ctx, login, sessionID); err != nil {
			slog.Warn("github: session reset failed; this attempt may inherit stale history",
				"component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		}
	}

	// One unified fetch-diff-persist path for issues and PRs alike (#459):
	// fetch the full current GitHub state, diff it against the snapshot stored
	// from the last dispatch (seeding on first load, delta-only on resume),
	// and persist the new snapshot for next time.
	gh := e.loadGithubContext(ctx, sessionID, owner, repo, number, p.Issue.PullRequest != nil, p.Comment.ID, resetSession)

	// Never run a label-triggered work request (plan/implement) blind: if
	// GitHub's required context fetch failed outright (retries exhausted),
	// the agent would be told to "follow the plan/discussion below" with
	// nothing actually injected - exactly the #467 failure mode. A
	// conversational @mention is exempt: answering from the trigger comment
	// alone is a legitimate degraded mode there.
	if p.isLabelTrigger && gh.contextUnavailable {
		slog.Warn("github: label-triggered work request has no usable GitHub context; aborting rather than running blind",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		abortCtx, abortCancel := context.WithTimeout(context.Background(), reactionTimeout)
		abortMsg := "Couldn't load this issue's plan and discussion from GitHub (a transient error fetching it) - not running blind. Re-apply the label to retry."
		if err := e.app.postIssueComment(abortCtx, owner, repo, number, abortMsg); err != nil {
			slog.Warn("github: abort comment failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		}
		abortCancel()
		return
	}

	isPR := p.Issue.PullRequest != nil

	// Compute this run's permission grant (#657, #662) ONCE here, from the
	// labels currently on the thread, authorship, and the fork check - the
	// planner, the gate, AND this run's own envelope all trust this, never a
	// re-derivation of their own. An authorship-check failure denies rather
	// than grants (fail closed).
	authored := false
	if isPR {
		if a, aerr := e.authoredByQuack(ctx, owner, repo, number); aerr != nil {
			slog.Warn("github: authorship check failed computing this run's grant; treating as not-authored",
				"component", "github", "repo", owner+"/"+repo, "issue", number, "err", aerr)
		} else {
			authored = a
		}
	}
	grant := computeGrant(e.labels, gh.snap.Labels, isPR, authored, gh.snap.Fork)

	// Sibling context directory (#659/#660): best-effort. A harness that never
	// wired SetJail (most tests) simply gets no <context> block - the same
	// degrade every other e.store == nil guard in this package uses.
	var ctxDir string
	var ctxFiles []ContextFile
	if e.jail != nil {
		if dir, derr := e.jail.EnsureDir(e.workspaceUserID, sessionID, workspace.ContextDirScope); derr != nil {
			slog.Warn("github: context dir setup failed; running without one", "component", "github",
				"repo", owner+"/"+repo, "issue", number, "err", derr)
		} else {
			ctxDir = dir
			if werr := e.app.WriteContextDir(ctx, ctxDir, ContextRequest{
				Owner: owner, Repo: repo, Number: number, IsPR: isPR, CheckSHA: p.checkSHA,
			}); werr != nil {
				slog.Warn("github: context dir write failed; running with a partial or empty one", "component", "github",
					"repo", owner+"/"+repo, "issue", number, "err", werr)
			}
			ctxFiles = contextDirFiles(ctxDir, owner, repo, number, p.checkSHA)
		}
	}

	message := e.buildEnvelope(ctx, p, task, gh, grant, ctxDir, ctxFiles)

	slog.Info("github run dispatched", "component", "github", "repo", owner+"/"+repo, "issue", number)

	// Persist this dispatch as a turn on the session's chat, exactly like a
	// UI-initiated run, so it shows up in getChat (DAG + progress) rather than
	// leaving the chat row (already created by persistGithubLink) with no turns.
	var pub *runlog.Publisher
	turnID := uuid.NewString()
	if e.store != nil {
		_ = e.store.SaveTurn(ctx, sessionID, turnID)
		e.eventLog.Reset(ctx, sessionID)
		pub = runlog.NewPublisher(e.hub, e.eventLog, sessionID)
		pub.Publish(stream.ResponseCreated(turnID))
	}

	// Wrap the run context with a cancel and register it on the SHARED hub -
	// the same registry the REST handler's DELETE/stop-button paths use for
	// UI-initiated runs - so both drivers of a run are cancellable uniformly
	// (#468: previously this ctx was Background-derived and unreachable by
	// either). Registered synchronously (before the goroutine gets to run) so
	// the cancel endpoint can never miss the run.
	runCtx, cancelRun := context.WithCancel(ctx)
	// Stamp the deterministic Setup facts (#661): repo + base_ref are ground
	// truth off the webhook event for EVERY GitHub-originated run, so the
	// planner is never asked for them. WorkBranch defaults to a fresh
	// per-issue name; WithGitHubPR below (via OverrideExistingPRHead)
	// replaces it with the PR's real head when this run is bound to one.
	runCtx = tools.WithGitHubSetup(runCtx, dag.Setup{
		Repo:       p.Repository.CloneURL,
		BaseRef:    setupBaseRef(p, gh),
		WorkBranch: fmt.Sprintf("quack/issue-%d", number),
	})
	// Stamp the AUTHORITATIVE repo/PR this run is on - the only source
	// correct_review_finding trusts for where a conversational correction may
	// write, and the only source the plan tool trusts for a review's real head
	// branch (never the model's own say-so; see tools.WithGitHubPR). Only for a
	// PR (not a plain issue): there is no finding to correct, or head branch to
	// override, on an issue thread.
	if isPR {
		runCtx = tools.WithGitHubPR(runCtx, owner, repo, number, gh.snap.HeadRef)
	}
	// grant was already computed above (before the envelope was built) - the
	// planner and the gate trust the SAME value the envelope's <permissions>
	// stated, never a re-derivation of their own.
	runCtx = tools.WithGrant(runCtx, grant)
	e.hub.RegisterRun(sessionID, turnID, cancelRun)
	// dispatch is ALREADY a goroutine (handleIssues calls it via `go`), so the run
	// stays INLINE - wrapping it in another goroutine would let this function's
	// `defer cancel()` (run ctx) and `defer e.inflight.Delete` (dedup claim) fire
	// the moment it spawned, before the run finished. Deregister LAST (deferred
	// cancel+unregister, then hub.Close) so a viewer sees the stream close only
	// after the run is already off the registry (cancelling it then 404s/no-ops).
	defer e.hub.Close(sessionID)
	defer func() {
		cancelRun()
		e.hub.UnregisterRun(sessionID)
	}()

	// A WORK request (review/implement) always runs as a plan. A mid-tier
	// orchestrator model sometimes answers in prose without calling plan - "Let me
	// start by cloning the repo…" - and that preamble would be posted as if it were
	// the review. So if a LABEL-triggered run produced no plan, nudge once to
	// actually run it: a label is an unambiguous work request, and its task text
	// is synthesized here rather than written by a human.
	//
	// A mention is never nudged. Whether it wants work is the orchestrator's call
	// - it either plans or answers - and second-guessing that from the prose was
	// worse than trusting it: matching work verbs read `it.migrate(connection)` in
	// a quoted snippet as "migrate something", so correcting a review forced a
	// whole re-review and discarded the reply the model had already written.
	planRan, judgePassed, paused, needsInput := e.drive(runCtx, login, sessionID, message, owner, repo, number, turnID, pub)
	if !planRan && !paused && p.isLabelTrigger {
		slog.Warn("github: work request produced no plan; nudging it to run the work once",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		_, jp2, _, _ := e.drive(runCtx, login, sessionID, runNudge, owner, repo, number, turnID, pub)
		judgePassed = judgePassed || jp2
	}
	if pub != nil {
		pub.Publish(stream.Done())
		_ = e.store.Touch(runCtx, sessionID)
	}

	// A judge-passed work request already had its staged review/PR posted by the
	// trust gate itself (commitDelivery) - posting the run's text summary too would
	// duplicate it. Only fall back to a summary comment when nothing was delivered.
	// takeDeliveryDetail is AUTHORITATIVE when present (the delivery call's own
	// outcome, not a proxy): a gate that passed but whose push then failed must NOT
	// read as delivered. A plan-only run NEVER delivers a PR/review - its
	// deliverable is the plan comment - so it must never read as delivered no
	// matter how work-verby the task text is.
	// No delivery record means NOTHING was delivered: since the native
	// delivery tools were deleted (0.6.0), the gate's commitDelivery is the
	// only path to GitHub, and it always records its outcome. (The old
	// judge-passed default dates from workers that pushed via their own tools
	// recordlessly - kept, it masked a staged-nothing run as delivered.)
	delivered := false
	if d, ok := takeDeliveryDetail(sessionID); ok {
		delivered = d.err == nil
		if d.err != nil {
			slog.Error("github: staged delivery failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", d.err)
		} else {
			slog.Info("github: delivery verified against GitHub", "component", "github", "repo", owner+"/"+repo, "issue", number,
				"pr_number", d.prNumber, "pr_url", d.prURL, "pushed_sha", d.pushedSHA)
			// This run ACTUALLY delivered a review (not a plan/PR/comment, and
			// not a conversational dispatch that delivered nothing) - advance the
			// review baseline to what was reviewed: the PR's commits as fetched at
			// THIS dispatch's snapshot (gh.snap), before the run started. This is
			// the ONLY place the baseline advances (#459 incremental-review fix).
			if d.reviewDelivered {
				// A fresh, short-lived context: runCtx may already be past its
				// deadline on a slow run (the same reason the tail-comment logic
				// below uses its own fresh context), and this write must not be
				// skipped just because the run itself timed out.
				baselineCtx, baselineCancel := context.WithTimeout(context.Background(), 10*time.Second)
				e.advanceReviewBaseline(baselineCtx, sessionID, gh.snap.Commits)
				baselineCancel()

				// A review was just ACTUALLY posted to GitHub (not merely staged) -
				// the hook point for a quack:merge label applied before this review
				// existed (see mergeIfApproved/tryMergeStandingIntent). gh.snap.HeadSHA
				// is the commit this review was against.
				mergeCtx, mergeCancel := context.WithTimeout(context.Background(), mergeTimeout)
				e.tryMergeStandingIntent(mergeCtx, owner, repo, number, sessionID, gh.snap.HeadSHA)
				mergeCancel()
			}
		}
	}
	if delivered {
		e.persistGithubSnapshot(sessionID, gh)
		slog.Info("github: work delivered on the PR; skipping the duplicate summary comment",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		return
	}

	// A HITL pause means the run is NOT finished - a node asked the human for
	// input. Post the question as a GitHub comment so the maintainer can answer
	// on-thread; the reply feeds back into the same session and resumes the
	// paused node (orchestrator.Run → resumeNodeRun). Never post the "produced
	// no answer" tail in this case: doing so would bury the HITL question.
	if paused {
		comment := fmt.Sprintf("⏸️ quack has a question before proceeding:\n\n**%s**\n\n%s",
			needsInput.NodeID, needsInput.Message)
		hitlCtx, hitlCancel := context.WithTimeout(context.Background(), time.Minute)
		defer hitlCancel()
		if err := e.app.postIssueComment(hitlCtx, owner, repo, number, comment); err != nil {
			slog.Error("github: HITL question comment post failed", "component", "github",
				"repo", owner+"/"+repo, "issue", number, "err", err)
		} else {
			slog.Info("github: HITL question posted", "component", "github",
				"repo", owner+"/"+repo, "issue", number, "node", needsInput.NodeID)
		}
		return
	}

	// A hub-cancelled run is NOT a timeout: runCtx is cancellable from the shared
	// registry that DELETE/stop AND a re-run supersede use. Reporting "hit its run
	// deadline (4h)" there is a lie - the run may have been stopped after seconds -
	// and on a supersede a SIBLING run is still delivering, so a "nothing delivered,
	// re-apply the label" comment actively misleads. Say nothing and let it. Only a
	// genuine DeadlineExceeded gets the tail treatment below.
	if errors.Is(runCtx.Err(), context.Canceled) {
		slog.Info("github: run cancelled (stopped or superseded); skipping tail comment",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		return
	}
	// The tail must OUTLIVE the run: after a deadline kill, ctx is dead and both
	// LatestAnswer and the comment post would fail with it - the run then dies
	// with zero external signal (#286: a 2h-deadline kill posted nothing). Use a
	// fresh bounded context, and say what actually happened.
	tailCtx := runCtx
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if timedOut {
		var tailCancel context.CancelFunc
		tailCtx, tailCancel = context.WithTimeout(context.Background(), time.Minute)
		defer tailCancel()
	} else {
		// Reached the natural end of the run (not cancelled, not deadlined) -
		// genuine completion, whether or not it produced a useful answer.
		e.persistGithubSnapshot(sessionID, gh)
	}
	answer := strings.TrimSpace(e.runner.LatestAnswer(tailCtx, login, sessionID))
	if timedOut {
		answer = fmt.Sprintf("⚠️ quack hit its run deadline (%s) before finishing; nothing was delivered. Re-apply the label to retry.\n\nLast progress:\n\n%s",
			e.runTimeout, answer)
	} else if answer == "" {
		// The silent-gap class (#568): the run hit no deadline and was not
		// cancelled, yet persisted no final answer - from outside, indistinguishable
		// from a run that legitimately had nothing to say. Say so explicitly and
		// leave a queryable trace, the same treatment as gate.checks.skipped/
		// judge.unavailable/delivery.outcome=none.
		otelobs.RecordRunNoAnswer()
		slog.Warn("github: run completed with no final answer", "component", "github", "repo", owner+"/"+repo, "issue", number)
		answer = "⚠️ quack finished this run but produced no answer - no error, no failed node, nothing delivered. " +
			"That's a silent-gap failure, not a run with nothing to say. Re-apply the label to retry."
	} else {
		// This is the orchestrator's own write-up, not a gated worker answer -
		// it never runs through mermaidCriterion (#480 regression, #483). Check
		// and revise it directly.
		answer = e.reviseInvalidMermaid(runCtx, login, sessionID, owner, repo, number, turnID, pub, answer)
		if p.planOnly {
			// A genuine plan (not a timeout/empty placeholder): collapse any PRIOR
			// plan comment on this issue before posting the new one, so the thread
			// shows the CURRENT plan, not a pile of dead attempts. The marker
			// is what a later run's collapse finds.
			e.app.collapsePriorComments(tailCtx, owner, repo, number, "plan")
			answer += "\n\n" + deliveryMarker("plan")
		}
	}
	if err := e.app.postIssueComment(tailCtx, owner, repo, number, answer); err != nil {
		slog.Error("github comment post failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		return
	}
	slog.Info("github comment posted", "component", "github", "repo", owner+"/"+repo, "issue", number, "timed_out", timedOut)
}

// reviseInvalidMermaid nudges the same run once more (mirrors the runNudge
// re-drive above) with the concrete parse error, then falls back to
// vetting.DegradeInvalidMermaid if it's still invalid - a visible, labeled
// degrade, never a silent strip. runCtx must still be alive (the caller only
// reaches this once cancellation/timeout are ruled out).
func (e *Extension) reviseInvalidMermaid(runCtx context.Context, uid, sessionID, owner, repo string, number int, turnID string, pub *runlog.Publisher, answer string) string {
	issues := vetting.FindInvalidMermaid(answer)
	if len(issues) == 0 {
		return answer
	}
	feedback := make([]string, len(issues))
	for i, iss := range issues {
		feedback[i] = iss.Feedback()
	}
	slog.Warn("github: plan/research answer has invalid mermaid; nudging one revise",
		"component", "github", "repo", owner+"/"+repo, "issue", number, "issues", feedback)
	nudge := fmt.Sprintf(
		"Your last answer contains invalid mermaid diagram(s) that GitHub cannot render:\n\n- %s\n\nFix each diagram so it parses, or remove it if it isn't essential, then repost your complete answer.",
		strings.Join(feedback, "\n- "))
	e.drive(runCtx, uid, sessionID, nudge, owner, repo, number, turnID, pub)
	if revised := strings.TrimSpace(e.runner.LatestAnswer(runCtx, uid, sessionID)); revised != "" {
		answer = revised
	}
	if stillInvalid := vetting.FindInvalidMermaid(answer); len(stillInvalid) > 0 {
		degraded, issues := vetting.DegradeInvalidMermaid(answer)
		feedback = feedback[:0]
		for _, iss := range issues {
			feedback = append(feedback, iss.Feedback())
		}
		slog.Warn("github: plan/research answer still has invalid mermaid after one revise; degrading to a visible text block",
			"component", "github", "repo", owner+"/"+repo, "issue", number, "issues", feedback)
		return degraded
	}
	return answer
}

// persistGithubLink stores the web URL of the originating issue/PR on the
// session's chat row, for the frontend's GitHub tab, and - on first dispatch
// only - the ADK session identity this run is writing under (login, #512)
// so later reads/deletes agree with the write. isPR selects "pull" vs
// "issues" in the URL; unknown defaults to "issues" (GitHub redirects PRs
// requested at the issues path). Best-effort: a failure here must not block
// the run.
func (e *Extension) persistGithubLink(ctx context.Context, sessionID, login, owner, repo string, number int, isPR bool) {
	if e.store == nil {
		return
	}
	kind := "issues"
	if isPR {
		kind = "pull"
	}
	url := fmt.Sprintf("https://github.com/%s/%s/%s/%d", owner, repo, kind, number)
	if err := e.store.SetChatGitHub(ctx, sessionID, owner+"/"+repo, url, login); err != nil {
		slog.Warn("github: persist chat link failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// ensureTitle gives a github-origin chat a real title (#380): unlike a
// first-party chat's runChat, dispatch never called generateTitle, so these
// chats sat at the "New chat" placeholder forever. No live model call is
// needed - the triggering issue/PR title is already a decent chat title.
// Only sets it once (mirrors runChat's own titleCh check), so a later
// conversational follow-up on the same session never clobbers it.
func (e *Extension) ensureTitle(ctx context.Context, sessionID string, p issueCommentPayload, task string) {
	if e.store == nil {
		return
	}
	c, err := e.store.GetChat(ctx, sessionID)
	if err != nil {
		slog.Warn("github: title lookup failed", "component", "github", "chat", sessionID, "err", err)
		return
	}
	if c != nil && c.Title != "" {
		return
	}
	title := strings.TrimSpace(p.Issue.Title)
	if title == "" {
		title = truncate(task, 80)
	}
	if title == "" {
		return
	}
	if err := e.store.UpdateTitle(ctx, sessionID, title); err != nil {
		slog.Warn("github: title update failed", "component", "github", "chat", sessionID, "err", err)
	}
}

// runNudge is delivered when a webhook run answered without running a plan - a
// firm instruction to actually do the work rather than narrate intent.
const runNudge = "You answered without running anything. Do NOT reply in prose: use the plan and execute tools NOW to actually clone the repo, read the change, and carry out the review (or the requested change). Nothing has run yet and the user is waiting."

// drive runs one orchestrator turn to completion and reports whether it
// EXECUTED a plan (a dag_plan event) and whether ANY node's trust gate PASSED
// its judge round. paused indicates the run hit a HITL pause (node_needs_input)
// - in that case, no answer was produced yet and dispatch must NOT post the
// default tail comment; instead it posts the question as a GitHub comment so
// the maintainer can answer on-thread which resumes the same session.
// A run with no plan produced only a direct-text answer (the work never
// happened). judgePassed is dispatch's proxy for "the staged delivery set was
// posted": commitDelivery runs synchronously inside the gate, strictly before
// node_done fires (see internal/vetting/node.go), so by the time node_done
// reports a pass here, delivery has already been attempted - a failed delivery
// is still logged loudly (slog.Error) even though this proxy can't distinguish
// it from "nothing was staged" (a conversational node gated the same way).
// dispatch only trusts this proxy for a LABEL-triggered run, which demanded
// delivery in the first place - see its caller.
//
// pub is nil when the extension has no store (test harnesses that don't need
// persistence) - every persistence step below is then a no-op, matching drive's
// old behavior exactly.
func (e *Extension) drive(ctx context.Context, uid, sessionID, message, owner, repo string, number int, turnID string, pub *runlog.Publisher) (planRan, judgePassed bool, paused bool, needsInput stream.NodeNeedsInputData) {
	var planID string
	for ev, err := range e.runner.Run(ctx, uid, sessionID, message, nil) {
		if err != nil {
			slog.Warn("github run error", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
			continue
		}
		switch ev.Name {
		case stream.EventDagPlan:
			planRan = true
			if d, ok := ev.Data.(stream.DagPlanData); ok {
				planID = d.PlanID
				if pub != nil {
					runlog.SaveDagPlan(e.store, sessionID, turnID, d)
				}
			}
		case stream.EventNodeDone:
			if d, ok := ev.Data.(stream.NodeDoneData); ok && d.JudgePassed {
				judgePassed = true
			}
		case stream.EventNodeNeedsInput:
			if d, ok := ev.Data.(stream.NodeNeedsInputData); ok {
				paused = true
				needsInput = d
			}
		}
		if pub != nil {
			if planID != "" {
				runlog.PersistNodeEvent(e.store, planID, ev)
			}
			pub.Publish(ev)
		}
	}
	return planRan, judgePassed, paused, needsInput
}

// githubContext is the loaded, ready-to-inject GitHub state for one dispatch
// (#459) - the single result loadGithubContext produces and buildEnvelope
// consumes, for an issue or a PR alike.
type githubContext struct {
	snap Snapshot
	// delta is the diff against the snapshot stored from the last dispatch -
	// nil on first load (session creation seeds the whole ask, #666), set
	// (possibly Empty()) on resume. buildEnvelope's <comments> block reads
	// this to decide seed-everything vs seed-the-delta-only.
	delta *Delta
	// firstLoad is true when no prior snapshot existed for this session (or
	// one was deliberately forgotten - see loadGithubContext's forceReseed).
	// Equivalent to delta == nil; kept as its own field so a decode failure
	// (falls back to "treat as first load") doesn't have to fabricate a delta.
	firstLoad bool
	// contextUnavailable is true when fetchSnapshot's REQUIRED meta call
	// (issueMeta/pullMeta) failed - as opposed to a legitimately empty
	// issue/PR. Distinct from firstLoad: a fresh issue with no comments yet
	// is also firstLoad but has real (empty) context; this flags "GitHub
	// could not be read at all" so a label-triggered work request can abort
	// instead of running with an empty snapshot it mistakes for the truth
	// (#467).
	contextUnavailable bool
	// newCommits are the PR commits to scope an incremental review to (see
	// Delta.NewCommits) - nil on a first load (review everything), a
	// (possibly empty) slice on resume.
	newCommits []snapshotCommit
}

// loadGithubContext is the ONE fetch-diff path every dispatch goes through,
// issue or PR, work request or conversational follow-up (#459's "one unified
// path" - replaces gatherReviewContext/issueThreadContext/discussionSummary
// cherry-picking, and #457/#458's inject-everything interim). It fetches the
// CURRENT full GitHub state, diffs it against the snapshot stored from the
// last dispatch (or seeds, on first load), and returns it ready for
// buildEnvelope to render as a session EVENT via e.runner.Run's message
// argument - never as bare UserContent (llmagent builds its request from
// Session().Events() only; a fresh prompt passed any other way is silently
// dropped).
//
// It does NOT persist the new snapshot - that's persistGithubSnapshot,
// called by dispatch only once the run actually completes (#665: persisting
// here, before the run, meant a crashed/cancelled run permanently lost the
// delta it never got to act on).
//
// forceReseed treats this dispatch as a first load regardless of what's
// stored: a label-driven work request resets the ADK session (T4 hygiene)
// before this runs, and a fresh session needs the FULL context, not a delta
// against a snapshot from a conversation the reset just erased.
func (e *Extension) loadGithubContext(ctx context.Context, sessionID, owner, repo string, number int, isPR bool, triggerCommentID int64, forceReseed bool) githubContext {
	snap, err := e.fetchSnapshot(ctx, owner, repo, number, isPR)
	if err != nil {
		// The required meta call (issueMeta/pullMeta, already retried at the
		// HTTP layer for transient failures) still failed - this is NOT a
		// legitimately empty issue, it's GitHub unreachable. Flag it so a
		// label-triggered work request can refuse to run blind rather than
		// silently treating the empty snapshot as "no discussion yet" (#467).
		slog.Warn("github: fetchSnapshot failed; this turn has no usable GitHub context",
			"component", "github", "repo", owner+"/"+repo, "number", number, "err", err)
		return githubContext{snap: snap, firstLoad: true, contextUnavailable: true}
	}

	var prevJSON string
	var hasPrev bool
	if e.store != nil && !forceReseed {
		prevJSON, hasPrev, err = e.store.GetGithubSnapshot(ctx, sessionID)
		if err != nil {
			slog.Warn("github: GetGithubSnapshot failed; treating this as a first load",
				"component", "github", "chat", sessionID, "err", err)
			hasPrev = false
		}
	}

	gh := githubContext{snap: snap}
	if !hasPrev {
		gh.firstLoad = true
	} else {
		prev, uerr := unmarshalSnapshot(prevJSON)
		if uerr != nil {
			slog.Warn("github: stored snapshot did not decode; treating this as a first load",
				"component", "github", "chat", sessionID, "err", uerr)
			gh.firstLoad = true
		} else {
			delta := diffSnapshots(prev, snap, triggerCommentID)
			gh.delta = &delta
		}
	}
	// The incremental-review scope is DELIBERATELY not delta.NewCommits above:
	// that delta advances on every dispatch (comment/label/etc. included), so
	// scoping a review off it would under-scope whenever a conversational
	// dispatch landed between two reviews. reviewScope reads a SEPARATE
	// baseline that only a delivered review advances (see advanceReviewBaseline,
	// called from dispatch after a run that actually posted one).
	if isPR {
		gh.newCommits = e.reviewScope(ctx, sessionID, snap)
	}

	return gh
}

// persistGithubSnapshot upserts the snapshot this dispatch already fetched
// (gh.snap, captured by loadGithubContext before the run) as the new
// conversation watermark - called by dispatch ONLY once the run reaches
// genuine completion, never on cancellation, timeout, or HITL pause, so a
// run that doesn't finish leaves its delta for the next trigger to see
// (#665). Deliberately re-persists the SAME pre-run snapshot rather than
// re-fetching a fresh one: a fresh fetch would mark comments that arrived
// mid-run as "seen" without the model ever having been shown them, which
// combined with drop-not-queue dedup (#668) would lose them for good.
//
// A fresh short-lived context: runCtx may already be past its deadline on a
// slow run, the same reason the tail-comment logic uses its own.
func (e *Extension) persistGithubSnapshot(sessionID string, gh githubContext) {
	if e.store == nil || gh.contextUnavailable {
		return
	}
	j, err := marshalSnapshot(gh.snap)
	if err != nil {
		slog.Warn("github: marshal snapshot failed; not persisted", "component", "github", "chat", sessionID, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.store.SetGithubSnapshot(ctx, sessionID, j); err != nil {
		slog.Warn("github: SetGithubSnapshot failed; next resume may re-see this turn's changes",
			"component", "github", "chat", sessionID, "err", err)
	}
}

// reviewScope returns the PR commits not yet covered by quack's last
// DELIVERED review (nil - not an empty slice - when no review has ever been
// delivered on this chat, meaning "review everything"; see
// newCommitsAgainstBaseline). Best-effort: a lookup/decode failure logs and
// falls back to nil (review everything) rather than silently under-scoping.
func (e *Extension) reviewScope(ctx context.Context, sessionID string, snap Snapshot) []snapshotCommit {
	if e.store == nil {
		return nil
	}
	raw, ok, err := e.store.GetGithubReviewBaseline(ctx, sessionID)
	if err != nil {
		slog.Warn("github: GetGithubReviewBaseline failed; reviewing everything this run",
			"component", "github", "chat", sessionID, "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	ids, err := unmarshalPatchIDs(raw)
	if err != nil {
		slog.Warn("github: stored review baseline did not decode; reviewing everything this run",
			"component", "github", "chat", sessionID, "err", err)
		return nil
	}
	reviewed := make(map[string]bool, len(ids))
	for _, id := range ids {
		reviewed[id] = true
	}
	return newCommitsAgainstBaseline(snap.Commits, reviewed)
}

// advanceReviewBaseline persists the current PR commits' patch-ids as
// "reviewed" - called ONLY after a dispatch that actually DELIVERED a review
// this run (see dispatch's use of deliveryOutcome.reviewDelivered). A
// conversational/plan/implement dispatch must NEVER call this: that's the
// exact bug this baseline exists to avoid (scoping the next review off
// whatever the general snapshot last happened to see, rather than off what
// was actually reviewed).
func (e *Extension) advanceReviewBaseline(ctx context.Context, sessionID string, commits []snapshotCommit) {
	if e.store == nil {
		return
	}
	ids := make([]string, 0, len(commits))
	for _, c := range commits {
		if c.PatchID != "" {
			ids = append(ids, c.PatchID)
		}
	}
	j, err := marshalPatchIDs(ids)
	if err != nil {
		slog.Warn("github: marshal review baseline failed; not persisted", "component", "github", "chat", sessionID, "err", err)
		return
	}
	if err := e.store.SetGithubReviewBaseline(ctx, sessionID, j); err != nil {
		slog.Warn("github: SetGithubReviewBaseline failed; the next review may under-scope",
			"component", "github", "chat", sessionID, "err", err)
	}
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

// setupBaseRef is base_ref for a GitHub-originated run's deterministic Setup
// (#661): the PR's own base branch when this run is on a PR, else the repo's
// default branch.
func setupBaseRef(p issueCommentPayload, gh githubContext) string {
	if gh.snap.BaseRef != "" {
		return gh.snap.BaseRef
	}
	if p.Repository.DefaultBranch != "" {
		return p.Repository.DefaultBranch
	}
	return "main"
}
