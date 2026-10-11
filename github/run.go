package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// githubContext is the loaded GitHub state for one dispatch.
type githubContext struct {
	snap               Snapshot
	delta              *Delta // nil on first load, set on resume
	firstLoad          bool
	contextUnavailable bool             // fetchSnapshot's meta call failed; label-triggered work aborts
	newCommits         []snapshotCommit // PR commits for incremental review scope; nil = review everything
	checks             []checkRunView   // current head-commit check runs (PR only); nil = not a PR or the fetch failed
}

// pendingRun is what dispatch stores so RunEnded, arriving later keyed only by chatID, can finish the job:
// Host.Dispatch is fire-and-forget.
type pendingRun struct {
	sessionID string
	// claimedAt is the inflight lease token this run holds; finalize releases only its own claim.
	claimedAt      time.Time
	owner, repo    string
	number         int
	isPR           bool
	login          string
	gh             githubContext
	isPlan         bool
	isLabelTrigger bool
	explain        bool // the walkthrough lives in quack's chat; finalize posts nothing
	// isReview: dispatched as autoReviewTask, so a finished run with no delivered review has no verdict.
	// ponytail: not in PendingRunRow, so a run rebuilt after a restart posts its answer as before.
	isReview bool

	// dispatched is the original request, so the no-plan nudge re-sends its Run/Ask context; a fresh
	// Run drops Setup and strands the planner without the PR's real head branch.
	dispatched sdk.DispatchRequest

	// nudged is set once the one-shot "you answered without running anything" retry has fired.
	nudged bool

	// defaultBranch/installationID let a re-review from finalize rebuild the label trigger's payload.
	defaultBranch  string
	installationID int64
	// reReview is set by finalize when the merge found the head moved; RunEnded
	// dispatches it only AFTER finalize's inflight/pending cleanup has run.
	reReview *issueCommentPayload
}

// RunEnded correlates a run's outcome back to its pendingRun: a run with no plan gets one nudge
// re-dispatch, otherwise finalize settles the chat.
func (e *Extension) RunEnded(chatID string, outcome sdk.RunOutcome) {
	v, ok := e.pending.Load(chatID)
	var pr *pendingRun
	if ok {
		pr = v.(*pendingRun)
	} else {
		// e.pending is in-memory, so this is the normal path for any run resumed at boot:
		// rebuild from the durable row rather than dropping the outcome.
		rebuilt, rerr := e.rebuildPendingRun(chatID)
		if rerr != nil {
			e.host.Log.Warn("github: RunEnded for a chat with no pending dispatch; dropping the outcome",
				"chat", chatID, "status", outcome.Status, "err", rerr)
			return
		}
		pr = rebuilt
	}

	// RunFailed already carries a definite cause (rejected plan, guard hard
	// stop, gateway outage); nudging it only re-asks a question quack answered.
	if !pr.nudged && !outcome.PlanRan && pr.isLabelTrigger && outcome.Status == sdk.RunDone {
		pr.nudged = true
		// The nudge is a second run under the same claim - restart the lease so
		// primary+nudge can't outlive it and get taken over mid-flight.
		pr.claimedAt = time.Now()
		e.inflight.Store(pr.sessionID, pr.claimedAt)
		// Chat is not copied: ResetHistory would wipe the turn the nudge needs, and a nil
		// Origin leaves the stamped one in place.
		nudgeReq := sdk.DispatchRequest{
			Chat: sdk.ChatRef{LocalID: pr.sessionID, User: pr.login},
			Ask: sdk.Ask{
				Message:      runNudge,
				NodeContext:  pr.dispatched.Ask.NodeContext,
				ContextItems: pr.dispatched.Ask.ContextItems,
			},
			Run:      pr.dispatched.Run,
			Delivery: pr.dispatched.Delivery,
		}
		e.host.Log.Warn("github: work request produced no plan; nudging it to run the work once",
			"repo", pr.owner+"/"+pr.repo, "issue", pr.number)
		if err := e.host.Dispatch(context.Background(), nudgeReq); err != nil {
			e.host.Log.Error("github: nudge dispatch failed; finalizing with what we have",
				"repo", pr.owner+"/"+pr.repo, "issue", pr.number, "err", err)
			e.finish(chatID, pr, outcome)
		}
		return // wait for the nudge's own RunEnded
	}
	e.finish(chatID, pr, outcome)
}

// rebuildPendingRun reconstructs a pendingRun from the durable row after a restart; gh is re-fetched.
// ponytail: nudged forced true (pr.dispatched isn't persisted); persist it if resumed no-plan runs matter.
func (e *Extension) rebuildPendingRun(chatID string) (*pendingRun, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	row, err := e.store.GetPendingRun(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("GetPendingRun: %w", err)
	}
	if row == nil {
		return nil, fmt.Errorf("no persisted pending run")
	}
	gh := e.loadGithubContext(ctx, chatID, row.Owner, row.Repo, row.Number, row.IsPR, row.CommentID, false)
	// Reuse the claim Start restored, so finalize's compare-and-delete releases it.
	claimedAt := time.Now()
	if v, ok := e.inflight.Load(row.SessionID); ok {
		claimedAt = v.(time.Time)
	}
	return &pendingRun{
		sessionID: row.SessionID, claimedAt: claimedAt, owner: row.Owner, repo: row.Repo, number: row.Number,
		isPR: row.IsPR, login: row.Login, gh: gh, isPlan: row.IsPlan, isLabelTrigger: row.IsLabelTrigger,
		explain:        row.SessionID == explainSessionID(row.Owner, row.Repo, row.Number, row.Login),
		nudged:         true,
		defaultBranch:  row.DefaultBranch,
		installationID: row.InstallationID,
		dispatched:     sdk.DispatchRequest{Run: sdk.RunConfig{Setup: &sdk.Setup{Repo: row.CloneURL}}},
	}, nil
}

// finish runs finalize, then any re-review it asked for: dispatching earlier, finalize's deferred
// inflight release and pending delete would dedup-drop the new run or delete its pendingRun.
func (e *Extension) finish(chatID string, pr *pendingRun, outcome sdk.RunOutcome) {
	e.finalize(chatID, pr, outcome)
	if pr.reReview != nil {
		e.dispatch(*pr.reReview, autoReviewTask)
	}
}

// finalize settles a finished run (or its nudge): verified delivery, a HITL question, or the answer comment.
func (e *Extension) finalize(chatID string, pr *pendingRun, outcome sdk.RunOutcome) {
	defer e.inflight.CompareAndDelete(pr.sessionID, pr.claimedAt)
	defer e.pending.Delete(chatID)
	defer func() {
		if err := e.store.DeletePendingRun(context.Background(), chatID); err != nil {
			e.host.Log.Warn("github: DeletePendingRun failed; a future restart may redundantly rebuild this run", "chat", chatID, "err", err)
		}
	}()
	owner, repo, number := pr.owner, pr.repo, pr.number

	if pr.explain {
		e.host.Log.Info("github: explain run ended; nothing to post", "repo", owner+"/"+repo, "pr", number, "status", outcome.Status)
		return
	}
	if e.settleDelivery(chatID, pr) {
		return
	}

	// User cancelled: Answer is mid-thought, so post nothing; just settle records.
	if outcome.Status == sdk.RunCancelled {
		e.persistGithubSnapshot(chatID, pr.gh)
		e.host.Log.Info("github: run cancelled by user; no comment posted", "repo", owner+"/"+repo, "issue", number)
		return
	}

	// HITL pause: post the question as a comment; the reply resumes the paused node.
	if outcome.Status == sdk.RunNeedsInput {
		// Orchestrator-level questions (e.g. plan-loop) have no node id.
		prefix := "Question before continuing"
		if outcome.NodeID != "" {
			prefix += " (" + outcome.NodeID + ")"
		}
		comment := prefix + ":\n\n" + outcome.Question
		hitlCtx, hitlCancel := context.WithTimeout(context.Background(), time.Minute)
		defer hitlCancel()
		if err := e.app.postIssueComment(hitlCtx, owner, repo, number, comment); err != nil {
			e.host.Log.Error("github: HITL question comment post failed", "repo", owner+"/"+repo, "issue", number, "err", err)
		} else {
			e.host.Log.Info("github: HITL question posted", "repo", owner+"/"+repo, "issue", number, "node", outcome.NodeID)
		}
		return
	}

	if pr.isReview && outcome.Status == sdk.RunDone && !outcome.TimedOut {
		e.postNoVerdict(chatID, pr)
		return
	}

	answer := e.shapeAnswer(pr, outcome, owner, repo, number)

	tailCtx, tailCancel := context.WithTimeout(context.Background(), time.Minute)
	defer tailCancel()
	if err := e.app.postIssueComment(tailCtx, owner, repo, number, e.app.withFooter(answer, chatID)); err != nil {
		e.host.Log.Error("github comment post failed", "repo", owner+"/"+repo, "issue", number, "err", err)
		return
	}
	if !outcome.TimedOut {
		e.persistGithubSnapshot(chatID, pr.gh)
	}
	e.host.Log.Info("github comment posted", "repo", owner+"/"+repo, "issue", number, "timed_out", outcome.TimedOut)
}

// settleDelivery settles a run with verified delivery detail: post a failure report, or persist and (for a
// delivered review) settle the merge. True means nothing more should be posted.
func (e *Extension) settleDelivery(chatID string, pr *pendingRun) bool {
	// A push that left the head unchanged is not delivered work: GitHub shows no trace of the run,
	// so the answer is the only place its analysis survives.
	d, ok := takeDeliveryDetail(chatID)
	if !ok {
		return false
	}
	owner, repo, number := pr.owner, pr.repo, pr.number
	if d.err != nil {
		// A worker's own report can't be trusted here; it may claim success it never had.
		e.host.Log.Error("github: staged delivery failed", "repo", owner+"/"+repo, "issue", number, "err", d.err)
		e.postDeliveryFailure(owner, repo, number, d)
		return true
	}
	e.host.Log.Info("github: delivery verified against GitHub", "repo", owner+"/"+repo, "issue", number,
		"pr_number", d.prNumber, "pr_url", d.prURL, "pushed_sha", d.pushedSHA)
	headUnchanged := d.pushedSHA != "" && d.pushedSHA == pr.gh.snap.HeadSHA
	if d.reviewDelivered || !headUnchanged {
		if d.reviewDelivered {
			baselineCtx, baselineCancel := context.WithTimeout(context.Background(), 10*time.Second)
			e.advanceReviewBaseline(baselineCtx, chatID, pr.gh.snap.Commits)
			baselineCancel()

			mergeCtx, mergeCancel := context.WithTimeout(context.Background(), mergeTimeout)
			mo, merr := e.tryMerge(mergeCtx, owner, repo, number)
			if merr != nil {
				e.host.Log.Warn("github: merge evaluation after review delivery failed", "repo", owner+"/"+repo, "pr", number, "err", merr)
			}
			if mo == mergeStale {
				// Head moved under the approving review: re-review it.
				pr.reReview = e.reReviewMovedHead(mergeCtx, pr)
			}
			mergeCancel()
		}
		e.persistGithubSnapshot(chatID, pr.gh)
		e.host.Log.Info("github: work delivered on the PR; skipping the duplicate summary comment", "repo", owner+"/"+repo, "issue", number)
		return true
	}
	e.host.Log.Info("github: push left the PR head unchanged and no review was posted; posting the run's answer instead of a silent no-op",
		"repo", owner+"/"+repo, "issue", number, "pushed_sha", d.pushedSHA)
	return false
}

// shapeAnswer turns a finished run's outcome into the closing comment: retry wording for its trigger,
// plus the timed-out / failed / silent / plan variants.
func (e *Extension) shapeAnswer(pr *pendingRun, outcome sdk.RunOutcome, owner, repo string, number int) string {
	answer := strings.TrimSpace(outcome.Answer)
	retry := "Re-apply the label to retry."
	if !pr.isLabelTrigger {
		retry = "Repeat the request to retry."
	}
	switch {
	case outcome.TimedOut:
		answer = fmt.Sprintf("Run deadline reached; nothing delivered. %s\n\nLast progress:\n\n%s", retry, answer)
	case outcome.Status == sdk.RunFailed && outcome.Error != "":
		answer = fmt.Sprintf("Run failed: %s\n\n%s", outcome.Error, retry)
	case answer == "":
		// Run finished (or failed) with nothing to say.
		e.host.Log.Warn("github: run completed with no final answer", "repo", owner+"/"+repo, "issue", number, "status", outcome.Status)
		answer = "Run finished with no answer, no error, and nothing delivered. " + retry
	case pr.isPlan:
		e.app.collapsePriorComments(context.Background(), owner, repo, number, "plan")
		answer += "\n\n" + deliveryMarker("plan")
	}
	return answer
}

// postNoVerdict replaces a review run's analysis prose with a short note: prose is not a
// review, and posting it as a comment reads like one while the PR gets no verdict.
func (e *Extension) postNoVerdict(chatID string, pr *pendingRun) {
	owner, repo, number := pr.owner, pr.repo, pr.number
	e.host.Log.Error("github: review run ended without staging a verdict; not posting its analysis", "repo", owner+"/"+repo, "pr", number)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg := "Review finished without a verdict, so nothing was posted. Comment `/review` to run it again."
	if err := e.app.postIssueComment(ctx, owner, repo, number, e.app.withFooter(msg, chatID)); err != nil {
		e.host.Log.Error("github: no-verdict note post failed", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	e.persistGithubSnapshot(chatID, pr.gh)
}

// postDeliveryFailure reports a failed delivery, so a pushed-but-unopened branch is recoverable by hand.
func (e *Extension) postDeliveryFailure(owner, repo string, number int, d deliveryOutcome) {
	msg := fmt.Sprintf("Delivery failed: %s", d.err)
	if d.branch != "" {
		msg += fmt.Sprintf("\n\nBranch `%s` holds the undelivered work.", d.branch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.app.postIssueComment(ctx, owner, repo, number, msg); err != nil {
		e.host.Log.Error("github: delivery-failure comment post failed", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// loadGithubContext fetches current GitHub state and diffs it against the stored snapshot.
// Does NOT persist — persistGithubSnapshot runs on completion.
func (e *Extension) loadGithubContext(ctx context.Context, chatID, owner, repo string, number int, isPR bool, triggerCommentID int64, forceReseed bool) githubContext {
	snap, err := e.fetchSnapshot(ctx, owner, repo, number, isPR)
	if err != nil {
		// The required meta call failed after HTTP retries: GitHub is unreachable, not the issue empty.
		// Flag it so label-triggered work refuses to run blind.
		e.host.Log.Warn("github: fetchSnapshot failed; this turn has no usable GitHub context",
			"repo", owner+"/"+repo, "number", number, "err", err)
		return githubContext{snap: snap, firstLoad: true, contextUnavailable: true}
	}

	var prevJSON string
	var hasPrev bool
	if !forceReseed {
		prevJSON, hasPrev, err = e.store.GetSnapshot(ctx, chatID)
		if err != nil {
			e.host.Log.Warn("github: GetSnapshot failed; treating this as a first load", "chat", chatID, "err", err)
			hasPrev = false
		}
	}

	gh := githubContext{snap: snap}
	if !hasPrev {
		gh.firstLoad = true
	} else {
		prev, uerr := unmarshalJSON[Snapshot](prevJSON)
		if uerr != nil {
			e.host.Log.Warn("github: stored snapshot did not decode; treating this as a first load", "chat", chatID, "err", uerr)
			gh.firstLoad = true
		} else {
			delta := diffSnapshots(prev, snap, triggerCommentID)
			gh.delta = &delta
		}
	}
	// Review scope is not delta.NewCommits: that advances on every dispatch and would under-scope a review
	// after a conversational turn. reviewScope reads a baseline only a delivered review advances.
	if isPR {
		gh.newCommits = e.reviewScope(ctx, chatID, snap)
		// A review that never sees CI status can approve red. Best-effort: a fetch failure leaves
		// the envelope silent on CI.
		if snap.HeadSHA != "" {
			if checks, cerr := e.app.listCheckRuns(ctx, owner, repo, snap.HeadSHA); cerr != nil {
				e.host.Log.Warn("github: check-runs fetch failed; envelope carries no CI status", "repo", owner+"/"+repo, "pr", number, "err", cerr)
			} else {
				e.app.enrichFailingChecks(ctx, owner, repo, checks)
				gh.checks = checks
			}
		}
	}
	return gh
}

// persistGithubSnapshot upserts the pre-run snapshot as the new watermark — only on genuine completion.
func (e *Extension) persistGithubSnapshot(chatID string, gh githubContext) {
	if gh.contextUnavailable {
		return
	}
	j, err := marshalJSON(gh.snap)
	if err != nil {
		e.host.Log.Warn("github: marshal snapshot failed; not persisted", "chat", chatID, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.store.SetSnapshot(ctx, chatID, j); err != nil {
		e.host.Log.Warn("github: SetSnapshot failed; next resume may re-see this turn's changes", "chat", chatID, "err", err)
	}
}

// reviewScope returns commits not yet covered by quack's last delivered review. Falls back to nil (review everything).
func (e *Extension) reviewScope(ctx context.Context, chatID string, snap Snapshot) []snapshotCommit {
	raw, ok, err := e.store.GetReviewBaseline(ctx, chatID)
	if err != nil {
		e.host.Log.Warn("github: GetReviewBaseline failed; reviewing everything this run", "chat", chatID, "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	ids, err := unmarshalJSON[[]string](raw)
	if err != nil {
		e.host.Log.Warn("github: stored review baseline did not decode; reviewing everything this run", "chat", chatID, "err", err)
		return nil
	}
	reviewed := make(map[string]bool, len(ids))
	for _, id := range ids {
		reviewed[id] = true
	}
	return newCommitsAgainstBaseline(snap.Commits, reviewed)
}

// advanceReviewBaseline persists current PR commits' patch-ids — only after a review is actually delivered.
func (e *Extension) advanceReviewBaseline(ctx context.Context, chatID string, commits []snapshotCommit) {
	ids := make([]string, 0, len(commits))
	for _, c := range commits {
		if c.PatchID != "" {
			ids = append(ids, c.PatchID)
		}
	}
	j, err := marshalJSON(ids)
	if err != nil {
		e.host.Log.Warn("github: marshal review baseline failed; not persisted", "chat", chatID, "err", err)
		return
	}
	if err := e.store.SetReviewBaseline(ctx, chatID, j); err != nil {
		e.host.Log.Warn("github: SetReviewBaseline failed; the next review may under-scope", "chat", chatID, "err", err)
	}
}
