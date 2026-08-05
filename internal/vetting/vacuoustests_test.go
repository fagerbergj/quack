package vetting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAndCommit writes files into repo and commits them as "worker change" -
// the commit vacuousTestsCriterion diffs against the clone's original HEAD.
func writeAndCommit(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "worker change")
}

// Regression fixture: issue #716 - the trust gate passed a Kotlin test that
// declares a lambda, invokes it, and asserts the lambda it just invoked ran.
// SettingsScreen is never named.
const vacuousKotlinTest = "package com.example.app\n\n" +
	"import org.junit.Test\n" +
	"import com.google.common.truth.Truth.assertThat\n\n" +
	"class SettingsScreenTest {\n" +
	"    @Test\n" +
	"    fun `toggling BAC notification fires callback`() {\n" +
	"        var bacCalled = false\n" +
	"        val cb: (Boolean) -> Unit = { _ -> bacCalled = true }\n" +
	"        cb(true)\n" +
	"        assertThat(bacCalled).isTrue()\n" +
	"    }\n" +
	"}\n"

const settingsScreenProd = "package com.example.app\n\n" +
	"@Composable\n" +
	"fun SettingsScreen(viewModel: SettingsViewModel) {\n" +
	"    // renders settings\n" +
	"}\n"

func TestVacuousTestsCriterion_FlagsIssue716Example(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{"SettingsScreen.kt": settingsScreenProd})
	writeAndCommit(t, repo, map[string]string{"SettingsScreenTest.kt": vacuousKotlinTest})

	got, ok := vacuousTestsCriterion(cfg)
	if !ok {
		t.Fatal("no_vacuous_tests should apply - an added Kotlin test file with @Test")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (the test never references SettingsScreen): %s", got.Score, got.Reason)
	}
	if !strings.Contains(got.Reason, "SettingsScreenTest.kt") {
		t.Errorf("Reason = %q, want it to name the vacuous file", got.Reason)
	}
}

func TestVacuousTestsCriterion_PassesLegitKotlinTest(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{"SettingsScreen.kt": settingsScreenProd})
	legit := "package com.example.app\n\n" +
		"import org.junit.Test\n" +
		"import com.google.common.truth.Truth.assertThat\n\n" +
		"class SettingsScreenTest {\n" +
		"    @Test\n" +
		"    fun `settings screen renders`() {\n" +
		"        val screen = SettingsScreen(FakeViewModel())\n" +
		"        assertThat(screen).isNotNull()\n" +
		"    }\n" +
		"}\n"
	writeAndCommit(t, repo, map[string]string{"SettingsScreenTest.kt": legit})

	got, ok := vacuousTestsCriterion(cfg)
	if !ok {
		t.Fatal("no_vacuous_tests should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (the test references SettingsScreen): %s", got.Score, got.Reason)
	}
}

func TestVacuousTestsCriterion_FlagsSelfReferentialGoTest(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{
		"go.mod":      "module example.com/x\n\ngo 1.24\n",
		"mathutil.go": "package mathutil\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
	})
	vacuous := "package mathutil\n\n" +
		"import \"testing\"\n\n" +
		"func TestFakeAdd(t *testing.T) {\n" +
		"\tcalled := false\n" +
		"\tcb := func() { called = true }\n" +
		"\tcb()\n" +
		"\tif !called {\n" +
		"\t\tt.Fatal(\"callback not called\")\n" +
		"\t}\n" +
		"}\n"
	writeAndCommit(t, repo, map[string]string{"mathutil_fake_test.go": vacuous})

	got, ok := vacuousTestsCriterion(cfg)
	if !ok {
		t.Fatal("no_vacuous_tests should apply - an added _test.go with func Test")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (never references Add): %s", got.Score, got.Reason)
	}
	if !strings.Contains(got.Reason, "mathutil_fake_test.go") {
		t.Errorf("Reason = %q, want it to name the vacuous file", got.Reason)
	}
}

func TestVacuousTestsCriterion_PassesTableDrivenGoTest(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{
		"go.mod":      "module example.com/x\n\ngo 1.24\n",
		"mathutil.go": "package mathutil\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
	})
	tableTest := "package mathutil\n\n" +
		"import \"testing\"\n\n" +
		"func TestAdd(t *testing.T) {\n" +
		"\tcases := []struct{ a, b, want int }{{1, 2, 3}, {2, 3, 5}}\n" +
		"\tfor _, c := range cases {\n" +
		"\t\tif got := Add(c.a, c.b); got != c.want {\n" +
		"\t\t\tt.Errorf(\"Add(%d,%d) = %d, want %d\", c.a, c.b, got, c.want)\n" +
		"\t\t}\n" +
		"\t}\n" +
		"}\n"
	writeAndCommit(t, repo, map[string]string{"mathutil_test.go": tableTest})

	got, ok := vacuousTestsCriterion(cfg)
	if !ok {
		t.Fatal("no_vacuous_tests should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (a table-driven test calling the real Add): %s", got.Score, got.Reason)
	}
}

// A helper/fixture file has no func Test.../func Example... declaration, so it
// is never treated as an executed test in the first place - it must not trip
// the check even though it adds nothing referencing production code.
func TestVacuousTestsCriterion_SkipsHelperFixtureFile(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{
		"go.mod": "module example.com/x\n\ngo 1.24\n",
	})
	fixture := "package mathutil\n\n" +
		"type fixture struct{ a, b, want int }\n\n" +
		"func newFixture(a, b, want int) fixture {\n" +
		"\treturn fixture{a, b, want}\n" +
		"}\n"
	writeAndCommit(t, repo, map[string]string{"fixtures_test.go": fixture})

	if _, ok := vacuousTestsCriterion(cfg); ok {
		t.Error("no_vacuous_tests must not apply - the added file declares no test function")
	}
}

// A language this check doesn't know how to parse must be skipped outright,
// never guessed at - the flaky-gate rule (prefer false negatives).
func TestVacuousTestsCriterion_SkipsUnknownLanguage(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{"lib.rb": "def add(a, b)\n  a + b\nend\n"})
	writeAndCommit(t, repo, map[string]string{"lib_spec.rb": "it 'adds' do\n  expect(true).to eq(true)\nend\n"})

	if _, ok := vacuousTestsCriterion(cfg); ok {
		t.Error("no_vacuous_tests must not apply - Ruby is not a language this check parses")
	}
}

// The issue scopes this check to ADDED test files only. A pre-existing test
// file gutted down to a vacuous body in place (git status: modified, not
// added) is out of scope - see issue #716's "REQUIRED FIX".
func TestVacuousTestsCriterion_IgnoresModifiedNotAddedTestFile(t *testing.T) {
	legitAtBase := "package mathutil\n\n" +
		"import \"testing\"\n\n" +
		"func TestAdd(t *testing.T) {\n" +
		"\tif Add(1, 2) != 3 {\n" +
		"\t\tt.Fatal(\"bad\")\n" +
		"\t}\n" +
		"}\n"
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{
		"go.mod":           "module example.com/x\n\ngo 1.24\n",
		"mathutil.go":      "package mathutil\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"mathutil_test.go": legitAtBase,
	})
	vacuous := "package mathutil\n\n" +
		"import \"testing\"\n\n" +
		"func TestAdd(t *testing.T) {\n" +
		"\tcalled := false\n" +
		"\tcb := func() { called = true }\n" +
		"\tcb()\n" +
		"\tif !called {\n" +
		"\t\tt.Fatal(\"bad\")\n" +
		"\t}\n" +
		"}\n"
	writeAndCommit(t, repo, map[string]string{"mathutil_test.go": vacuous})

	if _, ok := vacuousTestsCriterion(cfg); ok {
		t.Error("no_vacuous_tests must not apply - the only change is a MODIFIED test file, not an added one")
	}
}

func TestVacuousTestsCriterion_NoCommitsSkips(t *testing.T) {
	cfg, _ := clonedRepoConfig(t, nil, map[string]string{"go.mod": "module example.com/x\n\ngo 1.24\n"})
	if _, ok := vacuousTestsCriterion(cfg); ok {
		t.Error("no_vacuous_tests must not apply - no commits since base")
	}
}

func TestVacuousTestsCriterion_NoWorkspaceSkips(t *testing.T) {
	if _, ok := vacuousTestsCriterion(Config{}); ok {
		t.Error("no_vacuous_tests must not apply with no workspace wired up")
	}
}

func TestVacuousTestsCriterion_ReadOnlySkips(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{"go.mod": "module example.com/x\n\ngo 1.24\n"})
	cfg.ReadOnly = true
	writeAndCommit(t, repo, map[string]string{"pkg_test.go": "package mathutil\n\nfunc TestNothing(t *testing.T) {}\n"})
	if _, ok := vacuousTestsCriterion(cfg); ok {
		t.Error("no_vacuous_tests must not apply to a read-only (reviewer/explorer) node")
	}
}

func TestVacuousTestsCriterion_TypeScript(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{
		"mathUtil.ts": "export function add(a: number, b: number): number {\n  return a + b;\n}\n",
	})
	legit := "import { add } from './mathUtil';\n\n" +
		"test('adds numbers', () => {\n  expect(add(1, 2)).toBe(3);\n});\n"
	vacuous := "test('callback fires', () => {\n" +
		"  let called = false;\n" +
		"  const cb = () => { called = true; };\n" +
		"  cb();\n" +
		"  expect(called).toBe(true);\n" +
		"});\n"
	writeAndCommit(t, repo, map[string]string{
		"mathUtil.test.ts": legit,
		"fake.test.ts":     vacuous,
	})

	got, ok := vacuousTestsCriterion(cfg)
	if !ok {
		t.Fatal("no_vacuous_tests should apply")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (fake.test.ts is vacuous): %s", got.Score, got.Reason)
	}
	if !strings.Contains(got.Reason, "fake.test.ts") {
		t.Errorf("Reason = %q, want it to name fake.test.ts", got.Reason)
	}
	if strings.Contains(got.Reason, "mathUtil.test.ts") {
		t.Errorf("Reason = %q, must NOT name the legit test file", got.Reason)
	}
}

func TestVacuousTestsCriterion_Python(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{
		"mathutil.py": "def add(a, b):\n    return a + b\n",
	})
	legit := "from mathutil import add\n\n\ndef test_add():\n    assert add(1, 2) == 3\n"
	writeAndCommit(t, repo, map[string]string{"test_mathutil.py": legit})

	got, ok := vacuousTestsCriterion(cfg)
	if !ok {
		t.Fatal("no_vacuous_tests should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (the test calls the real add): %s", got.Score, got.Reason)
	}
}

func TestVacuousTestsCriterion_PythonVacuous(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, nil, map[string]string{
		"mathutil.py": "def add(a, b):\n    return a + b\n",
	})
	vacuous := "def test_callback():\n" +
		"    called = False\n\n" +
		"    def cb():\n" +
		"        nonlocal called\n" +
		"        called = True\n\n" +
		"    cb()\n" +
		"    assert called\n"
	writeAndCommit(t, repo, map[string]string{"test_fake.py": vacuous})

	got, ok := vacuousTestsCriterion(cfg)
	if !ok {
		t.Fatal("no_vacuous_tests should apply")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (never references add): %s", got.Score, got.Reason)
	}
}
