package vetting

import (
	"context"
	"strings"
	"testing"
)

// Regression (live e2e 2026-07-13, TC2): a code-implementer node told to "Add a
// Flappy Bird game … and open it as a pull request … Commit on a branch named
// exactly add-flappy-bird-quack-v4" cloned the repo, wrote the game, ran the
// tests - and then STOPPED, ending its answer with a markdown code block showing
// the contents of the registration file it was supposed to write. It never wrote
// that file, never committed, never pushed, never opened the PR. The judge PASSED
// it at 0.7: task_completeness is an LLM judgment and it is flaky. Delivery is
// mechanically checkable, so it is checked mechanically.

// prTask is the shape of the live task text.
const prTask = "Add a Flappy Bird game to https://github.com/fagerbergj/games and open it as a pull request. " +
	"Commit on a branch named exactly add-flappy-bird-quack-v4."

func TestDeliveryCriterionFailsWhenNothingWasDelivered(t *testing.T) {
	// The live ledger: files written, nothing committed or pushed.
	act := workerActivity{written: []string{"games/app/games/flappy-bird/page.tsx"}}
	got, ok := deliveryCriterion(prTask, act)
	if !ok {
		t.Fatal("delivery_complete must apply to a task that demands a pull request")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (nothing was committed or pushed)", got.Score)
	}
	for _, want := range []string{"git_commit", "git_push", "pull request"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("Reason = %q, want it to name %q", got.Reason, want)
		}
	}
}

func TestDeliveryCriterionPassesWhenCommittedAndPushed(t *testing.T) {
	act := workerActivity{
		written:   []string{"games/app/games/flappy-bird/page.tsx"},
		committed: true,
		pushed:    true,
	}
	got, ok := deliveryCriterion(prTask, act)
	if !ok {
		t.Fatal("delivery_complete must apply to a task that demands a pull request")
	}
	if got.Score != 1 {
		t.Errorf("got %+v, want Score 1 - the work WAS committed and pushed", got)
	}
}

// No delivery language, no git activity: a pure-research node must never trip
// this check. A false positive here would block legitimate work.
func TestDeliveryCriterionDoesNotFireOnResearchTask(t *testing.T) {
	tasks := []string{
		"Research the top 3 approaches to rate limiting and cite your sources.",
		"Summarise the repository's architecture and how its modules depend on each other.",
		"Write a report on the project's branching conventions.", // impl verb + "branch", but no commit/push/PR demand
	}
	for _, task := range tasks {
		if _, ok := deliveryCriterion(task, workerActivity{}); ok {
			t.Errorf("delivery_complete fired on a non-delivery task: %q", task)
		}
	}
}

// Only SUCCESSFUL calls count: a git_commit that errored delivered nothing.
func TestDeliveryCriterionFailsWhenCommitErrored(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "write_file", map[string]any{"path": "games/app/flappy.tsx"}),
		fnResp("1", "write_file", map[string]any{"bytes": float64(120), "created": true}),
		fnCall("2", "git_commit", map[string]any{"dir": "games", "message": "feat: flappy bird"}),
		fnResp("2", "git_commit", map[string]any{"error": "nothing to commit, working tree clean"}),
	))
	if act.committed {
		t.Fatal("activityFromSession recorded a FAILED git_commit as committed")
	}
	got, ok := deliveryCriterion(prTask, act)
	if !ok || got.Score != 0 {
		t.Errorf("got %+v (applies=%v), want Score 0 - the commit failed, so nothing was delivered", got, ok)
	}
}

// The ledger records the delivery actions that DID happen, from the session.
func TestActivityFromSessionRecordsDelivery(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "git_commit", map[string]any{"dir": "games", "message": "feat: flappy bird"}),
		fnResp("1", "git_commit", map[string]any{"sha": "abc123", "files_changed": float64(3)}),
		fnCall("2", "git_push", map[string]any{"dir": "games"}),
		fnResp("2", "git_push", map[string]any{"remote": "origin", "branch": "add-flappy-bird-quack-v4", "sha": "abc123"}),
		fnCall("3", "github_pull_request", map[string]any{"owner": "fagerbergj", "repo": "games", "title": "Add Flappy Bird", "head": "add-flappy-bird-quack-v4"}),
		fnResp("3", "github_pull_request", map[string]any{"url": "https://github.com/fagerbergj/games/pull/7"}),
	))
	if !act.committed || !act.pushed || !act.prOpened {
		t.Errorf("committed=%v pushed=%v prOpened=%v, want all true", act.committed, act.pushed, act.prOpened)
	}
	// The PR call also belongs in the ledger the judge sees (its URL is exactly
	// the kind of outcome an answer claims).
	if ws := buildWorkspaceSection(act); !strings.Contains(ws, "github_pull_request") || !strings.Contains(ws, "pull/7") {
		t.Errorf("workspace ledger = %q, want the github_pull_request call and its URL", ws)
	}
}

// A missing push alone is named precisely - the feedback must be actionable.
func TestDeliveryCriterionNamesOnlyWhatIsMissing(t *testing.T) {
	act := workerActivity{committed: true}
	got, _ := deliveryCriterion("Fix the flaky test and push the commit to the branch.", act)
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (nothing was pushed)", got.Score)
	}
	if strings.Contains(got.Reason, "no successful `git_commit`") {
		t.Errorf("Reason = %q, must not claim a missing commit - the worker DID commit", got.Reason)
	}
	if !strings.Contains(got.Reason, "git_push") {
		t.Errorf("Reason = %q, want it to name the missing push", got.Reason)
	}
}

// Regression (live, 2026-07-13): a code REVIEW of PR #4 on branch
// `add-flappy-bird-openhands` was classified as implement-and-deliver - the impl
// verb matched only INSIDE the branch name (\b sits on the hyphen) - so the
// planner's routing backstop demanded a code-implementer node for a read-only
// review and rejected the plan 8 times in a row, burning the whole re-plan budget.
const reviewPrompt = "Review pull request #4 on the GitHub repository https://github.com/fagerbergj/games " +
	"(branch add-flappy-bird-openhands). Clone the repo, check out the PR branch, and review the change " +
	"thoroughly. Post your findings as inline review comments, then submit the review with an overall verdict."

func TestImplementationIntent(t *testing.T) {
	cases := map[string]bool{
		// The live false positive: a review whose only impl verb is a branch name.
		reviewPrompt: false,
		// An impl verb inside a URL or an identifier is a NAME, not an instruction.
		"Summarise https://github.com/fagerbergj/games/pull/4 and the branch add-flappy-bird": false,
		"Check out feature/add-x and describe what the PR does":                               false,
		// A review/audit ask is never implement-and-deliver, however well-formed.
		"Review the PR that will add a game":                    false,
		"Audit the commits on the branch that implement search": false,
		// …unless it ALSO directs a change: review-then-change still counts.
		"Review PR #4 and fix the bugs you find, then push": true,
		"Review the branch and implement the changes":       true,
		// The case the backstop exists for (must not regress).
		"Add a Flappy Bird game to https://github.com/fagerbergj/games and open it as a pull request": true,
		"Fix the login bug and push a branch": true,
		// Both halves are still required.
		"Implement the parser":                     false, // impl verb, no delivery term
		"Explain what a pull request is":           false, // delivery term, no impl verb
		"What are the top 3 game engines in 2026?": false, // pure research
	}
	for text, want := range cases {
		if got := ImplementationIntent(text); got != want {
			t.Errorf("ImplementationIntent(%q) = %v, want %v", text, got, want)
		}
	}
}

// The node-level delivery check keys off the NODE's task text, and a task that
// directs a commit/push demands delivery on its own terms - the intent heuristic
// must not be the only way in.
func TestDeliveryCriterionAppliesToADirectedDeliveryTask(t *testing.T) {
	if _, ok := deliveryCriterion("Commit on branch add-foo and open a PR.", workerActivity{}); !ok {
		t.Error("delivery_complete must apply to a task that directs a commit and a PR")
	}
}

// The criterion is folded into the verdict deterministically, so a judge that
// (wrongly) loves the answer cannot pass the node anyway (weakest-link).
func TestFoldDeterministicHardFailsUndeliveredNode(t *testing.T) {
	v := verdict{Score: 0.7, Criteria: map[string]criterionScore{"task_completeness": {Score: 0.7}}}
	got := foldDeterministic(context.Background(), v, strings.Repeat("the game is done. ", 40), workerActivity{written: []string{"a.ts"}}, Config{Task: prTask})
	if c, ok := got.Criteria["delivery_complete"]; !ok || c.Score != 0 {
		t.Fatalf("delivery_complete = %+v (present=%v), want a hard 0", c, ok)
	}
	if got.Score >= 0.6 {
		t.Errorf("verdict score = %v, want a weakest-link fail below any threshold", got.Score)
	}
}

// ---------------------------------------------------------------------------
// The review half of the same mechanism.
//
// Live e2e 2026-07-13: a code-reviewer node told to review a pull request and
// post its findings produced a NON-EMPTY answer - a status update ("I hit
// shallow-clone difficulties…") - and posted NOTHING: zero inline comments,
// zero reviews on the PR. Because the answer wasn't empty and the task demanded
// no commit/push, workIncomplete said "done", no continuation fired, and the
// half-finished work went to the judge. Posting a review is mechanically
// checkable, so it is checked mechanically.
// ---------------------------------------------------------------------------

// reviewTask is the shape of the live task text.
const reviewTask = "Review pull request #4 on https://github.com/fagerbergj/games (branch add-flappy-bird-openhands). " +
	"Post your findings as inline review comments and submit the review."

func TestReviewCriterionFailsWhenNothingWasPosted(t *testing.T) {
	act := workerActivity{paths: map[string]bool{"games/README.md": true}}
	got, ok := reviewCriterion(reviewTask, act, true)
	if !ok {
		t.Fatal("review_posted must apply to a reviewer node")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (no review was posted)", got.Score)
	}
	if !strings.Contains(got.Reason, "github_submit_review") {
		t.Errorf("Reason = %q, want it to name github_submit_review", got.Reason)
	}
}

func TestReviewCriterionPassesWhenReviewSubmitted(t *testing.T) {
	act := workerActivity{reviewCommented: true, reviewSubmitted: true}
	got, ok := reviewCriterion(reviewTask, act, true)
	if !ok {
		t.Fatal("review_posted must apply to a reviewer node")
	}
	if got.Score != 1 {
		t.Errorf("got %+v, want Score 1 - the review WAS submitted", got)
	}
}

// Drafted comments are not a posted review: github_add_review_comment only
// accumulates a draft (see internal/github) - the review exists on the PR only
// after github_submit_review.
func TestReviewCriterionFailsOnDraftedButUnsubmittedComments(t *testing.T) {
	got, ok := reviewCriterion(reviewTask, workerActivity{reviewCommented: true}, true)
	if !ok || got.Score != 0 {
		t.Fatalf("got %+v (applies=%v), want Score 0 - drafted comments were never submitted", got, ok)
	}
	if !strings.Contains(got.Reason, "draft") {
		t.Errorf("Reason = %q, want it to explain the draft was never submitted", got.Reason)
	}
}

// The gate is structural now (#482): review_posted never fires for a node that
// isn't the code-reviewer agent, no matter how the task reads - including the
// bare label-review default that has no posting verb at all.
func TestReviewCriterionKeysOnIsReviewerNotTaskText(t *testing.T) {
	nonReviewerTasks := []string{
		"What do you think of this code? Explain the tradeoffs.",
		"Summarise the diff on pull request #4 and report what changed.",
		"Review the architecture of the repository and report your findings.",
		"Research how other projects post code reviews and cite your sources.",
	}
	for _, task := range nonReviewerTasks {
		if _, ok := reviewCriterion(task, workerActivity{}, false); ok {
			t.Errorf("review_posted fired for a non-reviewer node: %q", task)
		}
	}
	// A reviewer node with the bare label-review task (no posting verb -
	// dag.autoReviewTask's shape pre-#482) still applies the criterion.
	if _, ok := reviewCriterion("Review this pull request.", workerActivity{}, true); !ok {
		t.Error("review_posted must apply to a reviewer node even when the task names no posting verb (#482)")
	}
}

// Only SUCCESSFUL calls count: a github_submit_review that errored posted nothing.
func TestReviewCriterionFailsWhenSubmitErrored(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "github_add_review_comment", map[string]any{"owner": "fagerbergj", "repo": "games", "pull_number": float64(4), "path": "app/games.ts", "line": float64(12)}),
		fnResp("1", "github_add_review_comment", map[string]any{"index": float64(0), "draft_count": float64(1)}),
		fnCall("2", "github_submit_review", map[string]any{"owner": "fagerbergj", "repo": "games", "pull_number": float64(4), "event": "REQUEST_CHANGES"}),
		fnResp("2", "github_submit_review", map[string]any{"error": "422 Unprocessable Entity"}),
	))
	if act.reviewSubmitted {
		t.Fatal("activityFromSession recorded a FAILED github_submit_review as submitted")
	}
	if !act.reviewCommented {
		t.Error("the successful github_add_review_comment should be recorded")
	}
	got, ok := reviewCriterion(reviewTask, act, true)
	if !ok || got.Score != 0 {
		t.Errorf("got %+v (applies=%v), want Score 0 - the submit failed, so nothing was posted", got, ok)
	}
}

func TestActivityFromSessionRecordsReview(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "github_add_review_comment", map[string]any{"owner": "fagerbergj", "repo": "games", "pull_number": float64(4), "path": "app/games.ts", "line": float64(12)}),
		fnResp("1", "github_add_review_comment", map[string]any{"index": float64(0), "draft_count": float64(1)}),
		fnCall("2", "github_submit_review", map[string]any{"owner": "fagerbergj", "repo": "games", "pull_number": float64(4), "event": "REQUEST_CHANGES"}),
		fnResp("2", "github_submit_review", map[string]any{"url": "https://github.com/fagerbergj/games/pull/4#pullrequestreview-1", "comments": float64(1)}),
	))
	if !act.reviewCommented || !act.reviewSubmitted {
		t.Errorf("reviewCommented=%v reviewSubmitted=%v, want both true", act.reviewCommented, act.reviewSubmitted)
	}
	if ws := buildWorkspaceSection(act); !strings.Contains(ws, "github_submit_review") || !strings.Contains(ws, "pullrequestreview-1") {
		t.Errorf("workspace ledger = %q, want the github_submit_review call and its URL", ws)
	}
}

// A read-only reviewer (ReadOnly=true - no commit/push tools) must NOT be held to a
// delivery demand read off a task polluted with the PR's own "Add …/open a PR"
// wording. It CANNOT commit, so demanding it loops forever; its completion is
// review_posted, not delivery.
func TestReadOnlyReviewerNotHeldToDelivery(t *testing.T) {
	pollutedTask := "Review PR #5, and open a pull request is what it does - it will Add a Flappy Bird game. " +
		"Read the diff and post inline review comments; submit the review."
	act := workerActivity{reviewSubmitted: true, ranCommand: true}
	if !workIncomplete("Reviewed.", pollutedTask, act, false, true) {
		t.Skip("polluted task no longer reads as implement-and-deliver; the ReadOnly guard would not fire")
	}
	if workIncomplete("Reviewed.", pollutedTask, act, true, true) {
		t.Error("a read-only reviewer with a submitted review must be COMPLETE - delivery must not apply to an agent that cannot commit")
	}
}

// The continuation condition: a non-empty answer that posted no review is NOT
// done - this is the exact live regression (a status update passed as an answer).
func TestWorkIncompleteOnAnUnpostedReview(t *testing.T) {
	statusUpdate := "I encountered technical difficulties with the shallow clone and could not complete the review."
	if !workIncomplete(statusUpdate, reviewTask, workerActivity{}, false, true) {
		t.Error("a non-empty answer that posted no review must be incomplete - the continuation loop has to re-invoke the reviewer with its tools")
	}
	if workIncomplete("Reviewed and requested changes.", reviewTask, workerActivity{reviewSubmitted: true, ranCommand: true}, false, true) {
		t.Error("a submitted review is complete work")
	}
	if workIncomplete("Here's what I think of the code: …", "What do you think of this code?", workerActivity{}, false, false) {
		t.Error("a prose task with a non-empty answer must not be held incomplete")
	}
}

// ---------------------------------------------------------------------------
// behaviour_verified: a code review must EXECUTE the change, not just read it.
//
// Live e2e 2026-07-13: given run_command + write_file, the code-reviewer wrote a
// throwaway trace harness, ran it, and printed "Start Y: 285.0, Final Y after 30
// frames: 285.0 → BUG CONFIRMED - bird Y NEVER CHANGES" - a show-stopper in a PR
// that passed typecheck, lint and all 19 of its own unit tests (the tests assert
// the same absent behaviour). On the NEXT run, same PR, it wrote no probe, read
// the diff, and called the game "fully functional". Prompt guidance alone is a
// coin flip; execution is now a deterministic requirement.
// ---------------------------------------------------------------------------

func TestBehaviourCriterionFailsOnAReadOnlyReview(t *testing.T) {
	// Only reads: exactly the run that missed the bug.
	act := activityFromSession(newTestSession(t,
		fnCall("1", "git_checkout", map[string]any{"dir": "games", "ref": "add-flappy-bird-openhands"}),
		fnResp("1", "git_checkout", map[string]any{"branch": "add-flappy-bird-openhands", "head": "abc1234"}),
		fnCall("2", "read_file", map[string]any{"path": "games/app/flappy/game.ts"}),
		fnResp("2", "read_file", map[string]any{"content": "export function step() {}"}),
	))
	got, ok := behaviourCriterion(reviewTask, act, true)
	if !ok {
		t.Fatal("behaviour_verified must apply to a review of a real code change")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 - the reviewer executed nothing", got.Score)
	}
	if !strings.Contains(got.Reason, "run_command") {
		t.Errorf("Reason = %q, want it to name run_command", got.Reason)
	}
	if !workIncomplete("The game is fully functional.", reviewTask, act, false, true) {
		t.Error("a read-only review must be INCOMPLETE work - the continuation loop has to hand the reviewer its tools back")
	}
}

func TestBehaviourCriterionPassesWhenTheReviewerRanTheCode(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "read_file", map[string]any{"path": "games/app/flappy/game.ts"}),
		fnResp("1", "read_file", map[string]any{"content": "export function step() {}"}),
		fnCall("2", "run_command", map[string]any{"dir": "games", "command": "npx tsx /tmp/probe.ts"}),
		fnResp("2", "run_command", map[string]any{"exit_code": float64(0), "stdout": "Start Y: 285.0, Final Y: 285.0"}),
	))
	if !act.ranCommand {
		t.Fatal("activityFromSession must record a successful run_command")
	}
	got, ok := behaviourCriterion(reviewTask, act, true)
	if !ok || got.Score != 1 {
		t.Fatalf("got %+v (applies=%v), want Score 1 - the reviewer executed the code", got, ok)
	}
}

// A run_command that ERRORED never executed anything - same rule as
// written/committed/pushed: successful calls only.
func TestBehaviourCriterionFailsWhenTheCommandErrored(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "read_file", map[string]any{"path": "games/app/flappy/game.ts"}),
		fnResp("1", "read_file", map[string]any{"content": "export function step() {}"}),
		fnCall("2", "run_command", map[string]any{"dir": "games", "command": "npm test"}),
		fnResp("2", "run_command", map[string]any{"error": "command not allowed"}),
	))
	if act.ranCommand {
		t.Fatal("a FAILED run_command must not count as an execution")
	}
	got, ok := behaviourCriterion(reviewTask, act, true)
	if !ok || got.Score != 0 {
		t.Errorf("got %+v (applies=%v), want Score 0 - nothing ran", got, ok)
	}
}

// A prose ask about a snippet has no code change to execute - a false positive
// would deadlock the node in continuation rounds it can never satisfy.
func TestBehaviourCriterionDoesNotFireOnProseTask(t *testing.T) {
	tasks := []string{
		"What do you think of this code? Explain the tradeoffs.",
		"Summarise the diff on pull request #4 and report what changed.",
		"Review the architecture of the repository and report your findings.",
	}
	for _, task := range tasks {
		if _, ok := behaviourCriterion(task, workerActivity{}, false); ok {
			t.Errorf("behaviour_verified fired on a task with no code change to execute: %q", task)
		}
		if workIncomplete("…", task, workerActivity{}, false, false) {
			t.Errorf("prose task held incomplete: %q", task)
		}
	}
}

// A review of a change with no runnable surface (docs/config only) is exempt:
// there is nothing to execute, so demanding an execution would deadlock it.
func TestBehaviourCriterionExemptsADocsOnlyReview(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "read_file", map[string]any{"path": "games/README.md"}),
		fnResp("1", "read_file", map[string]any{"content": "# Games"}),
		fnCall("2", "read_file", map[string]any{"path": "games/.github/workflows/ci.yaml"}),
		fnResp("2", "read_file", map[string]any{"content": "on: push"}),
	))
	if _, ok := behaviourCriterion(reviewTask, act, true); ok {
		t.Error("behaviour_verified must not fire on a review whose change has no runnable surface (.md/.yaml only)")
	}
	if workIncomplete("Docs look good.", reviewTask, workerActivity{
		paths: act.paths, reviewSubmitted: true,
	}, false, true) {
		t.Error("a submitted docs-only review is complete work")
	}
}

// The criterion sinks the round on its own (weakest-link), no matter what the
// judge thought of the prose.
func TestFoldDeterministicHardFailsUnpostedReview(t *testing.T) {
	v := verdict{Score: 0.9, Criteria: map[string]criterionScore{"review_quality": {Score: 0.9}}}
	got := foldDeterministic(context.Background(), v, "I could not access the PR's code.", workerActivity{}, Config{Task: reviewTask, IsReviewer: true})
	if c := got.Criteria["review_posted"]; c.Score != 0 {
		t.Fatalf("review_posted = %+v, want Score 0", c)
	}
	if got.Score >= 0.6 {
		t.Errorf("overall = %v, want the unposted review to sink the round", got.Score)
	}
}
