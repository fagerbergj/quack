// sloplint: the slop-gate half no standard tool covers - a comment-run
// gate over diff-touched lines (diff <ref>) + a report-only repo ledger (repo); CC/dup gating is golangci-lint's (.golangci.yml).
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	root, _ := os.Getwd()
	switch os.Args[1] {
	case "diff":
		if len(os.Args) < 3 {
			usage()
		}
		if diffMode(os.Args[2], root) {
			os.Exit(1)
		}
		fmt.Println("sloplint: ok")
	case "repo":
		if repoMode(root) {
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: sloplint <diff <git-ref> | repo>")
	os.Exit(2)
}

// ---------- comment-run gate (the only diff check) ----------

const maxCommentRun = 2

type rng [2]int

func diffMode(ref, root string) bool {
	// Hunk-aware: the repo has ~1800 pre-existing >2-line comment runs,
	// so only runs the diff itself touches are gated (changed-code-only).
	out, err := exec.Command("git", "diff", "-U0", ref+"...HEAD").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			fmt.Fprintln(os.Stderr, "git diff failed:", strings.TrimSpace(string(ee.Stderr)))
		} else {
			fmt.Fprintln(os.Stderr, "git diff failed:", err)
		}
		return true
	}
	fail := false
	for p, rs := range changedRanges(string(out)) {
		if checkSlopFile(filepath.Join(root, p), p, rs) {
			fail = true
		}
	}
	return fail
}

// changedRanges: -U0 diff -> changed line ranges per file (new-file numbers).
func changedRanges(out string) map[string][]rng {
	changed := map[string][]rng{}
	file := ""
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, `diff --git "a/`):
			// git quotes paths containing spaces etc. (escaped quotes in
			// filenames are not supported - none exist here)
			inner := strings.TrimPrefix(l, `diff --git "a/`)
			if _, b, ok := strings.Cut(inner, `" `); ok {
				if bb, _, ok := strings.Cut(strings.TrimPrefix(b, `"b/`), `"`); ok {
					file = bb
				}
			}
		case strings.HasPrefix(l, "diff --git a/"):
			file = strings.TrimPrefix(strings.SplitN(l, " b/", 2)[1], " b/")
		}
		if file == "" && l == "" {
			continue
		}
		rest, ok := strings.CutPrefix(l, "@@ -")
		if !ok || file == "" {
			continue
		}
		// rest: "a,b +c,d @@ ..." (an omitted count means 1 line)
		var start, n int
		if parts := strings.Fields(rest); len(parts) >= 2 {
			plus := strings.TrimPrefix(parts[1], "+")
			if idx := strings.Index(plus, ","); idx >= 0 {
				start, _ = strconv.Atoi(plus[:idx])
				n, _ = strconv.Atoi(plus[idx+1:])
			} else {
				start, _ = strconv.Atoi(plus)
			}
		}
		if start == 0 {
			start = 1
		}
		if n == 0 {
			n = 1
		}
		changed[file] = append(changed[file], rng{start, start + n - 1})
	}
	return changed
}

func overlaps(s, e int, rs []rng) bool {
	for _, r := range rs {
		if s <= r[1] && e >= r[0] {
			return true
		}
	}
	return false
}

func checkSlopFile(path, rel string, rs []rng) bool {
	if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") ||
		strings.Contains(rel, "/testdata/") || strings.Contains(rel, "/schema/") {
		return false
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return false
	}
	return commentRunsFail(fset, f, rel, rs) || funcsFail(fset, f, rel, rs)
}

func commentRunsFail(fset *token.FileSet, f *ast.File, rel string, rs []rng) bool {
	fail := false
	for _, cg := range f.Comments {
		if len(cg.List) <= maxCommentRun {
			continue
		}
		s, e := fset.Position(cg.Pos()).Line, fset.Position(cg.End()).Line
		if overlaps(s, e, rs) {
			fmt.Printf("%s:%d: %d-line comment run touches changed lines\n", rel, s, len(cg.List))
			fail = true
		}
	}
	return fail
}

// CC on changed functions: golangci --new-from-rev anchors findings at the
// decl line and misses extensions past CC 15; the hunk ranges catch them.
func funcsFail(fset *token.FileSet, f *ast.File, rel string, rs []rng) bool {
	fail := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		s, e := fset.Position(fd.Body.Pos()).Line, fset.Position(fd.End()).Line
		if !overlaps(s, e, rs) {
			continue
		}
		if cc := funcCC(fd); cc > 15 {
			fmt.Printf("%s:%d: %s CC %d > 15 (changed)\n", rel, s, fd.Name.Name, cc)
			fail = true
		}
	}
	return fail
}

// ---------- report-only repo mode ----------

func goFiles(root string) []string {
	var files []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && (d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".") ||
				d.Name() == "testdata" || d.Name() == "vendor" || d.Name() == "dist" ||
				d.Name() == "generated") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") &&
			!strings.Contains(p, "/schema/") { // generated code is out of all counts
			files = append(files, p)
		}
		return nil
	})
	return files
}

func sloc(src string) int {
	n := 0
	for _, l := range strings.Split(src, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

type fnInfo struct {
	name, file string
	cc, sl     int
}

// funcCC mirrors fzipp/gocyclo's counting (what golangci-lint v2 runs):
// If/For/Range/case/comm branches plus && / || - no SelectStmt, no calls.
func funcCC(fd *ast.FuncDecl) int {
	cc := 1
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.CaseClause,
			*ast.CommClause:
			cc++
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				cc++
			}
		}
		return true
	})
	return cc
}

func repoMode(root string) bool {
	var all []fnInfo
	var totalMass, overMass float64
	over := 0
	for _, file := range goFiles(root) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			continue
		}
		rel := strings.TrimPrefix(file, root+"/")
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Type == nil {
				continue
			}
			cc := funcCC(fd)
			sl := fset.Position(fd.End()).Line - fset.Position(fd.Body.Pos()).Line + 1
			all = append(all, fnInfo{fd.Name.Name, rel, cc, sl})
			m := float64(cc) * float64(sl)
			totalMass += m
			if cc > 10 {
				over++
				overMass += m
			}
		}
	}
	if len(all) == 0 {
		fmt.Fprintln(os.Stderr, "no go functions found under", root)
		return true
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].cc*all[i].sl > all[j].cc*all[j].sl
	})
	top := all
	if len(top) > 15 {
		top = top[:15]
	}
	fmt.Printf("functions: %d total, %d with CC > 10 (\"worth splitting\")\n", len(all), over)
	fmt.Printf("mass concentration (CC>10 share of CC*SLOC): %.0f%%  (skill: ~30%% is maintenance-level, agent code runs ~68%%)\n",
		100*overMass/totalMass)
	fmt.Println("\ntop CC * SLOC:")
	for _, fn := range top {
		fmt.Printf("  CC%-4d body%-5d  %s  %s\n", fn.cc, fn.sl, fn.file, fn.name)
	}
	return printDupl(root) || printTestRatio(root)
}

// stmtLines: non-blank, non-comment lines normalized to single spaces.
func stmtLines(root string) map[string][]string {
	files := map[string][]string{}
	for _, file := range goFiles(root) {
		src, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var st []string
		for _, l := range strings.Split(string(src), "\n") {
			t := strings.TrimSpace(l)
			if t != "" && !strings.HasPrefix(t, "//") {
				st = append(st, strings.Join(strings.Fields(t), " "))
			}
		}
		files[strings.TrimPrefix(file, root+"/")] = st
	}
	return files
}

// scanRuns finds 6-line runs repeated within a file and across files.
func scanRuns(files map[string][]string) (pairs [][4]any, cross int) {
	const runLen = 6
	type run struct {
		f string
		i int
	}
	first := map[string]run{}
	runIn := map[string][]run{}
	for f, st := range files {
		for i := 0; i+runLen <= len(st); i++ {
			key := strings.Join(st[i:i+runLen], "\x00")
			runIn[key] = append(runIn[key], run{f, i})
			if prev, ok := first[key]; ok && prev.f == f && i-prev.i > 2 {
				pairs = append(pairs, [4]any{f, prev.i + 1, i + 1, runLen})
			}
			if _, ok := first[key]; !ok {
				first[key] = run{f, i}
			}
		}
	}
	for _, rs := range runIn {
		fs := map[string]bool{}
		for _, r := range rs {
			fs[r.f] = true
		}
		if len(fs) > 1 {
			cross++
		}
	}
	return
}

func printDupl(root string) bool {
	pairs, cross := scanRuns(stmtLines(root))
	fmt.Printf("\nrepeated 6-line statement runs: %d intra-file pairs, %d runs shared by 2+ files\n", len(pairs), cross)
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i][0].(string) < pairs[j][0].(string)
	})
	for i, p := range pairs {
		if i >= 10 {
			fmt.Printf("  ... and %d more\n", len(pairs)-10)
			break
		}
		a, b, n := p[1].(int), p[2].(int), p[3].(int)
		fmt.Printf("  %s  lines %d-%d and %d-%d\n", p[0], a, a+n-1, b, b+n-1)
	}
	return false
}

// testCounts: logic vs test SLOC per package.
func testCounts(root string) map[string]struct{ logic, test int } {
	pkgs := map[string]struct{ logic, test int }{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.Contains(p, "/testdata/") || strings.Contains(p, "/schema/") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		c := pkgs[filepath.Dir(rel)]
		if strings.HasSuffix(p, "_test.go") {
			c.test += sloc(string(src))
		} else {
			c.logic += sloc(string(src))
		}
		return nil
	})
	return pkgs
}

func printTestRatio(root string) bool {
	pkgs := testCounts(root)
	var names []string
	for n := range pkgs {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println("\ntest vs logic (SLOC):")
	for _, n := range names {
		c := pkgs[n]
		fmt.Printf("  %-34s %6d total   %4.0f%% test\n", n, c.logic+c.test, 100*float64(c.test)/float64(c.logic+c.test))
	}
	return false
}
