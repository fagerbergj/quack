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
	"strconv"
	"strings"
	"sync"
	"time"
)

// fenceOpenRe matches a fence-opening line (CommonMark: 3 leading spaces, 3+ backticks/tildes, optional info).
var fenceOpenRe = regexp.MustCompile(`(?i)^( {0,3})(` + "`{3,}|~{3,}" + `)[ \t]*(\S*)`)

// mermaidLineRe matches jison's fixed "Parse error on line N:" header - part
// of jison's own generated-parser boilerplate, not mermaid's prose about the
// diagram (#735).
var mermaidLineRe = regexp.MustCompile(`^Parse error on line (\d+):$`)

// mermaidGotTokenRe pulls the terminal jison actually saw out of its
// "Expecting '...', got '<TOKEN>'" tail. That token is the signal; the
// Expecting list is grammar-internal noise we deliberately drop.
var mermaidGotTokenRe = regexp.MustCompile(`got '([^']*)'\s*$`)

// mermaidLabelPunctuation maps a jison terminal to the unquoted character
// that produced it, for the single family of errors this translates: a
// punctuation character mermaid treats as a label terminator. Confirmed
// against the live parser (each character here reproduced, and quoting the
// label fixed, every case below) - not guessed from mermaid's grammar names.
var mermaidLabelPunctuation = map[string]string{
	"PS":            "(",
	"PE":            ")",
	"SQS":           "[",
	"DIAMOND_START": "{",
	"DIAMOND_STOP":  "}",
	"PIPE":          "|",
	"STR":           `"`,
}

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
// feeds the gate.
func (i mermaidIssue) Feedback() string {
	return fmt.Sprintf("line %d: %s", i.line, i.err)
}

// FeedbackBlock renders one issue as a markdown fenced block: github/webhook.go's
// revise nudge embeds this in a GitHub comment, where bare newlines inside a
// "- " bullet break the list AND misalign the parser's caret (#735).
func (i mermaidIssue) FeedbackBlock() string {
	return fmt.Sprintf("line %d:\n```\n%s\n```", i.line, i.err)
}

// FormatMermaidNudgeBody joins each issue as its own fenced markdown block -
// github/webhook.go's revise nudge embeds this in a GitHub comment, where a
// "- " bullet list would break on the issues' embedded newlines and misalign
// the parser's caret (#735).
func FormatMermaidNudgeBody(issues []mermaidIssue) string {
	blocks := make([]string, len(issues))
	for i, iss := range issues {
		blocks[i] = iss.FeedbackBlock()
	}
	return strings.Join(blocks, "\n\n")
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
		return translateMermaidError(res.Error)
	}
	return ""
}

// translateMermaidError turns mermaid's raw jison parse error into a message
// a worker can act on: keep the diagram's own line/column and source excerpt
// (the caret genuinely points at the offending column), drop the
// grammar-internal "Expecting '...'" token list, and translate the "got
// '<TOKEN>'" terminal into a plain-language cause when it's a known
// unquoted-punctuation case. Parses jison's fixed output structure, never
// mermaid's English prose (#735) - that changes between mermaid versions,
// the structure doesn't.
func translateMermaidError(raw string) string {
	lines := strings.Split(raw, "\n")
	if len(lines) < 4 {
		return unrecognizedMermaidError(raw)
	}
	m := mermaidLineRe.FindStringSubmatch(lines[0])
	if m == nil {
		return unrecognizedMermaidError(raw)
	}
	excerpt, caretLine := lines[1], lines[2]
	caretIdx := strings.IndexByte(caretLine, '^')
	if caretIdx < 0 || strings.Trim(caretLine[:caretIdx], "-") != "" {
		return unrecognizedMermaidError(raw)
	}
	tail := strings.Join(lines[3:], "\n")

	tok := mermaidGotTokenRe.FindStringSubmatch(tail)
	if tok == nil {
		return unrecognizedMermaidError(raw)
	}
	ch, known := mermaidLabelPunctuation[tok[1]]
	if !known {
		return unrecognizedMermaidError(raw)
	}
	return fmt.Sprintf(
		"parse error: diagram line %s, column %d:\n    %s\n    %s\n"+
			"unquoted %q inside a node label - mermaid treats it as ending the label there.\n"+
			"Wrap the whole label in double quotes (e.g. G[\"...\"]) so %q is read as literal text.",
		m[1], caretIdx+1, excerpt, caretLine, ch, ch)
}

// unrecognizedMermaidError is the fallback for any error shape or "got" token
// translateMermaidError doesn't recognize: still names where it broke (when
// jison's structure is present) and a generic quoting hint, and always keeps
// the raw parser text - never swallowed into a generic message (#735).
func unrecognizedMermaidError(raw string) string {
	lines := strings.Split(raw, "\n")
	var located string
	if m := mermaidLineRe.FindStringSubmatch(lines[0]); m != nil && len(lines) >= 3 {
		if caretIdx := strings.IndexByte(lines[2], '^'); caretIdx >= 0 {
			located = fmt.Sprintf("diagram line %s, column %d:\n    %s\n    %s\n", m[1], caretIdx+1, lines[1], lines[2])
		}
	}
	return "parse error: " + located +
		"if a node label contains punctuation such as ( ) [ ] { } | \" , wrapping it in double quotes " +
		"(e.g. G[\"...\"]) usually fixes it. Raw parser error:\n" + raw
}

// mermaidLocatedRe pulls line/column back out of translateMermaidError's/
// unrecognizedMermaidError's "diagram line N, column C:" prefix.
var mermaidLocatedRe = regexp.MustCompile(`^parse error: diagram line (\d+), column (\d+):`)

// CheckMermaid validates one mermaid diagram's source (no fence wrapper) via
// the SAME validator the delivery gate runs (mermaidError) - so a worker
// calling this tool before submitting gets exactly the gate's verdict, not a
// second reimplementation that could disagree with it. line/column are 1-based,
// 0 when the error has no known location.
func CheckMermaid(source string) (ok bool, line, column int, message string) {
	msg := mermaidError(source)
	if msg == "" {
		return true, 0, 0, ""
	}
	if m := mermaidLocatedRe.FindStringSubmatch(msg); m != nil {
		line, _ = strconv.Atoi(m[1])
		column, _ = strconv.Atoi(m[2])
	}
	return false, line, column, msg
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
