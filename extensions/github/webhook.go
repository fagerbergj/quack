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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// maxWebhookBody bounds a hostile/oversized request.
const maxWebhookBody = 5 << 20

type ghRepository struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

type ghInstallation struct {
	ID int64 `json:"id"`
}

// issueCommentPayload is the subset of GitHub's issue_comment webhook we use.
type issueCommentPayload struct {
	Action  string `json:"action"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		AuthorAssociation string `json:"author_association"`
	} `json:"comment"`
	Issue struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		// Present only when the issue is a PR.
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Repository   ghRepository   `json:"repository"`
	Installation ghInstallation `json:"installation"`

	// Synthetic payload fields — not part of the GitHub webhook.
	planOnly        bool            // label-driven plan: produce a plan, touch no code.
	isLabelTrigger  bool            // label/pr_opened trigger vs @mention; resets the session.
	deliverableHint string          // fixed deliverable for synthetic triggers (CI auto-heal, own-PR).
	rawEvent        json.RawMessage // originating webhook JSON → envelope's <event> block.
	eventName       string          // originating webhook dotted name.
	checkSHA        string          // CI commit: write the "check-runs" input artifact. "" = plan/review/mention run.
	explain         bool            // /explain: own per-user chat, no delivery, nothing posted but the chat link.
	// issueDeliverableCache is shared by pointer across copies of p so one dispatch classifies at most once;
	// nil outside a dispatch.
	issueDeliverableCache *issueDeliverableResult
	deliverableKind       *string // the envelope's deliverable kind, for the intent point; nil outside a dispatch
}

// issuesPayload is the issues webhook subset for the label-driven issue workflow.
type issuesPayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		// Present when the "issue" is actually a PR.
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
	Repository   ghRepository   `json:"repository"`
	Installation ghInstallation `json:"installation"`
	Sender       struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// pullRequestPayload is the PR webhook subset for opened/labeled/closed/reopened actions.
type pullRequestPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Title string `json:"title"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
		State  string `json:"state"`
		Merged bool   `json:"merged"` // set on "closed" - distinguishes a merge from a plain close
	} `json:"pull_request"`
	Label struct {
		Name string `json:"name"` // present on the "labeled" action
	} `json:"label"`
	Repository   ghRepository   `json:"repository"`
	Installation ghInstallation `json:"installation"`
	Sender       struct {
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
		e.handlePullRequest(w, body, r.Header.Get("X-GitHub-Delivery"))
	case "pull_request_review":
		e.handlePullRequestReview(w, body)
	case "issues":
		e.handleIssues(w, body)
	case "workflow_run":
		e.mergeOnCheckEvent("workflow_run", body)
		e.handleWorkflowRun(w, body)
	case "check_suite", "check_run":
		e.mergeOnCheckEvent(r.Header.Get("X-GitHub-Event"), body)
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusOK) // unhandled event type: no-op ack
	}
}

// rejectDisallowedInvoker logs and 200-acks a webhook from a user not in
// allowed_users; it returns true when the request was rejected.
func (e *Extension) rejectDisallowedInvoker(w http.ResponseWriter, repo, kind string, number int, login string) bool {
	if e.isInvokerAllowed(login) {
		return false
	}
	slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
		"repo", repo, kind, number, "user", login)
	w.WriteHeader(http.StatusOK)
	return true
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
	if cmd, task, ok := e.prCommand(&p); ok {
		if e.rejectDisallowedInvoker(w, p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number, p.Comment.User.Login) {
			return
		}
		p.rawEvent = json.RawMessage(body)
		p.eventName = "issue_comment." + p.Action
		slog.Info("github webhook received", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Issue.Number,
			"command", cmd, "user", p.Comment.User.Login, "installation", p.Installation.ID)
		e.spawn(func() { e.ackReaction(p) })
		e.spawn(func() { e.dispatch(p, task) })
		w.WriteHeader(http.StatusAccepted)
		return
	}
	task, ok := e.triggerTask(p)
	if !ok {
		w.WriteHeader(http.StatusOK) // not a mention we act on: no-op ack
		return
	}
	p.rawEvent = json.RawMessage(body)
	p.eventName = "issue_comment." + p.Action
	if e.rejectDisallowedInvoker(w, p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number, p.Comment.User.Login) {
		return
	}

	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number,
		"user", p.Comment.User.Login, "installation", p.Installation.ID)
	// Instant 👀 "quack saw it", independent of the model run.
	e.spawn(func() { e.ackReaction(p) })
	e.spawn(func() { e.dispatch(p, task) })
	w.WriteHeader(http.StatusAccepted)
}

// handlePullRequest routes PR actions: auto-review, merge/fix labels, synchronize, and badge refresh.
// sloplint: cc-allow flat webhook action dispatcher - one case per action, no shared logic to extract
func (e *Extension) handlePullRequest(w http.ResponseWriter, body []byte, deliveryID string) {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// Logged before any dedup: the one place to tell a delivery GitHub never sent from one this handler dropped.
	if p.Action == "labeled" || p.Action == "unlabeled" {
		slog.Info("github webhook: label delivery received", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
			"action", p.Action, "label", p.Label.Name, "sender", p.Sender.Login, "delivery_id", deliveryID)
	}

	if p.Action == "closed" || p.Action == "reopened" {
		badge, state := "open", sdk.SubjectOpen
		switch {
		case p.Action == "closed" && p.PullRequest.Merged:
			badge, state = "merged", sdk.SubjectMerged
		case p.Action == "closed":
			badge, state = "closed", sdk.SubjectClosed
		}
		e.refreshChatOrigin(p.Repository.Owner.Login, p.Repository.Name, true, p.Number, badge, state)
		if p.Action == "closed" {
			chatID := globalChatID(issueSessionID(p.Repository.Owner.Login, p.Repository.Name, p.Number))
			e.clearMergeIntent(context.Background(), chatID)
			e.forgetChatHistory(chatID)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// Removing the merge label withdraws the standing authorization.
	if p.Action == "unlabeled" && p.Label.Name == e.labels.Merge {
		e.clearMergeIntent(context.Background(), globalChatID(issueSessionID(p.Repository.Owner.Login, p.Repository.Name, p.Number)))
		w.WriteHeader(http.StatusOK)
		return
	}

	// The merge label is a standing human authorization, merged once quack approves the head and CI is green.
	if e.labeledTrigger(w, p, "merge", e.labels.Merge, func() { e.spawn(func() { e.mergeIfApproved(p, body) }) }) {
		return
	}

	// quack:fix is a persistent flag: it re-arms auto-heal and fixes CI if currently failing.
	if e.labeledTrigger(w, p, "ci_fix", e.labels.Fix, func() { e.spawn(func() { e.fixLabelApplied(p, body) }) }) {
		return
	}

	// A push under a running review strands its clone; core decides if refreshing is safe.
	if p.Action == "synchronize" {
		e.invalidateSetup(p.Repository.Owner.Login, p.Repository.Name, p.Number)
		e.spawn(func() {
			e.mergeOnEvent(p.Repository.Owner.Login, p.Repository.Name, p.Number, "pull_request.synchronize")
		})
		e.spawn(func() { e.reviewOnMovedHeadUnderIntent(p, body) })
		w.WriteHeader(http.StatusOK)
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
	e.spawn(func() { e.dispatch(autoReviewPayload(p, body), autoReviewTask) })
	w.WriteHeader(http.StatusAccepted)
}

// labeledTrigger fires kick (202) on a matching human label, 200s a non-invoker, and returns false
// with nothing written when the trigger doesn't match.
func (e *Extension) labeledTrigger(w http.ResponseWriter, p pullRequestPayload, trigger, label string, kick func()) bool {
	if p.Action != "labeled" || !e.triggers[trigger] || p.Label.Name != label ||
		strings.HasSuffix(p.Sender.Login, "[bot]") {
		return false
	}
	if !e.isInvokerAllowed(p.Sender.Login) {
		slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
			"label", p.Label.Name, "user", p.Sender.Login)
		w.WriteHeader(http.StatusOK)
		return true
	}
	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
		"label", p.Label.Name, "user", p.Sender.Login, "installation", p.Installation.ID)
	kick()
	w.WriteHeader(http.StatusAccepted)
	return true
}

// invalidateSetup signals a moved branch only while a run is in flight; otherwise there is no clone to refresh.
func (e *Extension) invalidateSetup(owner, repo string, number int) {
	if e.host.InvalidateSetup == nil {
		return
	}
	chatID := globalChatID(issueSessionID(owner, repo, number))
	if _, running := e.pending.Load(chatID); !running {
		return
	}
	slog.Info("github: branch moved under a running review; invalidating its clone",
		"component", "github", "repo", owner+"/"+repo, "pr", number)
	if err := e.host.InvalidateSetup(chatID); err != nil {
		slog.Warn("github: invalidate setup failed", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
	}
}

// autoReviewPayload shapes a PR event as an issueCommentPayload for the mention path's dispatch.
func autoReviewPayload(p pullRequestPayload, rawBody []byte) issueCommentPayload {
	return repoInfoOf(p.Repository, p.Installation).autoReview(p.Number, p.PullRequest.Title, rawBody, "pull_request."+p.Action)
}

// pullRequestReviewPayload is the pull_request_review webhook subset.
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
	Repository   ghRepository   `json:"repository"`
	Installation ghInstallation `json:"installation"`
}

// handlePullRequestReview engages only on request_changes to a PR quack authored — gated on ci_fix.
func (e *Extension) handlePullRequestReview(w http.ResponseWriter, body []byte) {
	var p pullRequestReviewPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if p.Action == "submitted" {
		e.spawn(func() {
			e.mergeOnEvent(p.Repository.Owner.Login, p.Repository.Name, p.PullRequest.Number, "pull_request_review.submitted")
		})
	}
	if p.Action != "submitted" || p.Review.State != "changes_requested" || !e.triggers["ci_fix"] {
		w.WriteHeader(http.StatusOK)
		return
	}
	if strings.HasSuffix(p.Review.User.Login, "[bot]") {
		w.WriteHeader(http.StatusOK)
		return
	}
	if e.rejectDisallowedInvoker(w, p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.PullRequest.Number, p.Review.User.Login) {
		return
	}
	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.PullRequest.Number,
		"user", p.Review.User.Login, "installation", p.Installation.ID)
	e.spawn(func() { e.engageOwnPRReview(p, body) })
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

	sessionID := issueSessionID(owner, repo, number)
	login := p.Review.User.Login
	if login != "" && e.host.ChatUser != nil {
		if u, ok := e.host.ChatUser(globalChatID(sessionID)); ok && u != "" {
			login = u
		}
	}

	synthetic := repoInfoOf(p.Repository, p.Installation).synthetic(number, login)
	synthetic.Issue.PullRequest = &struct{}{}
	// isLabelTrigger stays false: this continues the PR's existing session.
	synthetic.rawEvent = json.RawMessage(rawBody)
	synthetic.eventName = "pull_request_review." + p.Action
	synthetic.deliverableHint = "a commit addressing every finding in the review that requested changes"

	slog.Info("github: engaging own PR after requested changes", "component", "github", "repo", owner+"/"+repo, "pr", number)
	e.dispatch(synthetic, fmt.Sprintf(
		"%s requested changes on this pull request, which you authored. Address every finding: read the review comments and the current diff, make the fix, run the repo's own checks to verify, and commit the change on this PR's existing head branch.",
		p.Review.User.Login))
}

// handleIssues drives the label-driven issue workflow (plan/implement labels)
// and refreshes the sidebar badge on close/reopen.
func (e *Extension) handleIssues(w http.ResponseWriter, body []byte) {
	var p issuesPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// A real issue's own state change - not the label-driven work triggers below.
	if p.Issue.PullRequest == nil && (p.Action == "closed" || p.Action == "reopened") {
		badge, state := "open", sdk.SubjectOpen
		if p.Action == "closed" {
			badge, state = "closed", sdk.SubjectClosed
			e.forgetChatHistory(globalChatID(issueSessionID(p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number)))
		}
		e.refreshChatOrigin(p.Repository.Owner.Login, p.Repository.Name, false, p.Issue.Number, badge, state)
		w.WriteHeader(http.StatusOK)
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
	synthetic := repoInfoOf(p.Repository, p.Installation).synthetic(p.Issue.Number, p.Sender.Login)
	synthetic.Issue.Title = p.Issue.Title
	synthetic.isLabelTrigger = true // quack:plan/quack:implement, never a mention
	synthetic.rawEvent = json.RawMessage(body)
	synthetic.eventName = "issues.labeled"

	switch {
	case e.triggers["issue_plan"] && p.Label.Name == e.labels.Plan:
		synthetic.planOnly = true
		// Instant 👀 on the issue - the label path's equivalent of ackReaction.
		e.spawn(func() { e.ackLabelReaction(p) })
		e.spawn(func() { e.dispatch(synthetic, planTask(p)) })
	case e.triggers["issue_implement"] && p.Label.Name == e.labels.Implement:
		e.spawn(func() { e.ackLabelReaction(p) })
		e.spawn(func() { e.runImplement(p, synthetic) })
	default:
		w.WriteHeader(http.StatusOK)
		return
	}
	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number,
		"label", p.Label.Name, "user", p.Sender.Login, "installation", p.Installation.ID)
	w.WriteHeader(http.StatusAccepted)
}

// runImplement dispatches on the planning run's session so the plan is in history; current labels
// decide whether the task asks for a Closes keyword (not on partial-fix).
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

// issueImplementDeliverable is the PR-implementing deliverable text, shared by
// the label trigger and a comment classified/heuristically read as implement.
func issueImplementDeliverable(partialFixLabel string, labels []string, issueNumber int) string {
	if slices.Contains(labels, partialFixLabel) {
		return "a pull request implementing the changes, without a Closes keyword (this is a partial fix)"
	}
	return fmt.Sprintf("a pull request implementing the approved plan, body containing `Closes #%d`", issueNumber)
}

// implementTask synthesizes the implement classification signal (fed to implementationIntent).
func implementTask(p issuesPayload, labels []string, partialFixLabel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Implement issue #%d: %s\n", p.Issue.Number, strings.TrimSpace(p.Issue.Title))

	if body := strings.TrimSpace(p.Issue.Body); body != "" {
		fmt.Fprintf(&b, "\nIssue description (may be incomplete - see discussion below):\n%s\n", truncate(body, 4000))
	}

	isPartial := slices.Contains(labels, partialFixLabel)
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

// planTask synthesizes the planning request for a plan-labeled issue.
func planTask(p issuesPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Produce an implementation plan for issue #%d: %s\n", p.Issue.Number, strings.TrimSpace(p.Issue.Title))
	b.WriteString("\nInvestigate the repository first, then lay out a concrete plan: the approach, the files to change, and how to verify it. A maintainer will review the plan before any implementation happens.")
	return b.String()
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

// prCommand matches a bare PR slash command and shapes p for its dispatch.
func (e *Extension) prCommand(p *issueCommentPayload) (cmd, task string, ok bool) {
	switch {
	// A bare "/review" re-runs the label-triggered review: cycling the label
	// off/on fast enough coalesces into no webhook at all.
	case e.isReviewCommand(*p):
		p.isLabelTrigger = true // same run the labeled event dispatches, not a mention
		return "/review", autoReviewTask, true
	case e.isPRCommand(*p, "explain", "/explain"):
		return "/explain", shapeExplain(p), true
	}
	return "", "", false
}

// isPRCommand reports whether a comment is exactly cmd (surrounding
// whitespace allowed) on a PR, from a write/admin author, with trigger enabled.
func (e *Extension) isPRCommand(p issueCommentPayload, trigger, cmd string) bool {
	if !e.triggers[trigger] || p.Action != "created" || p.Issue.PullRequest == nil {
		return false
	}
	if strings.TrimSpace(p.Comment.Body) != cmd {
		return false
	}
	switch p.Comment.AuthorAssociation {
	case "OWNER", "MEMBER", "COLLABORATOR": // write or admin
		return true
	}
	return false
}

// isReviewCommand: a bare "/review" on a PR carrying the review label, gated
// on the same "label" trigger as the labeled-event path.
func (e *Extension) isReviewCommand(p issueCommentPayload) bool {
	return e.isPRCommand(p, "label", "/review") && e.prHasReviewLabel(p)
}

// triggerTask extracts the task from a mention at the start of a line, so quote-replies don't trigger.
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

// SignWebhookBody computes the X-Hub-Signature-256 header value for body -
// the QA mock sender's counterpart to verifySignature below.
func SignWebhookBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// verifySignature checks GitHub's X-Hub-Signature-256 using constant-time compare — the trust boundary.
func verifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	return hmac.Equal([]byte(header), []byte(SignWebhookBody(secret, body)))
}

// runNudge follows a run that answered without running a plan. It names no tool: quack owns and
// renames its planning tools.
const runNudge = "You answered without running anything. Do NOT reply in prose: plan and run the work NOW - clone the repo, read the change, and carry out the review (or the requested change). Nothing has run yet and the user is waiting."

// inflightLease bounds a claim, since a force-cancelled or killed run never reaches RunEnded and a bare
// flag would wedge the session. The margin covers finalize's GitHub calls after the run deadline.
func (e *Extension) inflightLease() time.Duration { return e.runTimeout + 10*time.Minute }

// inflightActive reports a session's claim age and whether it is still live.
// An expired claim is a leak, not a running review.
func (e *Extension) inflightActive(sessionID string) (time.Duration, bool) {
	v, ok := e.inflight.Load(sessionID)
	if !ok {
		return 0, false
	}
	age := time.Since(v.(time.Time))
	return age, age < e.inflightLease()
}

// claimInflight takes sessionID's slot, taking over an expired claim; age is the displaced claim's.
// claimedAt is finalize's release token, so a late run can't free a claim a takeover re-took.
func (e *Extension) claimInflight(sessionID string) (claimedAt time.Time, age time.Duration, claimed bool) {
	lease := e.inflightLease()
	for {
		now := time.Now()
		v, loaded := e.inflight.LoadOrStore(sessionID, now)
		if !loaded {
			return now, 0, true
		}
		prev := v.(time.Time)
		if age = now.Sub(prev); age < lease {
			return time.Time{}, age, false
		}
		if e.inflight.CompareAndSwap(sessionID, prev, now) {
			return now, age, true
		}
	}
}

// dispatch sends one fire-and-forget Host.Dispatch for a trigger; pendingRun and RunEnded (run.go)
// take over once it is accepted.
func (e *Extension) dispatch(p issueCommentPayload, task string) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number

	// The commenter's login is the run's session identity.
	login := p.Comment.User.Login
	if login == "" {
		login = runUserID
	}
	// One run per session: a second trigger is dropped, not queued.
	sessionID := sessionIDFor(p, login)
	chatID := globalChatID(sessionID)
	claimedAt, claimed := e.claimOrAck(p, sessionID, owner, repo, number)
	if !claimed {
		return
	}
	// Released exactly once, here on failure or by finalize; compare-and-delete so a straggler
	// can't release a claim a takeover re-took.
	clearInflight := func() { e.inflight.CompareAndDelete(sessionID, claimedAt) }

	// Pre-dispatch fetches and classifier calls need the API-phase budget, not the reaction-ack one.
	ctx, cancel := context.WithTimeout(context.Background(), fixContextTimeout)
	defer cancel()

	isPR := p.Issue.PullRequest != nil

	gh := e.loadGithubContext(ctx, chatID, owner, repo, number, isPR, p.Comment.ID, false)

	// Label-driven work starts a fresh session — must not inherit prior events.
	resetSession := p.isLabelTrigger
	if resetSession {
		gh = e.loadGithubContext(ctx, chatID, owner, repo, number, isPR, p.Comment.ID, true)
	}

	if e.abortBlind(p, gh, owner, repo, number) {
		clearInflight()
		return
	}

	allowedKinds := e.dispatchGrant(ctx, p, owner, repo, number, isPR, gh)

	p.issueDeliverableCache = &issueDeliverableResult{}
	p.deliverableKind = new(string)
	isPlan := e.deliverableIsPlan(ctx, p, task, allowedKinds, isPR)

	// Input artifacts hold heavy evidence a worker only sometimes needs; best-effort, skipped without the capability.
	manifest := e.writeDispatchArtifacts(ctx, chatID, login, p, owner, repo, number, isPR)

	message := e.buildEnvelope(ctx, p, task, gh, allowedKinds, manifest)
	e.observeIntent(ctx, p, chatID, allowedKinds, *p.deliverableKind)
	workerAsk := e.buildWorkerAsk(ctx, p, task, gh, allowedKinds, manifest)
	contextItems := e.nodeContextItems(ctx, owner, repo, number, p.checkSHA)

	title := dispatchTitle(p, task)

	setup := &sdk.Setup{Repo: p.Repository.CloneURL, BaseRef: setupBaseRef(p, gh), WorkBranch: fmt.Sprintf("quack/issue-%d", number)}
	if isPR {
		if !e.resolvePRHead(ctx, p, owner, repo, number, gh, setup, clearInflight) {
			return
		}
	}

	badge, subjectState := e.prBadgeState(gh, isPR)
	o := chatOrigin(owner, repo, isPR, number, badge, subjectState)
	origin := &o

	req := sdk.DispatchRequest{
		Chat: sdk.ChatRef{
			LocalID:      sessionID,
			User:         login,
			Title:        title,
			Origin:       origin,
			ResetHistory: resetSession,
		},
		Ask: sdk.Ask{
			Message:      message,
			NodeContext:  workerAsk,
			ContextItems: contextItems,
		},
		Run: sdk.RunConfig{
			Setup:    setup,
			ReadOnly: p.planOnly,
			Timeout:  e.runTimeout,
		},
		Delivery: sdk.DeliveryAuthority{AllowedKinds: sdkDeliveryKinds(allowedKinds)},
	}

	// Stored BEFORE Dispatch: RunEnded can fire as soon as Dispatch returns.
	e.pending.Store(chatID, &pendingRun{
		sessionID: sessionID, claimedAt: claimedAt, owner: owner, repo: repo, number: number,
		isPR: isPR, login: login, gh: gh, isPlan: isPlan, isLabelTrigger: p.isLabelTrigger, explain: p.explain,
		isReview: task == autoReviewTask, dispatched: req, defaultBranch: p.Repository.DefaultBranch, installationID: p.Installation.ID,
	})
	// Durable twin of e.pending, so a run resumed after a restart can still be finalized.
	if err := e.store.SetPendingRun(ctx, PendingRunRow{
		ChatID: chatID, SessionID: sessionID, Owner: owner, Repo: repo, Number: number,
		IsPR: isPR, Login: login, IsPlan: isPlan, IsLabelTrigger: p.isLabelTrigger,
		CommentID: p.Comment.ID, DefaultBranch: p.Repository.DefaultBranch,
		InstallationID: p.Installation.ID, CloneURL: p.Repository.CloneURL,
	}); err != nil {
		slog.Warn("github: SetPendingRun failed; a restart before this run ends will drop its outcome",
			"component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}

	slog.Info("github run dispatched", "component", "github", "repo", owner+"/"+repo, "issue", number)
	if err := e.host.Dispatch(ctx, req); err != nil {
		slog.Error("github: dispatch failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		e.pending.Delete(chatID)
		clearInflight()
		// Keep the in-memory and durable records in lockstep; finalize does this on success paths.
		if derr := e.store.DeletePendingRun(ctx, chatID); derr != nil {
			slog.Warn("github: DeletePendingRun after failed dispatch", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", derr)
		}
		return
	}
	if p.explain {
		e.postExplainLink(ctx, owner, repo, number, login, chatID)
	}
}

// sessionIDFor keys the run's chat: one per issue/PR, or per requester for /explain.
func sessionIDFor(p issueCommentPayload, login string) string {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number
	if p.explain {
		return explainSessionID(owner, repo, number, login)
	}
	return issueSessionID(owner, repo, number)
}

// issueSessionID is the shared per-issue/PR session key.
func issueSessionID(owner, repo string, number int) string {
	return fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
}

// dispatchTitle: the chat title - the issue/PR title, else the task.
func dispatchTitle(p issueCommentPayload, task string) string {
	title := strings.TrimSpace(p.Issue.Title)
	if title == "" {
		title = truncate(task, 80)
	}
	if p.explain {
		return "Explain: " + title
	}
	return title
}

// dispatchGrant computes the run's delivery grant once; authorship-check
// failure denies rather than grants. Non-nil empty = deny-all (nil = unrestricted).
func (e *Extension) dispatchGrant(ctx context.Context, p issueCommentPayload, owner, repo string, number int, isPR bool, gh githubContext) []string {
	if p.explain {
		return []string{}
	}
	_, allowedKinds := e.grantForDispatch(ctx, owner, repo, number, isPR, gh)
	return allowedKinds
}

// abortBlind refuses a label-triggered or /explain run with no usable GitHub context and says so
// on the thread; true means do not dispatch.
func (e *Extension) abortBlind(p issueCommentPayload, gh githubContext, owner, repo string, number int) bool {
	if !gh.contextUnavailable || !(p.isLabelTrigger || p.explain) {
		return false
	}
	slog.Warn("github: work request has no usable GitHub context; aborting rather than running blind",
		"component", "github", "repo", owner+"/"+repo, "issue", number)
	abortCtx, abortCancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer abortCancel()
	abortMsg := "Could not load this issue from GitHub; not running. Re-apply the label to retry."
	if p.explain {
		abortMsg = "Could not load this pull request from GitHub; not running. Comment /explain again to retry."
	}
	if err := e.app.postIssueComment(abortCtx, owner, repo, number, abortMsg); err != nil {
		slog.Warn("github: abort comment failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
	return true
}

// nodeContextItems: the CI-check detail a checkSHA-scoped run injects per
// node - a failed fetch means no nodes get any, not a failed dispatch.
func (e *Extension) nodeContextItems(ctx context.Context, owner, repo string, number int, checkSHA string) []sdk.NamedContext {
	if checkSHA == "" {
		return nil
	}
	checks, cerr := e.failingChecks(ctx, owner, repo, checkSHA)
	if cerr != nil {
		slog.Warn("github: CI-check fetch for node-scoped detail failed; nodes get none", "component", "github",
			"repo", owner+"/"+repo, "issue", number, "err", cerr)
		return nil
	}
	return ciChecksForNodes(checks)
}

// prBadgeState: the sidebar badge + typed subject state for a chat origin -
// merged beats draft; draft is a badge-only distinction while still open.
func (e *Extension) prBadgeState(gh githubContext, isPR bool) (string, sdk.SubjectState) {
	badge := gh.snap.State
	subjectState := sdk.SubjectOpen
	if gh.snap.State == "closed" {
		subjectState = sdk.SubjectClosed
	}
	if isPR && gh.snap.Merged {
		badge, subjectState = "merged", sdk.SubjectMerged
	} else if isPR && gh.snap.Draft {
		badge = "draft" // still open - draft is a badge-only distinction
	}
	return badge, subjectState
}

// claimOrAck is the one-run-per-session gate: a live claim drops the trigger with an ack, an
// expired one is taken over with a warning.
func (e *Extension) claimOrAck(p issueCommentPayload, sessionID, owner, repo string, number int) (claimedAt time.Time, claimed bool) {
	var age time.Duration
	claimedAt, age, claimed = e.claimInflight(sessionID)
	if !claimed {
		slog.Info("deduplicated trigger: a run for this session is still in flight",
			"component", "github", "sessionID", sessionID, "repo", owner+"/"+repo, "issue", number,
			"claim_age", age.Round(time.Second), "lease", e.inflightLease())
		// A comment trigger already got a reaction on its comment; reacting on the issue too would double it.
		if p.Comment.ID != 0 {
			e.spawn(func() { e.ackReaction(p) })
		} else {
			e.spawn(func() { e.ackIssue(owner, repo, number) })
		}
		if p.explain {
			e.spawn(func() { e.postExplainLinkNow(owner, repo, number, p.Comment.User.Login, globalChatID(sessionID)) })
		}
		return claimedAt, false
	}
	if age > 0 {
		slog.Warn("github: took over an expired in-flight claim; the run holding it never settled",
			"component", "github", "sessionID", sessionID, "repo", owner+"/"+repo, "issue", number,
			"claim_age", age.Round(time.Second), "lease", e.inflightLease())
	}
	return claimedAt, true
}

// grantForDispatch computes the run's delivery grant once: the authorship
// check (a failed check denies rather than grants) feeds computeGrant.
func (e *Extension) grantForDispatch(ctx context.Context, owner, repo string, number int, isPR bool, gh githubContext) (authored bool, allowedKinds []string) {
	if isPR {
		if a, aerr := e.authoredByQuack(ctx, owner, repo, number); aerr != nil {
			slog.Warn("github: authorship check failed computing this run's grant; treating as not-authored",
				"component", "github", "repo", owner+"/"+repo, "issue", number, "err", aerr)
		} else {
			authored = a
		}
	}
	return authored, computeGrant(e.labels, gh.snap.Labels, isPR, authored, gh.snap.Fork)
}

// writeDispatchArtifacts: the per-dispatch input artifacts plus the raw
// event, name-sorted - all best-effort, all skipped without the capability.
func (e *Extension) writeDispatchArtifacts(ctx context.Context, chatID, login string, p issueCommentPayload, owner, repo string, number int, isPR bool) []artifactEntry {
	if e.host.WriteArtifact == nil {
		return nil
	}
	manifest := e.writeInputArtifacts(ctx, chatID, login, ContextRequest{
		Owner: owner, Repo: repo, Number: number, IsPR: isPR, CheckSHA: p.checkSHA,
	})
	if entry := writeArtifact(e.host, chatID, login, "event", "application/json", p.rawEvent, eventNote(p)); entry != nil {
		manifest = append(manifest, *entry)
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Name < manifest[j].Name })
	return manifest
}

// resolvePRHead sets setup.ExistingHeadRef to the PR's head branch, refetching once and refusing
// to dispatch with a blank ref.
func (e *Extension) resolvePRHead(ctx context.Context, p issueCommentPayload, owner, repo string, number int, gh githubContext, setup *sdk.Setup, clearInflight func()) bool {
	headRef := gh.snap.HeadRef
	if headRef != "" {
		setup.ExistingHeadRef = headRef
		return true
	}
	// The snapshot's pullMeta can fail transiently without tripping contextUnavailable; refetch once.
	m, merr := e.app.pullMeta(ctx, owner, repo, number)
	if merr == nil && m.HeadRef != "" {
		setup.ExistingHeadRef = m.HeadRef
		return true
	}
	slog.Error("github: PR dispatch has no head ref even after refetch; refusing to send a blank ExistingHeadRef",
		"component", "github", "repo", owner+"/"+repo, "issue", number, "err", merr)
	abortCtx, abortCancel := context.WithTimeout(context.Background(), reactionTimeout)
	abortMsg := "Could not determine this PR's head branch; not running. Retry the command."
	if perr := e.app.postIssueComment(abortCtx, owner, repo, number, abortMsg); perr != nil {
		slog.Warn("github: abort comment failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", perr)
	}
	abortCancel()
	clearInflight()
	return false
}

// sdkDeliveryKinds types computeGrant's allowlist; nil stays nil (unrestricted).
func sdkDeliveryKinds(kinds []string) []sdk.DeliveryKind {
	if kinds == nil {
		return nil
	}
	out := make([]sdk.DeliveryKind, len(kinds))
	for i, k := range kinds {
		out[i] = sdk.DeliveryKind(k)
	}
	return out
}

// chatOrigin builds an issue/PR chat's provenance, stamped by dispatch and refreshChatOrigin.
func chatOrigin(owner, repo string, isPR bool, number int, badge string, state sdk.SubjectState) sdk.ChatOrigin {
	// kind is the SDK's documented grouping vocabulary; seg is GitHub's URL
	// spelling. Same distinction, different spellings - don't merge them.
	kind, seg := "issue", "issues"
	if isPR {
		kind, seg = "pr", "pull"
	}
	return sdk.ChatOrigin{
		Extension: extensionName,
		Label:     fmt.Sprintf("%s/%s %s#%d", owner, repo, kind, number),
		Kind:      kind,
		Href:      fmt.Sprintf("https://github.com/%s/%s/%s/%d", owner, repo, seg, number),
		Badge:     badge,
		State:     state,
		Labels:    map[string][]sdk.LabelValue{"repo": {{Value: owner + "/" + repo, Href: fmt.Sprintf("https://github.com/%s/%s", owner, repo)}}},
	}
}

// refreshChatOrigin advances Badge and State after a state-change webhook. Most issues never had a
// chat, so ErrUnknownChat is expected and logged at Debug.
func (e *Extension) refreshChatOrigin(owner, repo string, isPR bool, number int, badge string, state sdk.SubjectState) {
	if e.host.UpdateChatOrigin == nil {
		return
	}
	sessionID := issueSessionID(owner, repo, number)
	err := e.host.UpdateChatOrigin(sessionID, chatOrigin(owner, repo, isPR, number, badge, state))
	switch {
	case err == nil:
	case errors.Is(err, sdk.ErrUnknownChat):
		e.host.Log.Debug("github: origin refresh skipped; no chat dispatched for this issue/PR",
			"repo", owner+"/"+repo, "number", number)
	default:
		e.host.Log.Warn("github: origin refresh failed", "repo", owner+"/"+repo, "number", number, "err", err)
	}
}

// forgetChatHistory drops a closed chat's snapshot and baseline; a reopen starts from a first load.
// ponytail: a run finishing after the close re-persists its row; sweep by age if that ever piles up.
func (e *Extension) forgetChatHistory(chatID string) {
	if err := e.store.DeleteChatHistory(context.Background(), chatID); err != nil {
		e.host.Log.Warn("github: chat history cleanup failed", "chat", chatID, "err", err)
	}
}

// setupBaseRef returns the PR's base branch, or the repo's default branch for an issue run.
func setupBaseRef(p issueCommentPayload, gh githubContext) string {
	if gh.snap.BaseRef != "" {
		return gh.snap.BaseRef
	}
	if p.Repository.DefaultBranch != "" {
		return p.Repository.DefaultBranch
	}
	return "main"
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
