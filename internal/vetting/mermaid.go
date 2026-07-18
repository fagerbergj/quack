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

// mermaidFenceRe locates ```mermaid fenced blocks for splicing purposes only
// — it knows nothing about mermaid syntax itself (any fenced-code-block
// pattern would do). Parsing and validating what's INSIDE the fence is
// entirely github.com/sammcj/mermaid-check's job: a real AST parser +
// validator covering 20+ diagram types. Note the import path: the same
// upstream author's github.com/sammcj/go-mermaid (the name this issue's
// original ask named) is the SAME project pre-rename — its last release
// under that path (v0.0.2) barely parses a flowchart (zero statements for
// ordinary valid input, confirmed by hand), so this uses the module's
// current, functional identity, mermaid-check (v0.0.3+), which is what the
// real parsing and validation logic actually lives at.
var mermaidFenceRe = regexp.MustCompile("(?s)```mermaid[ \\t]*\\r?\\n(.*?)```")

// validateAndRepairMermaid scans md for ```mermaid fenced blocks and replaces
// any block that fails to parse, or fails strict validation, with a
// plain-text fallback (the raw source in a plain code fence, with a note) —
// never auto-repaired: mermaid-check's errors come from a real parser, not a
// guess, so there's nothing to mechanically patch with confidence. Either the
// diagram is valid or it ships as text instead of a broken rendering.
// changed reports whether md was modified.
//
// Strip, not fail-the-gate: delivery must never block on a diagram, and the
// gate has no reliable way to make a model fix mermaid syntax on a revise
// round (see the package doc above) — a stripped diagram degrades to
// readable text instead of sinking a round the model can't win.
func validateAndRepairMermaid(md string) (out string, changed bool) {
	if !strings.Contains(md, "```mermaid") {
		return md, false
	}
	out = mermaidFenceRe.ReplaceAllStringFunc(md, func(block string) string {
		body := mermaidFenceRe.FindStringSubmatch(block)[1]
		if mermaidValid(body) {
			return block
		}
		changed = true
		return "_[diagram omitted: invalid mermaid syntax]_\n\n```text\n" + strings.TrimRight(body, "\n") + "\n```"
	})
	return out, changed
}

// mermaidValid reports whether body parses AND passes STRICT validation.
// Strict, because one of the issue's named failure modes — an unquoted
// label containing parentheses — surfaces as a validator warning
// (NoParenthesesInLabels), not a parse error, and a diagram that renders
// wrong on GitHub is exactly what this exists to catch regardless of the
// severity mermaid-check assigns it.
func mermaidValid(body string) bool {
	diagram, err := mermaid.Parse(body)
	if err != nil {
		return false
	}
	return len(mermaid.Validate(diagram, true)) == 0
}
