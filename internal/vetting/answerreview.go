// The answer-derived review probe: an EXTERNAL (ACP) reviewer has no
// stage_review tool, so its deliverable - the posted review - must come out of
// its ANSWER. The reviewer's preamble (agents/code-reviewer/prompt.md)
// instructs a structured tail:
//
//	VERDICT: approve | request_changes | comment
//	FINDINGS:
//	- path/to/file.go:42: the finding text
//	- other/file.ts:7: another finding
//
// augmentFromAnswer parses that into a staged review (with line-anchored
// inline comments) exactly as if the worker had called stage_review - the
// delivery spine (commitDelivery → github Deliver) stays gate-owned and
// unchanged. The companion of the git disk probe (gitprobe.go).
package vetting

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	verdictRe = regexp.MustCompile(`(?mi)^\s*VERDICT:\s*(approve|request_changes|comment)\s*$`)
	findingRe = regexp.MustCompile(`(?m)^\s*[-*]\s+([^\s:]+):(\d+):\s*(.+)$`)
	// fallbackPreambleRe matches a reviewer's own aside about falling back to the
	// VERDICT/FINDINGS tail (agents/code-reviewer/prompt.md's fallback path) -
	// meant to explain the tail to us, never to a human reader.
	fallbackPreambleRe = regexp.MustCompile(`(?mi)^.*\bstaging tools?\b.*\bfallback\b.*$\n?`)
)

// parseAnswerReview extracts the structured verdict and inline findings from a
// reviewer's answer. ok is false when no VERDICT line exists - the caller then
// falls back to a plain comment-review of the whole answer.
func parseAnswerReview(answer string) (event string, comments []ReviewComment, ok bool) {
	m := verdictRe.FindStringSubmatch(answer)
	if m == nil {
		return "", nil, false
	}
	event = strings.ToLower(m[1])
	for _, f := range findingRe.FindAllStringSubmatch(answer, -1) {
		line, err := strconv.Atoi(f[2])
		if err != nil || line <= 0 {
			continue
		}
		comments = append(comments, ReviewComment{Path: f[1], Line: line, Body: strings.TrimSpace(f[3])})
	}
	return event, comments, true
}

// StripVerdictTail returns answer with the machine-parseable VERDICT/FINDINGS
// tail (see parseAnswerReview) and any fallback-format preamble removed - the
// clean, human-facing text for a review posted as a plain comment (the own-PR
// path, deliverOne in internal/github/tools.go, which can't post a formal
// verdict at all and must not leak the parser's tail to the reader).
func StripVerdictTail(answer string) string {
	s := fallbackPreambleRe.ReplaceAllString(answer, "")
	if loc := verdictRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	return strings.TrimSpace(s)
}

// augmentFromReviewStage folds an external reviewer's TOOL-staged review - the
// review MCP surface's stage_review_comment/stage_review calls (internal/acp) -
// into the activity, resolved via the node's advisor-thread token → MemSecret →
// MemSession.Review. It runs BEFORE augmentFromAnswer so a tool-staged review
// always beats the answer-tail parse; augmentFromAnswer's own "already staged"
// guard then makes the fallback a no-op. A Snapshot (non-clearing): it fires on
// every gate round and stays readable until the node's session is drained.
func augmentFromReviewStage(act *workerActivity, advisorToken string) {
	if advisorToken == "" {
		return
	}
	t, ok := LookupAdvisorThread(advisorToken)
	if !ok || t.MemSecret == "" {
		return
	}
	ms, ok := LookupMemSession(t.MemSecret)
	if !ok || ms.Review == nil {
		return
	}
	sd, ok := ms.Review.Snapshot()
	if !ok {
		return
	}
	if act.stagedDelivery == nil {
		act.stagedDelivery = map[string]StagedDelivery{}
	}
	act.stagedDelivery["review"] = sd
}

// augmentFromPRStage folds a stage_pr-staged PR (the implementer authored the
// title+body via the pr-authoring skill) OVER augmentFromRepo's commit-subject
// fallback, keeping the branch the disk probe resolved (the worker authored only
// text). No stage_pr call ⇒ Snapshot ok is false ⇒ no-op, and the fallback
// stands. Runs in actFor, after augmentFromRepo has staged the fallback.
func augmentFromPRStage(act *workerActivity, advisorToken string) {
	if advisorToken == "" {
		return
	}
	t, ok := LookupAdvisorThread(advisorToken)
	if !ok || t.MemSecret == "" {
		return
	}
	ms, ok := LookupMemSession(t.MemSecret)
	if !ok || ms.PRStage == nil {
		return
	}
	sd, ok := ms.PRStage.Snapshot()
	if !ok {
		return
	}
	if act.stagedDelivery == nil {
		act.stagedDelivery = map[string]StagedDelivery{}
	}
	if cur, ok := act.stagedDelivery["pr"]; ok {
		sd.Branch = cur.Branch
	}
	act.stagedDelivery["pr"] = sd
}

// augmentFromAnswer stages an external reviewer's answer as its review. Fires
// only for an ACP-backed code-reviewer node (cfg.IsReviewer), and only when
// nothing is staged yet (a worker-staged review always wins - there isn't one
// on the ACP path, but the guard keeps this probe monotonic like the git probe:
// it fills gaps, never replaces).
//
// A verdict-less answer still stages a plain comment-review: an honest wall of
// findings without the structured tail must post as a comment rather than
// deadlock the node in continuation rounds it can never satisfy.
func augmentFromAnswer(act *workerActivity, cfg Config, answer string) {
	if !cfg.ExternalWorker || strings.TrimSpace(answer) == "" {
		return
	}
	// Only a READ-ONLY node can BE the code-reviewer this probe is for - belt and
	// suspenders alongside cfg.IsReviewer below (an implementer synthesizes a PR,
	// a reviewer does not; a reviewer synthesizes a review, an implementer does
	// not - see augmentFromRepo's ReadOnly guard).
	if !cfg.ReadOnly {
		return
	}
	// No provisioned clone/PR ⇒ nothing to review against: a read-only node whose
	// TASK merely mentions reviews (e.g. a code-explorer investigating the review
	// path on an ISSUE) would otherwise stage a review that delivery then can't
	// post - "'' is not a github.com clone URL".
	if cfg.Setup == nil {
		return
	}
	// The structural signal (Config.IsReviewer, stamped from the node's AGENT -
	// dag.reviewerAgent - not the task's wording): a task-text regex left this
	// path dead for a task with no posting verb, e.g. the label-review default
	// "Review this pull request." (#482). Also the guard that actually excludes
	// code-explorer now that it's setup-provisioned too (dag.setupQualifyingAgent)
	// - explorer answers never get this staged-review treatment.
	if !cfg.IsReviewer {
		return
	}
	if _, staged := act.stagedDelivery["review"]; staged {
		return
	}
	event, comments, ok := parseAnswerReview(answer)
	if !ok {
		event = "comment"
	}
	if act.stagedDelivery == nil {
		act.stagedDelivery = map[string]StagedDelivery{}
	}
	act.stagedDelivery["review"] = StagedDelivery{
		Kind:     "review",
		Event:    event,
		Body:     answer,
		Comments: comments,
	}
}
