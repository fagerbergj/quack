package vetting

import (
	"fmt"
	"path/filepath"
	"strings"
)

// pointerKeywords: words that, near a locating preposition, mark the answer naming where the deliverable lives
// ("saved to X") rather than citing X as a source ("see X for the schema").
var pointerKeywords = []string{
	"written", "wrote", "saved", "save", "stored", "located",
	"available", "complete", "output", "result",
}

// danglingDeliverablePathCriterion flags an answer pointing at a path this run wrote but never committed: the
// working dir is discarded at run end. Only pointer phrasing counts, since honest prose often names such files.
func danglingDeliverablePathCriterion(answer string, act workerActivity, nodeDir string) (criterionScore, bool) {
	if act.committed {
		return criterionScore{}, false
	}
	for _, w := range act.written {
		// Strip the node's own working-dir prefix - invisible to the model, so
		// matching it against the answer text would never hit.
		rel := w
		if nodeDir != "" {
			rel = strings.TrimPrefix(w, nodeDir+"/")
		}
		base := filepath.Base(rel)
		if base == "" || base == "." || base == "/" {
			continue
		}
		candidates := []string{rel}
		if base != rel {
			candidates = append(candidates, base)
		}
		for _, c := range candidates {
			if pointerPhraseNear(answer, c) {
				return criterionScore{Score: 0, Reason: fmt.Sprintf(
					"deterministic: the answer points to %q as the deliverable, but it was only written to this run's "+
						"discarded working directory - never committed, so it exists nowhere the reader can reach it. "+
						"Your answer TEXT is the deliverable; state the result there instead of pointing at a file.", c)}, true
			}
		}
	}
	return criterionScore{}, false
}

// pointerPhraseNear reports whether some occurrence of path follows a locating preposition with a pointerKeyword
// earlier in the same short window ("the plan is complete at `X`", not "the config is in `X`").
func pointerPhraseNear(answer, path string) bool {
	const lookback = 60
	from := 0
	for {
		idx := strings.Index(answer[from:], path)
		if idx < 0 {
			return false
		}
		pos := from + idx
		start := pos - lookback
		if start < 0 {
			start = 0
		}
		if hasPointerShape(answer[start:pos]) {
			return true
		}
		from = pos + len(path)
	}
}

// hasPointerShape: the window's last word is a locating preposition, and a
// pointer keyword appears somewhere before it.
func hasPointerShape(window string) bool {
	trimmed := strings.TrimRight(window, "`'\"([ \t\n:—-")
	lower := strings.ToLower(trimmed)
	switch lastWord(lower) {
	case "at", "in", "to":
	default:
		return false
	}
	for _, kw := range pointerKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func lastWord(s string) string {
	s = strings.TrimRight(s, " \t\n")
	if i := strings.LastIndexAny(s, " \t\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
