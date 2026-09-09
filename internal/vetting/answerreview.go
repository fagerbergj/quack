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
	// sectionHeaderRe: a tail section header line (FINDINGS:/DISMISSED:/CLEAN:/
	// VERIFIED:/NOTES:), used to bound each section so one header's lines
	// don't bleed into another's.
	sectionHeaderRe = regexp.MustCompile(`(?mi)^\s*(FINDINGS|DISMISSED|CLEAN|VERIFIED|NOTES):\s*$`)
	// cleanLineRe: a CLEAN: section entry, bare path (no line number).
	cleanLineRe = regexp.MustCompile(`(?m)^\s*[-*]\s+(\S+)\s*$`)
	// bulletLineRe: a free-text bullet, VERIFIED:/NOTES: entries (unlike
	// CLEAN's bare path, these carry a full sentence).
	bulletLineRe = regexp.MustCompile(`(?m)^\s*[-*]\s+(.+)$`)
	// takeawayRe: the tail's one-sentence takeaway, same field stage_review's
	// takeaway arg carries - the structured-tail fallback states this as a
	// fact too, never as prose ahead of the tags (one fixed review format).
	takeawayRe = regexp.MustCompile(`(?mi)^\s*TAKEAWAY:\s*(.+)$`)
	// Matches reviewer's fallback preamble (explanation for us, not human reader).
	fallbackPreambleRe = regexp.MustCompile(`(?mi)^.*\bstaging tools?\b.*\bfallback\b.*$\n?`)
)

// AnswerReview: parsed sections of a reviewer's answer tail.
type AnswerReview struct {
	Event     string
	Findings  []ReviewComment
	Dismissed []ReviewComment // DISMISSED: "- path:line: why dropped"
	Clean     []string        // CLEAN: "- path"
	Takeaway  string          // TAKEAWAY: <one sentence>
	Verified  []string        // VERIFIED: "- item"
	Notes     []string        // NOTES: "- item"
	OK        bool
}

// sectionBody returns the text of the named section (up to the next header or
// end of answer), or "" if the header is absent.
func sectionBody(answer, header string) string {
	locs := sectionHeaderRe.FindAllStringSubmatchIndex(answer, -1)
	for i, loc := range locs {
		if !strings.EqualFold(answer[loc[2]:loc[3]], header) {
			continue
		}
		start := loc[1]
		end := len(answer)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		return answer[start:end]
	}
	return ""
}

func parseFindingLines(body string) []ReviewComment {
	var out []ReviewComment
	for _, f := range findingRe.FindAllStringSubmatch(body, -1) {
		line, err := strconv.Atoi(f[2])
		if err != nil || line <= 0 {
			continue
		}
		out = append(out, ReviewComment{Path: f[1], Line: line, Body: strings.TrimSpace(f[3])})
	}
	return out
}

// parseAnswerReview: extracts verdict + findings from reviewer answer. Falls back to comment-review.
// Widened (#1006) to also read DISMISSED:/CLEAN: sections; kept for existing
// callers as a (event, comments, ok) view onto ParseAnswerReviewSections.
func parseAnswerReview(answer string) (event string, comments []ReviewComment, ok bool) {
	r := ParseAnswerReviewSections(answer)
	return r.Event, r.Findings, r.OK
}

// ParseAnswerReviewSections is the section-aware parse (VERDICT/FINDINGS/
// DISMISSED/CLEAN). When a FINDINGS: header is present, findings come only
// from that section; otherwise the unscoped whole-answer scan is the
// fallback (today's behavior, kept so unstructured answers still work).
func ParseAnswerReviewSections(answer string) AnswerReview {
	m := verdictRe.FindStringSubmatch(answer)
	if m == nil {
		return AnswerReview{}
	}
	r := AnswerReview{Event: strings.ToLower(m[1]), OK: true}

	if fb := sectionBody(answer, "FINDINGS"); fb != "" {
		r.Findings = parseFindingLines(fb)
	} else if !sectionHeaderRe.MatchString(answer) {
		// No structured sections at all: unscoped fallback (pre-#1006 behavior).
		r.Findings = parseFindingLines(answer)
	}
	if db := sectionBody(answer, "DISMISSED"); db != "" {
		r.Dismissed = parseFindingLines(db)
	}
	if cb := sectionBody(answer, "CLEAN"); cb != "" {
		for _, m := range cleanLineRe.FindAllStringSubmatch(cb, -1) {
			r.Clean = append(r.Clean, m[1])
		}
	}
	if vb := sectionBody(answer, "VERIFIED"); vb != "" {
		for _, m := range bulletLineRe.FindAllStringSubmatch(vb, -1) {
			r.Verified = append(r.Verified, strings.TrimSpace(m[1]))
		}
	}
	if nb := sectionBody(answer, "NOTES"); nb != "" {
		for _, m := range bulletLineRe.FindAllStringSubmatch(nb, -1) {
			r.Notes = append(r.Notes, strings.TrimSpace(m[1]))
		}
	}
	if tm := takeawayRe.FindStringSubmatch(answer); tm != nil {
		r.Takeaway = strings.TrimSpace(tm[1])
	}
	return r
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
	r := ParseAnswerReviewSections(answer)
	event, comments, ok := r.Event, r.Findings, r.OK
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
		Body:      renderReviewOverview(reviewOverviewInput{Verdict: event, Takeaway: r.Takeaway, Verified: r.Verified, Notes: r.Notes, Comments: comments}),
		Comments:  comments,
		Takeaway:  r.Takeaway,
		Verified:  r.Verified,
		Notes:     r.Notes,
		Recovered: true,
	}
}
