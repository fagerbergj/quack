// The deterministic delivery check: a node whose task says "commit / push /
// open a PR" cannot pass unless the workspace ledger shows it actually did.
//
// Live e2e 2026-07-13 (TC2): a code-implementer told to add a game and open a
// pull request cloned the repo, wrote the game, ran the tests — then STOPPED,
// ending its answer with a markdown code block showing the contents of the
// registration file it was supposed to write. It never wrote that file, never
// committed, never pushed, never opened the PR. The judge PASSED it at 0.7. The
// rubric's task_completeness criterion is meant to catch exactly this (and did,
// in two earlier runs) — but it is an LLM judgment, and it is flaky. Delivery is
// mechanically checkable, so it is checked mechanically, in the cheap
// deterministic stage, where a failure drives a targeted revise round BEFORE the
// judge and sinks the round on its own (weakest-link) no matter what the judge
// thinks.
package vetting

import (
	"fmt"
	"regexp"
	"strings"
)

// The shared implementation/delivery vocabulary. implVerbRe matches an
// imperative code verb ("add a game", "implement X", "fix the bug"); deliveryRe
// a version-control term that means the ask is to SHIP the code, not merely
// describe it. Both must match for text to read as implement-AND-deliver
// (ImplementationIntent) — which keeps pure-research asks ("how does X work")
// from ever tripping either consumer: dag's planner routing backstop
// (checkImplementationRouting) and this check.
var (
	implVerbs  = `add|implement|create|write|fix|refactor|build|port|migrate|scaffold|generate`
	implVerbRe = regexp.MustCompile(`(?i)\b(` + implVerbs + `)\b`)
	deliveryRe = regexp.MustCompile(`(?i)(pull[ -]?request|\bpr\b|\bcommit\b|\bpush\b|\bbranch\b|\bmerge\b)`)

	// identRe matches a URL, a path, or a hyphen/underscore-joined identifier. A
	// verb inside one of those is part of a NAME, not an instruction — reviewing
	// branch `add-flappy-bird-openhands` is not asking anyone to add anything. \b
	// is no help here: it sits on the hyphen, so `\badd\b` matches inside the
	// branch name. Identifiers are dropped before any verb is looked for.
	identRe = regexp.MustCompile(`\S*[-_/]\S*`)

	// A verb is DIRECTED at the worker when it opens a sentence/clause or follows
	// and/then/also/please: "review the PR and fix the bugs" directs a change;
	// "review the PR that will add a game" merely describes one.
	clauseStart       = `(?i)(?:^|[.;:!?\n]\s*|\b(?:and|then|also|please)\s+)`
	implDirectedRe    = regexp.MustCompile(clauseStart + `(?:` + implVerbs + `)\b`)
	deliverDirectedRe = regexp.MustCompile(clauseStart + `(?:commit|push)\b`)

	// A review/audit ask is read-only by default (see ImplementationIntent).
	reviewRe = regexp.MustCompile(`(?i)\b(review|audit|critique|assess)(s|ed|ing)?\b`)

	// The per-action terms, narrower than deliveryRe: a task that merely names a
	// "branch" is not asking for a push (a report on a repo's branching
	// conventions must not trip this), so `branch` is deliberately NOT a trigger
	// on its own.
	commitRe = regexp.MustCompile(`(?i)\bcommit(s|ted|ting)?\b`)
	pushRe   = regexp.MustCompile(`(?i)\bpush(es|ed|ing)?\b`)
	prRe     = regexp.MustCompile(`(?i)(pull[ -]?requests?|\bPRs?\b)`)
)

// ImplementationIntent reports whether text asks for code to be implemented AND
// delivered — an imperative code verb plus a version-control term. Conservative
// by construction (both must match, and the two guards below only ever say NO).
// Shared with internal/dag's planner backstop so there is ONE delivery
// vocabulary, not two that drift.
//
// Live 2026-07-13: "Review pull request #4 … (branch add-flappy-bird-openhands)"
// was classified as implement-and-deliver — the verb matched only inside the
// BRANCH NAME — so the planner's backstop demanded a code-implementer node for a
// read-only review and rejected the plan 8 times, exhausting the re-plan budget.
// Hence: verbs are looked for in prose only (identifiers and URLs are dropped),
// and a request whose instruction is to REVIEW is not a request to change.
func ImplementationIntent(text string) bool {
	if !deliveryRe.MatchString(text) {
		return false
	}
	prose := identRe.ReplaceAllString(text, " ")
	if !implVerbRe.MatchString(prose) {
		return false
	}
	// A review/audit ask is read-only — unless it ALSO directs a change ("review
	// PR #4 and fix the bugs", "review the branch and implement the changes").
	return !reviewRe.MatchString(prose) || implDirectedRe.MatchString(prose)
}

// deliveryDemand is what a node's task requires be delivered. The implications
// are mechanical: opening a PR requires a pushed branch, and pushing requires a
// commit — so a task that says only "open a PR" still demands all three.
type deliveryDemand struct{ commit, push, pr bool }

// A node's task demands delivery when it reads as implement-and-deliver, or when
// it simply DIRECTS a commit/push ("Commit on branch add-foo and open a PR") —
// the latter is an instruction to ship in its own right, while a research task
// that merely mentions "the commits on main" directs nothing and is left alone.
func demandedDelivery(task string) deliveryDemand {
	if !ImplementationIntent(task) && !deliverDirectedRe.MatchString(task) {
		return deliveryDemand{}
	}
	d := deliveryDemand{pr: prRe.MatchString(task)}
	d.push = d.pr || pushRe.MatchString(task)
	d.commit = d.push || commitRe.MatchString(task)
	return d
}

// deliveryCriterion scores `delivery_complete` for a node: 0 when the task
// demands the work be committed/pushed and the worker's ledger holds no such
// SUCCESSFUL call (act.committed/act.pushed — see activityFromSession), 1 when it
// does. ok=false ⇒ the criterion does not apply at all (the task asks for no
// delivery: research, analysis, synthesis), leaving those nodes untouched.
//
// The hard requirement stops at git_push on purpose. Committing and pushing are
// mechanically universal (every code-implementer has the git tools) and are
// exactly what the live failure lacked. Opening the PR itself goes through
// github_pull_request, an OPTIONAL extension tool (a deployment without the
// GitHub App installed has no way to call it) — hard-requiring it would deadlock
// such a node in revise rounds it can never satisfy. It is recorded
// (act.prOpened) and named in the feedback, and the ledger shows the judge
// whether it happened.
func deliveryCriterion(task string, act workerActivity) (criterionScore, bool) {
	d := demandedDelivery(task)
	if !d.commit && !d.push {
		return criterionScore{}, false
	}
	var missing []string
	if d.commit && !act.committed {
		missing = append(missing, "no successful `git_commit`")
	}
	if d.push && !act.pushed {
		missing = append(missing, "no successful `git_push`")
	}
	if len(missing) == 0 {
		return criterionScore{Score: 1, Reason: "deterministic: the work was committed and pushed"}, true
	}
	want := "commit and push your work"
	if d.pr {
		want = "commit your work on the branch the task names, push it, and open the pull request with `github_pull_request`"
	}
	return criterionScore{Score: 0, Reason: fmt.Sprintf(
		"deterministic: your task requires the work be delivered (%s), but the workspace ledger contains %s. "+
			"You have not delivered your work — %s, then report what you actually did. "+
			"Printing file contents in your answer is NOT writing them, and describing a commit is NOT making one: "+
			"every file must be WRITTEN to disk with write_file/edit_file before you commit.",
		deliveryWording(d), strings.Join(missing, " and "), want)}, true
}

// postedReviewRe matches a task that asks for a review to be POSTED on the pull
// request — a posting verb next to the thing posted ("post your findings as
// inline review comments", "submit the review", "leave a review"), or the
// passive equivalent ("the review must be submitted"). Deliberately narrow: a
// prose ask ("what do you think of this code?", "summarise the diff on PR #4",
// "review the architecture and report your findings") is answered IN the answer,
// and a false positive there would deadlock the node in continuation rounds it
// could never satisfy — the same discipline as the delivery check above.
var postedReviewRe = regexp.MustCompile(
	`(?i)(\b(post|submit|leave|publish)\b[^.\n]{0,80}\b(reviews?|inline comments?|review comments?)\b` +
		`|\b(reviews?|comments?)\b[^.\n]{0,40}\b(posted|submitted|published)\b)`)

// reviewCriterion scores `review_posted` for a node: 0 when the task demands a
// review be posted on a pull request and the worker's ledger shows no SUCCESSFUL
// github_submit_review, 1 when it does. ok=false ⇒ the criterion does not apply
// (the task asks for no posted review), leaving every other node untouched.
//
// Live e2e 2026-07-13: a code-reviewer told to review a PR and post its findings
// produced a non-empty answer — a status update about shallow-clone trouble —
// and posted NOTHING (0 inline comments, 0 reviews on the PR). Non-empty answer,
// no commit/push demanded ⇒ workIncomplete said "done" and the half-finished work
// went to the flaky judge. Posting a review is mechanically checkable, so it is
// checked mechanically, exactly like a commit/push.
//
// The submit is the whole requirement: github_add_review_comment only accumulates
// a process-local DRAFT (see internal/github) — nothing is on the PR until
// github_submit_review succeeds.
func reviewCriterion(task string, act workerActivity) (criterionScore, bool) {
	if !prRe.MatchString(task) || !postedReviewRe.MatchString(task) {
		return criterionScore{}, false
	}
	if act.reviewSubmitted {
		return criterionScore{Score: 1, Reason: "deterministic: the review was submitted on the pull request"}, true
	}
	drafted := "the ledger shows no successful `github_add_review_comment` either"
	if act.reviewCommented {
		drafted = "your inline comments are still only a draft — `github_add_review_comment` posts nothing on its own"
	}
	return criterionScore{Score: 0, Reason: fmt.Sprintf(
		"deterministic: your task requires posting a review on the pull request, but the workspace ledger contains "+
			"no successful `github_submit_review` (%s). Describing your findings in your answer is NOT posting them: "+
			"record each finding with `github_add_review_comment`, then call `github_submit_review` with your summary "+
			"and verdict, then report what you actually posted.", drafted)}, true
}

// workIncomplete reports whether the worker's turn left the WORK unfinished — the
// gate's continuation condition (RunGatedRefine). Three mechanical signals, no LLM
// judgment:
//
//   - an EMPTY answer: a reasoning model that spends its whole output budget on
//     thinking returns no content. ADK ends the run there, and "the model emitted
//     no text" is indistinguishable from "the model is done" — except that it
//     almost always means the opposite (mid-task, out of road).
//   - an UNDELIVERED implement-and-deliver task: the task demanded a commit/push
//     and the ledger holds none, so whatever the worker wrote is a description of
//     work it never shipped.
//   - an UNPOSTED review task: the task demanded a review be posted on a PR and
//     the ledger holds no submit, so the "review" exists only as prose in the
//     answer — the reviewer's exact analogue of the undelivered commit.
//
// Everything else (research, analysis, synthesis; a delivered coding task) is
// complete as far as the gate is concerned — the judge takes it from here.
func workIncomplete(answer, task string, act workerActivity) bool {
	if strings.TrimSpace(answer) == "" {
		return true
	}
	for _, c := range incompleteCriteria(task, act) {
		if c.Score < 1 {
			return true
		}
	}
	return false
}

// incompleteCriteria returns the deterministic completion criteria that APPLY to
// this task — the one definition of "the work is actually done", shared by
// workIncomplete, foldDeterministic and the continuation prompt so they can
// never drift apart.
func incompleteCriteria(task string, act workerActivity) map[string]criterionScore {
	out := map[string]criterionScore{}
	if c, ok := deliveryCriterion(task, act); ok {
		out["delivery_complete"] = c
	}
	if c, ok := reviewCriterion(task, act); ok {
		out["review_posted"] = c
	}
	return out
}

// deliveryWording names, in the task's own terms, what delivery it asked for.
func deliveryWording(d deliveryDemand) string {
	switch {
	case d.pr:
		return "opening a pull request, which requires a committed and pushed branch"
	case d.push:
		return "pushing your commit"
	default:
		return "committing your changes"
	}
}
