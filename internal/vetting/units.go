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
	Kind  string // "number", "percent", "currency", "date", "quote"
	Value string // as written
	Norm  string // digits-only or lower-cased form used to locate it in evidence
}

var (
	fenceRe       = regexp.MustCompile("(?s)```.*?```")
	headingRe     = regexp.MustCompile(`^\s*#{1,6}\s`)
	sectionNumRe  = regexp.MustCompile(`^\s*\**\d+(?:\.\d+)+\.?\**\s+(?:\*\*)?[A-Z]`) // "2.3 Consolidate", never "2.5 years"
	listItemRe    = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	tableRowRe    = regexp.MustCompile(`^\s*\|.*\|\s*$`)
	tableRuleRe   = regexp.MustCompile(`^\s*\|?\s*:?-{2,}`)
	bareURLRe     = regexp.MustCompile(`https?://[^\s)\]>"']+`)
	refMarkerRe   = regexp.MustCompile(`\[(\d{1,3})\]`)
	refDefRe      = regexp.MustCompile(`^\s*\[(\d{1,3})\]:?\s+(.+)$`) // the target is the first URL on the line
	sentenceEndRe = regexp.MustCompile(`([.!?])\s+(?:[A-Z"'(\[]|\d)`)
	percentRe     = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?\s?%`)
	currencyRe    = regexp.MustCompile(`(?:[$€£]\s?\d[\d,]*(?:\.\d+)?[KMBkmb]?|\d[\d,]*(?:\.\d+)?[KMBkmb]?\s?(?:USD|EUR|GBP))`)
	dateRe        = regexp.MustCompile(`\b(?:\d{4}-\d{2}-\d{2}|(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\.? \d{1,2}(?:, \d{4})?|\d{1,2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]* \d{4})\b`)
	numberRe      = regexp.MustCompile(`(?:^|[^\w.$€£%-])(-?\d[\d,]*(?:\.\d+)?)(?:[^\w%-]|$)`)
	quoteRe       = regexp.MustCompile(`"([^"]{4,})"`)
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
		case headingRe.MatchString(ln):
			flush() // a heading names a section; it makes no checkable claim
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
	return Unit{Kind: kind, Text: text, Paragraph: pi, Specifics: findSpecifics(withoutLinks(text)), Citations: citationsIn(text, refs)}
}

// withoutLinks drops link targets and bare URLs so a date or number inside a
// URL path is never taken for a claim's specific; the link text stays.
func withoutLinks(text string) string {
	text = markdownLinkRe.ReplaceAllStringFunc(text, func(m string) string { return m[:strings.Index(m, "](")+1] })
	text = refMarkerRe.ReplaceAllString(text, " ") // a [1] marker is a citation, not a figure
	return bareURLRe.ReplaceAllString(text, " ")
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
			target := bareURLRe.FindString(m[2]) // "[1]: \"Title\" (https://...)" resolves to the URL
			if target == "" {
				target = strings.Fields(m[2])[0]
			}
			refs[m[1]] = strings.TrimRight(target, ".,;)")
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
	if m := sectionNumRe.FindStringIndex(text); m != nil { // "2.3 Consolidate..." numbers a section, it is not a figure
		taken = text[m[1]-1:]
	}
	take := func(kind string, re *regexp.Regexp, group int) {
		for _, m := range re.FindAllStringSubmatchIndex(taken, -1) {
			v := strings.TrimSpace(taken[m[2*group]:m[2*group+1]])
			if strings.HasPrefix(v, "-") && m[2*group] > 0 && isDigit(taken[m[2*group]-1]) {
				v = v[1:] // "15-25%" is a range, not minus 25
			}
			out = append(out, Specific{Kind: kind, Value: v, Norm: normalizeSpecific(kind, v)})
		}
		taken = re.ReplaceAllString(taken, " ")
	}
	take("date", dateRe, 0)
	take("percent", percentRe, 0)
	take("currency", currencyRe, 0)
	take("quote", quoteRe, 1)
	take("number", numberRe, 1)
	// ponytail: no proper-name specifics - Title Case matched headings and phrases; add a real detector if names prove worth checking.
	return out
}

func normalizeSpecific(kind, v string) string {
	switch kind {
	case "number", "percent", "currency":
		return strings.TrimSuffix(nonDigitRe.ReplaceAllString(v, ""), ".")
	case "date":
		if iso := canonicalDate(v); iso != "" {
			return iso
		}
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return strings.ToLower(strings.TrimSpace(markupRe.ReplaceAllString(v, "")))
	}
}

var markupRe = regexp.MustCompile("[*_`]+")

var monthNum = map[string]string{"jan": "01", "feb": "02", "mar": "03", "apr": "04", "may": "05", "jun": "06", "jul": "07", "aug": "08", "sep": "09", "oct": "10", "nov": "11", "dec": "12"}

var dateParts = regexp.MustCompile(`(?i)^(?:(\d{4})-(\d{2})-(\d{2})|([a-z]{3})[a-z]*\.? (\d{1,2})(?:, (\d{4}))?|(\d{1,2}) ([a-z]{3})[a-z]* (\d{4}))$`)

// canonicalDate renders any date form FindUnits accepts as YYYY-MM-DD; a month-day
// with no year yields MM-DD so it can still match either rendering.
func canonicalDate(v string) string {
	m := dateParts.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return ""
	}
	pad := func(d string) string {
		if len(d) == 1 {
			return "0" + d
		}
		return d
	}
	switch {
	case m[1] != "":
		return m[1] + "-" + m[2] + "-" + m[3]
	case m[4] != "":
		md := monthNum[strings.ToLower(m[4])] + "-" + pad(m[5])
		if m[6] != "" {
			return m[6] + "-" + md
		}
		return md
	default:
		return m[9] + "-" + monthNum[strings.ToLower(m[8])] + "-" + pad(m[7])
	}
}
