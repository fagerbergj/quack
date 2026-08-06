// delivery.go: deterministic delivery check - ledger must show commit/push when task demands it.
package vetting

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Implementation/delivery vocabulary: implVerbRe + deliveryRe for implement-AND-deliver.
var (
	implVerbs  = `add|implement|create|write|fix|refactor|build|port|migrate|scaffold|generate`
	implVerbRe = regexp.MustCompile(`(?i)\b(` + implVerbs + `)\b`)
	deliveryRe = regexp.MustCompile(`(?i)(pull[ -]?request|\bpr\b|\bcommit\b|\bpush\b|\bbranch\b|\bmerge\b)`)

	// identRe: matches URLs/paths/identifiers (verbs inside names are not instructions).
	identRe = regexp.MustCompile(`\S*[-_/]\S*`)

	// Directed verb: opens a sentence or follows and/then/also/please.
	clauseStart       = `(?i)(?:^|[.;:!?\n]\s*|\b(?:and|then|also|please)\s+)`
	implDirectedRe    = regexp.MustCompile(clauseStart + `(?:` + implVerbs + `)\b`)
	deliverDirectedRe = regexp.MustCompile(clauseStart + `(?:commit|push)\b`)

	// review/audit ask is read-only by default.
	reviewRe = regexp.MustCompile(`(?i)\b(review|audit|critique|assess)(s|ed|ing)?\b`)

	// Per-action terms: `branch` is NOT a trigger (mention ≠ push demand).
	commitRe = regexp.MustCompile(`(?i)\bcommit(s|ted|ting)?\b`)
	pushRe   = regexp.MustCompile(`(?i)\bpush(es|ed|ing)?\b`)
	prRe     = regexp.MustCompile(`(?i)(pull[ -]?requests?|\bPRs?\b)`)
)

// ImplementationIntent: reports if text asks for code AND delivery (identifiers/URLs excluded).
func ImplementationIntent(text string) bool {
	if !deliveryRe.MatchString(text) {
		return false
	}
	prose := identRe.ReplaceAllString(text, " ")
	if !implVerbRe.MatchString(prose) {
		return false
	}
	// review/audit is read-only unless it also directs a change.
	return !reviewRe.MatchString(prose) || implDirectedRe.MatchString(prose)
}

// deliveryDemand: commit/push/PR - PR implies push, push implies commit.
type deliveryDemand struct{ commit, push, pr bool }

// A node's task demands delivery when it directs a commit/push, not from research text that merely mentions commits.
func demandedDelivery(task string) deliveryDemand {
	if !ImplementationIntent(task) && !deliverDirectedRe.MatchString(task) {
		return deliveryDemand{}
	}
	d := deliveryDemand{pr: prRe.MatchString(task)}
	d.push = d.pr || pushRe.MatchString(task)
	d.commit = d.push || commitRe.MatchString(task)
	return d
}

// deliveryCriterion scores delivery_complete. Stops at git_push (PR is optional).
func hasStagedPR(act workerActivity) bool {
	_, ok := act.stagedDelivery["pr"]
	return ok
}

// stageToolName: the delivery tool THIS run actually has - stage_pr opens a
// new PR, stage_push hands off a commit to one that's already open. A run
// only ever gets one of the two (internal/acp/acp.go's mcpToolNames), so the
// guidance text must name whichever it was given, never the other (#724).
func stageToolName(existingPR bool) string {
	if existingPR {
		return "stage_push"
	}
	return "stage_pr"
}

func deliveryCriterion(task string, act workerActivity, existingPR bool) (criterionScore, bool) {
	d := demandedDelivery(task)
	if !d.commit && !d.push {
		return criterionScore{}, false
	}
	tool := stageToolName(existingPR)
	var missing []string
	if d.commit && !act.committed {
		missing = append(missing, "no successful `git_commit`")
	}
	if d.push && !act.pushed && !hasStagedPR(act) {
		missing = append(missing, fmt.Sprintf("no successful `git_push` and no `%s` call", tool))
	}
	if len(missing) == 0 {
		return criterionScore{Score: 1, Reason: "deterministic: the work was committed and pushed (or staged for delivery)"}, true
	}
	want := fmt.Sprintf("commit your work, then call `%s` to hand off delivery - the gate pushes it after your answer passes", tool)
	if d.pr {
		if existingPR {
			want = "commit your work on the branch the task names, then call `stage_push()` - title and body are optional, pass one only if you're deliberately changing it - the gate pushes the branch after your answer passes"
		} else {
			want = "commit your work on the branch the task names, then call `stage_pr(title, body)` - the gate pushes the branch and opens the pull request after your answer passes (or push it and open the pull request with `github_pull_request` yourself, if you have that tool)"
		}
	}
	return criterionScore{Score: 0, Reason: fmt.Sprintf(
		"deterministic: your task requires the work be delivered (%s), but the workspace ledger contains %s. "+
			"You have not delivered your work - %s, then report what you actually did. "+
			"Printing file contents in your answer is NOT writing them, and describing a commit is NOT making one: "+
			"every file must be WRITTEN to disk with write_file/edit_file before you commit.",
		deliveryWording(d), strings.Join(missing, " and "), want)}, true
}

// reviewCriterion scores review_posted: github_submit_review must succeed (add_review_comment is just a draft).
func reviewCriterion(task string, act workerActivity, isReviewer bool) (criterionScore, bool) {
	if !isReviewer {
		return criterionScore{}, false
	}
	if act.reviewSubmitted {
		return criterionScore{Score: 1, Reason: "deterministic: the review was submitted directly on the pull request (`github_submit_review`)"}, true
	}
	// Distinguish recovered reviews from tool-staged ones (#688).
	if sd, staged := act.stagedDelivery["review"]; staged {
		if sd.Recovered {
			return criterionScore{Score: 1, Reason: "deterministic: the review was RECOVERED from the answer's VERDICT/FINDINGS tail - " +
				"the review MCP tools (stage_review/stage_review_comment) were not used this round"}, true
		}
		return criterionScore{Score: 1, Reason: "deterministic: the review was staged for delivery via the review MCP tools (stage_review/stage_review_comment)"}, true
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

// workIncomplete: empty answer, undelivered task, unposted review, or unverified review (no run_command).
func workIncomplete(answer, task string, act workerActivity, readOnly, isReviewer, existingPR bool) bool {
	if strings.TrimSpace(answer) == "" {
		return true
	}
	for _, c := range incompleteCriteria(task, act, readOnly, isReviewer, existingPR) {
		if c.Score < 1 {
			return true
		}
	}
	return false
}

// proseExts: file extensions with no runnable surface (behaviour_verified exemption).
var proseExts = map[string]bool{".md": true, ".markdown": true, ".rst": true, ".txt": true, ".yaml": true, ".yml": true}

// noRunnableSurface: every touched file is prose/config (docs-only).
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

// behaviourCriterion: 0 for a reviewer with no successful run_command. Execution required mechanically.
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

// incompleteCriteria: deterministic completion criteria shared by workIncomplete, foldDeterministic, and continuation prompt.
func incompleteCriteria(task string, act workerActivity, readOnly, isReviewer, existingPR bool) map[string]criterionScore {
	out := map[string]criterionScore{}
	// Read-only agents have no commit/push tools - skip delivery demand.
	if !readOnly {
		if c, ok := deliveryCriterion(task, act, existingPR); ok {
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

// deliveryWording: the task's own delivery terms.
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
