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

// maxWebhookBody bounds a hostile/oversized request.
const maxWebhookBody = 5 << 20

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
		// Present only when the issue is a PR.
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

	// Synthetic payload fields — not part of the GitHub webhook.
	planOnly        bool            // label-driven plan: produce a plan, touch no code.
	isLabelTrigger  bool            // label/pr_opened trigger vs @mention (T4 session reset).
	deliverableHint string          // fixed deliverable for synthetic triggers (CI auto-heal, own-PR).
	rawEvent        json.RawMessage // originating webhook JSON → envelope's <event> block.
	eventName       string          // originating webhook dotted name.
	checkSHA        string          // CI commit: dump check-runs.json. "" = plan/review/mention run.
	// issueDeliverableCache memoizes classifyIssueDeliverable for one dispatch
	// (#731): shared by pointer across every copy of p passed to
	// buildEnvelope/buildWorkerAsk/deliverableIsPlan, so a live classifier
	// call happens at most once regardless of how many of them need the
	// answer. nil when a caller (e.g. a test) invokes one of those directly.
	issueDeliverableCache *issueDeliverableResult
}

// issuesPayload is the issues webhook subset for the label-driven issue workflow.
type issuesPayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		// Present when the "issue" is actually a PR.
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

// pullRequestPayload is the PR webhook subset for opened/labeled actions.
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

// autoReviewTask for a pr_opened/label-triggered auto-review.
const autoReviewTask = "Review this pull request and post your findings as inline review comments and a verdict."

// autoReviewUser is the synthetic commenter for an auto-review run.
const autoReviewUser = "quack-auto-review"

// handleWebhook verifies HMAC signature, dispatches by event type, and returns
// fast — the run happens in a goroutine (GitHub enforces ~10s webhook timeout).
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
	// Never act on another bot's comments.
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

// handlePullRequest fires an auto-review on "opened" or "labeled" with the configured auto_review_label.
func (e *Extension) handlePullRequest(w http.ResponseWriter, body []byte) {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	// The merge label is a human authorization — checks quack's verdict and merges (or explains why not).
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

	// quack:fix is a persistent capability flag (#656) — re-arms auto-heal; fixes CI if currently failing.
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

// autoReviewPayload shapes a PR event as an issueCommentPayload so the mention path's dispatch/envelope builder handles it.
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

// pullRequestReviewPayload handles request_changes on a PR quack authored (#656).
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

// handlePullRequestReview engages only on request_changes to a PR quack authored — gated on ci_fix.
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

// engageOwnPRReview dispatches a fix-the-findings run on the PR's existing session.
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

// handleIssues drives the label-driven issue workflow (plan/implement labels).
func (e *Extension) handleIssues(w http.ResponseWriter, body []byte) {
	var p issuesPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	// Only human-applied labels on real issues.
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

// issueImplementDeliverable is the PR-implementing deliverable text, shared by
// the label trigger and a comment classified/heuristically read as implement (#713).
func issueImplementDeliverable(partialFixLabel string, labels []string, issueNumber int) string {
	if hasPartialFix(partialFixLabel, labels) {
		return "a pull request implementing the changes, without a Closes keyword (this is a partial fix)"
	}
	return fmt.Sprintf("a pull request implementing the approved plan, body containing `Closes #%d`", issueNumber)
}

// implementTask synthesizes the implement classification signal (fed to vetting.ImplementationIntent).
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

// planTask synthesizes the planning request for a plan-labeled issue (for vetting.ImplementationIntent and chat-title fallback).
func planTask(p issuesPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Produce an implementation plan for issue #%d: %s\n", p.Issue.Number, strings.TrimSpace(p.Issue.Title))
	b.WriteString("\nInvestigate the repository first, then lay out a concrete plan: the approach, the files to change, and how to verify it. A maintainer will review the plan before any implementation happens.")
	return b.String()
}

// mergeTimeout bounds the deterministic merge-label handler (a few API calls).
const mergeTimeout = 2 * time.Minute

// mergeIfApproved merges only at the intersection of a human's merge label and quack's own approving verdict.
// A non-approving verdict records a standing intent — merge fires when a later review approves.
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

// reviewVerdictMarkerRe extracts quack's verdict from the hidden marker in an own-PR review comment (GitHub forbids self-review).
var reviewVerdictMarkerRe = regexp.MustCompile(`<!-- quack:delivery:review:(approve|request_changes|comment) -->`)

// formalReviewVerdicts maps GitHub review states to the same vocabulary as reviewVerdictMarkerRe.
var formalReviewVerdicts = map[string]string{
	"APPROVED":          "approve",
	"CHANGES_REQUESTED": "request_changes",
	"COMMENTED":         "comment",
}

// latestQuackVerdict returns quack's most recent review verdict — reads both formal reviews and own-PR comment markers.
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

// tryMergeStandingIntent consumes a merge intent after a review is actually posted.
// headSHA pins the merge to the commit the review was against.
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

// ackReaction posts a 👀 reaction on the mentioning comment — instant code-level acknowledgment, best effort.
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

// ackLabelReaction posts a 👀 reaction on the issue (no comment ID on a label event). Best effort.
func (e *Extension) ackLabelReaction(p issuesPayload) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	if _, err := e.app.reactToIssue(ctx, owner, repo, p.Issue.Number, "eyes"); err != nil {
		slog.Warn("github label ack reaction failed", "component", "github",
			"repo", owner+"/"+repo, "issue", p.Issue.Number, "err", err)
	}
}

// ackDedup fires a 👀 reaction when a dispatch is dropped (run already in-flight). Best effort.
func (e *Extension) ackDedup(owner, repo string, number int) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	// reactToIssue works for both plain issues and PRs.
	if _, err := e.app.reactToIssue(ctx, owner, repo, number, "eyes"); err != nil {
		slog.Warn("github dedup ack reaction failed", "component", "github",
			"repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// triggerTask extracts the task from a mention at the START OF A LINE (leading spaces/tabs only) — makes quote-reply safe.
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

// isTokenRune rejects a mention match that's a prefix of a longer word.
func isTokenRune(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// verifySignature checks GitHub's X-Hub-Signature-256 using constant-time compare — the trust boundary.
func verifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	// hmac.Equal is constant time.
	return hmac.Equal([]byte(header), []byte(expected))
}

// dispatch runs the orchestrator on the task and posts the answer back as a comment.
func (e *Extension) dispatch(p issueCommentPayload, task string) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number

	// Key by commenter's login so sessions are partitioned per-person (#262).
	login := p.Comment.User.Login
	if login == "" {
		login = runUserID
	}

	// Dedup: one run per session — second trigger is dropped, not queued (#665, #668).
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	if _, inflight := e.inflight.LoadOrStore(sessionID, struct{}{}); inflight {
		slog.Info("deduplicated trigger", "sessionID", sessionID)
		go e.ackDedup(owner, repo, number)
		return
	}

	// No deadline — the run deadline is the orchestrator's, covering the server-wide run queue.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer e.inflight.Delete(sessionID)

	e.persistGithubLink(ctx, sessionID, login, owner, repo, number, p.Issue.PullRequest != nil)
	e.ensureTitle(ctx, sessionID, p, task)

	// Label-driven work starts a fresh session — must not inherit prior events.
	resetSession := p.isLabelTrigger
	if resetSession {
		if err := e.runner.ResetSession(ctx, login, sessionID); err != nil {
			slog.Warn("github: session reset failed; this attempt may inherit stale history",
				"component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		}
	}

	// Fetch, diff, and persist the GitHub state (#459).
	gh := e.loadGithubContext(ctx, sessionID, owner, repo, number, p.Issue.PullRequest != nil, p.Comment.ID, resetSession)

	// Never run a label-triggered work request blind — #467 failure mode.
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

	// Compute permission grant once — authorship-check failure denies rather than grants.
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

	// Shares one classifyIssueDeliverable answer across every consumer below
	// (#731) - buildEnvelope, buildWorkerAsk, and deliverableIsPlan must never
	// see two different live classifications for the same run.
	p.issueDeliverableCache = &issueDeliverableResult{}

	// Computed once, reused by the tail (#731): whether this run's deliverable
	// is a plan, regardless of which trigger asked for it.
	isPlan := e.deliverableIsPlan(ctx, p, task, grant, isPR)

	// Context directory (#659/#660): best-effort, skipped when no jail wired.
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
	// #664 consumer split: nodes get the ask-only text, never the orchestrator's evidence.
	workerAsk := e.buildWorkerAsk(ctx, p, task, gh, grant, ctxDir)
	var ciChecks []dag.CICheck
	if p.checkSHA != "" {
		if checks, cerr := e.failingChecks(ctx, owner, repo, p.checkSHA); cerr != nil {
			slog.Warn("github: CI-check fetch for node-scoped detail failed; nodes get none", "component", "github",
				"repo", owner+"/"+repo, "issue", number, "err", cerr)
		} else {
			ciChecks = ciChecksForNodes(checks)
		}
	}

	slog.Info("github run dispatched", "component", "github", "repo", owner+"/"+repo, "issue", number)

	// Persist as a turn so it shows up in getChat.
	var pub *runlog.Publisher
	turnID := uuid.NewString()
	if e.store != nil {
		_ = e.store.SaveTurn(ctx, sessionID, turnID)
		e.eventLog.Reset(ctx, sessionID)
		pub = runlog.NewPublisher(e.hub, e.eventLog, sessionID)
		pub.Publish(stream.ResponseCreated(turnID))
	}

	// Register on the shared hub so REST handler's stop-button can cancel it too (#468).
	runCtx, cancelRun := context.WithCancel(ctx)
	// Stamp deterministic setup facts from the webhook event (#661).
	runCtx = tools.WithGitHubSetup(runCtx, dag.Setup{
		Repo:       p.Repository.CloneURL,
		BaseRef:    setupBaseRef(p, gh),
		WorkBranch: fmt.Sprintf("quack/issue-%d", number),
	})
	// Stamp the authoritative repo/PR for correct_review_finding and plan tool's review head branch.
	if isPR {
		runCtx = tools.WithGitHubPR(runCtx, owner, repo, number, gh.snap.HeadRef)
	}
	// grant computed once above; planner and gate trust the same value.
	runCtx = tools.WithGrant(runCtx, grant)
	// #664: workerAsk/ciChecks computed once above, never re-derived.
	runCtx = tools.WithWorkerAsk(runCtx, workerAsk)
	runCtx = tools.WithCIChecks(runCtx, ciChecks)
	e.hub.RegisterRun(sessionID, turnID, cancelRun)
	// Run stays inline (dispatch already is a goroutine). Deregister last.
	defer e.hub.Close(sessionID)
	defer func() {
		cancelRun()
		e.hub.UnregisterRun(sessionID)
	}()

	// LABEL-triggered runs with no plan get nudged once — a label is an unambiguous work request.
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

	// Only post a summary when nothing was delivered — commitDelivery already posted the review/PR.
	delivered := false
	if d, ok := takeDeliveryDetail(sessionID); ok {
		if d.err != nil {
			// A worker's own report can't be trusted here (#714) — it may claim success it never had.
			slog.Error("github: staged delivery failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", d.err)
			e.postDeliveryFailure(owner, repo, number, d)
			return
		}
		delivered = true
		slog.Info("github: delivery verified against GitHub", "component", "github", "repo", owner+"/"+repo, "issue", number,
			"pr_number", d.prNumber, "pr_url", d.prURL, "pushed_sha", d.pushedSHA)
		// Advance the review baseline with a short-lived context (runCtx may be past deadline).
		if d.reviewDelivered {
			baselineCtx, baselineCancel := context.WithTimeout(context.Background(), 10*time.Second)
			e.advanceReviewBaseline(baselineCtx, sessionID, gh.snap.Commits)
			baselineCancel()

			// Hook point for standing merge intent.
			mergeCtx, mergeCancel := context.WithTimeout(context.Background(), mergeTimeout)
			e.tryMergeStandingIntent(mergeCtx, owner, repo, number, sessionID, gh.snap.HeadSHA)
			mergeCancel()
		}
	}
	if delivered {
		e.persistGithubSnapshot(sessionID, gh)
		slog.Info("github: work delivered on the PR; skipping the duplicate summary comment",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		return
	}

	// HITL pause: post the question as a comment; the reply resumes the paused node.
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

	// A hub-cancelled run is not a timeout — say nothing and let the superseding run deliver.
	if errors.Is(runCtx.Err(), context.Canceled) {
		slog.Info("github: run cancelled (stopped or superseded); skipping tail comment",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		return
	}
	// Fresh context for the tail — runCtx is dead after deadline kill (#286).
	tailCtx := runCtx
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if timedOut {
		var tailCancel context.CancelFunc
		tailCtx, tailCancel = context.WithTimeout(context.Background(), time.Minute)
		defer tailCancel()
	} else {
		e.persistGithubSnapshot(sessionID, gh)
	}
	answer := strings.TrimSpace(e.runner.LatestAnswer(tailCtx, login, sessionID))
	if timedOut {
		answer = fmt.Sprintf("⚠️ quack hit its run deadline (%s) before finishing; nothing was delivered. Re-apply the label to retry.\n\nLast progress:\n\n%s",
			e.runTimeout, answer)
	} else if answer == "" {
		// Silent-gap (#568) — run hit no deadline/cancel yet has no answer.
		otelobs.RecordRunNoAnswer()
		slog.Warn("github: run completed with no final answer", "component", "github", "repo", owner+"/"+repo, "issue", number)
		answer = "⚠️ quack finished this run but produced no answer - no error, no failed node, nothing delivered. " +
			"That's a silent-gap failure, not a run with nothing to say. Re-apply the label to retry."
	} else {
		answer = e.reviseInvalidMermaid(runCtx, login, sessionID, owner, repo, number, turnID, pub, answer)
		if isPlan {
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

// postDeliveryFailure reports a failed delivery on GitHub, so a pushed-but-unopened branch is recoverable by hand instead of sitting silently invisible (#714).
func (e *Extension) postDeliveryFailure(owner, repo string, number int, d deliveryOutcome) {
	msg := fmt.Sprintf("⚠️ delivery failed: %s", d.err)
	if d.branch != "" {
		msg += fmt.Sprintf("\n\nBranch `%s` was not delivered — recover it by hand.", d.branch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.app.postIssueComment(ctx, owner, repo, number, msg); err != nil {
		slog.Error("github: delivery-failure comment post failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// reviseInvalidMermaid nudges the run once, then degrades if still invalid (never silently strips).
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
	// Each issue gets its own fenced block, not a "- " bullet: bare newlines
	// inside a markdown list item break the list AND misalign the parser's
	// caret (#735).
	nudge := fmt.Sprintf(
		"Your last answer contains invalid mermaid diagram(s) that GitHub cannot render:\n\n%s\n\n"+
			"Fix each diagram so it parses, or remove it if it isn't essential, then repost your complete answer.",
		vetting.FormatMermaidNudgeBody(issues))
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

// persistGithubLink stores the issue/PR URL on the session's chat row. Best-effort.
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

// ensureTitle uses the issue/PR title for the chat title (dispatch never calls generateTitle, #380).
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

// drive runs one orchestrator turn to completion. Reports planRan, judgePassed, and whether the run paused (HITL).
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

// githubContext is the loaded GitHub state for one dispatch (#459).
type githubContext struct {
	snap               Snapshot
	delta              *Delta // nil on first load (#666), set on resume
	firstLoad          bool
	contextUnavailable bool             // fetchSnapshot's meta call failed — label-triggered work aborts (#467)
	newCommits         []snapshotCommit // PR commits for incremental review scope; nil = review everything
}

// loadGithubContext fetches current GitHub state and diffs it against the stored snapshot.
// Does NOT persist — persistGithubSnapshot runs on completion.
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

// persistGithubSnapshot upserts the pre-run snapshot as the new watermark — only on genuine completion.
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

// reviewScope returns commits not yet covered by quack's last delivered review. Falls back to nil (review everything).
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

// advanceReviewBaseline persists current PR commits' patch-ids — only after a review is actually delivered.
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

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// setupBaseRef returns the PR's base branch, or the repo's default branch for an issue run (#661).
func setupBaseRef(p issueCommentPayload, gh githubContext) string {
	if gh.snap.BaseRef != "" {
		return gh.snap.BaseRef
	}
	if p.Repository.DefaultBranch != "" {
		return p.Repository.DefaultBranch
	}
	return "main"
}
