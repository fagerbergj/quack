// A deterministic scan of every ```mermaid fenced block bound for delivery
// (#448): GitHub renders an invalid diagram as a broken box, and nothing
// upstream checks the syntax before it ships. This package used to STRIP an
// invalid block to plain text (#371) - reversed by #448: stripping hides the
// defect instead of fixing it, and the agent that wrote the bad diagram is
// the one who can fix it. So this now only DETECTS and reports; mermaidCriterion
// (checks.go-adjacent, wired in node.go's foldDeterministic) fails the
// deterministic gate with the concrete error, feeding a revise round instead
// of silently degrading. #358 argued a bare "write valid mermaid" instruction
// is exactly what a model ignores intermittently - true, but irrelevant here:
// the feedback below always names the actual parse error and which block,
// which is the actionable instruction #358 didn't have.
//
// #574: validation used to run through github.com/sammcj/mermaid-check, a Go
// REIMPLEMENTATION of mermaid's grammar - looser than the real thing (0 of 7
// real plan diagrams flagged; GitHub's own jison-generated parser rejected
// 5). This now shells out to scripts/mermaid-validate.mjs, the real
// mermaid.parse() - the same parser GitHub renders with.
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

// fenceOpenRe matches a fence-opening line - up to 3 leading spaces (CommonMark's
// indented-code-block cutoff), 3+ backticks or tildes, then an optional info
// string. Case-insensitive: GitHub renders ```Mermaid / ```MERMAID the same as
// ```mermaid.
var fenceOpenRe = regexp.MustCompile(`(?i)^( {0,3})(` + "`{3,}|~{3,}" + `)[ \t]*(\S*)`)

// mermaidIssue is one invalid top-level ```mermaid block found while scanning
// markdown: the 1-based line number of its fence-open line (so revise
// feedback can point at it in a long answer) and the concrete reason it's
// invalid.
type mermaidIssue struct {
	line int
	err  string
}

// FindInvalidMermaid walks md fence-depth-aware, one line at a time, tracking
// which code fence (if any) is currently open - a ```mermaid opener only
// starts a mermaid block when it's a genuine TOP-LEVEL fence, never one
// merely quoted inside an unrelated fence's body (a ```go block whose content
// contains the literal text "```mermaid" is never descended into). Detection
// only - md is never modified; see the package doc for why.
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

// walkMermaidBlocks is the one fence-walker shared by FindInvalidMermaid and
// DegradeInvalidMermaid: it visits each genuine top-level ```mermaid block's
// 0-based fence-open/fence-close line indices plus its body text (fence
// lines excluded) - validating is entirely up to the caller's visit func.
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
			// Unterminated fence: everything to EOF is inside it - nothing here is
			// a genuine, closed top-level block worth checking.
			return
		}
		if info == "mermaid" {
			visit(i, close, strings.Join(lines[i+1:close], "\n"))
		}
		i = close + 1
	}
}

// DegradeInvalidMermaid is the plan/research path's last-resort ceiling
// (github/webhook.go, after one failed revise): rewrites each still-invalid
// ```mermaid fence into a labeled ```text fence with a visible warning note -
// a visible degradation, not the old silent strip. Returns md unchanged and
// issues=nil when nothing is invalid.
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

// findFenceClose returns the index of the line that closes a fence opened
// with fenceChar repeated fenceLen+ times, searching from start, or -1 if
// none exists before EOF. A closing line: up to 3 leading spaces, the SAME
// fence character repeated at least fenceLen times, and nothing but
// whitespace after - CommonMark's rule, which is also what stops a fence
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

// mermaidValidatorPath is scripts/mermaid-validate.mjs. In production it's
// found relative to the server's own CWD - repo root in dev, "/" in the
// runtime image, same convention as every config/agents/skills path (see the
// Dockerfile). `go test`'s CWD is instead the package's own directory, which
// differs per package (internal/vetting vs internal/github, both consumers -
// see FindInvalidMermaid/DegradeInvalidMermaid) - resolveMermaidValidatorPath
// falls back to a path relative to THIS source file so every package's tests
// find the same repo checkout without each needing its own override.
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

// mermaidError returns "" if body is valid mermaid per the REAL mermaid.js
// parser (scripts/mermaid-validate.mjs, run over stdin), else a reason
// naming the parser's own error. Node absent, or the script missing, derives
// nothing rather than failing every node with a diagram - mirrors
// toolchainPresent's posture for derived checks (checks.go) - logged once so
// an operator can tell "validated clean" from "never validated".
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
			// A launch failure, not the script reporting a bad diagram - degrade
			// like a missing validator rather than mislabel it as invalid mermaid.
			warnMermaidValidatorUnavailable()
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

// mermaidCriterion is the GATE side of #448: scans the answer plus every
// currently staged delivery body (StagedDelivery.Body - the PR/review/comment
// text about to ship to GitHub) for an invalid ```mermaid block. ok=false
// means nothing invalid was found (matches sufficient_length's convention
// above - a passing check adds no entry to v.Criteria). The first offending
// block's line + reason becomes the Reason, which composeFeedback (node.go)
// folds into the next revise prompt - the model gets the actual parser error
// and the block it came from, not a generic "check your mermaid".
func mermaidCriterion(answer string, act workerActivity) (criterionScore, bool) {
	texts := make([]string, 0, len(act.stagedDelivery)+1)
	texts = append(texts, answer)
	for _, sd := range act.stagedDelivery {
		texts = append(texts, sd.Body)
	}
	for _, t := range texts {
		if issues := FindInvalidMermaid(t); len(issues) > 0 {
			iss := issues[0]
			return criterionScore{Score: 0, Reason: fmt.Sprintf(
				"deterministic: invalid mermaid diagram at line %d: %s", iss.line, iss.err)}, true
		}
	}
	return criterionScore{}, false
}
