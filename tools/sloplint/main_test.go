package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// The -U0 diff parser is load-bearing for the gate: a misparsed hunk
// silently un-gates a function, so the parsing and overlap logic get
// table tests even though they are 30 lines.

func TestChangedRanges(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string][]rng
	}{
		{"empty", "", map[string][]rng{}},
		{"single hunk",
			"diff --git a/x/y.go b/x/y.go\n@@ -10,2 +10,3 @@ func f() {\n+line\n",
			map[string][]rng{"x/y.go": {{10, 12}}}},
		{"omitted count means one line",
			"diff --git a/a.go b/a.go\n@@ -5 +5 @@\n+line\n",
			map[string][]rng{"a.go": {{5, 5}}}},
		{"two files",
			"diff --git a/p.go b/p.go\n@@ -1,1 +1,2 @@\n+x\ndiff --git a/q/r.go b/q/r.go\n@@ -20,0 +21,4 @@\n+a\n",
			map[string][]rng{"p.go": {{1, 2}}, "q/r.go": {{21, 24}}}},
		{"quoted path",
			"diff --git \"a/we ird.go\" \"b/we ird.go\"\n@@ -1,1 +1,1 @@\n",
			map[string][]rng{"we ird.go": {{1, 1}}}},
		{"hunk before any file header is dropped",
			"@@ -1,1 +1,1 @@\n+x\ndiff --git a/z.go b/z.go\n@@ -7,1 +7,1 @@\n",
			map[string][]rng{"z.go": {{7, 7}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := changedRanges(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d: %v", len(got), len(tc.want), got)
			}
			for f, w := range tc.want {
				if len(got[f]) != len(w) {
					t.Fatalf("%s = %v, want %v", f, got[f], w)
				}
				for i := range w {
					if got[f][i] != w[i] {
						t.Fatalf("%s[%d] = %v, want %v", f, i, got[f][i], w[i])
					}
				}
			}
		})
	}
}

func TestOverlaps(t *testing.T) {
	rs := []rng{{10, 12}, {20, 24}}
	cases := []struct {
		s, e   int
		want   bool
		reason string
	}{
		{11, 13, true, "interior"},
		{13, 15, false, "past the first range, before the second"},
		{10, 12, true, "exact"},
		{9, 10, true, "touching from before"},
		{12, 20, true, "bridging both ranges"},
		{1, 9, false, "before everything"},
	}
	for _, tc := range cases {
		if got := overlaps(tc.s, tc.e, rs); got != tc.want {
			t.Errorf("overlaps(%d,%d) = %v, want %v (%s)", tc.s, tc.e, got, tc.want, tc.reason)
		}
	}
}

func TestCcAllowed(t *testing.T) {
	// a function with CC > 15: ten distinct if/else branches
	body := "func big(x int) int {\n"
	for i := 0; i < 16; i++ {
		body += "\tif x == " + strconv.Itoa(i) + " {\n\t\treturn " + strconv.Itoa(i) + "\n\t}\n"
	}
	body += "\treturn -1\n}\n"
	parse := func(doc string) ([]byte, *token.FileSet, *ast.FuncDecl) {
		src := "package main\n"
		if doc != "" {
			src += doc + "\n"
		}
		src += body
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "t.go", []byte(src), 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				return []byte(src), fset, fd
			}
		}
		t.Fatal("no func")
		return nil, nil, nil
	}
	src0, fset0, big := parse("")
	if cc := funcCC(big); cc <= 15 {
		t.Fatalf("fixture CC = %d, want > 15", cc)
	}
	if ccAllowed(src0, fset0, big) {
		t.Error("no doc: ccAllowed = true, want false")
	}
	if s, fs, fd := parse("// some doc\n"); ccAllowed(s, fs, fd) {
		t.Error("doc without directive: ccAllowed = true, want false")
	}
	if s, fs, fd := parse("// sloplint: cc-allow flat protocol decoder\n"); !ccAllowed(s, fs, fd) {
		t.Error("directed doc: ccAllowed = false, want true")
	}
	if s, fs, fd := parse("// doc line above\n// sloplint: cc-allow flat protocol decoder\n"); !ccAllowed(s, fs, fd) {
		t.Error("directive under a doc line: ccAllowed = false, want true")
	}
	if s, fs, fd := parse("// sloplint: cc-allow\n"); ccAllowed(s, fs, fd) {
		t.Error("directive without reason: ccAllowed = true, want false")
	}
}
