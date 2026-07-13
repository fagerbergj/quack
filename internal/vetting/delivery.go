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
	implVerbRe = regexp.MustCompile(`(?i)\b(add|implement|create|write|fix|refactor|build|port|migrate|scaffold|generate)\b`)
	deliveryRe = regexp.MustCompile(`(?i)(pull[ -]?request|\bpr\b|\bcommit\b|\bpush\b|\bbranch\b|\bmerge\b)`)

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
// by construction (both must match). Shared with internal/dag's planner backstop
// so there is ONE delivery vocabulary, not two that drift.
func ImplementationIntent(text string) bool {
	return implVerbRe.MatchString(text) && deliveryRe.MatchString(text)
}

// deliveryDemand is what a node's task requires be delivered. The implications
// are mechanical: opening a PR requires a pushed branch, and pushing requires a
// commit — so a task that says only "open a PR" still demands all three.
type deliveryDemand struct{ commit, push, pr bool }

func demandedDelivery(task string) deliveryDemand {
	if !ImplementationIntent(task) {
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

// workIncomplete reports whether the worker's turn left the WORK unfinished — the
// gate's continuation condition (RunGatedRefine). Two mechanical signals, no LLM
// judgment:
//
//   - an EMPTY answer: a reasoning model that spends its whole output budget on
//     thinking returns no content. ADK ends the run there, and "the model emitted
//     no text" is indistinguishable from "the model is done" — except that it
//     almost always means the opposite (mid-task, out of road).
//   - an UNDELIVERED implement-and-deliver task: the task demanded a commit/push
//     and the ledger holds none, so whatever the worker wrote is a description of
//     work it never shipped.
//
// Everything else (research, analysis, synthesis; a delivered coding task) is
// complete as far as the gate is concerned — the judge takes it from here.
func workIncomplete(answer, task string, act workerActivity) bool {
	if strings.TrimSpace(answer) == "" {
		return true
	}
	c, applies := deliveryCriterion(task, act)
	return applies && c.Score < 1
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
