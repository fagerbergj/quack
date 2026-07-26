// A deterministic scan of every ```mermaid fenced block bound for delivery
// (#448): GitHub renders an invalid diagram as a broken box, and nothing
// upstream checks the syntax before it ships. This package used to STRIP an
// invalid block to plain text (#371) - reversed by #448: stripping hides the
// defect instead of fixing it, and the agent that wrote the bad diagram is
// the one who can fix it. So this now only DETECTS and reports; mermaidCriterion
// (checks.go-adjacent, wired in node.go's foldDeterministic) fails the
// deterministic gate with the concrete error, feeding a revise round instead
// of silently degrading. #358 argued a bare "write valid mermaid" instruction
// is exactly what a model ignores intermittently - true, but irrelevant here:
// the feedback below always names the actual parse/validation error and which
// block, which is the actionable instruction #358 didn't have.
package vetting

import (
	"fmt"
	"regexp"
	"strings"

	mermaid "github.com/sammcj/mermaid-check"
)

// fenceOpenRe matches a fence-opening line - up to 3 leading spaces (CommonMark's
// indented-code-block cutoff), 3+ backticks or tildes, then an optional info
// string. Case-insensitive: GitHub renders ```Mermaid / ```MERMAID the same as
// ```mermaid.
var fenceOpenRe = regexp.MustCompile(`(?i)^( {0,3})(` + "`{3,}|~{3,}" + `)[ \t]*(\S*)`)

// mermaidIssue is one invalid top-level ```mermaid block found while scanning
// markdown: the 1-based line number of its fence-open line (so revise
// feedback can point at it in a long answer) and the concrete reason it's
// invalid.
type mermaidIssue struct {
	line int
	err  string
}

// FindInvalidMermaid walks md fence-depth-aware, one line at a time, tracking
// which code fence (if any) is currently open - a ```mermaid opener only
// starts a mermaid block when it's a genuine TOP-LEVEL fence, never one
// merely quoted inside an unrelated fence's body (a ```go block whose content
// contains the literal text "```mermaid" is never descended into). Detection
// only - md is never modified; see the package doc for why.
func FindInvalidMermaid(md string) []mermaidIssue {
	if !strings.Contains(md, "```") && !strings.Contains(md, "~~~") {
		return nil
	}
	lines := strings.Split(md, "\n")
	var issues []mermaidIssue
	walkMermaidBlocks(lines, func(openLine, _ int, reason string) {
		if reason != "" {
			issues = append(issues, mermaidIssue{line: openLine + 1, err: reason})
		}
	})
	return issues
}

// Feedback formats one issue as "line N: reason" - the shape mermaidCriterion
// feeds the gate; github/webhook.go reuses it for the plan/research nudge.
func (i mermaidIssue) Feedback() string {
	return fmt.Sprintf("line %d: %s", i.line, i.err)
}

// walkMermaidBlocks is the one fence-walker shared by FindInvalidMermaid and
// DegradeInvalidMermaid: it visits each genuine top-level ```mermaid block's
// 0-based fence-open/fence-close line indices plus its validation reason ("" if
// valid).
func walkMermaidBlocks(lines []string, visit func(openLine, closeLine int, reason string)) {
	for i := 0; i < len(lines); {
		m := fenceOpenRe.FindStringSubmatch(lines[i])
		if m == nil {
			i++
			continue
		}
		fenceChar, fenceLen, info := m[2][0], len(m[2]), strings.ToLower(m[3])
		close := findFenceClose(lines, i+1, fenceChar, fenceLen)
		if close == -1 {
			// Unterminated fence: everything to EOF is inside it - nothing here is
			// a genuine, closed top-level block worth checking.
			return
		}
		if info == "mermaid" {
			body := strings.Join(lines[i+1:close], "\n")
			visit(i, close, mermaidError(body))
		}
		i = close + 1
	}
}

// DegradeInvalidMermaid is the plan/research path's last-resort ceiling
// (github/webhook.go, after one failed revise): rewrites each still-invalid
// ```mermaid fence into a labeled ```text fence with a visible warning note -
// a visible degradation, not the old silent strip. Returns md unchanged and
// issues=nil when nothing is invalid.
func DegradeInvalidMermaid(md string) (string, []mermaidIssue) {
	if !strings.Contains(md, "```") && !strings.Contains(md, "~~~") {
		return md, nil
	}
	lines := strings.Split(md, "\n")
	var issues []mermaidIssue
	out := make([]string, 0, len(lines))
	last := 0
	walkMermaidBlocks(lines, func(openLine, closeLine int, reason string) {
		if reason == "" {
			return
		}
		issues = append(issues, mermaidIssue{line: openLine + 1, err: reason})
		out = append(out, lines[last:openLine]...)
		m := fenceOpenRe.FindStringSubmatch(lines[openLine])
		out = append(out, fmt.Sprintf("> ⚠️ invalid mermaid diagram (%s) - shown as text, not rendered", reason))
		out = append(out, m[1]+m[2]+"text")
		out = append(out, lines[openLine+1:closeLine+1]...)
		last = closeLine + 1
	})
	if len(issues) == 0 {
		return md, nil
	}
	out = append(out, lines[last:]...)
	return strings.Join(out, "\n"), issues
}

// findFenceClose returns the index of the line that closes a fence opened
// with fenceChar repeated fenceLen+ times, searching from start, or -1 if
// none exists before EOF. A closing line: up to 3 leading spaces, the SAME
// fence character repeated at least fenceLen times, and nothing but
// whitespace after - CommonMark's rule, which is also what stops a fence
// quoted mid-content (e.g. inside a comment, indented, or followed by other
// text) from ever being mistaken for a real close.
func findFenceClose(lines []string, start int, fenceChar byte, fenceLen int) int {
	for j := start; j < len(lines); j++ {
		line := lines[j]
		sp := 0
		for sp < len(line) && sp < 3 && line[sp] == ' ' {
			sp++
		}
		rest := line[sp:]
		n := 0
		for n < len(rest) && rest[n] == fenceChar {
			n++
		}
		if n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
			return j
		}
	}
	return -1
}

// mermaidError returns "" if body is valid mermaid, else a concrete,
// actionable reason it isn't - a real parser/validator error, or the
// supplementary quoted-label rule below. Strict validation, because one of
// the issue's named failure modes - an unquoted label containing parentheses
// - surfaces as a validator warning (NoParenthesesInLabels), not a parse
// error, and a diagram that renders wrong on GitHub is exactly what this
// exists to catch regardless of the severity mermaid-check assigns it.
//
// recover()-guarded: mermaid-check (v0.2.0) still calls itself "not
// production-ready" - a panic here must degrade to "invalid" with a reason,
// never take the gate round down with it.
func mermaidError(body string) (reason string) {
	defer func() {
		if recover() != nil {
			reason = "the mermaid checker could not process this diagram - simplify it"
		}
	}()
	if r := quotedLabelIssue(body); r != "" {
		return r
	}
	diagram, err := mermaid.Parse(body)
	if err != nil {
		return fmt.Sprintf("parse error: %v", err)
	}
	if warnings := mermaid.Validate(diagram, true); len(warnings) > 0 {
		return fmt.Sprintf("validation error: %v", warnings[0])
	}
	return ""
}

// mermaidCriterion is the GATE side of #448: scans the answer plus every
// currently staged delivery body (StagedDelivery.Body - the PR/review/comment
// text about to ship to GitHub) for an invalid ```mermaid block. ok=false
// means nothing invalid was found (matches sufficient_length's convention
// above - a passing check adds no entry to v.Criteria). The first offending
// block's line + reason becomes the Reason, which composeFeedback (node.go)
// folds into the next revise prompt - the model gets the actual parser error
// and the block it came from, not a generic "check your mermaid".
func mermaidCriterion(answer string, act workerActivity) (criterionScore, bool) {
	texts := make([]string, 0, len(act.stagedDelivery)+1)
	texts = append(texts, answer)
	for _, sd := range act.stagedDelivery {
		texts = append(texts, sd.Body)
	}
	for _, t := range texts {
		if issues := FindInvalidMermaid(t); len(issues) > 0 {
			iss := issues[0]
			return criterionScore{Score: 0, Reason: fmt.Sprintf(
				"deterministic: invalid mermaid diagram at line %d: %s", iss.line, iss.err)}, true
		}
	}
	return criterionScore{}, false
}

// mermaidLabelRe finds a single-level bracket/paren/brace node label
// ([...], (...), {...}) - the label text itself is not allowed to contain
// another one of these delimiters, which is enough to isolate each label
// without a real parse.
var mermaidLabelRe = regexp.MustCompile(`[\[({]([^\[\]{}()\n]*)[\])}]`)

// quotedLabelIssue is the supplementary check mermaid-check still misses as
// of v0.2.0 (#448, re-verified on the bump to v0.2.0): a bracket/paren/brace
// node label containing a bare double-quote that is NOT the whole label
// quoted (mermaid's own escape form, A["label with \"quotes\""]) breaks
// GitHub's real mermaid.js parser even though mermaid-check parses and
// strictly validates it clean - verified empirically against the issue's own
// example (A[bundle name<br/>e.g. "code-reviewer"]).
func quotedLabelIssue(body string) string {
	for _, m := range mermaidLabelRe.FindAllStringSubmatch(body, -1) {
		label := m[1]
		if !strings.Contains(label, `"`) {
			continue
		}
		trimmed := strings.TrimSpace(label)
		if len(trimmed) >= 2 && strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) {
			continue // fully-quoted label - mermaid's own escape form, valid
		}
		return fmt.Sprintf(
			`label %q has a double-quote inside an unquoted bracket/paren/brace label - GitHub's parser rejects this; wrap the WHOLE label in double quotes and escape inner quotes (e.g. ["label with \"quotes\""]) or remove the quotes`,
			label)
	}
	return ""
}
