package workspace

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestContainsShellMetachar(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"go test ./...", false},
		{"echo hi", false},
		// Pipes are NOT metachars anymore: SplitPipeline/RunPipeline implement
		// them natively (chained argv processes, no shell).
		{"echo hi | grep x", false},
		{"grep -r pattern . | head -50", false},
		// Everything else stays unexpressible.
		{"echo hi; rm -rf /", true},
		{"echo $HOME", true},
		{"echo `whoami`", true},
		{"cmd < in", true},
		{"cmd > out", true},
		{"echo hi && echo bye", true},
		{"(echo hi)", true},
	}
	for _, c := range cases {
		if got := ContainsShellMetachar(c.s); got != c.want {
			t.Errorf("ContainsShellMetachar(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// SplitPipeline
// ---------------------------------------------------------------------------

func TestSplitPipelineStages(t *testing.T) {
	stages, err := SplitPipeline("grep -r pattern . | sort | head -50")
	if err != nil {
		t.Fatalf("SplitPipeline: %v", err)
	}
	want := [][]string{
		{"grep", "-r", "pattern", "."},
		{"sort"},
		{"head", "-50"},
	}
	if len(stages) != len(want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	for i := range want {
		if len(stages[i]) != len(want[i]) {
			t.Fatalf("stage %d = %v, want %v", i, stages[i], want[i])
		}
		for j := range want[i] {
			if stages[i][j] != want[i][j] {
				t.Errorf("stage %d arg %d = %q, want %q", i, j, stages[i][j], want[i][j])
			}
		}
	}
}

func TestSplitPipelineQuotedPipeIsLiteral(t *testing.T) {
	stages, err := SplitPipeline(`grep "a|b" file.txt`)
	if err != nil {
		t.Fatalf("SplitPipeline: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("stages = %v, want a single stage (quoted | is literal)", stages)
	}
	if stages[0][1] != "a|b" {
		t.Errorf("arg = %q, want %q", stages[0][1], "a|b")
	}
	// Single-quoted too.
	stages, err = SplitPipeline(`grep 'x|y' f`)
	if err != nil {
		t.Fatalf("SplitPipeline: %v", err)
	}
	if len(stages) != 1 || stages[0][1] != "x|y" {
		t.Errorf("stages = %v, want one stage with literal x|y", stages)
	}
}

func TestSplitPipelineNoPipeIsOneStage(t *testing.T) {
	stages, err := SplitPipeline("go test ./...")
	if err != nil {
		t.Fatalf("SplitPipeline: %v", err)
	}
	if len(stages) != 1 || len(stages[0]) != 3 {
		t.Fatalf("stages = %v, want one 3-arg stage", stages)
	}
}

func TestSplitPipelineEmptyStageErrors(t *testing.T) {
	for _, s := range []string{"| head", "grep x |", "a || b", "grep x | | head"} {
		if _, err := SplitPipeline(s); err == nil {
			t.Errorf("SplitPipeline(%q): want empty-stage error", s)
		}
	}
}

func TestSplitArgv(t *testing.T) {
	cases := []struct {
		s    string
		want []string
	}{
		{"go test ./...", []string{"go", "test", "./..."}},
		{`git commit -m "a message with spaces"`, []string{"git", "commit", "-m", "a message with spaces"}},
		{"echo 'single quoted'", []string{"echo", "single quoted"}},
		{"  leading  spaces  ", []string{"leading", "spaces"}},
	}
	for _, c := range cases {
		got, err := SplitArgv(c.s)
		if err != nil {
			t.Fatalf("SplitArgv(%q): %v", c.s, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("SplitArgv(%q) = %v, want %v", c.s, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("SplitArgv(%q)[%d] = %q, want %q", c.s, i, got[i], c.want[i])
			}
		}
	}
}

func TestSplitArgvErrors(t *testing.T) {
	for _, s := range []string{"", "   ", `unterminated "quote`, `trailing\`} {
		if _, err := SplitArgv(s); err == nil {
			t.Errorf("SplitArgv(%q): want error", s)
		}
	}
}

func TestRunArgvSuccess(t *testing.T) {
	res, err := RunArgv(context.Background(), t.TempDir(), []string{"echo", "hello"}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("Output = %q, want to contain 'hello'", res.Output)
	}
}

func TestRunArgvNonZeroExitIsNotAnError(t *testing.T) {
	res, err := RunArgv(context.Background(), t.TempDir(), []string{"false"}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunArgv: unexpected error for a plain non-zero exit: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero")
	}
}

func TestRunArgvMissingBinaryErrors(t *testing.T) {
	_, err := RunArgv(context.Background(), t.TempDir(), []string{"this-binary-does-not-exist-xyz"}, DefaultCaps())
	if err == nil {
		t.Fatal("RunArgv: want error for a missing binary")
	}
}

func TestRunArgvCwdIsPinned(t *testing.T) {
	dir := t.TempDir()
	res, err := RunArgv(context.Background(), dir, []string{"pwd"}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if !strings.Contains(strings.TrimSpace(res.Output), dir) {
		t.Errorf("pwd output = %q, want to contain jailed dir %q", res.Output, dir)
	}
}

func TestChildHomeFallsBackToDirWhenCapsHomeUnset(t *testing.T) {
	if got := childHome("/some/repo", Caps{}); got != "/some/repo" {
		t.Errorf("childHome(no HomeDir) = %q, want the cwd itself (/some/repo)", got)
	}
}

func TestChildHomeUsesCapsHomeDirWhenSet(t *testing.T) {
	got := childHome("/some/repo", Caps{HomeDir: "/isolated/home"})
	if got != "/isolated/home" {
		t.Errorf("childHome(HomeDir set) = %q, want /isolated/home (never the repo dir)", got)
	}
}

// TestRunArgvHomeIsolatedFromCwd is the regression test for the live bug: a
// coding task's cwd IS the target repo, so HOME must NEVER default to it once
// Caps.HomeDir is wired up — otherwise a child tool (npm, pip, …) writing its
// own cache to $HOME writes it straight into the repo, where git_commit's
// add_all can sweep it up.
func TestRunArgvHomeIsolatedFromCwd(t *testing.T) {
	repoDir := t.TempDir()
	homeDir := t.TempDir()
	caps := DefaultCaps()
	caps.HomeDir = homeDir

	res, err := RunArgv(context.Background(), repoDir, []string{"sh", "-c", "echo $HOME"}, caps)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	got := strings.TrimSpace(res.Output)
	if got != homeDir {
		t.Errorf("child HOME = %q, want isolated homeDir %q", got, homeDir)
	}
	if got == repoDir {
		t.Error("child HOME resolved to the repo's own cwd — the isolation this fix exists for is broken")
	}
}

// TestRunArgvHomeDefaultsToDirWithoutCapsHomeDir preserves the pre-fix
// behavior for any caller (or test) that hasn't wired Caps.HomeDir up.
func TestRunArgvHomeDefaultsToDirWithoutCapsHomeDir(t *testing.T) {
	dir := t.TempDir()
	res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", "echo $HOME"}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if got := strings.TrimSpace(res.Output); got != dir {
		t.Errorf("child HOME = %q, want dir %q (fallback when Caps.HomeDir is unset)", got, dir)
	}
}

func TestRunPipelineHomeIsolatedFromCwd(t *testing.T) {
	repoDir := t.TempDir()
	homeDir := t.TempDir()
	caps := DefaultCaps()
	caps.HomeDir = homeDir

	res, err := RunPipeline(context.Background(), repoDir, [][]string{{"sh", "-c", "echo $HOME"}, {"cat"}}, caps)
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if got := strings.TrimSpace(res.Output); got != homeDir {
		t.Errorf("child HOME = %q, want isolated homeDir %q", got, homeDir)
	}
}

func TestRunArgvTimeout(t *testing.T) {
	caps := DefaultCaps()
	caps.Timeout = 50 * time.Millisecond
	res, err := RunArgv(context.Background(), t.TempDir(), []string{"sleep", "5"}, caps)
	if err == nil {
		t.Fatal("RunArgv: want a timeout error")
	}
	if !res.TimedOut {
		t.Error("ExecResult.TimedOut = false, want true")
	}
}

func TestRunArgvOutputCap(t *testing.T) {
	caps := DefaultCaps()
	caps.MaxOutputBytes = 10
	res, err := RunArgv(context.Background(), t.TempDir(), []string{"echo", "this is a much longer line than the cap"}, caps)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Errorf("Output = %q, want a truncation marker", res.Output)
	}
	if int64(len(res.Output)) > caps.MaxOutputBytes+int64(len("... (truncated)\n")) {
		t.Errorf("Output length %d exceeds the cap plus marker", len(res.Output))
	}
}

// ---------------------------------------------------------------------------
// RunPipeline
// ---------------------------------------------------------------------------

func TestRunPipelineTwoStageHappyPath(t *testing.T) {
	res, err := RunPipeline(context.Background(), t.TempDir(),
		[][]string{{"echo", "cherry\napple\nbanana"}, {"sort"}}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if want := "apple\nbanana\ncherry"; !strings.Contains(res.Output, want) {
		t.Errorf("Output = %q, want sorted lines %q", res.Output, want)
	}
}

func TestRunPipelineThreeStages(t *testing.T) {
	// echo two lines | grep one | tr to upper — proves chaining beyond a pair.
	res, err := RunPipeline(context.Background(), t.TempDir(),
		[][]string{{"printf", "keep\ndrop\n"}, {"grep", "keep"}, {"tr", "a-z", "A-Z"}}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "KEEP") || strings.Contains(res.Output, "DROP") {
		t.Errorf("Output = %q, want KEEP only", res.Output)
	}
}

func TestRunPipelinePipefailMiddleStage(t *testing.T) {
	// Middle stage fails (false exits 1) while the tail succeeds — pipefail
	// must surface the failure, and the output must name the failing stage.
	res, err := RunPipeline(context.Background(), t.TempDir(),
		[][]string{{"echo", "hi"}, {"false"}, {"cat"}}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunPipeline: unexpected error for a stage's non-zero exit: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("ExitCode = 0, want non-zero (pipefail)")
	}
	if !strings.Contains(res.Output, "stage 2 of 3") {
		t.Errorf("Output = %q, want the failing stage named", res.Output)
	}
}

func TestRunPipelineLastNonZeroWins(t *testing.T) {
	// sh -c 'exit 3' is argv-only here (sh is just argv[0]); the LAST failing
	// stage's code is the pipeline's code.
	res, err := RunPipeline(context.Background(), t.TempDir(),
		[][]string{{"false"}, {"sh", "-c", "exit 3"}}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3 (last non-zero stage)", res.ExitCode)
	}
}

func TestRunPipelineStderrCombined(t *testing.T) {
	res, err := RunPipeline(context.Background(), t.TempDir(),
		[][]string{{"sh", "-c", "echo boom >&2; echo data"}, {"cat"}}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if !strings.Contains(res.Output, "boom") {
		t.Errorf("Output = %q, want stage stderr included", res.Output)
	}
	if !strings.Contains(res.Output, "data") {
		t.Errorf("Output = %q, want final stdout included", res.Output)
	}
}

func TestRunPipelineTimeoutKillsWholePipeline(t *testing.T) {
	caps := DefaultCaps()
	caps.Timeout = 100 * time.Millisecond
	start := time.Now()
	res, err := RunPipeline(context.Background(), t.TempDir(),
		[][]string{{"sleep", "5"}, {"cat"}}, caps)
	if err == nil {
		t.Fatal("RunPipeline: want a timeout error")
	}
	if !res.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("pipeline took %s to die; the deadline should kill every stage", elapsed)
	}
}

func TestRunPipelineSingleStageDelegates(t *testing.T) {
	// One stage == RunArgv semantics exactly (same runner).
	res, err := RunPipeline(context.Background(), t.TempDir(), [][]string{{"echo", "solo"}}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "solo") {
		t.Errorf("res = %+v, want echo output with exit 0", res)
	}
}

func TestRunPipelineMissingBinaryErrors(t *testing.T) {
	if _, err := RunPipeline(context.Background(), t.TempDir(),
		[][]string{{"echo", "x"}, {"no-such-binary-zzz"}}, DefaultCaps()); err == nil {
		t.Fatal("RunPipeline: want error for a missing stage binary")
	}
}

// TestChildPathExtras: workspace.exec_path extras go FIRST (a configured
// toolchain wins over a stale system one); empty extras = the fixed PATH alone.
func TestChildPathExtras(t *testing.T) {
	if got := childPath(Caps{}); got != execEnvPath {
		t.Errorf("childPath(no extras) = %q, want %q", got, execEnvPath)
	}
	got := childPath(Caps{ExtraPath: []string{"/opt/nvm/bin", "/opt/asdf/shims"}})
	want := "/opt/nvm/bin:/opt/asdf/shims:" + execEnvPath
	if got != want {
		t.Errorf("childPath(extras) = %q, want %q", got, want)
	}
}
