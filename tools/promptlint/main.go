// promptlint measures every prompt, rubric, skill and tool definition quack sends a model, and gates
// size budgets, broken references and overlapping tools offered to one agent (diff <ref> gates changes only).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

func main() {
	jsonOut := flag.String("json", "", "also write the machine-readable report to this file")
	flag.Usage = func() { fmt.Fprintln(os.Stderr, "usage: promptlint [-json report.json] <check | diff <git-ref>>") }
	flag.Parse()
	root, _ := os.Getwd()
	changed, ok := parseMode(flag.Args())
	if !ok {
		flag.Usage()
		os.Exit(2)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		fail(err)
	}
	defs, err := catalog()
	if err != nil {
		fail(err)
	}
	rep := run(root, cfg, defs, changed)
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			fail(err)
		}
	}
	if summarize(os.Stdout, rep) {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "promptlint:", err)
	os.Exit(2)
}

// parseMode returns nil changed files for a full check, the diff's files (never nil) for diff mode.
func parseMode(args []string) ([]string, bool) {
	switch {
	case len(args) == 1 && args[0] == "check":
		return nil, true
	case len(args) == 2 && args[0] == "diff":
		out, err := exec.Command("git", "diff", "--name-only", args[1]+"...HEAD").Output()
		if err != nil {
			fail(fmt.Errorf("git diff %s...HEAD: %w", args[1], err))
		}
		return append([]string{}, strings.Fields(string(out))...), true
	}
	return nil, false
}

// summarize prints the human report and returns whether any finding is gated.
func summarize(w io.Writer, rep report) bool {
	pf(w, "promptlint: %d items, %d agents, %d overlapping tool pairs (tokens ~ %s)\n\n",
		len(rep.Items), len(rep.Agents), len(rep.Overlaps), rep.Estimator)
	items := slices.Clone(rep.Items)
	slices.SortFunc(items, func(a, b item) int { return b.Tokens - a.Tokens })
	pf(w, "largest items (tokens / budget):\n")
	for _, it := range items[:min(12, len(items))] {
		pf(w, "  %6d / %-6d %-6s %s\n", it.Tokens, it.Budget, it.Kind, it.ID)
	}
	agents := slices.Clone(rep.Agents)
	slices.SortFunc(agents, func(a, b agentSpec) int { return b.Total - a.Total })
	pf(w, "\nper-agent surface (prompt + tools + skill list = total / budget):\n")
	for _, a := range agents {
		pf(w, "  %-18s %5d + %5d + %4d = %5d / %-5d  %d tools\n",
			a.Name, a.PromptTokens, a.ToolTokens, a.SkillTokens, a.Total, a.Budget, len(a.Tools))
	}
	pf(w, "\noverlapping tool pairs (similarity, agents offered both):\n")
	for _, o := range rep.Overlaps {
		pf(w, "  %.3f %-38s %s\n", o.Similarity, o.A+" ~ "+o.B, offeredBy(o))
	}
	return printFindings(w, rep.Findings)
}

func printFindings(w io.Writer, fs []finding) bool {
	gated := 0
	if len(fs) > 0 {
		pf(w, "\nfindings:\n")
	}
	for _, f := range fs {
		mark := "info"
		switch {
		case f.Gated:
			mark, gated = "FAIL", gated+1
		case f.warn:
			mark = "warn"
		}
		pf(w, "  %-4s %-26s %s: %s\n", mark, f.Rule, f.Subject, f.Message)
	}
	if gated > 0 {
		pf(w, "\npromptlint: %d gated finding(s); fix them or edit %s with a reason\n", gated, configPath)
		return true
	}
	pf(w, "\npromptlint: ok\n")
	return false
}

func offeredBy(o overlap) string {
	switch {
	case len(o.CoOfferedBy) == 0:
		return "never offered together"
	case o.Allowed == "":
		return "NOT ALLOWLISTED: " + strings.Join(o.CoOfferedBy, " ")
	}
	return "allowlisted: " + strings.Join(o.CoOfferedBy, " ")
}

// pf writes report text; a failed stdout write has nowhere better to be reported.
func pf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
