package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"
)

// ensureUserRoot creates the binding's user root on disk — run_command
// (unlike git_clone/write_file) never auto-creates its `dir`, so tests
// exercising a real cwd need the jail's user directory to already exist.
func ensureUserRoot(t *testing.T, b fsBinding) string {
	t.Helper()
	root, err := b.jail.Resolve(b.userID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRunCommandBasic(t *testing.T) {
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	res, err := b.runCommand(runCommandArgs{Dir: "", Command: "echo hello"})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("Output = %q, want to contain 'hello'", res.Output)
	}
}

func TestRunCommandAcceptsQuotedMetachars(t *testing.T) {
	// Regression (#276/#277): a quoted grep pattern with parens (a Go receiver)
	// is literal argv, not shell operators. The old metachar gate rejected it
	// and looped the worker; it must now run. With no shell, metachars are never
	// interpreted — an unquoted `;` is a literal arg, NOT a command separator.
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	res, err := b.runCommand(runCommandArgs{Command: `printf 'func (e *Extension) X()\n' | grep -Fn 'func (e *Extension)'`})
	if err != nil {
		t.Fatalf("runCommand(quoted parens): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "Extension") {
		t.Errorf("quoted grep: exit=%d out=%q, want match", res.ExitCode, res.Output)
	}
	// `;` must NOT spawn a second process — echo prints it literally, exit 0.
	res, err = b.runCommand(runCommandArgs{Command: "echo hi; rm -rf /tmp/nope"})
	if err != nil {
		t.Fatalf("runCommand(literal ;): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "rm -rf") {
		t.Errorf("literal ;: exit=%d out=%q, want the ';' passed to echo as a literal arg", res.ExitCode, res.Output)
	}
}

func TestRunCommandAcceptsNativePipes(t *testing.T) {
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	res, err := b.runCommand(runCommandArgs{Dir: "", Command: "printf 'b\\na\\nc\\n' | sort | head -2"})
	if err != nil {
		t.Fatalf("runCommand(pipeline): %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (output: %q)", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "a") || strings.Contains(res.Output, "c") {
		t.Errorf("Output = %q, want the sorted head only", res.Output)
	}
}

func TestRunCommandPipefailSurfacesStageFailure(t *testing.T) {
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	res, err := b.runCommand(runCommandArgs{Dir: "", Command: "false | cat"})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0, want non-zero (pipefail)")
	}
	if !strings.Contains(res.Output, "stage 1 of 2") {
		t.Errorf("Output = %q, want the failing stage named", res.Output)
	}
}

func TestRunCommandIsNotAllowlisted(t *testing.T) {
	// run_command is guard-gated, not allowlisted against workspace.check_commands
	// (that allowlist governs the PLANNER's per-node checks — see internal/dag)
	// — any non-shell argv is accepted here.
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	if _, err := b.runCommand(runCommandArgs{Dir: "", Command: "true"}); err != nil {
		t.Errorf("runCommand(true): unexpected error: %v", err)
	}
}

func TestRunCommandTimeout(t *testing.T) {
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	b.caps.Timeout = 50 * time.Millisecond
	if _, err := b.runCommand(runCommandArgs{Dir: "", Command: "sleep 5"}); err == nil {
		t.Fatal("runCommand(sleep 5): want a timeout error")
	}
}

func TestRunCommandOutputCap(t *testing.T) {
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	b.caps.MaxOutputBytes = 10
	res, err := b.runCommand(runCommandArgs{Dir: "", Command: "echo this is a much longer line than the cap"})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Errorf("Output = %q, want a truncation marker", res.Output)
	}
}

func TestRunCommandDirIsJailed(t *testing.T) {
	b := newTestBinding(t, "u1")
	if _, err := b.runCommand(runCommandArgs{Dir: "../escape", Command: "echo hi"}); err == nil {
		t.Fatal("runCommand(dir escaping jail): want error")
	} else if !strings.Contains(err.Error(), "escapes your workspace") {
		t.Errorf("err = %v, want an escape rejection", err)
	}
}

func TestRunCommandCwdIsJailedDir(t *testing.T) {
	b := newTestBinding(t, "u1")
	userRoot := ensureUserRoot(t, b)
	res, err := b.runCommand(runCommandArgs{Dir: "", Command: "pwd"})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if !strings.Contains(strings.TrimSpace(res.Output), userRoot) {
		t.Errorf("pwd output = %q, want to contain jailed root %q", res.Output, userRoot)
	}
}

func TestRunCommandRejectsEmpty(t *testing.T) {
	b := newTestBinding(t, "u1")
	if _, err := b.runCommand(runCommandArgs{Dir: "", Command: "   "}); err == nil {
		t.Fatal("runCommand(empty command): want error")
	}
}

// TestRunCommandCDPrefixNormalization covers the live-observed LLM habit of
// re-stating the dir argument as a `cd X &&` prefix: the prefix folds into
// dir resolution instead of tripping the metachar wall.
func TestRunCommandCDPrefixNormalization(t *testing.T) {
	b := newTestBinding(t, "u1")
	root := ensureUserRoot(t, b)
	for _, sub := range []string{"repo", "a/b"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name          string
		dir           string
		command       string
		wantCwdSuffix string
	}{
		{"dir restated by cd — no doubling", "repo", "cd repo && pwd", "/repo"},
		{"empty dir — cd supplies it", "", "cd repo && pwd", "/repo"},
		{"nested cd target", "", "cd a/b && pwd", "/a/b"},
		{"cd composes under dir", "a", "cd b && pwd", "/a/b"},
		// The exact live failure shape: both idioms in one command.
		{"cd prefix plus 2>&1", "repo", "cd repo && pwd 2>&1", "/repo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := b.runCommand(runCommandArgs{Dir: c.dir, Command: c.command})
			if err != nil {
				t.Fatalf("runCommand(dir=%q, %q): %v", c.dir, c.command, err)
			}
			cwd := strings.TrimSpace(res.Output)
			if !strings.HasSuffix(cwd, c.wantCwdSuffix) {
				t.Errorf("cwd = %q, want suffix %q", cwd, c.wantCwdSuffix)
			}
			if strings.HasSuffix(cwd, "/repo/repo") {
				t.Errorf("cwd = %q: dir doubled", cwd)
			}
		})
	}
}

func TestRunCommandCDPrefixJailStillApplies(t *testing.T) {
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	if _, err := b.runCommand(runCommandArgs{Dir: "", Command: "cd .. && pwd"}); err == nil {
		t.Fatal("runCommand(cd .. && pwd): want jail rejection")
	} else if !strings.Contains(err.Error(), "escapes your workspace") {
		t.Errorf("err = %v, want an escape rejection", err)
	}
}

// Only a single leading `cd X &&` is folded into the dir — a later `&&` or a
// non-leading `cd` is left as literal argv (no shell interprets it).
func TestRunCommandCDPrefixOnlyStripsThePrefix(t *testing.T) {
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	// Leading `cd . &&` folds; the SECOND `&&` survives as a literal arg to echo.
	res, err := b.runCommand(runCommandArgs{Command: "cd . && echo a && echo b"})
	if err != nil {
		t.Fatalf("runCommand(leading cd fold): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "&& echo b") {
		t.Errorf("only the leading cd should fold: exit=%d out=%q", res.ExitCode, res.Output)
	}
	// A non-leading `cd` is not folded — echo prints it literally, cd never runs.
	res, err = b.runCommand(runCommandArgs{Command: "echo hi && cd repo"})
	if err != nil {
		t.Fatalf("runCommand(non-leading cd): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "cd repo") {
		t.Errorf("non-leading cd should be literal: exit=%d out=%q", res.ExitCode, res.Output)
	}
}

func TestRunCommandStderrMergeTokenDropped(t *testing.T) {
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	// `2>&1` is a no-op for us (RunArgv merges stderr; RunPipeline appends
	// each stage's stderr to the output) — it must be dropped, not rejected,
	// and the pipeline must run.
	res, err := b.runCommand(runCommandArgs{Dir: "", Command: "printf 'a\\nb\\nc\\n' 2>&1 | head -1"})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (output: %q)", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "a") || strings.Contains(res.Output, "c") {
		t.Errorf("Output = %q, want head of the piped output", res.Output)
	}
	// Single-stage: stderr really is merged into the output without 2>&1.
	res, err = b.runCommand(runCommandArgs{Dir: "", Command: "ls --definitely-bogus-flag 2>&1"})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0, want non-zero")
	}
	if res.Output == "" {
		t.Error("Output empty, want ls's stderr merged in")
	}
}

func TestRunCommandQuotedStderrMergeUntouched(t *testing.T) {
	// A quoted "2>&1" is a literal argument, not the idiom — it is NOT stripped,
	// and (no shell) not rejected: echo prints it verbatim.
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	res, err := b.runCommand(runCommandArgs{Command: `echo "2>&1"`})
	if err != nil {
		t.Fatalf(`runCommand(echo "2>&1"): %v`, err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "2>&1") {
		t.Errorf("quoted 2>&1 should print literally: exit=%d out=%q", res.ExitCode, res.Output)
	}
}

// sanity: ContainsShellMetachar is no longer a rejection gate — it survives
// only as cd-fold eligibility (see internal/workspace/exec.go). This confirms
// the function's contract that run_command's fold check relies on.
func TestRunCommandUsesWorkspaceValidation(t *testing.T) {
	if workspace.ContainsShellMetachar("a|b") {
		t.Fatal("sanity: pipes must not be metachars (they run natively)")
	}
	if !workspace.ContainsShellMetachar("a;b") {
		t.Fatal("sanity: ContainsShellMetachar broken")
	}
}
