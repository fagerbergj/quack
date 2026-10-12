package main

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
)

type overlap struct {
	A           string   `json:"a"`
	B           string   `json:"b"`
	Similarity  float64  `json:"similarity"`
	CoOfferedBy []string `json:"co_offered_by,omitempty"`
	Allowed     string   `json:"allowlisted,omitempty"`
}

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields("the and for with are its this that from not you your use call when what which each one any all only into than then them they their per via can may must never does don't before after more most other same such but also has have was were will would should") {
		stopwords[w] = true
	}
}

// tokenSet is the stemmed, stopword-free words of a tool's name and description.
func tokenSet(d toolDef) map[string]bool {
	set := map[string]bool{}
	text := strings.ToLower(strings.ReplaceAll(d.Name, "_", " ") + " " + d.Description)
	for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) >= 3 && !stopwords[w] {
			set[stem(w)] = true
		}
	}
	return set
}

// stem folds plural and tense suffixes, then a final e, so "asks"/"ask" and "staged"/"stage" match.
func stem(w string) string {
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if r := strings.TrimSuffix(w, suf); r != w && len(r) >= 3 {
			w = r
			break
		}
	}
	if r := strings.TrimSuffix(w, "e"); len(r) >= 3 {
		w = r
	}
	return w
}

func jaccard(a, b map[string]bool) float64 {
	in := 0
	for w := range a {
		if b[w] {
			in++
		}
	}
	if union := len(a) + len(b) - in; union > 0 {
		return float64(in) / float64(union)
	}
	return 0
}

// checkOverlaps reports every pair at or above the threshold and fails one an agent is offered
// together without an allowlist entry.
func (l *linter) checkOverlaps(defs []toolDef) {
	sorted := slices.SortedFunc(slices.Values(defs), func(a, b toolDef) int { return strings.Compare(a.Name, b.Name) })
	sets := make([]map[string]bool, len(sorted))
	for i, d := range sorted {
		sets[i] = tokenSet(d)
	}
	used := map[int]bool{}
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			s := jaccard(sets[i], sets[j])
			if s < l.cfg.Overlap.Threshold {
				continue
			}
			o, idx := l.pair(sorted[i], sorted[j], math.Round(s*1000)/1000)
			used[idx] = used[idx] || len(o.CoOfferedBy) > 0
			l.rep.Overlaps = append(l.rep.Overlaps, o)
		}
	}
	for i, e := range l.cfg.Overlap.Allow {
		if !used[i] {
			l.add(finding{Rule: "stale_allowlist", Subject: strings.Join(e.Tools, ","), warn: true,
				Message: "allowlist entry covers no overlapping pair an agent is offered; drop it"})
		}
	}
}

func (l *linter) pair(a, b toolDef, s float64) (overlap, int) {
	o := overlap{A: a.Name, B: b.Name, Similarity: s}
	idx := l.cfg.allowIndex(a.Name, b.Name)
	if idx >= 0 {
		o.Allowed = l.cfg.Overlap.Allow[idx].Reason
	}
	sources := append(toolSources(a), toolSources(b)...)
	for _, ag := range l.rep.Agents {
		if slices.Contains(ag.Tools, a.Name) && slices.Contains(ag.Tools, b.Name) {
			o.CoOfferedBy = append(o.CoOfferedBy, ag.Name)
			sources = append(sources, ag.sources...)
		}
	}
	if len(o.CoOfferedBy) > 0 && o.Allowed == "" {
		l.add(finding{Rule: "overlap", Subject: a.Name + "," + b.Name, sources: sources,
			Message: fmt.Sprintf("similarity %.3f and offered together to %s; merge them, sharpen the descriptions, or allowlist the pair with a reason",
				s, strings.Join(o.CoOfferedBy, ", "))})
	}
	return o, idx
}
