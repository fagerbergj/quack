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
	// sixteen if branches: CC 17, over the gate
	body := "func big(x int) int {\n"
	for n := 0; n < 16; n++ {
		body += "\tif x == " + strconv.Itoa(n) + " {\n\t\treturn " + strconv.Itoa(n) + "\n\t}\n"
	}
	body += "\treturn -1\n}\n"
	// preamble lines sit directly above the declaration (adjacent doc block)
	build := func(preamble ...string) ([]byte, *token.FileSet, *ast.FuncDecl) {
		t.Helper()
		src := "package main\n"
		for _, p := range preamble {
			src += p + "\n"
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
	src0, fset0, big := build()
	if cc := funcCC(big); cc <= 15 {
		t.Fatalf("fixture CC = %d, want > 15", cc)
	}
	_ = src0
	_ = fset0
	cases := []struct {
		name      string
		preamble  []string
		wantAllow bool
	}{
		{"no doc", nil, false},
		{"doc without directive", []string{"// some doc"}, false},
		{"adjacent directive with reason", []string{"// sloplint: cc-allow flat protocol decoder"}, true},
		{"directive under a doc line", []string{"// doc line", "// sloplint: cc-allow flat protocol decoder"}, true},
		{"directive exactly three above", []string{"// sloplint: cc-allow flat protocol decoder", "// doc", "// doc"}, true},
		{"directive four above is out of window", []string{"// sloplint: cc-allow flat protocol decoder", "// doc", "// doc", "// doc"}, false},
		{"directive without reason", []string{"// sloplint: cc-allow"}, false},
		{"mark on a non-comment line does not exempt", []string{`var marker = "sloplint: cc-allow x"`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, fset, fd := build(tc.preamble...)
			if got := ccAllowed(src, fset, fd); got != tc.wantAllow {
				t.Errorf("ccAllowed = %v, want %v", got, tc.wantAllow)
			}
		})
	}
}

func TestFuncsFailCcAllowWiring(t *testing.T) {
	// the wiring ccAllowed alone cannot pin: a CC>15 function whose body a
	// hunk touches is exempt iff a directed line sits within 3 above
	src := "package main\n" +
		"// sloplint: cc-allow flat protocol decoder\n" +
		"func big(x int) int {\n"
	for n := 0; n < 16; n++ {
		src += "\tif x == " + strconv.Itoa(n) + " {\n\t\treturn " + strconv.Itoa(n) + "\n\t}\n"
	}
	src += "\treturn -1\n}\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "t.go", []byte(src), parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	// body spans lines 3..21; the hunk touches the middle
	if funcsFail(fset, f, "t.go", []rng{{10, 12}}, []byte(src)) {
		t.Error("directed function with an overlapping hunk: funcsFail = true, want false")
	}
	// no directive: same hunk fails
	src2 := "package main\n" + src[len("package main\n// sloplint: cc-allow flat protocol decoder\n"):]
	fset2 := token.NewFileSet()
	f2, err := parser.ParseFile(fset2, "t.go", []byte(src2), parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if !funcsFail(fset2, f2, "t.go", []rng{{10, 12}}, []byte(src2)) {
		t.Error("undirected CC>15 function with an overlapping hunk: funcsFail = false, want true")
	}
}
