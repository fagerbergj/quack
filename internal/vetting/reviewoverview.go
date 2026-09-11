// reviewoverview.go: the one fixed format every review overview renders through - sections 2-8 of the design (section 1, the trust-gate banner,
// and section 9, the merge outcome line, are both owned by the delivering extension and prepended/appended around this renderer's output). Two callers share it: renderReviewFromArtifact (deliveryartifact.go, full
// scope info from the durable record + a git probe) and the degraded fallbacks - ReviewStage.Snapshot (advisor_thread.go) and the answer-tail recovery path (answerreview.go) - which have no scope/history to report and simply omit those sections.
package vetting

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// reviewOverviewInput bundles everything the renderer needs. Every section
// beyond the verdict line degrades gracefully to omitted when its inputs
// are unknown - only the artifact-backed render (which has cfg to shell out to git and read prior rounds) fills in Scope/Since-last-review.
type reviewOverviewInput struct {
	Verdict string // approve | request_changes | comment

	// ScopeKnown gates the whole Scope line (section 3) - unset for every
	// caller but the artifact-backed render, which resolves it via a git probe.
	ScopeKnown   bool
	HeadSHA      string // full or short sha; "" omits "· head <sha7>"
	FirstReview  bool
	PriorHeadSHA string // re-review only; "" omits the whole "since <sha7>" clause

	// CommitsSinceKnown gates the "N commits since" clause independently of
	// PriorHeadSHA - a re-review still names the prior head even when
	// rev-list couldn't resolve a commit count (force-pushed away), but never claims "0 commits" when the count is actually unknown.
	CommitsSinceKnown bool
	CommitsSince      int
	FileCount         int // 0 omits the "(N files)" parenthetical

	Takeaway      string
	Verified      []string
	Notes         []string
	LegacySummary string // pre-migration record's summary, rendered under Notes (truncated)

	// Comments: live findings in staged order, Body starting with the Conventional-Comments label (blocking:/suggestion:/nit:/question:,
	// any of the emoji/bold variants) - source for the verdict line's
	// counts and the Highlights table. Never re-listed verbatim: findings live inline, only their label/first-sentence surface here.
	Comments []ReviewComment

	// SinceKnown gates section 5 (re-review only).
	SinceKnown bool
	Resolved   int
	Open       int
	Dismissed  []DismissedEntry
}

// escapeTableCell escapes a Markdown table delimiter so a finding's title,
// rationale, or path - free text this renderer didn't author - can never
// break the Highlights row it lands in.
func escapeTableCell(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

// reviewLabelOrder: verdict-line counts and Highlights precedence, always in
// this order - deterministic output, never a map iteration.
var reviewLabelOrder = []string{"blocking", "suggestion", "nit", "question"}

// commentLabelRe matches a Conventional-Comments label at the start of a finding's body: an optional short emoji/symbol prefix, optional bold markdown wrapping the label AND its colon (the bold closes right after
// the colon, not before it - "**blocking:**", not "**blocking**:"). Variants seen in the wild: "🚨 **blocking:**", "**blocking:**", "blocking:"
// - also a decoration like "blocking (security):", which still matches on the bare label. The prefix class excludes '*' so a leading emoji can never swallow the bold markers meant for \*{0,2}.
var commentLabelRe = regexp.MustCompile(`(?i)^\s*[^\pL\pN*]{0,4}\*{0,2}(blocking|suggestion|nit|question)\b[^:]*:\*{0,2}`)

// metaNarrationRe matches a takeaway/note/summary paragraph that narrates
// the review's own staging call (a revision number, its own inline-comment
// tally, "staged for PR #N") instead of the code under review. This only
// leaks in when a caller falls back to its raw chat reply instead of its
// structured takeaway/verified/notes fields - rejected here, the one
// renderer every review body goes through, rather than left to a prompt.
var metaNarrationRe = regexp.MustCompile(`(?i)\bstaged for (this )?(pr|pull request)\b|\bcode_review revision\b|\d+\s+inline comments?\s*\+\s*\d+\s+summary notes?`)

// sentenceAbbrevRe matches a trailing abbreviation (e.g/i.e/vs) right before
// a candidate sentence-ending period - firstSentence skips the period there
// rather than treating the abbreviation as the sentence's end.
var sentenceAbbrevRe = regexp.MustCompile(`(?i)\b(e\.g|i\.e|vs)$`)

// firstSentence extracts the Highlights table's "why" cell: the text up to (not including) the first sentence-ending period, or the whole string if none is found. A period only ends a sentence when it is followed by
// whitespace or the end of the string (so "cfg.Setup", "router.go:42", and
// a mid-sentence URL never truncate early - none of those periods are followed by a space), is not inside a backtick span, and doesn't close a known abbreviation (e.g./i.e./vs.). A newline always ends it, matching a finding body's own "one line, one finding" shape.
func firstSentence(s string) string {
	inBacktick := false
	for i, r := range s {
		switch r {
		case '`':
			inBacktick = !inBacktick
		case '\n':
			if !inBacktick {
				return strings.TrimSpace(s[:i])
			}
		case '.':
			if inBacktick {
				continue
			}
			if next := i + 1; next < len(s) {
				if nr := s[next]; nr != ' ' && nr != '\t' && nr != '\n' {
					continue
				}
			}
			if sentenceAbbrevRe.MatchString(s[:i]) {
				continue
			}
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

// commentLabel splits a finding's body into its Conventional-Comments label
// (lowercase, "" if none matched) and the first sentence after it - the Highlights table's "why". Falls back to "" (the caller substitutes the
// finding's path) when the label leaves nothing behind - a label-only body with no explanation.
func commentLabel(body string) (label, why string) {
	line := body
	rest := ""
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		line, rest = body[:i], body[i+1:]
	}
	m := commentLabelRe.FindStringSubmatchIndex(line)
	if m == nil {
		return "", firstSentence(body)
	}
	label = strings.ToLower(line[m[2]:m[3]])
	remainder := strings.TrimSpace(line[m[1]:])
	if remainder == "" {
		remainder = strings.TrimSpace(rest)
	}
	return label, firstSentence(remainder)
}

// HasCommentLabel reports whether body opens with a Conventional Comments
// label - the check every stage_review_comment call must pass, single
// reviewer or fan-out slice alike, so a finding always counts toward the
// verdict line and is eligible for the Highlights table.
func HasCommentLabel(body string) bool {
	label, _ := commentLabel(body)
	return label != ""
}

// dedupeComments drops a repeat of the same finding: FindingIDs equal, or
// one side has none and path+line+label all match - never on line alone.
func dedupeComments(comments []ReviewComment) []ReviewComment {
	out := make([]ReviewComment, 0, len(comments))
	for _, c := range comments {
		label, _ := commentLabel(c.Body)
		dup := false
		for _, seen := range out {
			if c.FindingID != "" && seen.FindingID != "" {
				if c.FindingID == seen.FindingID {
					dup = true
					break
				}
				continue
			}
			seenLabel, _ := commentLabel(seen.Body)
			if c.Path == seen.Path && c.Line == seen.Line && label == seenLabel {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

// countLabels tallies comments by Conventional-Comments label.
func countLabels(comments []ReviewComment) map[string]int {
	counts := make(map[string]int, len(reviewLabelOrder))
	for _, c := range comments {
		if label, _ := commentLabel(c.Body); label != "" {
			counts[label]++
		}
	}
	return counts
}

// pluralize: "1 file" vs "2 files" - the label word itself never pluralizes
// ("blocking" is a category, not a countable noun).
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func pluralizeLabel(label string, n int) string {
	if label == "blocking" || n == 1 {
		return label
	}
	return label + "s"
}

// sha7: the short sha the design's Scope/Verdict lines show.
func sha7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// truncateRunes caps s at n runes, backing up to the nearest preceding
// space so a review overview never ends mid-word, then marks the cut with
// "…". Falls back to a hard cut only when the truncated span has no space
// to back up to at all.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := n
	for cut > 0 && r[cut-1] != ' ' && r[cut-1] != '\n' {
		cut--
	}
	if cut == 0 {
		cut = n
	}
	return strings.TrimRight(string(r[:cut]), " \n") + "…"
}

// stripMetaNarration drops any paragraph of s that reads as the review's
// own staging narration rather than review content (see metaNarrationRe).
func stripMetaNarration(s string) string {
	paras := strings.Split(s, "\n\n")
	kept := paras[:0]
	for _, p := range paras {
		if !metaNarrationRe.MatchString(p) {
			kept = append(kept, p)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n\n"))
}

// legacySummaryDisplayCap: how much of a pre-migration record's free-text
// summary to show under Notes - just enough that history still reads, not a
// return to 1,000+ character run-ons.
const legacySummaryDisplayCap = 320

// renderReviewOverview produces sections 2-8 of the fixed review format (see
// this file's doc comment). Never returns an empty string for a non-empty verdict: the verdict line (section 2) always renders. Built as a slice of
// self-contained blocks joined with a single blank line, rather than ad-hoc "\n\n" concatenation, so an empty section can never leave a stray blank line behind (the bug a prior version had between Verified and Notes).
func renderReviewOverview(in reviewOverviewInput) string {
	var sections []string
	comments := dedupeComments(in.Comments)

	// Section 2: verdict line.
	verdictWord := in.Verdict
	switch in.Verdict {
	case "request_changes":
		verdictWord = "request changes"
	}
	parts := []string{"**Verdict: " + verdictWord + "**"}
	counts := countLabels(comments)
	for _, label := range reviewLabelOrder {
		if n := counts[label]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, pluralizeLabel(label, n)))
		}
	}
	if in.HeadSHA != "" {
		parts = append(parts, "head "+sha7(in.HeadSHA))
	}
	sections = append(sections, strings.Join(parts, " · "))

	// Section 3: scope line.
	if in.ScopeKnown {
		var scope string
		switch {
		case in.FirstReview:
			scope = "Scope: first review, whole PR"
		case in.PriorHeadSHA != "" && in.CommitsSinceKnown:
			scope = fmt.Sprintf("Scope: re-review, %d %s since %s",
				in.CommitsSince, pluralize(in.CommitsSince, "commit", "commits"), sha7(in.PriorHeadSHA))
		default:
			// Prior head unknown, or rev-list couldn't resolve a commit
			// count (force-pushed away) - never claim "0 commits since" when
			// the count is actually unknown.
			scope = "Scope: re-review"
		}
		if in.FileCount > 0 {
			scope += fmt.Sprintf(" (%d %s)", in.FileCount, pluralize(in.FileCount, "file", "files"))
		}
		sections = append(sections, scope)
	}

	// Section 4: takeaway.
	if t := strings.TrimSpace(in.Takeaway); t != "" && !metaNarrationRe.MatchString(t) {
		sections = append(sections, t)
	}

	// Section 5: since last review (re-review only).
	if in.SinceKnown {
		since := fmt.Sprintf("Since last review: %d resolved · %d open · %d dismissed",
			in.Resolved, in.Open, len(in.Dismissed))
		if len(in.Dismissed) > 0 {
			reasons := make([]string, len(in.Dismissed))
			for i, d := range in.Dismissed {
				reasons[i] = fmt.Sprintf("%s:%d: %s", d.Path, d.Line, d.Note)
			}
			since += " (" + strings.Join(reasons, "; ") + ")"
		}
		sections = append(sections, since)
	}

	// Section 6: highlights table - every blocking finding, else the top 2
	// suggestions, in staged order. Omitted when there's neither.
	var rows []ReviewComment
	for _, c := range comments {
		if label, _ := commentLabel(c.Body); label == "blocking" {
			rows = append(rows, c)
		}
	}
	if len(rows) == 0 {
		for _, c := range comments {
			if label, _ := commentLabel(c.Body); label == "suggestion" {
				rows = append(rows, c)
				if len(rows) == 2 {
					break
				}
			}
		}
	}
	if len(rows) > 0 {
		var b strings.Builder
		b.WriteString("### Highlights\n\n| Severity | Where | Why it matters |\n| --- | --- | --- |")
		for _, c := range rows {
			label, why := commentLabel(c.Body)
			// A label-only body (no explanation at all) falls back to the
			// finding's own path rather than an empty cell.
			if why == "" {
				why = c.Path
			}
			fmt.Fprintf(&b, "\n| %s | %s | %s |", escapeTableCell(label), escapeTableCell(fmt.Sprintf("%s:%d", c.Path, c.Line)), escapeTableCell(why))
		}
		sections = append(sections, b.String())
	}

	// Section 7: verified.
	if len(in.Verified) > 0 {
		items := make([]string, len(in.Verified))
		for i, v := range in.Verified {
			items[i] = "- " + v
		}
		sections = append(sections, "### Verified\n\n"+strings.Join(items, "\n"))
	}

	// Section 8: notes - the only free prose, plus a truncated legacy
	// summary (pre-migration records that never had takeaway/verified/notes).
	var notes []string
	for _, n := range in.Notes {
		if !metaNarrationRe.MatchString(n) {
			notes = append(notes, n)
		}
	}
	if ls := stripMetaNarration(strings.TrimSpace(in.LegacySummary)); ls != "" {
		notes = append([]string{truncateRunes(ls, legacySummaryDisplayCap)}, notes...)
	}
	if len(notes) > 0 {
		items := make([]string, len(notes))
		for i, n := range notes {
			items[i] = "- " + n
		}
		sections = append(sections, "### Notes\n\n"+strings.Join(items, "\n"))
	}

	return strings.Join(sections, "\n\n")
}

// reviewScope resolves one review's diff scope against the same base/head diffSince uses (the diff this review is actually reviewing): the current
// head and the file count for the Scope line, plus a resolver for "commits
// since a prior head" on a re-review. ok=false when there's no clone to resolve - the caller just omits the Scope line rather than guessing.
type reviewScope struct {
	dir       string
	caps      workspace.Caps
	head      string
	fileCount int
	ok        bool
}

// resolveReviewScope resolves the review's clone the same way diffSince does (cfg.NodeBaseSHA, falling back to the reflog's oldest HEAD) - see
// diffSince/buildReviewDiffSection in gitprobe.go, the established source of
// "the diff this review is OF". ok=false (zero value) when there's no clone yet, or its base/head can't be resolved.
func resolveReviewScope(cfg Config) reviewScope {
	if cfg.Setup == nil || cfg.Workspace == nil {
		return reviewScope{}
	}
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil || !isDir(filepath.Join(dir, ".git")) {
		return reviewScope{}
	}
	caps := checksCaps(cfg)
	base := cfg.NodeBaseSHA
	if base == "" {
		if base, err = baseCommit(dir, caps); err != nil {
			return reviewScope{}
		}
	}
	head := gitLine(dir, caps, "rev-parse", "HEAD")
	if head == "" {
		return reviewScope{}
	}
	return reviewScope{
		dir: dir, caps: caps, head: head, ok: true,
		fileCount: len(gitLines(dir, caps, "diff", "--name-only", base, head)),
	}
}

// commitsSince counts commits between a prior review's head and this one -
// 0/false when priorHead is empty or unresolvable (e.g. force-pushed away).
func (s reviewScope) commitsSince(priorHead string) (int, bool) {
	if !s.ok || priorHead == "" {
		return 0, false
	}
	out := gitLine(s.dir, s.caps, "rev-list", "--count", priorHead+".."+s.head)
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, false
	}
	return n, true
}
