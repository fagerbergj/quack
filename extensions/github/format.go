package github

import (
	"fmt"
	"regexp"
	"strings"
)

// mentionRe matches @user or @org/team; the leading class skips emails and URL paths. Scoped package
// names (@angular/core) match too and are stripped on purpose: GitHub pings them identically.
var mentionRe = regexp.MustCompile(`(^|[^\w/.:@])@([A-Za-z0-9][A-Za-z0-9-]*(?:/[A-Za-z0-9._-]+)?)`)

// stripMentions drops the "@" from every mention outside fenced or inline code, so nothing quack
// posts can ping a person; the login stays so the sentence still reads.
func stripMentions(s string) string {
	lines := strings.Split(s, "\n")
	// An odd fence count means an unterminated block that would hide every later mention;
	// fail safe and strip everywhere instead.
	fenceCount := 0
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenceCount++
		}
	}
	trackFences := fenceCount%2 == 0
	fenced := false
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			if trackFences {
				fenced = !fenced
			}
			continue
		}
		if fenced {
			continue
		}
		// Even segments are prose, odd ones inline code. An odd backtick count leaves an unterminated
		// span; fail safe like fences and strip every segment.
		parts := strings.Split(ln, "`")
		allProse := len(parts)%2 == 0
		for j := 0; j < len(parts); j++ {
			if !allProse && j%2 != 0 {
				continue
			}
			parts[j] = mentionRe.ReplaceAllString(parts[j], "${1}${2}")
		}
		lines[i] = strings.Join(parts, "`")
	}
	return strings.Join(lines, "\n")
}

// appendLine adds line to body once, so a re-evaluated outcome doesn't stack;
// a trailing quack footer stays last.
func appendLine(body, line string) (string, bool) {
	if strings.Contains(body, line) {
		return body, false
	}
	if loc := footerRe.FindStringIndex(body); loc != nil {
		return body[:loc[0]] + "\n\n" + line + body[loc[0]:], true
	}
	if body = strings.TrimRight(body, "\n"); body == "" {
		return line, true
	}
	return body + "\n\n" + line, true
}

// commandsSummary identifies quack's own block, so footerRe never takes an
// agent-written <details> section for the footer.
const commandsSummary = "Commands quack accepts here"

// footerRe matches the whole trailing footer region (commands block plus
// <sub> line, or the line alone) so appendLine slots new lines above it.
var footerRe = regexp.MustCompile(`(?s)\n\n(?:<details>\n<summary>` + commandsSummary + `</summary>\n\n.*?\n\n</details>(?:\n\n<sub>quack[^\n]*</sub>)?|<sub>quack[^\n]*</sub>)\s*\z`)

// withFooter appends the version/run-link footer once.
func (a *App) withFooter(body, chatID string) string {
	return appendFooter(body, footerText(a.version, a.publicURL, chatID))
}

// appendFooter appends text once; body is unchanged when text is empty or already present.
func appendFooter(body, text string) string {
	if text == "" || strings.Contains(body, text) {
		return body
	}
	if b := strings.TrimRight(body, "\n"); b != "" {
		return b + "\n\n" + text
	}
	return text
}

// footerText renders the <sub> footer, or "" rather than a bare "quack" with no version or link.
func footerText(version, publicURL, chatID string) string {
	if version == "" && publicURL == "" {
		return ""
	}
	s := "<sub>quack"
	if version != "" {
		s += " " + version
	}
	if publicURL != "" {
		s += ` · <a href="` + publicURL + "/chat/" + chatID + `">run</a>`
	}
	return s + "</sub>"
}

// commandsBlockText renders a posted review's collapsible commands block, naming only enabled
// triggers; "" when none are.
func commandsBlockText(mention string, labels Labels, triggers map[string]bool) string {
	var lines []string
	if triggers["mention"] && mention != "" {
		lines = append(lines,
			fmt.Sprintf("- `%s <request>` at the start of a line to continue the conversation; `%s` alone does nothing.", mention, mention))
	}
	if triggers["label"] {
		if labels.Review != "" {
			lines = append(lines, fmt.Sprintf("- `/review` as the entire comment (nothing else in it) from a repository owner, member or collaborator re-runs this review while the `%s` label is on the pull request.", labels.Review))
			lines = append(lines, fmt.Sprintf("- the `%s` label: runs a review.", labels.Review))
		}
	}
	if triggers["explain"] {
		lines = append(lines, "- `/explain` as the entire comment from a repository owner, member or collaborator: an interactive walkthrough and quiz of this pull request in quack, linked from a reply.")
	}
	if triggers["merge"] && labels.Merge != "" {
		lines = append(lines, fmt.Sprintf("- the `%s` label: merges once quack approves and checks are green.", labels.Merge))
	}
	if triggers["ci_fix"] && labels.Fix != "" {
		lines = append(lines, fmt.Sprintf("- the `%s` label: fixes red CI.", labels.Fix))
	}
	if triggers["issue_plan"] && labels.Plan != "" {
		lines = append(lines, fmt.Sprintf("- the `%s` label on an issue: drafts a plan.", labels.Plan))
	}
	if triggers["issue_implement"] && labels.Implement != "" {
		lines = append(lines, fmt.Sprintf("- the `%s` label on an issue: implements a plan.", labels.Implement))
	}
	if len(lines) == 0 {
		return ""
	}
	return "<details>\n<summary>" + commandsSummary + "</summary>\n\n" +
		strings.Join(lines, "\n") + "\n\n</details>"
}

// reviewFooterText is the commands block then footerText, either half optional.
func reviewFooterText(mention string, labels Labels, triggers map[string]bool, version, publicURL, chatID string) string {
	block := commandsBlockText(mention, labels, triggers)
	footer := footerText(version, publicURL, chatID)
	switch {
	case block == "":
		return footer
	case footer == "":
		return block
	default:
		return block + "\n\n" + footer
	}
}

// withReviewFooter is withFooter for a posted review, with the commands block above the <sub> line.
func (a *App) withReviewFooter(body, chatID string) string {
	return appendFooter(body, reviewFooterText(a.mention, a.labels, a.triggers, a.version, a.publicURL, chatID))
}
