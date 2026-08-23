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
	got, ok := deliveryCriterion(prTask, act, false)
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
	got, ok := deliveryCriterion(prTask, act, false)
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
		if _, ok := deliveryCriterion(task, workerActivity{}, false); ok {
			t.Errorf("delivery_complete fired on a non-delivery task: %q", task)
		}
	}
}

// Only SUCCESSFUL calls count: a git_commit that errored delivered nothing.
func TestDeliveryCriterionFailsWhenCommitErrored(t *testing.T) {
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "write_file", map[string]any{"path": "games/app/flappy.tsx"}),
		fnResp("1", "write_file", map[string]any{"bytes": float64(120), "created": true}),
		fnCall("2", "git_commit", map[string]any{"dir": "games", "message": "feat: flappy bird"}),
		fnResp("2", "git_commit", map[string]any{"error": "nothing to commit, working tree clean"}),
	), "")
	if act.committed {
		t.Fatal("activityFromSession recorded a FAILED git_commit as committed")
	}
	got, ok := deliveryCriterion(prTask, act, false)
	if !ok || got.Score != 0 {
		t.Errorf("got %+v (applies=%v), want Score 0 - the commit failed, so nothing was delivered", got, ok)
	}
}

// The ledger records the delivery actions that DID happen, from the session.
func TestActivityFromSessionRecordsDelivery(t *testing.T) {
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "git_commit", map[string]any{"dir": "games", "message": "feat: flappy bird"}),
		fnResp("1", "git_commit", map[string]any{"sha": "abc123", "files_changed": float64(3)}),
		fnCall("2", "git_push", map[string]any{"dir": "games"}),
		fnResp("2", "git_push", map[string]any{"remote": "origin", "branch": "add-flappy-bird-quack-v4", "sha": "abc123"}),
		fnCall("3", "github_pull_request", map[string]any{"owner": "fagerbergj", "repo": "games", "title": "Add Flappy Bird", "head": "add-flappy-bird-quack-v4"}),
		fnResp("3", "github_pull_request", map[string]any{"url": "https://github.com/fagerbergj/games/pull/7"}),
	), "")
	if !act.committed || !act.pushed {
		t.Errorf("committed=%v pushed=%v, want all true", act.committed, act.pushed)
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
	got, _ := deliveryCriterion("Fix the flaky test and push the commit to the branch.", act, false)
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

// TestDeliveryCriterionNamesTheOfferedTool pins #724: a run only ever has
// stage_pr XOR stage_push (internal/acp/acp.go's mcpToolNames), so the
// guidance text must name whichever one it actually has - never the other,
// which the agent has no way to call.
func TestDeliveryCriterionNamesTheOfferedTool(t *testing.T) {
	got, ok := deliveryCriterion(prTask, workerActivity{committed: true}, false)
	if !ok {
		t.Fatal("delivery_complete must apply")
	}
	if !strings.Contains(got.Reason, "stage_pr") || strings.Contains(got.Reason, "stage_push") {
		t.Errorf("new-PR run: Reason = %q, want stage_pr named and stage_push absent", got.Reason)
	}

	got, ok = deliveryCriterion(prTask, workerActivity{committed: true}, true)
	if !ok {
		t.Fatal("delivery_complete must apply")
	}
	if !strings.Contains(got.Reason, "stage_push") || strings.Contains(got.Reason, "stage_pr(") || strings.Contains(got.Reason, "`stage_pr`") {
		t.Errorf("existing-PR run: Reason = %q, want stage_push named and stage_pr absent", got.Reason)
	}
}

// The node-level delivery check keys off the NODE's task text, and a task that
// directs a commit/push demands delivery on its own terms - the intent heuristic
// must not be the only way in.
func TestDeliveryCriterionAppliesToADirectedDeliveryTask(t *testing.T) {
	if _, ok := deliveryCriterion("Commit on branch add-foo and open a PR.", workerActivity{}, false); !ok {
		t.Error("delivery_complete must apply to a task that directs a commit and a PR")
	}
}

// The criterion is folded into the verdict deterministically, so a judge that
// (wrongly) loves the answer cannot pass the node anyway (weakest-link).
func TestFoldDeterministicHardFailsUndeliveredNode(t *testing.T) {
	v := verdict{Score: 0.7, Criteria: map[string]criterionScore{"task_completeness": {Score: 0.7}}}
	// A terminal node: it has a delivery target, so the demand still applies.
	deliver := func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) { return nil, nil }
	det, _ := computeDeterministicCriteria(context.Background(), strings.Repeat("the game is done. ", 40), workerActivity{written: []string{"a.ts"}}, Config{Task: prTask, Deliver: deliver})
	got := mergeDeterministic(v, det, Config{Task: prTask, Deliver: deliver})
	if c, ok := got.Criteria["delivery_complete"]; !ok || c.Score != 0 {
		t.Fatalf("delivery_complete = %+v (present=%v), want a hard 0", c, ok)
	}
	if got.Score >= 0.6 {
		t.Errorf("verdict score = %v, want a weakest-link fail below any threshold", got.Score)
	}
}

// Regression (#764, live on quack#723): a non-terminal repo-chain node has no
// delivery target (dag/graph.go clears cfg.Deliver for it) but is not
// read-only either - it writes code. The old guard only checked ReadOnly, so
// this node fell into a delivery_complete criterion it structurally could not
// satisfy (no stage_pr/stage_push tool was ever offered) and burned every
// revise round on a task-level PR demand that wasn't its job to fulfil.
func TestIncompleteCriteria_NonTerminalChainNodeSkipsDeliveryDemand(t *testing.T) {
	act := workerActivity{written: []string{"a.ts"}, committed: true}
	crit := incompleteCriteria(prTask, act, false /* not read-only */, false /* no delivery target */, false, false)
	if _, ok := crit["delivery_complete"]; ok {
		t.Errorf("delivery_complete = %+v, want it absent - this node has no delivery target to be scored against", crit["delivery_complete"])
	}
}

// The counterpart: a TERMINAL node (Deliver set) with the identical task text
// still gets the criterion, and still fails when nothing was staged - only
// the delivery-target fact changes the outcome, never the task wording.
func TestIncompleteCriteria_TerminalNodeStillDemandsDelivery(t *testing.T) {
	act := workerActivity{written: []string{"a.ts"}}
	crit := incompleteCriteria(prTask, act, false, true /* has a delivery target */, false, false)
	c, ok := crit["delivery_complete"]
	if !ok {
		t.Fatal("delivery_complete must apply to a terminal node with a delivery target")
	}
	if c.Score != 0 {
		t.Errorf("Score = %v, want 0 - nothing was committed or pushed", c.Score)
	}
}

// A read-only node is unchanged by the delivery-target fact - it was already
// skipped on ReadOnly alone, and stays skipped whether or not Deliver is set.
func TestIncompleteCriteria_ReadOnlyNodeUnaffectedByDeliverTarget(t *testing.T) {
	act := workerActivity{}
	for _, hasTarget := range []bool{true, false} {
		crit := incompleteCriteria(prTask, act, true /* read-only */, hasTarget, false, false)
		if _, ok := crit["delivery_complete"]; ok {
			t.Errorf("hasDeliverTarget=%v: delivery_complete fired on a read-only node", hasTarget)
		}
	}
}

// Regression (#764, TC4): the continuation loop (workIncomplete) must not
// burn rounds re-asking a non-terminal node to deliver work it has no tool
// to deliver - the live log showed exactly this: "work not finished;
// continuing the worker with its tools ... committed=true pushed=false" on
// a node with no delivery target.
func TestWorkIncomplete_NonTerminalChainNodeNotHeldToDelivery(t *testing.T) {
	act := workerActivity{written: []string{"a.ts"}, committed: true}
	answer := "I implemented the change and committed it. This node does not deliver; a later node in the chain does."
	if workIncomplete(answer, prTask, act, false /* not read-only */, false /* no delivery target */, false, false) {
		t.Error("a non-terminal chain node must not be held incomplete solely for undelivered work it has no tool to deliver")
	}
}

// ---------------------------------------------------------------------------
// The review half of the same mechanism: a non-empty answer (e.g. a status
// update) with nothing posted used to read as "done" to workIncomplete, so
// posting a review is checked mechanically too.
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

// TestReviewCriterionDistinguishesRecoveredFromStaged pins #688: a review
// recovered from the answer's VERDICT/FINDINGS tail must not read identically
// to one staged via the review MCP tools, in the gate criteria - both are a
// real pass (the fallback keeps the node moving), but the Reason must say
// which path produced it.
func TestReviewCriterionDistinguishesRecoveredFromStaged(t *testing.T) {
	staged := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Event: "approve", Recovered: false},
	}}
	got, ok := reviewCriterion(reviewTask, staged, true)
	if !ok || got.Score != 1 {
		t.Fatalf("tool-staged review: got %+v (applies=%v), want Score 1", got, ok)
	}
	if strings.Contains(got.Reason, "RECOVERED") || strings.Contains(got.Reason, "tail") {
		t.Errorf("tool-staged Reason reads like a recovery: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "review MCP tools") {
		t.Errorf("tool-staged Reason should name the MCP tools path: %q", got.Reason)
	}

	recovered := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Event: "approve", Recovered: true},
	}}
	got, ok = reviewCriterion(reviewTask, recovered, true)
	if !ok || got.Score != 1 {
		t.Fatalf("recovered review: got %+v (applies=%v), want Score 1 (the fallback still keeps working)", got, ok)
	}
	if !strings.Contains(got.Reason, "RECOVERED") {
		t.Errorf("recovered Reason should flag the recovery: %q", got.Reason)
	}
}

// TestReviewCriterionDirectSubmitReasonDiffersFromStaging proves the three
// review_posted paths (direct github_submit_review, tool-staged, tail-
// recovered) each carry their own Reason text - never collapsed to one
// "submitted (or staged for delivery)" wording that can't tell them apart.
func TestReviewCriterionDirectSubmitReasonDiffersFromStaging(t *testing.T) {
	direct, _ := reviewCriterion(reviewTask, workerActivity{reviewSubmitted: true}, true)
	staged, _ := reviewCriterion(reviewTask, workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Recovered: false},
	}}, true)
	recovered, _ := reviewCriterion(reviewTask, workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Recovered: true},
	}}, true)
	if direct.Reason == staged.Reason || staged.Reason == recovered.Reason || direct.Reason == recovered.Reason {
		t.Fatalf("the three review_posted paths must carry distinct reasons:\ndirect=%q\nstaged=%q\nrecovered=%q",
			direct.Reason, staged.Reason, recovered.Reason)
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
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "github_add_review_comment", map[string]any{"owner": "fagerbergj", "repo": "games", "pull_number": float64(4), "path": "app/games.ts", "line": float64(12)}),
		fnResp("1", "github_add_review_comment", map[string]any{"index": float64(0), "draft_count": float64(1)}),
		fnCall("2", "github_submit_review", map[string]any{"owner": "fagerbergj", "repo": "games", "pull_number": float64(4), "event": "REQUEST_CHANGES"}),
		fnResp("2", "github_submit_review", map[string]any{"error": "422 Unprocessable Entity"}),
	), "")
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
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "github_add_review_comment", map[string]any{"owner": "fagerbergj", "repo": "games", "pull_number": float64(4), "path": "app/games.ts", "line": float64(12)}),
		fnResp("1", "github_add_review_comment", map[string]any{"index": float64(0), "draft_count": float64(1)}),
		fnCall("2", "github_submit_review", map[string]any{"owner": "fagerbergj", "repo": "games", "pull_number": float64(4), "event": "REQUEST_CHANGES"}),
		fnResp("2", "github_submit_review", map[string]any{"url": "https://github.com/fagerbergj/games/pull/4#pullrequestreview-1", "comments": float64(1)}),
	), "")
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
	if !workIncomplete("Reviewed.", pollutedTask, act, false, true, true, false) {
		t.Skip("polluted task no longer reads as implement-and-deliver; the ReadOnly guard would not fire")
	}
	if workIncomplete("Reviewed.", pollutedTask, act, true, true, true, false) {
		t.Error("a read-only reviewer with a submitted review must be COMPLETE - delivery must not apply to an agent that cannot commit")
	}
}

// The continuation condition: a non-empty answer that posted no review is NOT
// done - this is the exact live regression (a status update passed as an answer).
func TestWorkIncompleteOnAnUnpostedReview(t *testing.T) {
	statusUpdate := "I encountered technical difficulties with the shallow clone and could not complete the review."
	if !workIncomplete(statusUpdate, reviewTask, workerActivity{}, false, true, true, false) {
		t.Error("a non-empty answer that posted no review must be incomplete - the continuation loop has to re-invoke the reviewer with its tools")
	}
	if workIncomplete("Reviewed and requested changes.", reviewTask, workerActivity{reviewSubmitted: true, ranCommand: true}, false, true, true, false) {
		t.Error("a submitted review is complete work")
	}
	if workIncomplete("Here's what I think of the code: …", "What do you think of this code?", workerActivity{}, false, true, false, false) {
		t.Error("a prose task with a non-empty answer must not be held incomplete")
	}
}

// ---------------------------------------------------------------------------
// behaviour_verified: a code review must EXECUTE the change, not just read
// it - reading alone once missed a bug a probe on an earlier run had caught.
// Prompt guidance alone is a coin flip; execution is now a deterministic
// requirement.
// ---------------------------------------------------------------------------

func TestBehaviourCriterionFailsOnAReadOnlyReview(t *testing.T) {
	// Only reads: exactly the run that missed the bug.
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "git_checkout", map[string]any{"dir": "games", "ref": "add-flappy-bird-openhands"}),
		fnResp("1", "git_checkout", map[string]any{"branch": "add-flappy-bird-openhands", "head": "abc1234"}),
		fnCall("2", "read_file", map[string]any{"path": "games/app/flappy/game.ts"}),
		fnResp("2", "read_file", map[string]any{"content": "export function step() {}"}),
	), "")
	got, ok := behaviourCriterion(reviewTask, act, true)
	if !ok {
		t.Fatal("behaviour_verified must apply to a review of a real code change")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 - the reviewer executed nothing", got.Score)
	}
	if !strings.Contains(got.Reason, "EXECUTED") {
		t.Errorf("Reason = %q, want it to say the change was never executed", got.Reason)
	}
	if !workIncomplete("The game is fully functional.", reviewTask, act, false, true, true, false) {
		t.Error("a read-only review must be INCOMPLETE work - the continuation loop has to hand the reviewer its tools back")
	}
}

func TestBehaviourCriterionPassesWhenTheReviewerRanTheCode(t *testing.T) {
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "read_file", map[string]any{"path": "games/app/flappy/game.ts"}),
		fnResp("1", "read_file", map[string]any{"content": "export function step() {}"}),
		fnCall("2", "run_command", map[string]any{"dir": "games", "command": "npx tsx /tmp/probe.ts"}),
		fnResp("2", "run_command", map[string]any{"exit_code": float64(0), "stdout": "Start Y: 285.0, Final Y: 285.0"}),
	), "")
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
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "read_file", map[string]any{"path": "games/app/flappy/game.ts"}),
		fnResp("1", "read_file", map[string]any{"content": "export function step() {}"}),
		fnCall("2", "run_command", map[string]any{"dir": "games", "command": "npm test"}),
		fnResp("2", "run_command", map[string]any{"error": "command not allowed"}),
	), "")
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
		if workIncomplete("…", task, workerActivity{}, false, true, false, false) {
			t.Errorf("prose task held incomplete: %q", task)
		}
	}
}

// A review of a change with no runnable surface (docs/config only) is exempt:
// there is nothing to execute, so demanding an execution would deadlock it.
func TestBehaviourCriterionExemptsADocsOnlyReview(t *testing.T) {
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "read_file", map[string]any{"path": "games/README.md"}),
		fnResp("1", "read_file", map[string]any{"content": "# Games"}),
		fnCall("2", "read_file", map[string]any{"path": "games/.github/workflows/ci.yaml"}),
		fnResp("2", "read_file", map[string]any{"content": "on: push"}),
	), "")
	if _, ok := behaviourCriterion(reviewTask, act, true); ok {
		t.Error("behaviour_verified must not fire on a review whose change has no runnable surface (.md/.yaml only)")
	}
	if workIncomplete("Docs look good.", reviewTask, workerActivity{
		paths: act.paths, reviewSubmitted: true,
	}, false, true, true, false) {
		t.Error("a submitted docs-only review is complete work")
	}
}

// The criterion sinks the round on its own (weakest-link), no matter what the
// judge thought of the prose.
func TestFoldDeterministicHardFailsUnpostedReview(t *testing.T) {
	v := verdict{Score: 0.9, Criteria: map[string]criterionScore{"review_quality": {Score: 0.9}}}
	det, _ := computeDeterministicCriteria(context.Background(), "I could not access the PR's code.", workerActivity{}, Config{Task: reviewTask, IsReviewer: true})
	got := mergeDeterministic(v, det, Config{Task: reviewTask, IsReviewer: true})
	if c := got.Criteria["review_posted"]; c.Score != 0 {
		t.Fatalf("review_posted = %+v, want Score 0", c)
	}
	if got.Score >= 0.6 {
		t.Errorf("overall = %v, want the unposted review to sink the round", got.Score)
	}
}
