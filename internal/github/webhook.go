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
	"time"

	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/stream"
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

	// planOnly marks a synthetic payload for a label-driven planning run: the
	// task produces a plan comment and must not touch code. Not part of the
	// GitHub payload.
	planOnly bool
	// isLabelTrigger marks a synthetic payload built from a label/pr_opened
	// event (auto-review, quack:plan, quack:implement) — as opposed to a real
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
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
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
	case "issues":
		e.handleIssues(w, body)
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
	// Never act on another bot's comments — quack's own posted plans/summaries
	// and other integrations must not re-trigger runs.
	// ponytail: rejects all bots, not just self — bots don't mention quack legitimately.
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

	fires := (p.Action == "opened" && e.triggers["pr_opened"]) ||
		(p.Action == "labeled" && e.triggers["label"] && p.Label.Name == e.labels.Review)
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
	synthetic.isLabelTrigger = true // pr_opened/label auto-review, never a mention (T4)

	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
		"action", p.Action, "installation", p.Installation.ID)
	go e.dispatch(synthetic, autoReviewTask)
	w.WriteHeader(http.StatusAccepted)
}

// handleIssues drives the label-driven issue workflow: applying the configured
// plan label to an issue dispatches a planning run whose answer (the plan) is
// posted back as an issue comment. Labels double as the permission model —
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
		go e.ackLabelReaction(p) // instant 👀 on the issue — the label path's equivalent of ackReaction
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

// runImplement acks the implement label with a comment, gathers the issue's
// discussion (the posted plan lives there), and dispatches the implementation
// run on the issue's session — the same session the planning run used, so the
// plan is also in the model's own history.
func (e *Extension) runImplement(p issuesPayload, synthetic issueCommentPayload) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	ack := fmt.Sprintf("On it — implementing per the plan above. I'll open a pull request that references this issue (`Closes #%d`).", number)
	if err := e.app.postIssueComment(ctx, owner, repo, number, ack); err != nil {
		slog.Warn("github: implement ack comment failed", "component", "github",
			"repo", owner+"/"+repo, "issue", number, "err", err)
	}
	comments, err := e.app.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: listIssueComments failed; implementing from the issue body and session history alone",
			"component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
	cancel()
	e.dispatch(synthetic, implementTask(p, comments))
}

// implementTask synthesizes the implementation request for an implement-labeled
// issue: the issue itself plus its discussion (which carries the approved plan).
//
// ponytail: this used to also chain the new PR into the review-label flow
// (labels=[…] on open) — dropped because StagedDelivery carries no labels
// (the worker never opens the PR itself anymore). Restore once plan.Delivery
// does, or have the delivery step apply the review label itself post-open.
func implementTask(p issuesPayload, comments []commentView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Implement issue #%d: %s\n", p.Issue.Number, strings.TrimSpace(p.Issue.Title))
	if body := strings.TrimSpace(p.Issue.Body); body != "" {
		fmt.Fprintf(&b, "\nIssue description:\n%s\n", truncate(body, 4000))
	}
	if len(comments) > 0 {
		const maxComments = 40
		b.WriteString("\nIssue discussion (includes the approved plan — follow it):\n")
		for i, c := range comments {
			if i == maxComments {
				fmt.Fprintf(&b, "  … and %d more\n", len(comments)-maxComments)
				break
			}
			fmt.Fprintf(&b, "  @%s: %s\n", c.User, truncate(c.Body, 2000))
		}
	}
	fmt.Fprintf(&b, "\nA maintainer approved this for implementation. Implement it per the plan and discussion, commit your work locally, "+
		"then call stage_pr with a title and a body that includes `Closes #%d` — you do not push or open the pull request yourself; "+
		"the pull request is opened for you once your work passes review", p.Issue.Number)
	b.WriteString(". Never merge anything — merging is a human decision.")
	return b.String()
}

// planTask synthesizes the planning request for a plan-labeled issue — there is
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

// mergeIfApproved is the merge-label handler: merge ONLY at the intersection of
// a human's label (repo write access) and quack's own APPROVED review. Anything
// else gets an explanatory comment — re-applying the label after quack approves
// is the retry, so no state is stored.
func (e *Extension) mergeIfApproved(p pullRequestPayload) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Number
	ctx, cancel := context.WithTimeout(context.Background(), mergeTimeout)
	defer cancel()

	comment := func(text string) {
		if err := e.app.postIssueComment(ctx, owner, repo, number, text); err != nil {
			slog.Error("github: merge-label comment failed", "component", "github",
				"repo", owner+"/"+repo, "pr", number, "err", err)
		}
	}

	state, err := e.latestOwnReviewState(ctx, owner, repo, number)
	if err != nil {
		slog.Error("github: merge-label review lookup failed", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
		comment(fmt.Sprintf("Not merging: I could not read this PR's reviews (%v). Re-apply the `%s` label to retry.", err, e.labels.Merge))
		return
	}
	if state != "APPROVED" {
		if state == "" {
			state = "missing — I have not reviewed this PR"
		}
		comment(fmt.Sprintf("Not merging: my latest review is %s. Ask me to review (apply `%s` or mention me), then re-apply `%s` once I approve.",
			state, e.labels.Review, e.labels.Merge))
		return
	}
	if err := e.app.mergePR(ctx, owner, repo, number); err != nil {
		slog.Error("github merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		comment(fmt.Sprintf("Merge failed: %v", err))
		return
	}
	slog.Info("github pr merged", "component", "github", "repo", owner+"/"+repo, "pr", number, "user", p.Sender.Login)
	comment(fmt.Sprintf("Merged — my review approved this PR and @%s authorized the merge via the `%s` label.", p.Sender.Login, e.labels.Merge))
}

// latestOwnReviewState returns the state of quack's most recent VERDICT review
// (APPROVED / CHANGES_REQUESTED / DISMISSED) on the PR — COMMENTED reviews carry
// no verdict and are skipped. "" means quack has no verdict on this PR.
func (e *Extension) latestOwnReviewState(ctx context.Context, owner, repo string, number int) (string, error) {
	reviews, err := e.app.listReviews(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	bot, err := e.app.botLogin(ctx)
	if err != nil {
		return "", err
	}
	state := ""
	for _, r := range reviews { // chronological: the last verdict wins
		if r.User.Login == bot && r.State != "COMMENTED" {
			state = r.State
		}
	}
	return state, nil
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

// ackLabelReaction posts a deterministic 👀 reaction on the ISSUE as an instant
// acknowledgment that a label-triggered run (quack:plan/quack:implement) started
// — the label path's equivalent of ackReaction. It can't reuse ackReaction: a
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
	ctx, cancel := context.WithTimeout(context.Background(), e.runTimeout)
	defer cancel()

	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	e.persistGithubLink(ctx, sessionID, owner, repo, number, p.Issue.PullRequest != nil)
	// Serialise runs on one PR: a follow-up that lands while a review is still
	// running must WAIT, not run concurrently on the same session (concurrent runs
	// corrupt each other — the answer skip and cross-run tool events seen in
	// dogfooding). The webhook already 202'd, so blocking this goroutine is fine.
	lock := e.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	var rc reviewContext
	// Only a work request (review/implement) needs the plan-time PR context; a
	// conversational follow-up answers from the session and skips the API calls.
	if p.Issue.PullRequest != nil && isWorkRequest(task) {
		rc = e.gatherReviewContext(ctx, owner, repo, number)
	}
	message := e.runMessage(p, task, rc)

	// A LABEL-driven work request starts a FRESH session: unlike a conversational
	// @mention (kept for continuity), a new attempt must not inherit a prior
	// attempt's events, which can make the run conclude the work is "already
	// done" instead of doing it.
	if p.isLabelTrigger && isWorkRequest(task) {
		if err := e.runner.ResetSession(ctx, runUserID, sessionID); err != nil {
			slog.Warn("github: session reset failed; this attempt may inherit stale history",
				"component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		}
	}

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

	// Wrap the run context with a cancel so the UI's stop button can cancel
	// this run via the same activeCancels registry used for UI-initiated runs.
	// Registered synchronously (before the goroutine gets to run) so the cancel
	// endpoint can never miss the run.
	runCtx, cancelRun := context.WithCancel(ctx)
	e.activeCancels.Store(sessionID, &activeRun{responseID: turnID, cancel: cancelRun})
	// dispatch is ALREADY a goroutine (handleIssues calls it via `go`), so the run
	// stays INLINE — wrapping it in another goroutine would let this function's
	// `defer cancel()` (run ctx) and `defer lock.Unlock()` (session lock) fire the
	// moment it spawned, before the run finished. Deregister LAST (deferred
	// cancel+Delete, then hub.Close) so a viewer sees the stream close only after
	// the run is already off activeCancels (cancelling it then 404s).
	defer e.hub.Close(sessionID)
	defer func() {
		cancelRun()
		e.activeCancels.Delete(sessionID)
	}()

	// A WORK request (review/implement) always runs as a plan. A mid-tier
	// orchestrator model sometimes answers in prose without calling plan — "Let me
	// start by cloning the repo…" — and that preamble would be posted as if it were
	// the review. So if a work request ran no plan, nudge once to actually run it.
	// A purely conversational follow-up ("what did you mean by that finding?")
	// legitimately runs no plan and must be answered directly — never nudged.
	planRan, judgePassed := e.drive(runCtx, sessionID, message, owner, repo, number, turnID, pub)
	if !planRan && (isWorkRequest(task) || p.planOnly) {
		slog.Warn("github: work request produced no plan; nudging it to run the work once",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		_, jp2 := e.drive(runCtx, sessionID, runNudge, owner, repo, number, turnID, pub)
		judgePassed = judgePassed || jp2
	}
	if pub != nil {
		pub.Publish(stream.Done())
		_ = e.store.Touch(runCtx, sessionID)
	}

	// A judge-passed work request already had its staged review/PR posted by the
	// trust gate itself (commitDelivery) — posting the run's text summary too would
	// duplicate it. Only fall back to a summary comment when nothing was delivered.
	// takeDeliveryDetail is AUTHORITATIVE when present (the delivery call's own
	// outcome, not a proxy): a gate that passed but whose push then failed must NOT
	// read as delivered. A plan-only run NEVER delivers a PR/review — its
	// deliverable is the plan comment — so it must never read as delivered no
	// matter how work-verby the task text is.
	// No delivery record means NOTHING was delivered: since the native
	// delivery tools were deleted (0.6.0), the gate's commitDelivery is the
	// only path to GitHub, and it always records its outcome. (The old
	// judge-passed default dates from workers that pushed via their own tools
	// recordlessly — kept, it masked a staged-nothing run as delivered.)
	delivered := false
	if d, ok := takeDeliveryDetail(sessionID); ok {
		delivered = d.err == nil
		if d.err != nil {
			slog.Error("github: staged delivery failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", d.err)
		} else {
			slog.Info("github: delivery verified against GitHub", "component", "github", "repo", owner+"/"+repo, "issue", number,
				"pr_number", d.prNumber, "pr_url", d.prURL, "pushed_sha", d.pushedSHA)
		}
	}
	if delivered {
		slog.Info("github: work delivered on the PR; skipping the duplicate summary comment",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		return
	}

	// The tail must OUTLIVE the run: after a deadline kill, ctx is dead and both
	// LatestAnswer and the comment post would fail with it — the run then dies
	// with zero external signal (#286: a 2h-deadline kill posted nothing). Use a
	// fresh bounded context, and say what actually happened.
	tailCtx := runCtx
	timedOut := runCtx.Err() != nil
	if timedOut {
		var tailCancel context.CancelFunc
		tailCtx, tailCancel = context.WithTimeout(context.Background(), time.Minute)
		defer tailCancel()
	}
	answer := strings.TrimSpace(e.runner.LatestAnswer(tailCtx, runUserID, sessionID))
	if timedOut {
		answer = fmt.Sprintf("⚠️ quack hit its run deadline (%s) before finishing; nothing was delivered. Re-apply the label to retry.\n\nLast progress:\n\n%s",
			e.runTimeout, answer)
	} else if answer == "" {
		answer = "quack finished but produced no answer."
	} else if p.planOnly {
		// A genuine plan (not a timeout/empty placeholder): collapse any PRIOR
		// plan comment on this issue before posting the new one, so the thread
		// shows the CURRENT plan, not a pile of dead attempts. The marker
		// is what a later run's collapse finds.
		e.app.collapsePriorComments(tailCtx, owner, repo, number, "plan")
		answer += "\n\n" + deliveryMarker("plan")
	}
	if err := e.app.postIssueComment(tailCtx, owner, repo, number, answer); err != nil {
		slog.Error("github comment post failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		return
	}
	slog.Info("github comment posted", "component", "github", "repo", owner+"/"+repo, "issue", number, "timed_out", timedOut)
}

// persistGithubLink stores the web URL of the originating issue/PR on the
// session's chat row, for the frontend's GitHub tab. isPR selects "pull" vs
// "issues" in the URL; unknown defaults to "issues" (GitHub redirects PRs
// requested at the issues path). Best-effort: a failure here must not block
// the run.
func (e *Extension) persistGithubLink(ctx context.Context, sessionID, owner, repo string, number int, isPR bool) {
	if e.store == nil {
		return
	}
	kind := "issues"
	if isPR {
		kind = "pull"
	}
	url := fmt.Sprintf("https://github.com/%s/%s/%s/%d", owner, repo, kind, number)
	if err := e.store.SetChatGitHub(ctx, sessionID, owner+"/"+repo, url); err != nil {
		slog.Warn("github: persist chat link failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// runNudge is delivered when a webhook run answered without running a plan — a
// firm instruction to actually do the work rather than narrate intent.
const runNudge = "You answered without running anything. Do NOT reply in prose: use the plan and execute tools NOW to actually clone the repo, read the change, and carry out the review (or the requested change). Nothing has run yet and the user is waiting."

// drive runs one orchestrator turn to completion and reports whether it
// EXECUTED a plan (a dag_plan event) and whether ANY node's trust gate PASSED
// its judge round. A run with no plan produced only a direct-text answer (the
// work never happened). judgePassed is dispatch's proxy for "the staged
// delivery set was posted": commitDelivery runs synchronously inside the
// gate, strictly before node_done fires (see internal/vetting/node.go), so by
// the time node_done reports a pass here, delivery has already been attempted
// — a failed delivery is still logged loudly (slog.Error) even though this
// proxy can't distinguish it from "nothing was staged" (a conversational node
// gated the same way). dispatch only trusts this proxy for a task that
// DEMANDED delivery in the first place (isWorkRequest) — see its caller.
//
// pub is nil when the extension has no store (test harnesses that don't need
// persistence) — every persistence step below is then a no-op, matching drive's
// old behavior exactly.
func (e *Extension) drive(ctx context.Context, sessionID, message, owner, repo string, number int, turnID string, pub *runlog.Publisher) (planRan, judgePassed bool) {
	var planID string
	for ev, err := range e.runner.Run(ctx, runUserID, sessionID, message, nil) {
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
		}
		if pub != nil {
			if planID != "" {
				runlog.PersistNodeEvent(e.store, planID, ev)
			}
			pub.Publish(ev)
		}
	}
	return planRan, judgePassed
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
	owner, repo := p.Repository.Owner.Login, p.Repository.Name

	// A PR follow-up that asks for no work (review/implement) is CONVERSATIONAL —
	// "which finding matters most?", "what did you mean?". Answer it directly from
	// the durable session (which holds any review already posted); handing over the
	// clone-and-review playbook makes the orchestrator re-review instead of
	// answering.
	if isPR && !isWorkRequest(task) {
		var b strings.Builder
		fmt.Fprintf(&b, "GitHub user @%s asked a follow-up on %s/%s pull request #%d (pull_number=%d).\n\n",
			p.Comment.User.Login, owner, repo, p.Issue.Number, p.Issue.Number)
		fmt.Fprintf(&b, "Their message:\n%s\n\n", task)
		b.WriteString("This is a conversational follow-up. Answer it directly and concisely from THIS thread's prior conversation — including any review you already posted, which is in your context. Do NOT clone the repo, run git, or start a new review unless they EXPLICITLY ask you to review again. Your answer is posted back as a comment.\n\n")
		b.WriteString("If — and only if — their message explicitly corrects a SPECIFIC finding you posted as a FALSE POSITIVE (wrong, not a real issue), call correct_review_finding BEFORE replying: owner=" + owner + ", repo=" + repo + ", pr_number=" + fmt.Sprint(p.Issue.Number) + ", the finding you got wrong, and their reason — so the next review of similar code doesn't repeat it. Do not call it for anything else (general questions, disagreement without a concrete reason, or findings that still stand).")
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
	fmt.Fprintf(&b, "The repository is %s/%s (default branch %q, clone URL %s). Declare it in your plan's `setup` "+
		"(repo=the clone URL above, base_ref=%q, work_branch=a new branch name for this change) — the harness "+
		"clones it and checks out that branch for you, BEFORE any node runs, AT THE ROOT of each repo-touching "+
		"node's own working directory: the repo IS that node's working directory, not a subdirectory inside it. "+
		"That node's task MUST say the repo is ALREADY cloned and checked out right there — never instruct the "+
		"worker to clone THIS repository (no \"Clone <url>\" wording; the worker starts inside it) — and must "+
		"refer to files by plain repo-relative path (internal/foo.go, never ./repo/… or /workspace/…). A node "+
		"whose job is to examine a DIFFERENT repository (a comparison target, a dependency) SHOULD be told to "+
		"clone that other repo into its own working directory itself — that is allowed and expected. Repo-changing "+
		"work is committed locally; delivery pushes the branch and opens the PR after the trust gate passes — no "+
		"node ever pushes or opens a PR itself. ",
		owner, repo, base, p.Repository.CloneURL, base)
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
			// The clone (already your workspace root — no `cd` needed) only holds
			// the BASE branch (shallow), where `git diff base...HEAD` is EMPTY.
			// Check out the head branch to see the changes.
			fmt.Fprintf(&b, "The clone is your workspace root already. The PR's changes are on branch `%s` (head commit `%s`), based on `%s`. The clone only has the base branch, so `git diff %s...HEAD` is EMPTY until you check out the head: run git_checkout `%s` FIRST (fetch/unshallow if needed), then `git_diff %s...%s` is exactly this PR's diff. Do this before reviewing — the base branch alone shows no changes. ",
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
		fmt.Fprintf(&b, "%s (git_diff) and its existing discussion (github_list_pr_comments — inline comments, conversation, prior reviews) so you don't repeat what's been said, then record each finding the moment you spot it with github_add_review_comment (owner=%s, repo=%s, pull_number=%d, path, line — validated against the diff), and finish by calling stage_review with a summary body and an event verdict (approve / request_changes / comment) — you do not submit the review yourself; it is posted for you once your work passes review. Load and follow the `present-coding-plan` skill (load_skill) for how to structure and format the summary body. ",
			lead, owner, repo, p.Issue.Number)
	}
	if reviewOnly {
		// No commit/push/PR words: a review posts findings, it does not deliver code.
		b.WriteString("This is a REVIEW-ONLY task: do NOT create a branch, commit, or push — deliver your findings with the review tools (github_add_review_comment, stage_review). ")
		b.WriteString("Your final answer is posted back automatically. ")
		b.WriteString("Answer concisely and reference the review you staged.")
		return b.String()
	}
	if p.planOnly {
		// Like reviewOnly: no commit/push/PR words, or the vetting completion gate
		// reads a phantom delivery demand off the task and loops the worker.
		b.WriteString("This is a PLANNING-ONLY task: read the repository as needed to ground the plan, but do NOT change code or deliver anything to GitHub. ")
		b.WriteString("Your final answer — the plan — is posted back to the issue automatically.")
		return b.String()
	}
	b.WriteString("If the task needs code changes, work at your workspace root (the repo is already cloned and checked out there for you — plain relative paths, no prefix), commit your work locally on the branch already checked out for you, then call stage_pr with a title and body — you do not push or open the pull request yourself ")
	fmt.Fprintf(&b, "(owner=%s, repo=%s, base=%q); it is opened for you once your work passes review. ", owner, repo, base)
	b.WriteString("Your final answer is posted back automatically. ")
	b.WriteString("Answer concisely and reference any branch, PR, or review you staged.")
	return b.String()
}
