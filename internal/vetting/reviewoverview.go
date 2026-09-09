// reviewoverview.go: the one fixed format every review overview renders
// through - sections 2-8 of the design (section 1, the trust-gate banner,
// and section 9, the merge outcome line, are both owned by the delivering
// extension and prepended/appended around this renderer's output). Two
// callers share it: renderReviewFromArtifact (deliveryartifact.go, full
// scope info from the durable record + a git probe) and the degraded
// fallbacks - ReviewStage.Snapshot (advisor_thread.go) and the answer-tail
// recovery path (answerreview.go) - which have no scope/history to report
// and simply omit those sections.
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
// are unknown - only the artifact-backed render (which has cfg to shell out
// to git and read prior rounds) fills in Scope/Since-last-review.
type reviewOverviewInput struct {
	Verdict string // approve | request_changes | comment

	// ScopeKnown gates the whole Scope line (section 3) - unset for every
	// caller but the artifact-backed render, which resolves it via a git probe.
	ScopeKnown   bool
	HeadSHA      string // full or short sha; "" omits "· head <sha7>"
	FirstReview  bool
	PriorHeadSHA string // re-review only; "" omits the "since <sha7>" clause
	CommitsSince int
	FileCount int // 0 omits the "(N files)" parenthetical

	Takeaway      string
	Verified      []string
	Notes         []string
	LegacySummary string // pre-migration record's summary, rendered under Notes (truncated)

	// Comments: live findings in staged order, Body starting with the
	// Conventional-Comments label (blocking:/suggestion:/nit:/question:,
	// any of the emoji/bold variants) - source for the verdict line's
	// counts and the Highlights table. Never re-listed verbatim: findings
	// live inline, only their label/first-sentence surface here.
	Comments []ReviewComment

	// SinceKnown gates section 5 (re-review only).
	SinceKnown bool
	Resolved   int
	Open       int
	Dismissed  []DismissedEntry
}

// reviewLabelOrder: verdict-line counts and Highlights precedence, always in
// this order - deterministic output, never a map iteration.
var reviewLabelOrder = []string{"blocking", "suggestion", "nit", "question"}

// commentLabelRe matches a Conventional-Comments label at the start of a
// finding's body: an optional short emoji/symbol prefix, optional bold
// markdown wrapping the label AND its colon (the bold closes right after
// the colon, not before it - "**blocking:**", not "**blocking**:").
// Variants seen in the wild: "🚨 **blocking:**", "**blocking:**", "blocking:"
// - also a decoration like "blocking (security):", which still matches on
// the bare label. The prefix class excludes '*' so a leading emoji can
// never swallow the bold markers meant for \*{0,2}.
var commentLabelRe = regexp.MustCompile(`(?i)^\s*[^\pL\pN*]{0,4}\*{0,2}(blocking|suggestion|nit|question)\b[^:]*:\*{0,2}`)

// commentLabel splits a finding's body into its Conventional-Comments label
// (lowercase, "" if none matched) and the first sentence after it - the
// Highlights table's "why", per splitFirstSentence's own rule.
func commentLabel(body string) (label, why string) {
	line := body
	rest := ""
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		line, rest = body[:i], body[i+1:]
	}
	m := commentLabelRe.FindStringSubmatchIndex(line)
	if m == nil {
		title, _ := splitFirstSentence(body)
		return "", title
	}
	label = strings.ToLower(line[m[2]:m[3]])
	remainder := strings.TrimSpace(line[m[1]:])
	if remainder == "" {
		remainder = strings.TrimSpace(rest)
	}
	why, _ = splitFirstSentence(remainder)
	return label, why
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

// truncateRunes caps s at n runes, marking truncation with "…".
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// legacySummaryDisplayCap: how much of a pre-migration record's free-text
// summary to show under Notes - just enough that history still reads, not a
// return to 1,000+ character run-ons.
const legacySummaryDisplayCap = 320

// renderReviewOverview produces sections 2-8 of the fixed review format (see
// this file's doc comment). Never returns an empty string for a non-empty
// verdict: the verdict line (section 2) always renders.
func renderReviewOverview(in reviewOverviewInput) string {
	var sb strings.Builder

	// Section 2: verdict line.
	verdictWord := in.Verdict
	switch in.Verdict {
	case "approve":
		verdictWord = "approve"
	case "request_changes":
		verdictWord = "request changes"
	case "comment":
		verdictWord = "comment"
	}
	parts := []string{"**Verdict: " + verdictWord + "**"}
	counts := countLabels(in.Comments)
	for _, label := range reviewLabelOrder {
		if n := counts[label]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, pluralizeLabel(label, n)))
		}
	}
	if in.HeadSHA != "" {
		parts = append(parts, "head "+sha7(in.HeadSHA))
	}
	sb.WriteString(strings.Join(parts, " · "))

	// Section 3: scope line.
	if in.ScopeKnown {
		var scope string
		if in.FirstReview {
			scope = "Scope: first review, whole PR"
		} else if in.PriorHeadSHA != "" {
			scope = fmt.Sprintf("Scope: re-review, %d %s since %s",
				in.CommitsSince, pluralize(in.CommitsSince, "commit", "commits"), sha7(in.PriorHeadSHA))
		} else {
			scope = "Scope: re-review"
		}
		if in.FileCount > 0 {
			scope += fmt.Sprintf(" (%d %s)", in.FileCount, pluralize(in.FileCount, "file", "files"))
		}
		sb.WriteString("\n\n" + scope)
	}

	// Section 4: takeaway.
	if t := strings.TrimSpace(in.Takeaway); t != "" {
		sb.WriteString("\n\n" + t)
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
		sb.WriteString("\n\n" + since)
	}

	// Section 6: highlights table - every blocking finding, else the top 2
	// suggestions, in staged order. Omitted when there's neither.
	var rows []ReviewComment
	for _, c := range in.Comments {
		if label, _ := commentLabel(c.Body); label == "blocking" {
			rows = append(rows, c)
		}
	}
	if len(rows) == 0 {
		for _, c := range in.Comments {
			if label, _ := commentLabel(c.Body); label == "suggestion" {
				rows = append(rows, c)
				if len(rows) == 2 {
					break
				}
			}
		}
	}
	if len(rows) > 0 {
		sb.WriteString("\n\n### Highlights\n\n| Severity | Where | Why it matters |\n| --- | --- | --- |\n")
		for _, c := range rows {
			label, why := commentLabel(c.Body)
			sb.WriteString(fmt.Sprintf("| %s | %s:%d | %s |\n", label, c.Path, c.Line, why))
		}
	}

	// Section 7: verified.
	if len(in.Verified) > 0 {
		sb.WriteString("\n\n### Verified\n\n")
		for _, v := range in.Verified {
			sb.WriteString("- " + v + "\n")
		}
	}

	// Section 8: notes - the only free prose, plus a truncated legacy
	// summary (pre-migration records that never had takeaway/verified/notes).
	notes := in.Notes
	if ls := strings.TrimSpace(in.LegacySummary); ls != "" {
		notes = append([]string{truncateRunes(ls, legacySummaryDisplayCap)}, notes...)
	}
	if len(notes) > 0 {
		sb.WriteString("\n\n### Notes\n\n")
		for _, n := range notes {
			sb.WriteString("- " + n + "\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// reviewScope resolves one review's diff scope against the same base/head
// diffSince uses (the diff this review is actually reviewing): the current
// head and the file count for the Scope line, plus a resolver for "commits
// since a prior head" on a re-review. ok=false when there's no clone to
// resolve - the caller just omits the Scope line rather than guessing.
type reviewScope struct {
	dir       string
	caps      workspace.Caps
	head      string
	fileCount int
	ok        bool
}

// resolveReviewScope resolves the review's clone the same way diffSince does
// (cfg.NodeBaseSHA, falling back to the reflog's oldest HEAD) - see
// diffSince/buildReviewDiffSection in gitprobe.go, the established source of
// "the diff this review is OF". ok=false (zero value) when there's no clone
// yet, or its base/head can't be resolved.
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
