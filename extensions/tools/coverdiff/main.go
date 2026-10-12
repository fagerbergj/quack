// coverdiff gates changed-line coverage: lines added in `diff <ref> <root> <module>` must be >= minPct
// covered; test files and statement-less lines are outside the denominator.
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

const (
	minPct = 70.0
	// modulePrefix: the import-path prefix the profile records carry.
	modulePrefix = "quack-extensions/"
)

var (
	profileRe = regexp.MustCompile(`^(.*)\.go:(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$`)
	hunkRe    = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)
)

type miss struct {
	line int
	stmt int
}

func main() {
	if len(os.Args) != 5 || os.Args[1] != "diff" {
		fmt.Fprintln(os.Stderr, "usage: coverdiff diff <git-ref> <repo-root> <module>")
		os.Exit(2)
	}
	if !gate(os.Args[2], os.Args[3], os.Args[4]) {
		os.Exit(1)
	}
}

func gate(ref, root, module string) bool {
	diffArgs := []string{"-C", root, "diff", "-U0", ref + "...HEAD", "--", module}
	out, err := exec.Command("git", diffArgs...).Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git diff failed:", err)
		return false
	}
	changed := changedFiles(string(out), module) // repo-relative path -> new-file line numbers
	if len(changed) == 0 {
		fmt.Println("coverdiff: no changed non-test Go files in", module)
		return true
	}
	tmp, err := os.CreateTemp("", "coverdiff-*.out")
	if err != nil {
		fmt.Fprintln(os.Stderr, "coverdiff:", err)
		return false
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()
	cmd := exec.Command("go", "-C", root+"/"+module, "test", "-count=1", "-coverprofile="+tmp.Name(), "./...")
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
	return report(num, den, misses, module)
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
		// Overlap semantics: the statement is gated if any line of its
		// range was changed.
		start, _ := strconv.Atoi(m[2])
		end, _ := strconv.Atoi(m[4])
		numStmts, _ := strconv.Atoi(m[6])
		count, _ := strconv.Atoi(m[7])
		hit := false
		for ln := start; ln <= end; ln++ {
			if rs[ln] {
				hit = true
				break
			}
		}
		if !hit {
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

func report(num, den int, misses map[string][]miss, module string) bool {
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
	fmt.Printf("coverdiff: ok - changed-line coverage %.1f%% (%d/%d statements) in %s\n", pct, num, den, module)
	return true
}

// relRepoPath: "github.com/fagerbergj/quack-extensions/github/app" -> "github/app".
func relRepoPath(profilePath string) string {
	if i := strings.Index(profilePath, modulePrefix); i >= 0 {
		return profilePath[i+len(modulePrefix):]
	}
	return profilePath
}

func changedFiles(diff, module string) map[string]map[int]bool {
	res := map[string]map[int]bool{}
	cur := ""
	prefix := module + "/"
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "+++ b/") {
			p := strings.TrimPrefix(l, "+++ b/")
			if !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, ".gen.go") {
				cur = ""
				continue
			}
			k := strings.TrimSuffix(p, ".go")
			res[k] = map[int]bool{}
			cur = k
			continue
		}
		if cur == "" {
			continue
		}
		if m := hunkRe.FindStringSubmatch(l); m != nil {
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

func sortedMissFiles(m map[string][]miss) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
