package vetting

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// requireAstGrep skips a test that needs the REAL binary when this
// environment doesn't have one - packageDeclarationCriterion's own contract
// (loud skip, never fail the gate) applies equally to exercising it in CI;
// see TestPackageDeclarationCriterion_AstGrepMissingLoudSkip for the case
// that specifically tests absence.
func requireAstGrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH; skipping (install it - e.g. `npm i -g @ast-grep/cli` - to exercise this test for real)")
	}
}

// testPackageCheckConfig provisions a Config whose node workdir is a real
// directory tree (files under dir/<paths>), with a `.git` marker so
// checksDir's derive-mode FindRepos locates it directly - the same
// resolution checksPassCriterion uses for build/vet/test checks.
func testPackageCheckConfig(t *testing.T, checkCommands []string, files map[string]string) (Config, string) {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := j.EnsureDir("u1", "chat1", workspace.NodeDir("impl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Config{
		NodeID: "impl", ChatID: "chat1", WorkspaceUserID: "u1",
		Workspace: j, WorkspaceCaps: workspace.DefaultCaps(),
		CheckCommands: checkCommands,
	}, dir
}

var withAstGrepAllowed = []string{"go build", "ast-grep"}

// 1. A Kotlin file whose package does not match its directory FAILS, and the
// failure names the file and both paths.
func TestPackageDeclarationCriterion_KotlinMismatchNamesFileAndPaths(t *testing.T) {
	requireAstGrep(t)
	cfg, _ := testPackageCheckConfig(t, withAstGrepAllowed, map[string]string{
		"app/src/main/java/com/wit/jasonfagerberg/ui/theme/Theme.kt": "package com.wit.jasonfargerberg.ui.theme\n\nclass Theme\n",
	})
	c, ok := packageDeclarationCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("want ok=true - the declared package does not match its directory")
	}
	if c.Score != 0 {
		t.Errorf("Score = %v, want 0", c.Score)
	}
	for _, want := range []string{
		"app/src/main/java/com/wit/jasonfagerberg/ui/theme/Theme.kt", // the actual file
		"com.wit.jasonfargerberg.ui.theme",                           // the declared (wrong) package
		"com/wit/jasonfagerberg/ui/theme",                            // its actual directory
	} {
		if !strings.Contains(c.Reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", c.Reason, want)
		}
	}
}

// 2. Correct Kotlin, Java and Go files PASS.
func TestPackageDeclarationCriterion_CorrectFilesPass(t *testing.T) {
	requireAstGrep(t)
	cfg, _ := testPackageCheckConfig(t, withAstGrepAllowed, map[string]string{
		"app/src/main/java/com/wit/jasonfagerberg/ui/theme/Theme.kt": "package com.wit.jasonfagerberg.ui.theme\n\nclass Theme\n",
		"src/main/java/com/example/app/Main.java":                    "package com.example.app;\n\npublic class Main {}\n",
		"internal/foo/foo.go":                                        "package foo\n\nfunc Bar() {}\n",
	})
	if c, ok := packageDeclarationCriterion(context.Background(), cfg); ok {
		t.Fatalf("want ok=false (no failure) for correctly-placed files, got %+v", c)
	}
}

// 3. `package main` in Go, and Go `_test` package variants, do not trip it.
func TestPackageDeclarationCriterion_GoMainAndExternalTestExempt(t *testing.T) {
	requireAstGrep(t)
	cfg, _ := testPackageCheckConfig(t, withAstGrepAllowed, map[string]string{
		"cmd/tool/main.go":         "package main\n\nfunc main() {}\n",
		"internal/foo/foo.go":      "package foo\n\nfunc Bar() {}\n",
		"internal/foo/foo_test.go": "package foo_test\n\nimport \"testing\"\n\nfunc TestBar(t *testing.T) {}\n",
	})
	if c, ok := packageDeclarationCriterion(context.Background(), cfg); ok {
		t.Fatalf("want ok=false - main and the external _test package are legitimate, got %+v", c)
	}
}

// 4. A repo with NO Kotlin/Java/Go files skips cleanly rather than passing
// vacuously or failing. This never even reaches ast-grep (hasCandidateSource
// short-circuits first), so it runs regardless of whether ast-grep is
// installed here.
func TestPackageDeclarationCriterion_NoCandidateFilesSkipsCleanly(t *testing.T) {
	cfg, _ := testPackageCheckConfig(t, withAstGrepAllowed, map[string]string{
		"README.md":    "# hello\n",
		"main.py":      "print('hi')\n",
		"package.json": `{"name": "x"}`,
	})
	if c, ok := packageDeclarationCriterion(context.Background(), cfg); ok {
		t.Fatalf("want ok=false - nothing to scan, got %+v", c)
	}
}

// 5. Android resource references, method chains, URLs, and prose containing
// slashes cannot possibly trip this - the exact regression #685 shipped and
// #695 reverted. Planted as ordinary content inside real Kotlin files (not
// matched by the `package $PKG` grammar pattern at all), alongside a
// genuinely CORRECT package declaration.
func TestPackageDeclarationCriterion_ProseAndResourceRefsNeverTrip(t *testing.T) {
	requireAstGrep(t)
	cfg, _ := testPackageCheckConfig(t, withAstGrepAllowed, map[string]string{
		"app/src/main/java/com/wit/jasonfagerberg/ui/theme/Theme.kt": strings.Join([]string{
			"package com.wit.jasonfagerberg.ui.theme",
			"",
			"// see @color/colorLightGray, @string/notifications, @string/theme, @string/time",
			"// SettingsShim.edit().putBoolean()/putInt()",
			"// https://example.com/some/path/that/is/not/code",
			"class Theme",
			"",
		}, "\n"),
	})
	if c, ok := packageDeclarationCriterion(context.Background(), cfg); ok {
		t.Fatalf("want ok=false - none of the planted prose is a package declaration, got %+v", c)
	}
}

// 6. ast-grep missing from PATH => loud skip, gate not failed.
func TestPackageDeclarationCriterion_AstGrepMissingLoudSkip(t *testing.T) {
	cfg, _ := testPackageCheckConfig(t, withAstGrepAllowed, map[string]string{
		"internal/foo/foo.go": "package wrongname\n",
	})
	t.Setenv("PATH", t.TempDir()) // no ast-grep resolvable anywhere
	if c, ok := packageDeclarationCriterion(context.Background(), cfg); ok {
		t.Fatalf("want ok=false - ast-grep absent must skip, never fail the gate; got %+v", c)
	}
}

// 7. A generated/vendored directory (workspace.SkipDir) is not scanned - a
// mismatched package under vendor/node_modules/build/etc. must never surface.
func TestPackageDeclarationCriterion_VendoredDirNotScanned(t *testing.T) {
	requireAstGrep(t)
	cfg, _ := testPackageCheckConfig(t, withAstGrepAllowed, map[string]string{
		"internal/foo/foo.go":                "package foo\n",
		"vendor/thirdparty/vendored.go":      "package totallywrongname\n",
		"internal/foo/node_modules/x/gen.go": "package alsowrong\n",
	})
	if c, ok := packageDeclarationCriterion(context.Background(), cfg); ok {
		t.Fatalf("want ok=false - the vendored mismatch must never surface, got %+v", c)
	}
}

// The allowlist is the operator's off switch, same as every other derived
// check: without "ast-grep" in CheckCommands, this never runs even when the
// binary is present and a real mismatch exists.
func TestPackageDeclarationCriterion_NotAllowlistedSkips(t *testing.T) {
	cfg, _ := testPackageCheckConfig(t, []string{"go build"}, map[string]string{ // no "ast-grep" entry
		"internal/foo/foo.go": "package wrongname\n",
	})
	if c, ok := packageDeclarationCriterion(context.Background(), cfg); ok {
		t.Fatalf("want ok=false - ast-grep is not in CheckCommands, got %+v", c)
	}
}

// --- checkJVMPackage / checkGoPackage: pure logic, no ast-grep needed ---

func TestCheckJVMPackage(t *testing.T) {
	cases := []struct {
		name, relDir, pkg string
		wantMatch         bool
	}{
		{"exact depth", "com/wit/jasonfagerberg/ui/theme", "com.wit.jasonfagerberg.ui.theme", true},
		{"extra source-root prefix", "app/src/main/java/com/wit/jasonfagerberg/ui/theme", "com.wit.jasonfagerberg.ui.theme", true},
		{"misspelled directory", "com/wit/jasonfargerberg/ui/theme", "com.wit.jasonfagerberg.ui.theme", false},
		{"misspelled package", "com/wit/jasonfagerberg/ui/theme", "com.wit.jasonfargerberg.ui.theme", false},
		{"directory too shallow", "com/wit", "com.wit.jasonfagerberg.ui.theme", false},
		{"root file, non-empty package", ".", "com.example", false},
		{"single-segment package", "app/src/main/java/theme", "theme", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkJVMPackage(c.relDir, c.pkg) == ""
			if got != c.wantMatch {
				t.Errorf("checkJVMPackage(%q, %q) match = %v, want %v", c.relDir, c.pkg, got, c.wantMatch)
			}
		})
	}
}

func TestCheckGoPackage(t *testing.T) {
	cases := []struct {
		name, relFile, relDir, pkg, repoDir string
		wantMatch                           bool
	}{
		{"matches dir name", "internal/foo/foo.go", "internal/foo", "foo", "/repo", true},
		{"main always exempt", "cmd/tool/main.go", "cmd/tool", "main", "/repo", true},
		{"mismatched name", "internal/foo/foo.go", "internal/foo", "bar", "/repo", false},
		{"external test variant", "internal/foo/foo_test.go", "internal/foo", "foo_test", "/repo", true},
		{"internal test file keeps dir name", "internal/foo/foo_test.go", "internal/foo", "foo", "/repo", true},
		{"_test suffix on a non-test file is still wrong", "internal/foo/foo.go", "internal/foo", "foo_test", "/repo", false},
		{"root file uses repo dir's own name", "main.go", ".", "quackrepo", "/x/quackrepo", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkGoPackage(c.relFile, c.relDir, c.pkg, c.repoDir) == ""
			if got != c.wantMatch {
				t.Errorf("checkGoPackage(%q,%q,%q,%q) match = %v, want %v", c.relFile, c.relDir, c.pkg, c.repoDir, got, c.wantMatch)
			}
		})
	}
}

func TestHasCandidateSource(t *testing.T) {
	dir := t.TempDir()
	if hasCandidateSource(dir) {
		t.Fatal("empty dir should have no candidate source")
	}
	if err := os.MkdirAll(filepath.Join(dir, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vendor", "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasCandidateSource(dir) {
		t.Fatal("a .go file only under vendor/ should not count as candidate source")
	}
	if err := os.WriteFile(filepath.Join(dir, "real.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasCandidateSource(dir) {
		t.Fatal("a real top-level .go file should count as candidate source")
	}
}
