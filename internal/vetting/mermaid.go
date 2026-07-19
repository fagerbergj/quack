// A deterministic pass over every ```mermaid fenced block in delivered
// markdown (#371): GitHub renders an invalid diagram as a blank/broken box,
// and nothing upstream checks the syntax before it ships. Runs as part of
// commitDelivery, mutating each StagedDelivery.Body in place — mechanical, so
// it never needs a revise round the model can't reliably satisfy anyway
// (#358's buffer-reset reasoning: a "please write valid mermaid" instruction
// is exactly the kind of thing a model ignores intermittently).
package vetting

import (
	"regexp"
	"strings"

	mermaid "github.com/sammcj/mermaid-check"
)

// fenceOpenRe matches a fence-opening line — up to 3 leading spaces (CommonMark's
// indented-code-block cutoff), 3+ backticks or tildes, then an optional info
// string. Case-insensitive: GitHub renders ```Mermaid / ```MERMAID the same as
// ```mermaid.
var fenceOpenRe = regexp.MustCompile(`(?i)^( {0,3})(` + "`{3,}|~{3,}" + `)[ \t]*(\S*)`)

// ValidateAndRepairMermaid walks md fence-depth-aware, one line at a time,
// tracking which code fence (if any) is currently open — a ```mermaid opener
// only starts a mermaid block when it's a genuine TOP-LEVEL fence, never one
// merely quoted inside an unrelated fence's body (a ```go block whose content
// contains the literal text "```mermaid" must be left byte-for-byte alone).
// Any block that fails to parse, or fails strict validation, is replaced with
// a plain-text fallback (the raw source in a plain code fence, with a note) —
// never auto-repaired: mermaid-check's errors come from a real parser, not a
// guess, so there's nothing to mechanically patch with confidence. changed
// reports whether md was modified.
//
// Strip, not fail-the-gate: delivery must never block on a diagram, and the
// gate has no reliable way to make a model fix mermaid syntax on a revise
// round (see the package doc above) — a stripped diagram degrades to
// readable text instead of sinking a round the model can't win.
func ValidateAndRepairMermaid(md string) (out string, changed bool) {
	if !strings.Contains(md, "```") && !strings.Contains(md, "~~~") {
		return md, false
	}
	lines := strings.Split(md, "\n")
	result := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		m := fenceOpenRe.FindStringSubmatch(lines[i])
		if m == nil {
			result = append(result, lines[i])
			i++
			continue
		}
		fenceChar, fenceLen, info := m[2][0], len(m[2]), strings.ToLower(m[3])
		close := findFenceClose(lines, i+1, fenceChar, fenceLen)
		if close == -1 {
			// Unterminated fence: everything to EOF is inside it — copy through
			// untouched (nothing here is a genuine, closed top-level block).
			result = append(result, lines[i:]...)
			return strings.Join(result, "\n"), changed
		}
		if info == "mermaid" {
			body := strings.Join(lines[i+1:close], "\n")
			if mermaidValid(body) {
				result = append(result, lines[i:close+1]...)
			} else {
				changed = true
				result = append(result, "_[diagram omitted: invalid mermaid syntax]_", "", "```text")
				result = append(result, lines[i+1:close]...)
				result = append(result, "```")
			}
		} else {
			// A non-mermaid fence (or a mermaid-looking one nested inside another
			// fence — findFenceClose already consumed straight to ITS OWN close,
			// so nothing inside was inspected as a potential opener) — copied
			// through verbatim, never descended into.
			result = append(result, lines[i:close+1]...)
		}
		i = close + 1
	}
	return strings.Join(result, "\n"), changed
}

// findFenceClose returns the index of the line that closes a fence opened
// with fenceChar repeated fenceLen+ times, searching from start, or -1 if
// none exists before EOF. A closing line: up to 3 leading spaces, the SAME
// fence character repeated at least fenceLen times, and nothing but
// whitespace after — CommonMark's rule, which is also what stops a fence
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

// mermaidValid reports whether body parses AND passes STRICT validation.
// Strict, because one of the issue's named failure modes — an unquoted
// label containing parentheses — surfaces as a validator warning
// (NoParenthesesInLabels), not a parse error, and a diagram that renders
// wrong on GitHub is exactly what this exists to catch regardless of the
// severity mermaid-check assigns it.
//
// recover()-guarded: mermaid-check is a young (v0.0.4) third-party parser
// sitting on the single-shot, no-retry delivery path (commitDelivery blocks,
// no goroutine) — a panic here must degrade to "invalid, strip" rather than
// take the node down with it.
func mermaidValid(body string) (valid bool) {
	defer func() {
		if recover() != nil {
			valid = false
		}
	}()
	diagram, err := mermaid.Parse(body)
	if err != nil {
		return false
	}
	return len(mermaid.Validate(diagram, true)) == 0
}
