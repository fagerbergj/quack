// mermaid.go: validates ```mermaid blocks bound for delivery with the real mermaid parser.
package vetting

import (
	"context"
	"encoding/json"
	"errors"
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

// mermaidLineRe matches jison's fixed "Parse error on line N:" header, not mermaid's prose.
var mermaidLineRe = regexp.MustCompile(`^Parse error on line (\d+):$`)

// mermaidGotTokenRe pulls the terminal jison saw from its "Expecting '...', got '<TOKEN>'" tail;
// the Expecting list is grammar noise and is dropped.
var mermaidGotTokenRe = regexp.MustCompile(`got '([^']*)'\s*$`)

// mermaidLabelPunctuation maps a jison terminal to the unquoted label punctuation that produced it.
// Each entry was reproduced against the live parser, and quoting the label fixed it.
var mermaidLabelPunctuation = map[string]string{
	"PS":            "(",
	"PE":            ")",
	"SQS":           "[",
	"DIAMOND_START": "{",
	"DIAMOND_STOP":  "}",
	"PIPE":          "|",
	"STR":           `"`,
}

type mermaidIssue struct {
	line int
	err  string
}

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

var warnMermaidValidatorUnavailable = sync.OnceFunc(func() {
	slog.Warn("mermaid diagrams are not being validated: node is missing or scripts/mermaid-validate.mjs was not found",
		"component", "vetting")
})

// mermaidError validates body via mermaid.js; "" means valid or no verdict, so the gate fails open.
func mermaidError(body string) string {
	msg, _ := mermaidVerdict(body)
	return msg
}

// mermaidVerdict reports the validator's verdict on body. A non-nil error means no verdict was
// obtained (node missing, timed out, crashed): neither valid nor invalid.
func mermaidVerdict(body string) (string, error) {
	if _, err := exec.LookPath("node"); err != nil {
		warnMermaidValidatorUnavailable()
		return "", errors.New("node is not installed")
	}
	if !pathExists(mermaidValidatorPath) {
		warnMermaidValidatorUnavailable()
		return "", errors.New("the validator script is missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), MermaidValidateTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", mermaidValidatorPath)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.Output()
	// A kill on timeout arrives as an ExitError; a slow box must not report a valid diagram as broken.
	if ctx.Err() != nil {
		warnMermaidValidatorTimeout()
		return "", fmt.Errorf("the validator timed out after %s", MermaidValidateTimeout)
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	// The script catches parse errors and always exits 0 printing {"ok":...}, so a crash or
	// unparseable output means a broken validator, never an invalid diagram.
	if err != nil || json.Unmarshal(out, &res) != nil {
		warnMermaidValidatorBroken(err, out)
		return "", errors.New("the validator crashed")
	}
	if !res.OK {
		return translateMermaidError(res.Error), nil
	}
	return "", nil
}

// MermaidValidateTimeout bounds one validator run. Each run cold-starts node and loads jsdom plus
// mermaid (~1.5s idle, past 60s on a saturated host). Exported so tests can lift it.
var MermaidValidateTimeout = 60 * time.Second

var warnBrokenOnce sync.Once

func warnMermaidValidatorBroken(err error, out []byte) {
	warnBrokenOnce.Do(func() {
		slog.Warn("mermaid validator failed; diagrams are not being validated",
			"component", "vetting", "err", err, "output", string(out))
	})
}

func warnMermaidValidatorTimeout() {
	slog.Warn("mermaid validation timed out; this diagram was not validated and the gate let it through",
		"component", "vetting", "timeout", MermaidValidateTimeout)
}

// translateMermaidError keeps jison's line/column and excerpt, drops the Expecting list, and names
// known unquoted-punctuation causes. It parses jison's structure, not mermaid's prose, which drifts.
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

// unrecognizedMermaidError is the fallback for unknown shapes: where it broke (if known), a quoting
// hint, and always the raw parser text.
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

// CheckMermaidDescription is the check_mermaid tool description shared by the native and ACP surfaces.
const CheckMermaidDescription = "Validate one mermaid diagram's source before including it in your final answer. " +
	"Returns \"ok\", a parse error with line/column, or \"not validated\" when the validator gave no verdict. " +
	"Call this on every mermaid diagram before submitting."

// CheckMermaid is the check_mermaid tool's reply from the delivery gate's own validator: "ok",
// "invalid ..." (line/column when known), or "not validated: ..." when no verdict came back.
func CheckMermaid(source string) string {
	msg, err := mermaidVerdict(source)
	if err != nil {
		return "not validated: " + err.Error() + ". The syntax is unchecked, not passed: " +
			"call check_mermaid again, and check the syntax by hand if it keeps failing."
	}
	if msg == "" {
		return "ok"
	}
	if m := mermaidLocatedRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("invalid (line %s, column %s): %s", m[1], m[2], msg)
	}
	return "invalid: " + msg
}

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
