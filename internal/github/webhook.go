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

	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"

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
		go e.mergeIfApproved(p)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// (Re-)applying the monitor label re-arms auto-heal: a deterministic
	// counter reset, no model run. Not allowlist-gated - the ci_fix gate reads
	// the label's PRESENCE regardless of who applied it (write access is the
	// permission), so gating only the reset would secure nothing.
	if p.Action == "labeled" && e.triggers["ci_fix"] && p.Label.Name == e.labels.Monitor &&
		!strings.HasSuffix(p.Sender.Login, "[bot]") && e.store != nil {
		go e.resetFixState(p)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// The fix label is an explicit human work request: fix the currently
	// failing checks, once. Bot senders never chain label workflows.
	if p.Action == "labeled" && e.triggers["pr_fix"] && p.Label.Name == e.labels.Fix &&
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
		go e.runFixLabel(p)
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
	go e.dispatch(autoReviewPayload(p), autoReviewTask)
	w.WriteHeader(http.StatusAccepted)
}

// autoReviewPayload reuses the mention path's dispatch/runMessage by shaping
// a PR event as an issueCommentPayload - same session key, same review-tool
// guidance, no duplicated prompt. Shared by the pr_opened/label auto-review
// trigger and the merge label's own "no review yet" auto-dispatch
// (mergeIfApproved).
func autoReviewPayload(p pullRequestPayload) issueCommentPayload {
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
	return synthetic
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

	_, _, _, labels, err := e.app.issueMeta(ctx, owner, repo, number)
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

// implementTask synthesizes the implementation request for an implement-labeled
// issue - the issue itself; the approved plan and rest of the discussion
// arrive separately via loadGithubContext's injected context (#459).
// The labels param carries every label currently on the issue (fetched at
// dispatch start), used to conditionally suppress unconditional Closes #N.
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
// no human comment to extract a task from, only the issue itself.
func planTask(p issuesPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Produce an implementation plan for issue #%d: %s\n", p.Issue.Number, strings.TrimSpace(p.Issue.Title))
	if body := strings.TrimSpace(p.Issue.Body); body != "" {
		fmt.Fprintf(&b, "\nIssue description:\n%s\n", truncate(body, 4000))
	}
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
func (e *Extension) mergeIfApproved(p pullRequestPayload) {
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
			go e.dispatch(autoReviewPayload(p), autoReviewTask)
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
	// while a run is in-flight (same sessionID) is silently skipped with a
	// best-effort 👀 reaction on the triggering event - it never panics even
	// if the GitHub API call fails.
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	if _, inflight := e.inflight.LoadOrStore(sessionID, struct{}{}); inflight {
		slog.Info("deduplicated trigger", "sessionID", sessionID)
		go e.ackDedup(owner, repo, number)
		return
	}

	// No deadline here. The run deadline is the ORCHESTRATOR's (SetRunDeadline,
	// applied once a run slot is held) so that queueing is not charged against
	// it: this context covers the session lock and the server-wide run queue,
	// where a run can legitimately wait hours on a serial deployment. Starting
	// the clock here killed three implement runs at the 4h wall having
	// delivered nothing - they spent the budget waiting. Post-run GitHub calls
	// use their own short contexts (see tailCtx, reactionTimeout).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer e.inflight.Delete(sessionID)

	e.persistGithubLink(ctx, sessionID, login, owner, repo, number, p.Issue.PullRequest != nil)
	e.ensureTitle(ctx, sessionID, p, task)
	// Serialise runs on one PR: a follow-up that lands while a review is still
	// running must WAIT, not run concurrently on the same session (concurrent runs
	// corrupt each other - the answer skip and cross-run tool events seen in
	// dogfooding). The webhook already 202'd, so blocking this goroutine is fine.
	lock := e.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

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

	message := e.runMessage(ctx, p, task, gh)

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
	// Stamp the AUTHORITATIVE repo/PR this run is on - the only source
	// correct_review_finding trusts for where a conversational correction may
	// write, and the only source the plan tool trusts for a review's real head
	// branch (never the model's own say-so; see tools.WithGitHubPR). Only for a
	// PR (not a plain issue): there is no finding to correct, or head branch to
	// override, on an issue thread.
	if p.Issue.PullRequest != nil {
		runCtx = tools.WithGitHubPR(runCtx, owner, repo, number, gh.snap.HeadRef)
	}
	e.hub.RegisterRun(sessionID, turnID, cancelRun)
	// dispatch is ALREADY a goroutine (handleIssues calls it via `go`), so the run
	// stays INLINE - wrapping it in another goroutine would let this function's
	// `defer cancel()` (run ctx) and `defer lock.Unlock()` (session lock) fire the
	// moment it spawned, before the run finished. Deregister LAST (deferred
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
	}
	answer := strings.TrimSpace(e.runner.LatestAnswer(tailCtx, login, sessionID))
	if timedOut {
		answer = fmt.Sprintf("⚠️ quack hit its run deadline (%s) before finishing; nothing was delivered. Re-apply the label to retry.\n\nLast progress:\n\n%s",
			e.runTimeout, answer)
	} else if answer == "" {
		answer = "quack finished but produced no answer."
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
// (#459) - the single result loadGithubContext produces and runMessage
// consumes, for an issue or a PR alike.
type githubContext struct {
	snap Snapshot
	// text is the rendered context to inject this turn: the full seed on
	// first load, or just the delta on resume ("" when resuming onto an
	// unchanged snapshot - nothing to say).
	text string
	// firstLoad is true when no prior snapshot existed for this session (or
	// one was deliberately forgotten - see loadGithubContext's forceReseed).
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

// loadGithubContext is the ONE fetch-diff-persist path every dispatch goes
// through, issue or PR, work request or conversational follow-up (#459's
// "one unified path" - replaces gatherReviewContext/issueThreadContext/
// discussionSummary cherry-picking, and #457/#458's inject-everything
// interim). It fetches the CURRENT full GitHub state, diffs it against the
// snapshot stored from the last dispatch (or seeds, on first load), persists
// the new snapshot, and returns the rendered context ready to inject as a
// session EVENT via runMessage → e.runner.Run's message argument - never as
// bare UserContent (llmagent builds its request from Session().Events()
// only; a fresh prompt passed any other way is silently dropped).
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
		gh.text = renderSeedContext(snap, triggerCommentID)
	} else {
		prev, uerr := unmarshalSnapshot(prevJSON)
		if uerr != nil {
			slog.Warn("github: stored snapshot did not decode; treating this as a first load",
				"component", "github", "chat", sessionID, "err", uerr)
			gh.firstLoad = true
			gh.text = renderSeedContext(snap, triggerCommentID)
		} else {
			delta := diffSnapshots(prev, snap, triggerCommentID)
			gh.text = renderDeltaDetail(delta)
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

	if e.store != nil {
		if j, merr := marshalSnapshot(snap); merr != nil {
			slog.Warn("github: marshal snapshot failed; not persisted", "component", "github", "chat", sessionID, "err", merr)
		} else if serr := e.store.SetGithubSnapshot(ctx, sessionID, j); serr != nil {
			slog.Warn("github: SetGithubSnapshot failed; next resume may re-see this turn's changes",
				"component", "github", "chat", sessionID, "err", serr)
		}
	}
	return gh
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

// changedFilesSummary renders a compact, capped changed-files list for the plan
// prompt - paths + churn so the planner can slice the review by area. Capped so a
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
// (kept front-and-center - it carries their focus), where the repo is, and how to
// act. A PR request with no implement-and-deliver intent is a REVIEW and MUST
// carry no commit/push/PR language: otherwise the planner echoes it into the node
// task and the vetting completion gate reads a phantom delivery demand off it
// (delivery.go's demandedDelivery), looping the worker - re-cloning, re-reviewing
// - until maxContinueRounds. vetting.ImplementationIntent is the SAME
// discriminator the planner backstop uses, so the two can't drift.
//
// gh carries the loaded GitHub context (#459's unified snapshot+diff path) -
// head/base refs (a shallow clone's `git diff base...HEAD` is empty until the
// head is checked out), the PR's title/description (intent), the current
// commits (rebase-safe incremental-review scoping via gh.newCommits), and the
// rendered seed/delta text (the discussion, so the reviewer doesn't repeat it).
func (e *Extension) runMessage(ctx context.Context, p issueCommentPayload, task string, gh githubContext) string {
	isPR := p.Issue.PullRequest != nil
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	snap := gh.snap

	// A mention on a PR defaults to CONVERSATIONAL ("which finding matters
	// most?", "that finding was wrong") unless the classifier calls it a work
	// request (see isWorkRequest, intent.go) - fails safe to conversational,
	// since a wrong WORK verdict re-reviews and discards a written reply.
	if isPR && !p.isLabelTrigger && !e.isWorkRequest(ctx, task) {
		var b strings.Builder
		fmt.Fprintf(&b, "GitHub user @%s asked a follow-up on %s/%s pull request #%d (pull_number=%d).\n\n",
			p.Comment.User.Login, owner, repo, p.Issue.Number, p.Issue.Number)
		fmt.Fprintf(&b, "Their message:\n%s\n\n", task)
		// Inject the loaded context so the answer doesn't depend on session
		// continuity alone (#456) - the full discussion on first load, or just
		// what's new since the last dispatch on resume (#459).
		if gh.text != "" {
			fmt.Fprintf(&b, "The conversation so far on this pull request (your own prior reviews/answers included):\n%s\n", gh.text)
		}
		b.WriteString("This is a conversational follow-up. Answer it directly and concisely from the conversation above and any review you already posted. Do NOT clone the repo, run git, or start a new review unless they EXPLICITLY ask you to review again. Your answer is posted back as a comment.\n\n")
		b.WriteString("If - and only if - their message explicitly corrects a SPECIFIC finding you posted on THIS pull request as a FALSE POSITIVE (wrong, not a real issue), call correct_review_finding BEFORE replying, with the finding you got wrong and their reason - so the next review of similar code doesn't repeat it. Do not call it for anything else (general questions, disagreement without a concrete reason, or findings that still stand).")
		return b.String()
	}

	reviewOnly := isPR && !vetting.ImplementationIntent(task)
	kind := "issue"
	if isPR {
		kind = "pull request"
	}
	base := p.Repository.DefaultBranch
	if base == "" {
		base = "main"
	}
	diffBase := snap.BaseRef
	if diffBase == "" {
		diffBase = base
	}
	headSHA := snap.HeadSHA
	if headSHA == "" {
		headSHA = "HEAD"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are handling a request from GitHub user @%s, who mentioned you on %s/%s %s #%d.\n\n",
		p.Comment.User.Login, owner, repo, kind, p.Issue.Number)
	fmt.Fprintf(&b, "Their request:\n%s\n\n", task)
	// An issue's loaded context: the full thread on first load, just the delta
	// on resume (#459) - injected the SAME way for every trigger (mention,
	// quack:plan, quack:implement), unlike the old issueThreadContext/
	// implementTask split (#459 §7 - consolidate, don't duplicate).
	if !isPR && gh.text != "" {
		fmt.Fprintf(&b, "For context, here is the issue and the discussion so far:\n%s\n", gh.text)
	}
	fmt.Fprintf(&b, "The repository is %s/%s (default branch %q, clone URL %s). Declare it in your plan's `setup` "+
		"(repo=the clone URL above, base_ref=%q, work_branch=a new branch name for this change) - the harness "+
		"clones it and checks out that branch for you, BEFORE any node runs, AT THE ROOT of each repo-touching "+
		"node's own working directory: the repo IS that node's working directory, not a subdirectory inside it. "+
		"That node's task MUST say the repo is ALREADY cloned and checked out right there - never instruct the "+
		"worker to clone THIS repository (no \"Clone <url>\" wording; the worker starts inside it) - and must "+
		"refer to files by plain repo-relative path (internal/foo.go, never ./repo/… or /workspace/…). A node "+
		"whose job is to examine a DIFFERENT repository (a comparison target, a dependency) SHOULD be told to "+
		"clone that other repo into its own working directory itself - that is allowed and expected. Repo-changing "+
		"work is committed locally; delivery pushes the branch and opens the PR after the trust gate passes - no "+
		"node ever pushes or opens a PR itself. ",
		owner, repo, base, p.Repository.CloneURL, base)
	if isPR {
		fmt.Fprintf(&b, "This is pull request #%d (pull_number=%d).\n\n", p.Issue.Number, p.Issue.Number)
		if t := strings.TrimSpace(snap.Title); t != "" {
			fmt.Fprintf(&b, "PR title: %s\n", t)
		}
		if body := strings.TrimSpace(snap.Body); body != "" {
			fmt.Fprintf(&b, "PR description:\n%s\n", truncate(body, 1500))
		}
		if s := changedFilesSummary(snap.Files); s != "" {
			b.WriteString("\n" + s)
		}
		if reviewOnly && len(snap.Reviews) > 0 {
			// #506: framing the discussion below as "do NOT repeat it" reads, on its
			// own, as "already answered" once a prior review (quack's own or a
			// human's) is in it — the orchestrator then skips planning a reviewer
			// node and the run completes with nothing delivered. State the override
			// BEFORE the discussion, not after, so it isn't lost in whatever has
			// accumulated there. reviewOnly-gated: an implement request ("address the
			// review feedback and push a fix") must NOT be told to stage a review.
			b.WriteString("\nThis is a REQUEST FOR A REVIEW RIGHT NOW. Any prior review below — however many, yours or anyone else's — is background on what's already been said, never a reason to skip: you must still read the CURRENT diff and post a fresh review with stage_review. ")
		}
		if gh.text != "" {
			label := "Existing discussion - take it into account, do NOT repeat it:\n"
			if !gh.firstLoad {
				label = "" // gh.text is already framed as a "since your last look" delta below
			}
			b.WriteString("\n" + label + gh.text)
		}
		b.WriteString("\n")
		if snap.HeadRef != "" {
			// Setup (dag.Setup.CheckoutExistingHead - see internal/tools/setup.go)
			// already fetched and checked out this PR's real head branch, with
			// base's full history present, so the diff is ready with no checkout
			// needed.
			fmt.Fprintf(&b, "The clone is your workspace root already, already checked out on the PR's head branch `%s` (head commit `%s`), based on `%s`. `git_diff %s...%s` is exactly this PR's diff - no checkout needed. ",
				snap.HeadRef, headSHA, diffBase, diffBase, snap.HeadRef)
		}
		// Incremental review, rebase-safe (#459 §5): gh.newCommits is non-nil
		// only once quack has DELIVERED at least one review on this chat (see
		// reviewScope/advanceReviewBaseline) - the commits not yet covered by
		// that review, computed by patch-id, independent of SHA (a
		// rebase/force-push rewrites every SHA even when the underlying patch
		// is unchanged) and independent of the general context delta (which
		// advances on every dispatch, review or not).
		if gh.newCommits != nil {
			switch len(gh.newCommits) {
			case 0:
				b.WriteString("You have already looked at every commit currently on this pull request (by content - a rebase or force-push may have changed their SHAs without changing what they do). There is no new work to review; only respond to the discussion above. ")
			default:
				shas := make([]string, 0, len(gh.newCommits))
				for _, c := range gh.newCommits {
					shas = append(shas, shortSHA(c.SHA))
				}
				fmt.Fprintf(&b, "Focus your review on what's NEW since you last looked - %d commit(s) not seen before (by content, robust to any rebase/force-push): %s. Use `git show <sha>` for each rather than re-reviewing the whole PR, and take the existing review discussion into account - do NOT repeat findings you already made. ",
					len(gh.newCommits), strings.Join(shas, ", "))
			}
		}
		lead := "If the request is to REVIEW this PR: read its changes"
		if reviewOnly {
			lead = "Review it: read its changes"
		}
		fmt.Fprintf(&b, "%s (git_diff) and its existing discussion (github_list_pr_comments - inline comments, conversation, prior reviews) so you don't repeat what's been said, then record each finding the moment you spot it with github_add_review_comment (owner=%s, repo=%s, pull_number=%d, path, line - validated against the diff), and finish by calling stage_review with a summary body and an event verdict (approve / request_changes / comment) - you do not submit the review yourself; it is posted for you once your work passes review. Load and follow the `present-coding-plan` skill (load_skill) for how to structure and format the summary body. ",
			lead, owner, repo, p.Issue.Number)
	}
	if reviewOnly {
		// No commit/push/PR words: a review posts findings, it does not deliver code.
		b.WriteString("This is a REVIEW-ONLY task: do NOT create a branch, commit, or push - deliver your findings with the review tools (github_add_review_comment, stage_review). ")
		b.WriteString("Your final answer is posted back automatically. ")
		b.WriteString("Answer concisely and reference the review you staged.")
		return b.String()
	}
	if p.planOnly {
		// Like reviewOnly: no commit/push/PR words, or the vetting completion gate
		// reads a phantom delivery demand off the task and loops the worker.
		b.WriteString("This is a PLANNING-ONLY task: read the repository as needed to ground the plan, but do NOT change code or deliver anything to GitHub. ")
		// #569: a plan-only run wrote its plan to a file in the node's clone and
		// posted a comment pointing at it - the file was never committed (plan-only
		// commits nothing), so it existed nowhere the instant the run ended.
		// State the actual contract instead of leaving it for the model to guess.
		b.WriteString("Your ANSWER TEXT is the plan - it is posted back to the issue verbatim and automatically. Do NOT write the plan to a file and point at the path in your answer: any file this run writes is discarded with its working directory when the run ends, plan-only runs commit nothing, and a path reference to it is a dangling pointer to nothing. Write the actual plan content in your answer. ")
		b.WriteString("Do not assert a dependency version, action tag, or API detail from memory as if it were current - if you have not verified it this session (checked the repo, fetched a page), say \"the current stable X\" rather than naming a specific version number; a stale one recalled from training data reads as confidently wrong.")
		return b.String()
	}
	b.WriteString("If the task needs code changes, work at your workspace root (the repo is already cloned and checked out there for you - plain relative paths, no prefix), commit your work locally on the branch already checked out for you, then call stage_pr with a title and body - you do not push or open the pull request yourself ")
	fmt.Fprintf(&b, "(owner=%s, repo=%s, base=%q); it is opened for you once your work passes review. ", owner, repo, base)
	b.WriteString("Your final answer is posted back automatically. ")
	b.WriteString("Answer concisely and reference any branch, PR, or review you staged.")
	return b.String()
}
