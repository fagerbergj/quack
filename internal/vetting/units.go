package vetting

import (
	"regexp"
	"strings"
)

// A Unit is one checkable piece of a deliverable: a sentence, list item or
// table row, with the specifics code found in it and the citation it pairs with.
type Unit struct {
	Kind      string     // "sentence", "list_item" or "table_row"
	Text      string     // the unit verbatim
	Paragraph int        // 0-based paragraph (block) the unit came from
	Specifics []Specific // figures, dates, quotes and names found by code
	Citations []string   // link targets paired with the unit (inline, else the block's next one)
}

// A Specific is a claim detail a reader could check: found by pattern, never by a model.
type Specific struct {
	Kind  string // "number", "percent", "currency", "date", "quote", "name"
	Value string // as written
	Norm  string // digits-only or lower-cased form used to locate it in evidence
}

var (
	fenceRe       = regexp.MustCompile("(?s)```.*?```")
	listItemRe    = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	tableRowRe    = regexp.MustCompile(`^\s*\|.*\|\s*$`)
	tableRuleRe   = regexp.MustCompile(`^\s*\|?\s*:?-{2,}`)
	bareURLRe     = regexp.MustCompile(`https?://[^\s)\]>"']+`)
	refMarkerRe   = regexp.MustCompile(`\[(\d{1,3})\]`)
	refDefRe      = regexp.MustCompile(`^\s*\[(\d{1,3})\]:?\s+(\S+)`)
	sentenceEndRe = regexp.MustCompile(`([.!?])\s+(?:[A-Z"'(\[]|\d)`)
	percentRe     = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?\s?%`)
	currencyRe    = regexp.MustCompile(`(?:[$€£]\s?\d[\d,]*(?:\.\d+)?[KMBkmb]?|\d[\d,]*(?:\.\d+)?[KMBkmb]?\s?(?:USD|EUR|GBP))`)
	dateRe        = regexp.MustCompile(`\b(?:\d{4}-\d{2}-\d{2}|(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\.? \d{1,2}(?:, \d{4})?|\d{1,2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]* \d{4})\b`)
	numberRe      = regexp.MustCompile(`(?:^|[^\w.$€£%-])(-?\d[\d,]*(?:\.\d+)?)(?:[^\w%]|$)`)
	quoteRe       = regexp.MustCompile(`"([^"]{4,})"`)
	nameRe        = regexp.MustCompile(`\b[A-Z][a-z]+(?: [A-Z][a-z]+)+\b`)
	nonDigitRe    = regexp.MustCompile(`[^\d.]`)
)

// FindUnits segments a deliverable into units and pairs each with a citation.
// Fenced code is skipped; a references-only block pairs with nothing.
func FindUnits(text string) []Unit {
	text = fenceRe.ReplaceAllString(text, "")
	refs := referenceList(text)
	var units []Unit
	for pi, block := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if isReferencesOnly(lines) {
			continue
		}
		units = append(units, blockUnits(lines, pi, refs)...)
	}
	return units
}

// blockUnits splits one block: table rows and list items are one unit per line,
// prose is split into sentences; the block's citations back-fill uncited units.
func blockUnits(lines []string, pi int, refs map[string]string) []Unit {
	var out []Unit
	var prose []string
	flush := func() {
		if len(prose) > 0 {
			out = append(out, sentenceUnits(strings.Join(prose, " "), pi, refs)...)
			prose = nil
		}
	}
	for _, ln := range lines {
		switch {
		case tableRowRe.MatchString(ln):
			flush()
			if !tableRuleRe.MatchString(ln) {
				out = append(out, newUnit("table_row", ln, pi, refs))
			}
		case listItemRe.MatchString(ln):
			flush()
			out = append(out, newUnit("list_item", listItemRe.ReplaceAllString(ln, ""), pi, refs))
		default:
			prose = append(prose, ln)
		}
	}
	flush()
	return pairFollowing(out)
}

func sentenceUnits(prose string, pi int, refs map[string]string) []Unit {
	var out []Unit
	start := 0
	for _, m := range sentenceEndRe.FindAllStringSubmatchIndex(prose, -1) {
		end := m[3] // just past the terminator
		out = append(out, newUnit("sentence", prose[start:end], pi, refs))
		start = end
	}
	if rest := strings.TrimSpace(prose[start:]); rest != "" {
		out = append(out, newUnit("sentence", rest, pi, refs))
	}
	return out
}

func newUnit(kind, text string, pi int, refs map[string]string) Unit {
	text = strings.TrimSpace(text)
	return Unit{Kind: kind, Text: text, Paragraph: pi, Specifics: findSpecifics(text), Citations: citationsIn(text, refs)}
}

// pairFollowing gives an uncited unit the nearest following citation in its block:
// a figure in one sentence is commonly cited at the end of the next.
func pairFollowing(units []Unit) []Unit {
	for i := range units {
		if len(units[i].Citations) > 0 {
			continue
		}
		for j := i + 1; j < len(units); j++ {
			if len(units[j].Citations) > 0 {
				units[i].Citations = append([]string(nil), units[j].Citations...)
				break
			}
		}
	}
	return units
}

// citationsIn: inline markdown targets, bare URLs and [n] markers resolved
// through the reference list, deduplicated in order of appearance.
func citationsIn(text string, refs map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		t = strings.TrimRight(strings.TrimSpace(t), ".,;")
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, m := range markdownLinkRe.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	stripped := markdownLinkRe.ReplaceAllString(text, "")
	for _, u := range bareURLRe.FindAllString(stripped, -1) {
		add(u)
	}
	for _, m := range refMarkerRe.FindAllStringSubmatch(text, -1) {
		if t, ok := refs[m[1]]; ok {
			add(t)
		}
	}
	return out
}

// referenceList maps "[n]" markers to the targets a trailing reference list declares.
func referenceList(text string) map[string]string {
	refs := map[string]string{}
	for _, ln := range strings.Split(text, "\n") {
		if m := refDefRe.FindStringSubmatch(ln); m != nil {
			refs[m[1]] = strings.TrimRight(m[2], ".,;")
		}
	}
	return refs
}

func isReferencesOnly(lines []string) bool {
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return true
	}
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if !refDefRe.MatchString(ln) {
			return false
		}
	}
	return true
}

// findSpecifics finds every checkable detail once: a percentage is not also a
// number, a currency amount is not also a number, a date's digits are not numbers.
func findSpecifics(text string) []Specific {
	var out []Specific
	taken := text
	take := func(kind string, re *regexp.Regexp, group int) {
		for _, m := range re.FindAllStringSubmatch(taken, -1) {
			v := strings.TrimSpace(m[group])
			out = append(out, Specific{Kind: kind, Value: v, Norm: normalizeSpecific(kind, v)})
		}
		taken = re.ReplaceAllString(taken, " ")
	}
	take("date", dateRe, 0)
	take("percent", percentRe, 0)
	take("currency", currencyRe, 0)
	take("quote", quoteRe, 1)
	take("number", numberRe, 1)
	for _, m := range nameRe.FindAllString(taken, -1) {
		out = append(out, Specific{Kind: "name", Value: m, Norm: strings.ToLower(m)})
	}
	return out
}

func normalizeSpecific(kind, v string) string {
	switch kind {
	case "number", "percent", "currency":
		return strings.TrimSuffix(nonDigitRe.ReplaceAllString(v, ""), ".")
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}
