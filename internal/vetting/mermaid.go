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
)

// ponytail: a hand-rolled subset checker, not `mmdc` (mermaid-cli, a node.js
// dependency with no toolchain guarantee on a deployment) and not a vendored
// grammar (no maintained pure-Go mermaid parser exists). It catches the
// mechanical failure classes the issue names — missing diagram header,
// unbalanced brackets/quotes, a bare `->` where mermaid needs `-->`, a stray
// ``` fence nested inside the block — by construction, NOT full mermaid
// grammar. A diagram can pass this check and still be semantically invalid in
// a way only mmdc or the real renderer would catch; the ceiling is "catches
// the common mechanical slips", not "guarantees rendering".
var (
	mermaidFenceRe = regexp.MustCompile("(?s)```mermaid[ \\t]*\\r?\\n(.*?)```")

	mermaidHeaderRe = regexp.MustCompile(`(?im)^\s*(graph\s|flowchart\s|sequenceDiagram\b|classDiagram\b|stateDiagram(-v2)?\b|erDiagram\b|journey\b|gantt\b|pie\b|gitGraph\b|mindmap\b|timeline\b|quadrantChart\b|requirementDiagram\b|C4Context\b|sankey-beta\b)`)

	// A flowchart-shaped body (has `-->`-style edges) with no recognized header
	// — the single most common omission — gets `flowchart TD` prepended rather
	// than being stripped outright.
	edgeRe = regexp.MustCompile(`-{1,2}[.>=-]*>`)

	// A lone `->` (single dash) is not a valid mermaid arrow anywhere in the
	// flowchart/graph grammar (which wants `-->`, `-.->`, `==>`, `--o`, `--x`);
	// repair it to `-->` when the header identifies a flowchart/graph.
	singleDashArrowRe = regexp.MustCompile(`([^-.=])->([^>])`)

	// A bracket label containing an unquoted paren, quote, or another bracket
	// breaks mermaid's node-label grammar unless the whole label is wrapped in
	// double quotes: A[Login (OAuth)] must read A["Login (OAuth)"].
	unquotedSpecialLabelRe = regexp.MustCompile(`\[([^"\]\[]*[()][^\]\[]*)\]`)
)

// validateAndRepairMermaid scans md for ```mermaid fenced blocks and returns
// the markdown with each block either left untouched (already valid),
// mechanically repaired, or — when repair doesn't produce something that
// passes the structural check — replaced by a plain-text fallback (the raw
// source in a plain code fence, with a note) so a broken diagram never ships.
// changed reports whether anything in md was modified.
func validateAndRepairMermaid(md string) (out string, changed bool) {
	if !strings.Contains(md, "```mermaid") {
		return md, false
	}
	out = mermaidFenceRe.ReplaceAllStringFunc(md, func(block string) string {
		body := mermaidFenceRe.FindStringSubmatch(block)[1]
		fixed := repairMermaid(body)
		if mermaidStructurallyValid(fixed) {
			if fixed != body {
				changed = true
			}
			return "```mermaid\n" + strings.TrimRight(fixed, "\n") + "\n```"
		}
		changed = true
		return "_[diagram omitted: invalid mermaid syntax could not be repaired]_\n\n```text\n" + strings.TrimRight(body, "\n") + "\n```"
	})
	return out, changed
}

// repairMermaid applies the mechanical fixes named in #371, in order:
// prepend a missing header, fix bare `->` arrows, quote bracket labels that
// contain unescaped parens. Each fix is independently idempotent.
func repairMermaid(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return body
	}
	fixed := body
	// Strip a stray nested ``` fence some models leave inside the block.
	fixed = strings.ReplaceAll(fixed, "```", "")

	if !mermaidHeaderRe.MatchString(fixed) && edgeRe.MatchString(fixed) {
		fixed = "flowchart TD\n" + strings.TrimLeft(fixed, "\n")
	}

	if mermaidHeaderRe.MatchString(fixed) {
		header := mermaidHeaderRe.FindString(fixed)
		if strings.HasPrefix(strings.TrimSpace(header), "graph") || strings.HasPrefix(strings.TrimSpace(header), "flowchart") {
			fixed = singleDashArrowRe.ReplaceAllString(fixed, "$1-->$2")
		}
	}

	fixed = unquotedSpecialLabelRe.ReplaceAllStringFunc(fixed, func(m string) string {
		inner := m[1 : len(m)-1]
		if strings.Contains(inner, `"`) {
			return m // already has a quote in play — don't double-wrap
		}
		return `["` + inner + `"]`
	})

	return fixed
}

// mermaidStructurallyValid is the deterministic acceptance check: a
// recognized diagram-type header, and balanced [], (), {} across the whole
// block (mermaid's node/label/subgraph delimiters). Not a grammar — a subset
// check for the failure modes repairMermaid targets (see the ponytail note
// above for the named ceiling).
func mermaidStructurallyValid(body string) bool {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return false
	}
	if !mermaidHeaderRe.MatchString(trimmed) {
		return false
	}
	if strings.Contains(trimmed, "```") {
		return false
	}
	return balanced(trimmed, '[', ']') && balanced(trimmed, '(', ')') && balanced(trimmed, '{', '}')
}

func balanced(s string, open, close rune) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case open:
			depth++
		case close:
			depth--
		}
		if depth < 0 {
			return false
		}
	}
	return depth == 0
}
