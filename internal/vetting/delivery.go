// The deterministic delivery check: a node whose task says "commit / push /
// open a PR" cannot pass unless the workspace ledger shows it actually did.
//
// The rubric's task_completeness criterion is an LLM judgment and it is flaky
// (a live implementer "delivered" a markdown code block of the file it never
// wrote, and passed). Delivery is mechanically checkable, so it is checked
// mechanically, in the cheap deterministic stage, where a failure drives a
// targeted revise round BEFORE the judge and sinks the round on its own
// (weakest-link) no matter what the judge thinks.
package vetting

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// The shared implementation/delivery vocabulary. implVerbRe matches an
// imperative code verb ("add a game", "implement X", "fix the bug"); deliveryRe
// a version-control term that means the ask is to SHIP the code, not merely
// describe it. Both must match for text to read as implement-AND-deliver
// (ImplementationIntent) - which keeps pure-research asks ("how does X work")
// from ever tripping this deterministic delivery check. The dag package's plan
// routing check is now the rubric-judged PlanJudge (plan_judge.go), not this
// vocabulary - ImplementationIntent is scoped to gating a NODE's own delivery
// now, not to plan-time routing.
var (
	implVerbs  = `add|implement|create|write|fix|refactor|build|port|migrate|scaffold|generate`
	implVerbRe = regexp.MustCompile(`(?i)\b(` + implVerbs + `)\b`)
	deliveryRe = regexp.MustCompile(`(?i)(pull[ -]?request|\bpr\b|\bcommit\b|\bpush\b|\bbranch\b|\bmerge\b)`)

	// identRe matches a URL, a path, or a hyphen/underscore-joined identifier. A
	// verb inside one of those is part of a NAME, not an instruction - reviewing
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
// delivered - an imperative code verb plus a version-control term. Conservative
// by construction (both must match, and the two guards below only ever say NO).
// Shared with internal/dag's planner backstop so there is ONE delivery
// vocabulary, not two that drift.
//
// Verbs are looked for in prose only - identifiers and URLs are dropped (a
// delivery verb inside a branch name once forced a code-implementer node onto a
// read-only review) - and a request whose instruction is to REVIEW is not a
// request to change.
func ImplementationIntent(text string) bool {
	if !deliveryRe.MatchString(text) {
		return false
	}
	prose := identRe.ReplaceAllString(text, " ")
	if !implVerbRe.MatchString(prose) {
		return false
	}
	// A review/audit ask is read-only - unless it ALSO directs a change ("review
	// PR #4 and fix the bugs", "review the branch and implement the changes").
	return !reviewRe.MatchString(prose) || implDirectedRe.MatchString(prose)
}

// deliveryDemand is what a node's task requires be delivered. The implications
// are mechanical: opening a PR requires a pushed branch, and pushing requires a
// commit - so a task that says only "open a PR" still demands all three.
type deliveryDemand struct{ commit, push, pr bool }

// A node's task demands delivery when it reads as implement-and-deliver, or when
// it simply DIRECTS a commit/push ("Commit on branch add-foo and open a PR") -
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
// SUCCESSFUL call (act.committed/act.pushed - see activityFromSession), 1 when it
// does. ok=false ⇒ the criterion does not apply at all (the task asks for no
// delivery: research, analysis, synthesis), leaving those nodes untouched.
//
// The hard requirement stops at git_push on purpose. Committing and pushing are
// mechanically universal (every code-implementer has the git tools). Opening
// the PR itself goes through
// github_pull_request, an OPTIONAL extension tool (a deployment without the
// GitHub App installed has no way to call it) - hard-requiring it would deadlock
// such a node in revise rounds it can never satisfy. It is recorded
// (act.prOpened) and named in the feedback, and the ledger shows the judge
// whether it happened.
// hasStagedPR reports whether the worker has a live stage_pr call (handed the
// PR intent off to the gate - see StagedDelivery/commitDelivery). A
// worker that CAN still push/open a PR directly (act.pushed/act.prOpened,
// legacy toolset) satisfies delivery either way - staging is the new path,
// not a replacement requirement.
func hasStagedPR(act workerActivity) bool {
	_, ok := act.stagedDelivery["pr"]
	return ok
}

func deliveryCriterion(task string, act workerActivity) (criterionScore, bool) {
	d := demandedDelivery(task)
	if !d.commit && !d.push {
		return criterionScore{}, false
	}
	var missing []string
	if d.commit && !act.committed {
		missing = append(missing, "no successful `git_commit`")
	}
	if d.push && !act.pushed && !hasStagedPR(act) {
		missing = append(missing, "no successful `git_push` and no `stage_pr` call")
	}
	if len(missing) == 0 {
		return criterionScore{Score: 1, Reason: "deterministic: the work was committed and pushed (or staged for delivery)"}, true
	}
	want := "commit your work, then call `stage_pr(title, body)` to hand off delivery - the gate pushes it after your answer passes"
	if d.pr {
		want = "commit your work on the branch the task names, then call `stage_pr(title, body)` - the gate pushes the branch and opens the pull request after your answer passes (or push it and open the pull request with `github_pull_request` yourself, if you have that tool)"
	}
	return criterionScore{Score: 0, Reason: fmt.Sprintf(
		"deterministic: your task requires the work be delivered (%s), but the workspace ledger contains %s. "+
			"You have not delivered your work - %s, then report what you actually did. "+
			"Printing file contents in your answer is NOT writing them, and describing a commit is NOT making one: "+
			"every file must be WRITTEN to disk with write_file/edit_file before you commit.",
		deliveryWording(d), strings.Join(missing, " and "), want)}, true
}

// reviewCriterion scores `review_posted` for a node: 0 when the node IS a
// code-reviewer (isReviewer - a review-delivery node by construction, see
// Config.IsReviewer) and the worker's ledger shows no SUCCESSFUL
// github_submit_review, 1 when it does. ok=false ⇒ the criterion does not apply
// (not a reviewer node), leaving every other node untouched.
//
// A non-empty answer with nothing posted otherwise reads as "done" to
// workIncomplete. Posting a review is mechanically checkable, so it is checked
// mechanically, exactly like a commit/push.
//
// The submit is the whole requirement: github_add_review_comment only accumulates
// a process-local DRAFT (see internal/github) - nothing is on the PR until
// github_submit_review succeeds.
// hasStagedReview reports whether the worker has a live stage_review call
// (handed the submit intent off to the gate). A worker that can still submit
// directly (act.reviewSubmitted, legacy toolset) satisfies review either way.
func hasStagedReview(act workerActivity) bool {
	_, ok := act.stagedDelivery["review"]
	return ok
}

func reviewCriterion(task string, act workerActivity, isReviewer bool) (criterionScore, bool) {
	if !isReviewer {
		return criterionScore{}, false
	}
	if act.reviewSubmitted || hasStagedReview(act) {
		return criterionScore{Score: 1, Reason: "deterministic: the review was submitted (or staged for delivery) on the pull request"}, true
	}
	drafted := "the ledger shows no successful `github_add_review_comment` either"
	if act.reviewCommented {
		drafted = "your inline comments are still only a draft - `github_add_review_comment` posts nothing on its own"
	}
	return criterionScore{Score: 0, Reason: fmt.Sprintf(
		"deterministic: your task requires posting a review on the pull request, but the workspace ledger contains "+
			"no successful `github_submit_review` and no `stage_review` call (%s). Describing your findings in your "+
			"answer is NOT posting them: record each finding with `github_add_review_comment`, then call "+
			"`stage_review(event, body)` with your summary and verdict - the gate submits it after your answer passes "+
			"(or call `github_submit_review` yourself, if you have that tool) - then report what you actually did.", drafted)}, true
}

// workIncomplete reports whether the worker's turn left the WORK unfinished - the
// gate's continuation condition (RunGatedRefine). Three mechanical signals, no LLM
// judgment:
//
// - an EMPTY answer: a reasoning model that spends its whole output budget on
// thinking returns no content. ADK ends the run there, and "the model emitted
// no text" is indistinguishable from "the model is done" - except that it
// almost always means the opposite (mid-task, out of road).
// - an UNDELIVERED implement-and-deliver task: the task demanded a commit/push
// and the ledger holds none, so whatever the worker wrote is a description of
// work it never shipped.
// - an UNPOSTED review task: the task demanded a review be posted on a PR and
// the ledger holds no submit, so the "review" exists only as prose in the
// answer - the reviewer's exact analogue of the undelivered commit.
// - an UNVERIFIED review task: the task was to review a code change and the
// ledger holds no successful run_command, so the reviewer never executed the
// thing it is passing judgment on (behaviourCriterion).
//
// Everything else (research, analysis, synthesis; a delivered coding task) is
// complete as far as the gate is concerned - the judge takes it from here.
func workIncomplete(answer, task string, act workerActivity, readOnly, isReviewer bool) bool {
	if strings.TrimSpace(answer) == "" {
		return true
	}
	for _, c := range incompleteCriteria(task, act, readOnly, isReviewer) {
		if c.Score < 1 {
			return true
		}
	}
	return false
}

// proseExts are the file extensions with no runnable surface: a change confined
// to these cannot be executed, so behaviour_verified exempts it.
var proseExts = map[string]bool{".md": true, ".markdown": true, ".rst": true, ".txt": true, ".yaml": true, ".yml": true}

// noRunnableSurface reports whether every file the reviewer actually touched is
// prose/config - a docs-only change. Cheap and ledger-based (act.paths holds the
// paths of successful fs ops). A reviewer that touched NO files is not exempt:
// it has not even looked, let alone run anything.
func noRunnableSurface(act workerActivity) bool {
	if len(act.paths) == 0 {
		return false
	}
	for p := range act.paths {
		if !proseExts[strings.ToLower(filepath.Ext(p))] {
			return false
		}
	}
	return true
}

// behaviourCriterion scores `behaviour_verified` for a node: 0 when the node IS
// a code-reviewer (isReviewer) and the worker's ledger holds no SUCCESSFUL
// run_command - no test run, no build, no probe - 1 when it does. ok=false ⇒ the
// criterion does not apply (not a reviewer node, or nothing runnable in the
// change), leaving every other node untouched.
//
// Merely prompting a reviewer to run the code works about half the time (a live
// probe caught a show-stopper that a read-only review of the same PR then called
// "fully functional"). Prompt guidance is not reliability, so the execution is
// required mechanically: a read-only review is INCOMPLETE work (workIncomplete),
// which hands the reviewer its tools back in a continuation round.
//
// Deliberately weak on WHAT ran: any successful run_command counts (the test
// suite, a build, a probe). Grading the command itself would be an LLM judgment,
// which is the very thing this check exists to stop relying on.
func behaviourCriterion(task string, act workerActivity, isReviewer bool) (criterionScore, bool) {
	if !isReviewer || noRunnableSurface(act) {
		return criterionScore{}, false
	}
	if act.ranCommand {
		return criterionScore{Score: 1, Reason: "deterministic: the reviewer executed the code (successful `run_command`)"}, true
	}
	return criterionScore{Score: 0, Reason: "deterministic: your review has not EXECUTED the change - the ledger shows no successful `run_command`. " +
		"Reading cannot detect bugs of absence (a `step()` that updates velocity but never assigns the new position reads exactly like working physics, " +
		"and the tests pass because they assert the same absent behaviour). Install the dependencies, run the test suite, and write a throwaway harness " +
		"that drives the core loop and prints the state over time; then post what you find."}, true
}

// incompleteCriteria returns the deterministic completion criteria that APPLY to
// this task - the one definition of "the work is actually done", shared by
// workIncomplete, foldDeterministic and the continuation prompt so they can
// never drift apart.
func incompleteCriteria(task string, act workerActivity, readOnly, isReviewer bool) map[string]criterionScore {
	out := map[string]criterionScore{}
	// A read-only agent (code-reviewer / code-explorer) has no commit/push tools, so
	// a delivery demand read off its task is unsatisfiable - skip it. Its completion
	// is review_posted / exploration, never delivery.
	if !readOnly {
		if c, ok := deliveryCriterion(task, act); ok {
			out["delivery_complete"] = c
		}
	}
	if c, ok := reviewCriterion(task, act, isReviewer); ok {
		out["review_posted"] = c
	}
	if c, ok := behaviourCriterion(task, act, isReviewer); ok {
		out["behaviour_verified"] = c
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
