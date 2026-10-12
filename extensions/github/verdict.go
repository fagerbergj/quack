package github

import (
	"regexp"
	"strings"
)

var (
	verdictRe          = regexp.MustCompile(`(?mi)^\s*VERDICT:\s*(approve|request_changes|comment)\s*$`)
	fallbackPreambleRe = regexp.MustCompile(`(?mi)^.*\bstaging tools?\b.*\bfallback\b.*$\n?`)
)

// StripVerdictTail removes the machine-parseable VERDICT tail for human-facing text.
func StripVerdictTail(answer string) string {
	s := fallbackPreambleRe.ReplaceAllString(answer, "")
	if loc := verdictRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	return strings.TrimSpace(s)
}
