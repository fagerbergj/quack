package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// mergeTimeout bounds one merge evaluation (a few API calls).
const mergeTimeout = 2 * time.Minute

// requiredCheckFailingRe matches GitHub's merge-block message for one named
// required status check, e.g. `Required status check "go-test" is failing.`
var requiredCheckFailingRe = regexp.MustCompile(`(?i)required status check "([^"]+)" is failing`)

// mergePendingRe matches GitHub refusing a merge only because a required
// check has not finished yet - not a failure, the next check event retries.
var mergePendingRe = regexp.MustCompile(`(?i)required status check.*\b(in progress|expected|pending|queued)\b`)

// mergeAPIErrorMessage extracts "message" from a merge API error's trailing JSON body so the review
// never carries raw JSON; full detail stays in the log.
func mergeAPIErrorMessage(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '{'); i >= 0 {
		var body struct {
			Message string `json:"message"`
		}
		if jerr := json.Unmarshal([]byte(msg[i:]), &body); jerr == nil && body.Message != "" {
			return body.Message
		}
	}
	return msg
}

// isHeadBranchModified reports GitHub's "the tip advanced mid-merge" error.
func isHeadBranchModified(err error) bool {
	return strings.Contains(mergeAPIErrorMessage(err), "Head branch was modified")
}

func isMergePending(err error) bool {
	return mergePendingRe.MatchString(mergeAPIErrorMessage(err))
}

// mergeFailureLine turns a merge API error into the one line appended to the
// approving review. A named failing required check gets the actionable form.
func mergeFailureLine(err error, fixLabel string) string {
	msg := strings.TrimSuffix(mergeAPIErrorMessage(err), ".")
	if m := requiredCheckFailingRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("Merge blocked: required check `%s` is failing. Apply `%s` or push a fix; re-review follows.", m[1], fixLabel)
	}
	return "Merge failed: " + msg + "."
}

// mergeOutcome is what one tryMerge evaluation found.
type mergeOutcome int

const (
	mergeNoIntent     mergeOutcome = iota // no standing authorization, or the PR is already closed
	mergeUnreviewed                       // quack has no verdict on this PR yet
	mergeNotApproved                      // latest verdict is request_changes/comment
	mergeStale                            // approval is for an older head
	mergePending                          // no check runs yet, or one on the head isn't green
	mergeHumanBlocked                     // a non-dismissed CHANGES_REQUESTED from another reviewer stands on the current head
	mergeFailed                           // GitHub refused for a real reason (conflict, protection)
	mergeDone
)

// greenConclusions are check-run conclusions that count as passing.
var greenConclusions = map[string]bool{"success": true, "neutral": true, "skipped": true}

// allChecksGreen: an empty list is not green, since a review can arrive before any workflow queues
// and "nothing reported yet" would merge a PR CI hasn't touched.
func allChecksGreen(checks []checkRunView) bool {
	if len(checks) == 0 {
		return false
	}
	for _, c := range checks {
		if c.Status != "completed" || !greenConclusions[c.Conclusion] {
			return false
		}
	}
	return true
}

// ciSuiteState is what headHasPendingCI found among the head's check
// suites, beyond the empty check-run list that triggered the lookup.
type ciSuiteState int

const (
	ciClear   ciSuiteState = iota // nothing left that could ever post a run: attempt the merge
	ciWaiting                     // a suite that can still produce a run hasn't finished
	ciFailed                      // a suite finished with a non-green conclusion and posted no runs
)

// headHasPendingCI classifies check suites when no runs exist: queued (wait), failed without a run (stop),
// or no CI (clear). A completed suite never fires again, so only non-completed suites count as pending.
func (e *Extension) headHasPendingCI(ctx context.Context, owner, repo, sha string) (ciSuiteState, error) {
	suites, err := e.app.listCheckSuites(ctx, owner, repo, sha)
	if err != nil {
		return ciClear, err
	}
	state := ciClear
	for _, s := range suites {
		if !s.producesRuns() {
			continue
		}
		if s.Status != "completed" {
			return ciWaiting, nil
		}
		if !greenConclusions[s.Conclusion] {
			state = ciFailed
		}
	}
	return state, nil
}

// blockingHumanReviewer finds a reviewer whose latest review of the current head is CHANGES_REQUESTED:
// the merge label never overrides a live objection. Reviews of older heads are stale, like old approvals.
func blockingHumanReviewer(reviews []prReview, headSHA string) (login string, blocked bool) {
	type latest struct {
		state string
		at    time.Time
	}
	byUser := make(map[string]latest, len(reviews))
	for _, r := range reviews {
		if r.CommitID != headSHA {
			continue
		}
		at, _ := time.Parse(time.RFC3339, r.SubmittedAt)
		if prev, ok := byUser[r.User.Login]; ok && !at.After(prev.at) {
			continue
		}
		byUser[r.User.Login] = latest{state: r.State, at: at}
	}
	for user, l := range byUser {
		if l.state == "CHANGES_REQUESTED" {
			return user, true
		}
	}
	return "", false
}

// reviewVerdictMarkerRe extracts quack's verdict from an own-PR comment's hidden marker (GitHub forbids self-review).
var reviewVerdictMarkerRe = regexp.MustCompile(`<!-- quack:delivery:review:(approve|request_changes|comment) -->`)

// reviewHeadMarkerRe extracts the head SHA an own-PR verdict was pinned against; a plain issue
// comment has no commit_id of its own.
var reviewHeadMarkerRe = regexp.MustCompile(`<!-- quack:delivery:head:(\S+) -->`)

// formalReviewVerdicts maps GitHub review states to the same vocabulary as reviewVerdictMarkerRe.
var formalReviewVerdicts = map[string]string{
	"APPROVED":          "approve",
	"CHANGES_REQUESTED": "request_changes",
	"COMMENTED":         "comment",
}

// verdictRef is quack's latest verdict and where it lives, so a merge outcome can be appended to it.
// commitID is empty when neither commit_id nor a head marker is available.
type verdictRef struct {
	verdict   string
	commitID  string
	reviewID  int64
	commentID int64
	body      string
	at        time.Time
}

// latestQuackVerdict reads both formal reviews and own-PR comment markers.
func (e *Extension) latestQuackVerdict(ctx context.Context, owner, repo string, number int) (verdictRef, error) {
	bot, err := e.app.botLogin(ctx)
	if err != nil {
		return verdictRef{}, err
	}
	var verdicts []verdictRef

	reviews, err := e.app.listReviews(ctx, owner, repo, number)
	if err != nil {
		return verdictRef{}, err
	}
	for _, r := range reviews {
		if r.User.Login != bot {
			continue
		}
		at, _ := time.Parse(time.RFC3339, r.SubmittedAt)
		// Marker first: an own-PR review always submits as COMMENTED but carries the real verdict
		// in the marker.
		if m := reviewVerdictMarkerRe.FindStringSubmatch(r.Body); m != nil {
			commitID := r.CommitID
			if commitID == "" {
				if hm := reviewHeadMarkerRe.FindStringSubmatch(r.Body); hm != nil {
					commitID = hm[1]
				}
			}
			verdicts = append(verdicts, verdictRef{verdict: m[1], commitID: commitID, reviewID: r.ID, body: r.Body, at: at})
			continue
		}
		if v := formalReviewVerdicts[r.State]; v != "" {
			verdicts = append(verdicts, verdictRef{verdict: v, commitID: r.CommitID, reviewID: r.ID, body: r.Body, at: at})
		}
	}

	comments, err := e.app.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return verdictRef{}, err
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
		commitID := ""
		if hm := reviewHeadMarkerRe.FindStringSubmatch(c.Body); hm != nil {
			commitID = hm[1]
		}
		verdicts = append(verdicts, verdictRef{verdict: m[1], commitID: commitID, commentID: c.ID, body: c.Body, at: at})
	}

	if len(verdicts) == 0 {
		return verdictRef{}, nil
	}
	sort.SliceStable(verdicts, func(i, j int) bool { return verdicts[i].at.Before(verdicts[j].at) })
	return verdicts[len(verdicts)-1], nil
}

// appendToVerdict appends line once to the review or own-PR comment carrying the verdict; only
// when there is nothing to edit does it become its own comment.
func (e *Extension) appendToVerdict(ctx context.Context, owner, repo string, number int, ref verdictRef, line string) {
	body, changed := appendLine(ref.body, line)
	if !changed {
		return
	}
	var err error
	switch {
	case ref.reviewID != 0:
		err = e.app.updateReview(ctx, owner, repo, number, ref.reviewID, body)
	case ref.commentID != 0:
		err = e.app.editIssueComment(ctx, owner, repo, ref.commentID, body)
	default:
		err = fmt.Errorf("no review to edit")
	}
	if err == nil {
		return
	}
	slog.Warn("github: appending the merge outcome to the review failed; posting it as a comment",
		"component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
	if perr := e.app.postIssueComment(ctx, owner, repo, number, line); perr != nil {
		slog.Error("github: merge outcome comment failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", perr)
	}
}

// tryMerge merges iff an intent stands, quack approved the current head, its checks are green and no
// reviewer objects. Re-run per event, no polling; err is infrastructure failure, never a refusal.
func (e *Extension) tryMerge(ctx context.Context, owner, repo string, number int) (mergeOutcome, error) {
	sessionID := issueSessionID(owner, repo, number)
	chatID := globalChatID(sessionID)
	unlock := e.mergeMu.Lock(chatID)
	defer unlock()

	intent, err := e.store.GetMergeIntent(ctx, chatID)
	if err != nil {
		return mergeNoIntent, fmt.Errorf("merge-intent lookup: %w", err)
	}
	var m *prMeta // set here when adopted below, so the later lookup isn't repeated
	if intent == nil {
		intent, m, err = e.adoptMergeIntent(ctx, owner, repo, number, chatID)
		if err != nil || intent == nil {
			return mergeNoIntent, err
		}
	}
	ref, err := e.latestQuackVerdict(ctx, owner, repo, number)
	if err != nil {
		return mergeNoIntent, fmt.Errorf("review lookup: %w", err)
	}
	switch ref.verdict {
	case "":
		return mergeUnreviewed, nil
	case "approve":
	default:
		return mergeNotApproved, nil
	}
	if m == nil {
		meta, merr := e.app.pullMeta(ctx, owner, repo, number)
		if merr != nil {
			return mergeNoIntent, fmt.Errorf("pull lookup: %w", merr)
		}
		m = &meta
	}
	if o, err, blocked := e.checkMergeBlockers(ctx, owner, repo, number, chatID, m, ref); err != nil || blocked {
		return o, err
	}
	if o, pending := e.ciGate(ctx, owner, repo, number, ref, m.HeadSHA); pending {
		return o, nil
	}
	return e.mergeAndSettle(ctx, owner, repo, number, ref, m.HeadSHA, intent, chatID)
}

// checkMergeBlockers: the closed-PR, stale-approval and human-objection checks before the CI gate.
func (e *Extension) checkMergeBlockers(ctx context.Context, owner, repo string, number int, chatID string, m *prMeta, ref verdictRef) (mergeOutcome, error, bool) {
	if m.Merged || m.State == "closed" {
		e.clearMergeIntent(ctx, chatID)
		return mergeNoIntent, nil, true
	}
	// A push after the approval invalidates it; a marker comment with no commit_id is trusted as-is.
	if ref.commitID != "" && ref.commitID != m.HeadSHA {
		return mergeStale, nil, true
	}
	allReviews, rerr := e.app.listReviews(ctx, owner, repo, number)
	if rerr != nil {
		return mergeNoIntent, fmt.Errorf("review lookup: %w", rerr), true
	}
	if login, blocked := blockingHumanReviewer(allReviews, m.HeadSHA); blocked {
		slog.Debug("github: merge blocked by a standing human objection", "component", "github", "repo", owner+"/"+repo, "pr", number, "reviewer", login)
		return mergeHumanBlocked, nil, true
	}
	return 0, nil, false
}

// adoptMergeIntent adopts an intent from the quack:merge label itself, failing closed on any doubt
// about who applied it.
func (e *Extension) adoptMergeIntent(ctx context.Context, owner, repo string, number int, chatID string) (*MergeIntent, *prMeta, error) {
	meta, merr := e.app.pullMeta(ctx, owner, repo, number)
	if merr != nil {
		return nil, nil, fmt.Errorf("pull lookup: %w", merr)
	}
	if !slices.Contains(meta.Labels, e.labels.Merge) {
		return nil, nil, nil
	}
	// GET /pulls doesn't say who applied the label, so check the timeline actor like delivery does.
	actor, stillApplied, known, aerr := e.app.mergeLabelActor(ctx, owner, repo, number, e.labels.Merge)
	if aerr != nil || !known || !stillApplied || strings.HasSuffix(actor, "[bot]") || !e.isInvokerAllowed(actor) {
		slog.Warn("github: quack:merge label present but its actor is not an authorized invoker; not adopting",
			"component", "github", "repo", owner+"/"+repo, "pr", number, "actor", actor, "err", aerr)
		return nil, nil, nil
	}
	if serr := e.store.SetMergeIntent(ctx, chatID, actor); serr != nil {
		return nil, nil, fmt.Errorf("merge-intent adopt: %w", serr)
	}
	slog.Info("github: adopted merge intent from the quack:merge label directly; its labeled delivery was never handled",
		"component", "github", "repo", owner+"/"+repo, "pr", number, "actor", actor)
	return &MergeIntent{ChatID: chatID, RequestedBy: actor}, &meta, nil
}

// ciGate: the head's CI evidence -> proceed, or a terminal outcome (waiting
// for runs / CI failed with no posted run to retry).
func (e *Extension) ciGate(ctx context.Context, owner, repo string, number int, ref verdictRef, headSHA string) (mergeOutcome, bool) {
	checks, cerr := e.app.listCheckRuns(ctx, owner, repo, headSHA)
	if cerr != nil {
		slog.Warn("github: check-runs lookup failed before merge; letting GitHub decide", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", cerr)
		return mergeDone, false
	}
	if len(checks) == 0 {
		// Zero runs means queued, no CI at all, or a suite that failed without a run. Suites exist
		// before runs, so they tell the three apart.
		if state, serr := e.headHasPendingCI(ctx, owner, repo, headSHA); serr != nil {
			slog.Warn("github: check-suites lookup failed before merge; letting GitHub decide", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", serr)
		} else if state == ciWaiting {
			return mergePending, true
		} else if state == ciFailed {
			e.appendToVerdict(ctx, owner, repo, number, ref, "Merge blocked: CI failed on this head and posted no check run to retry.")
			return mergeFailed, true
		}
		// ciClear or lookup failed: attempt the merge; branch protection's refusal is the guard.
		return mergeDone, false
	}
	if !allChecksGreen(checks) {
		return mergePending, true
	}
	return mergeDone, false
}

// mergeAndSettle: the mergePR call, its error classification, the archive,
// and the intent clear.
func (e *Extension) mergeAndSettle(ctx context.Context, owner, repo string, number int, ref verdictRef, headSHA string, intent *MergeIntent, chatID string) (mergeOutcome, error) {
	sha, err := e.app.mergePR(ctx, owner, repo, number, headSHA)
	if err != nil {
		switch {
		case isHeadBranchModified(err):
			return mergeStale, nil
		case isMergePending(err):
			return mergePending, nil
		}
		slog.Error("github merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		e.appendToVerdict(ctx, owner, repo, number, ref, mergeFailureLine(err, e.labels.Fix))
		return mergeFailed, nil
	}
	slog.Info("github pr merged", "component", "github", "repo", owner+"/"+repo, "pr", number, "user", intent.RequestedBy, "sha", sha)
	if e.autoArchiveOnMerge && e.host.ArchiveChat != nil {
		if derr := e.host.ArchiveChat(chatID); derr != nil {
			slog.Warn("github: auto-archive on merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", derr)
		}
	}
	// Clear the intent BEFORE announcing: once the line is visible the intent
	// must already be gone, not racing whoever reads it next.
	e.clearMergeIntent(ctx, chatID)
	e.appendToVerdict(ctx, owner, repo, number, ref, "Merged as "+shortSHA(sha)+".")
	return mergeDone, nil
}

func (e *Extension) clearMergeIntent(ctx context.Context, chatID string) {
	if err := e.store.DeleteMergeIntent(ctx, chatID); err != nil {
		slog.Warn("github: merge-intent cleanup failed", "component", "github", "chat", chatID, "err", err)
	}
}

// mergeIfApproved records the label's standing authorization and evaluates the merge; an unreviewed
// head gets a review dispatched so the label never silently waits.
func (e *Extension) mergeIfApproved(p pullRequestPayload, rawBody []byte) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Number
	sessionID := issueSessionID(owner, repo, number)
	chatID := globalChatID(sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), mergeTimeout)
	defer cancel()

	// Recorded BEFORE anything else - fail CLOSED, an unrecorded intent must
	// never be acted on as one.
	if err := e.store.SetMergeIntent(ctx, chatID, p.Sender.Login); err != nil {
		slog.Error("github: merge-intent persist failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		e.comment(ctx, owner, repo, number, fmt.Sprintf("Could not record the merge request: %v. Re-apply `%s` to retry.", err, e.labels.Merge))
		return
	}
	e.ackIssue(owner, repo, number)

	outcome, err := e.tryMerge(ctx, owner, repo, number)
	if err != nil {
		slog.Error("github: merge-label evaluation failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		e.comment(ctx, owner, repo, number, fmt.Sprintf("Could not evaluate the merge: %v. Re-apply `%s` to retry.", err, e.labels.Merge))
		return
	}
	if outcome != mergeUnreviewed && outcome != mergeStale {
		return
	}
	if _, live := e.inflightActive(sessionID); live {
		return // the running review's delivery re-evaluates the merge
	}
	e.spawn(func() { e.dispatch(autoReviewPayload(p, rawBody), autoReviewTask) })
}

// reviewOnMovedHeadUnderIntent re-reviews a pushed head under a standing merge intent; the head SHA
// recorded on the intent row makes repeated synchronize events dispatch once.
func (e *Extension) reviewOnMovedHeadUnderIntent(p pullRequestPayload, rawBody []byte) {
	if !e.triggers["merge"] {
		return
	}
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Number
	head := p.PullRequest.Head.SHA
	if head == "" {
		return
	}
	sessionID := issueSessionID(owner, repo, number)
	chatID := globalChatID(sessionID)
	unlock := e.mergeMu.Lock(chatID)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	intent, err := e.store.GetMergeIntent(ctx, chatID)
	if err != nil {
		slog.Warn("github: merge-intent lookup for push re-review failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	if intent == nil || intent.DispatchedHead == head {
		return
	}
	if err := e.store.SetMergeIntentDispatchedHead(ctx, chatID, head); err != nil {
		slog.Warn("github: recording the push re-review's head failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	e.spawn(func() { e.dispatch(autoReviewPayload(p, rawBody), autoReviewTask) })
}

// mergeOnEvent re-evaluates the merge after a state-changing webhook. Silent: the outcome lands
// on the review once the PR is mergeable.
func (e *Extension) mergeOnEvent(owner, repo string, number int, event string) {
	if !e.triggers["merge"] {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mergeTimeout)
	defer cancel()
	outcome, err := e.tryMerge(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: merge re-evaluation failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "event", event, "err", err)
		return
	}
	slog.Debug("github: merge re-evaluated", "component", "github", "repo", owner+"/"+repo, "pr", number, "event", event, "outcome", outcome)
}

func (e *Extension) comment(ctx context.Context, owner, repo string, number int, text string) {
	if err := e.app.postIssueComment(ctx, owner, repo, number, text); err != nil {
		slog.Error("github: comment failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// ackIssue posts a 👀 reaction on the issue/PR itself - the ack for a trigger
// that has no comment to react to (a label, a CI event, a deduped run).
func (e *Extension) ackIssue(owner, repo string, number int) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	if _, err := e.app.reactToIssue(ctx, owner, repo, number, "eyes"); err != nil {
		slog.Warn("github ack reaction failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// reReviewMovedHead builds the auto-review for a head that moved under an approval; the intent
// stands and merges once this review approves the new head.
func (e *Extension) reReviewMovedHead(ctx context.Context, pr *pendingRun) *issueCommentPayload {
	owner, repo, number := pr.owner, pr.repo, pr.number
	title := ""
	if meta, err := e.app.pullMeta(ctx, owner, repo, number); err == nil {
		title = meta.Title
	}
	cloneURL := ""
	if pr.dispatched.Run.Setup != nil {
		cloneURL = pr.dispatched.Run.Setup.Repo
	}
	p := reReviewPayload(owner, repo, number, title, cloneURL, pr.defaultBranch, pr.installationID)
	return &p
}

// reReviewPayload is a quack-triggered auto-review with no backing webhook event.
func reReviewPayload(owner, repo string, number int, title, cloneURL, defaultBranch string, installationID int64) issueCommentPayload {
	ri := repoInfo{owner, repo, cloneURL, defaultBranch, installationID}
	return ri.autoReview(number, title, []byte(`{}`), "github.head_branch_modified_rereview")
}

// checkEventPayload is the shared subset of check_suite / check_run /
// workflow_run webhooks: which PRs the completed CI object belongs to.
type checkEventPayload struct {
	Action      string       `json:"action"`
	CheckSuite  *ciHead      `json:"check_suite"`
	CheckRun    *ciHead      `json:"check_run"`
	WorkflowRun *ciHead      `json:"workflow_run"`
	Repository  ghRepository `json:"repository"`
}

type ciHead struct {
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

// mergeOnCheckEvent fans a completed CI event out to tryMerge per PR. GitHub fills pull_requests only
// when head and base share the repo, so fork CI must run where the base repo sees it or merges stall.
func (e *Extension) mergeOnCheckEvent(event string, body []byte) {
	var p checkEventPayload
	if json.Unmarshal(body, &p) != nil || p.Action != "completed" {
		return
	}
	head := p.CheckSuite
	if head == nil {
		head = p.CheckRun
	}
	if head == nil {
		head = p.WorkflowRun
	}
	if head == nil {
		return
	}
	for _, pr := range head.PullRequests {
		e.spawn(func() { e.mergeOnEvent(p.Repository.Owner.Login, p.Repository.Name, pr.Number, event) })
	}
}
