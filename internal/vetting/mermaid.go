// mermaid.go: scans ```mermaid blocks bound for delivery and validates them via the real mermaid parser.
package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// fenceOpenRe matches a fence-opening line (CommonMark: 3 leading spaces, 3+ backticks/tildes, optional info).
var fenceOpenRe = regexp.MustCompile(`(?i)^( {0,3})(` + "`{3,}|~{3,}" + `)[ \t]*(\S*)`)

// mermaidIssue: one invalid top-level ```mermaid block: line number + reason.
type mermaidIssue struct {
	line int
	err  string
}

// FindInvalidMermaid walks md fence-depth-aware, detecting invalid top-level ```mermaid blocks.
func FindInvalidMermaid(md string) []mermaidIssue {
	if !strings.Contains(md, "```") && !strings.Contains(md, "~~~") {
		return nil
	}
	lines := strings.Split(md, "\n")
	var issues []mermaidIssue
	walkMermaidBlocks(lines, func(openLine, _ int, body string) {
		if reason := mermaidError(body); reason != "" {
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

// walkMermaidBlocks visits each top-level ```mermaid block with its body text.
func walkMermaidBlocks(lines []string, visit func(openLine, closeLine int, body string)) {
	for i := 0; i < len(lines); {
		m := fenceOpenRe.FindStringSubmatch(lines[i])
		if m == nil {
			i++
			continue
		}
		fenceChar, fenceLen, info := m[2][0], len(m[2]), strings.ToLower(m[3])
		close := findFenceClose(lines, i+1, fenceChar, fenceLen)
		if close == -1 {
			return // unterminated fence, nothing to check
		}
		if info == "mermaid" {
			visit(i, close, strings.Join(lines[i+1:close], "\n"))
		}
		i = close + 1
	}
}

// DegradeInvalidMermaid rewrites invalid ```mermaid fences to ```text with a warning (last-resort ceiling).
func DegradeInvalidMermaid(md string) (string, []mermaidIssue) {
	if !strings.Contains(md, "```") && !strings.Contains(md, "~~~") {
		return md, nil
	}
	lines := strings.Split(md, "\n")
	var issues []mermaidIssue
	out := make([]string, 0, len(lines))
	last := 0
	walkMermaidBlocks(lines, func(openLine, closeLine int, body string) {
		reason := mermaidError(body)
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

// findFenceClose searches for a closing fence line from start (CommonMark rules).
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

// mermaidValidatorPath: found relative to CWD (production) or this source file (tests).
var mermaidValidatorPath = resolveMermaidValidatorPath()

func resolveMermaidValidatorPath() string {
	const rel = "scripts/mermaid-validate.mjs"
	if _, err := os.Stat(rel); err == nil {
		return rel
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		if repoRel := filepath.Join(filepath.Dir(file), "..", "..", rel); pathExists(repoRel) {
			return repoRel
		}
	}
	return rel
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

var warnMermaidValidatorUnavailable = sync.OnceFunc(func() {
	slog.Warn("mermaid diagrams are not being validated: node is missing or scripts/mermaid-validate.mjs was not found",
		"component", "vetting")
})

// mermaidError validates body via the real mermaid.js parser. "" = valid; degrades gracefully when node/script absent.
func mermaidError(body string) string {
	if _, err := exec.LookPath("node"); err != nil {
		warnMermaidValidatorUnavailable()
		return ""
	}
	if !pathExists(mermaidValidatorPath) {
		warnMermaidValidatorUnavailable()
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", mermaidValidatorPath)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		if _, isExitErr := err.(*exec.ExitError); !isExitErr {
			warnMermaidValidatorUnavailable() // launch failure, not invalid diagram
			return ""
		}
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(out, &res) != nil {
		return fmt.Sprintf("the mermaid validator produced unreadable output: %s", out)
	}
	if !res.OK {
		return "parse error: " + res.Error
	}
	return ""
}

// mermaidCriterion scans the answer and staged delivery bodies for invalid ```mermaid blocks.
func mermaidCriterion(answer string, act workerActivity) (criterionScore, bool) {
	for _, t := range deliveryTexts(answer, act) {
		if issues := FindInvalidMermaid(t); len(issues) > 0 {
			iss := issues[0]
			return criterionScore{Score: 0, Reason: fmt.Sprintf(
				"deterministic: invalid mermaid diagram at line %d: %s", iss.line, iss.err)}, true
		}
	}
	return criterionScore{}, false
}
