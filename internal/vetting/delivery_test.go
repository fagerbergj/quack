package vetting

import (
	"strings"
	"testing"
)

// Regression (live e2e 2026-07-13, TC2): a code-implementer node told to "Add a
// Flappy Bird game … and open it as a pull request … Commit on a branch named
// exactly add-flappy-bird-quack-v4" cloned the repo, wrote the game, ran the
// tests — and then STOPPED, ending its answer with a markdown code block showing
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
		t.Errorf("got %+v, want Score 1 — the work WAS committed and pushed", got)
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
		t.Errorf("got %+v (applies=%v), want Score 0 — the commit failed, so nothing was delivered", got, ok)
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

// A missing push alone is named precisely — the feedback must be actionable.
func TestDeliveryCriterionNamesOnlyWhatIsMissing(t *testing.T) {
	act := workerActivity{committed: true}
	got, _ := deliveryCriterion("Fix the flaky test and push the commit to the branch.", act)
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (nothing was pushed)", got.Score)
	}
	if strings.Contains(got.Reason, "no successful `git_commit`") {
		t.Errorf("Reason = %q, must not claim a missing commit — the worker DID commit", got.Reason)
	}
	if !strings.Contains(got.Reason, "git_push") {
		t.Errorf("Reason = %q, want it to name the missing push", got.Reason)
	}
}

// Regression (live, 2026-07-13): a code REVIEW of PR #4 on branch
// `add-flappy-bird-openhands` was classified as implement-and-deliver — the impl
// verb matched only INSIDE the branch name (\b sits on the hyphen) — so the
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
// directs a commit/push demands delivery on its own terms — the intent heuristic
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
	got := foldDeterministic(v, strings.Repeat("the game is done. ", 40), workerActivity{written: []string{"a.ts"}}, Config{Task: prTask})
	if c, ok := got.Criteria["delivery_complete"]; !ok || c.Score != 0 {
		t.Fatalf("delivery_complete = %+v (present=%v), want a hard 0", c, ok)
	}
	if got.Score >= 0.6 {
		t.Errorf("verdict score = %v, want a weakest-link fail below any threshold", got.Score)
	}
}
