// answerreview.go: recovers staged review from ACP reviewer's answer tail (VERDICT/FINDINGS).
package vetting

import (
	"log/slog"
	"regexp"
	"strconv"
	"strings"
)

var (
	verdictRe = regexp.MustCompile(`(?mi)^\s*VERDICT:\s*(approve|request_changes|comment)\s*$`)
	findingRe = regexp.MustCompile(`(?m)^\s*[-*]\s+([^\s:]+):(\d+):\s*(.+)$`)
	// Matches reviewer's fallback preamble (explanation for us, not human reader).
	fallbackPreambleRe = regexp.MustCompile(`(?mi)^.*\bstaging tools?\b.*\bfallback\b.*$\n?`)
)

// parseAnswerReview: extracts verdict + findings from reviewer answer. Falls back to comment-review.
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

// StripVerdictTail: removes machine-parseable tail for human-facing text.
func StripVerdictTail(answer string) string {
	s := fallbackPreambleRe.ReplaceAllString(answer, "")
	if loc := verdictRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	return strings.TrimSpace(s)
}

// augmentFromReviewStage: folds tool-staged review into activity (runs before augmentFromAnswer, Snapshot - non-clearing).
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

// augmentFromPRStage folds a stage_pr/stage_push-staged PR over augmentFromRepo's fallback (keeps the disk-probe branch).
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

// augmentFromAnswer stages an external reviewer's answer as its review. Fills gaps, never replaces.
// A verdict-less answer stages a plain comment-review.
func augmentFromAnswer(act *workerActivity, cfg Config, answer string) {
	if !cfg.ExternalWorker || strings.TrimSpace(answer) == "" {
		return
	}
	// Only read-only nodes are reviewers (belt-and-suspenders alongside IsReviewer).
	if !cfg.ReadOnly {
		return
	}
	// No provisioned clone/PR - nothing to review against.
	if cfg.Setup == nil {
		return
	}
	// IsReviewer is stamped from the node's AGENT (dag.reviewerAgent), not the task's wording.
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
	// Loud: review MCP surface was not used this round.
	slog.Warn("review recovered from the answer's VERDICT/FINDINGS tail, not staged via the review MCP tools",
		"component", "vetting", "node", cfg.NodeID)
	act.stagedDelivery["review"] = StagedDelivery{
		Kind:      "review",
		Event:     event,
		Body:      answer,
		Comments:  comments,
		Recovered: true,
	}
}
