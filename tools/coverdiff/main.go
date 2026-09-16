// coverdiff gates changed-line statement coverage: lines added in
// `diff <ref>` must sit at >= minPct covered; test files, generated
// dirs and statement-less lines are outside the denominator.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const minPct = 70.0

var (
	// profile line: <file>.go:start.col,end.col numstmt count
	profileRe = regexp.MustCompile(`^(.*)\.go:(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$`)
	hunkRe    = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)
	// The standard Go generated-code marker (golang.org/s/generatedcode): any whole line
	// matching this, anywhere in the file, marks it generated regardless of directory.
	generatedRe = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)
)

type miss struct {
	line int
	stmt int
}

func main() {
	if len(os.Args) != 3 || os.Args[1] != "diff" {
		fmt.Fprintln(os.Stderr, "usage: coverdiff diff <git-ref>")
		os.Exit(2)
	}
	if !gate(os.Args[2]) {
		os.Exit(1)
	}
}

func gate(ref string) bool {
	out, err := exec.Command("git", "diff", "-U0", ref+"...HEAD").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git diff failed:", err)
		return false
	}
	changed := changedFiles(string(out)) // new-file line numbers per repo-relative .go path
	if len(changed) == 0 {
		fmt.Println("coverdiff: no changed non-test Go files")
		return true
	}
	tmp, err := os.CreateTemp("", "coverdiff-*.out")
	if err != nil {
		fmt.Fprintln(os.Stderr, "coverdiff:", err)
		return false
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()
	cmd := exec.Command("go", "test", "-count=1", "-coverprofile="+tmp.Name(), "./cmd/...", "./internal/...")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "go test failed (coverage prerequisite):", err)
		return false
	}
	var num, den int
	misses := map[string][]miss{}
	if err := matchProfile(tmp.Name(), changed, &num, &den, misses); err != nil {
		fmt.Fprintln(os.Stderr, "coverdiff:", err)
		return false
	}
	return report(num, den, misses)
}

// matchProfile: walk the coverprofile, count statements that overlap
// changed lines, collect the uncovered ones.
func matchProfile(path string, changed map[string]map[int]bool, num, den *int, misses map[string][]miss) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := profileRe.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue // mode: / import records
		}
		path := relRepoPath(m[1])
		rs, ok := changed[path]
		if !ok {
			continue
		}
		start, _ := strconv.Atoi(m[2])
		end, _ := strconv.Atoi(m[4])
		numStmts, _ := strconv.Atoi(m[6])
		count, _ := strconv.Atoi(m[7])
		overlap := false
		for ln := start; ln <= end; ln++ {
			if rs[ln] {
				overlap = true
				break
			}
		}
		if !overlap {
			continue
		}
		*den += numStmts
		if count > 0 {
			*num += numStmts
		} else {
			misses[path] = append(misses[path], miss{start, numStmts})
		}
	}
	return nil
}

func report(num, den int, misses map[string][]miss) bool {
	if den == 0 {
		fmt.Println("coverdiff: changed lines carry no statements")
		return true
	}
	pct := float64(num) * 100 / float64(den)
	if pct < minPct {
		fmt.Fprintf(os.Stderr, "coverdiff: changed-line coverage %.1f%% < %.0f%% (%d/%d statements)\n", pct, minPct, num, den)
		for _, p := range sortedMissFiles(misses) {
			for _, ms := range misses[p] {
				fmt.Fprintf(os.Stderr, "  uncovered: %s:%d (%d stmts)\n", p, ms.line, ms.stmt)
			}
		}
		return false
	}
	fmt.Printf("coverdiff: ok - changed-line coverage %.1f%% (%d/%d statements)\n", pct, num, den)
	return true
}

// relRepoPath: "github.com/fagerbergj/quack/internal/x/y" -> "internal/x/y" (no .go suffix; the diff side strips it to match).
func relRepoPath(profilePath string) string {
	if i := strings.Index(profilePath, "quack/"); i >= 0 {
		return profilePath[i+len("quack/"):]
	}
	return profilePath
}

func changedFiles(diff string) map[string]map[int]bool {
	res := map[string]map[int]bool{}
	cur := ""
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "+++ b/") {
			p := strings.TrimPrefix(l, "+++ b/")
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				continue
			}
			if isGenerated(p) {
				continue
			}
			res[strings.TrimSuffix(p, ".go")] = map[int]bool{}
			cur = strings.TrimSuffix(p, ".go")
			continue
		}
		if m := hunkRe.FindStringSubmatch(l); m != nil && cur != "" {
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			for i := 0; i < n; i++ {
				res[cur][start+i] = true
			}
		}
	}
	return res
}

func isGenerated(p string) bool {
	if strings.HasPrefix(p, "internal/schema/") || strings.HasPrefix(p, "frontend/src/generated/") {
		return true
	}
	return hasGeneratedHeader(p)
}

// hasGeneratedHeader reports whether p (repo-relative, cwd = repo root) carries the
// standard "// Code generated ... DO NOT EDIT." marker - catches a generated dir this
// list doesn't yet name (e.g. internal/langfuse/langfusegen) without editing this list.
func hasGeneratedHeader(p string) bool {
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	return generatedRe.Match(b)
}

func sortedMissFiles(m map[string][]miss) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
