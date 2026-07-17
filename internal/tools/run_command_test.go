package tools

import (
	"os"
	"strings"
	"testing"
	"time"
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

func TestRunCommandQuotedParensWork(t *testing.T) {
	// Regression (#276/#277): a quoted grep pattern with parens (a Go receiver)
	// tripped the old argv-only metachar gate and looped the worker. It must
	// run cleanly through the real shell run_command now always uses.
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
	res, err := b.runCommand(runCommandArgs{Command: `printf 'func (e *Extension) X()\n' | grep -Fn 'func (e *Extension)'`})
	if err != nil {
		t.Fatalf("runCommand(quoted parens): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "Extension") {
		t.Errorf("quoted grep: exit=%d out=%q, want match", res.ExitCode, res.Output)
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

// `cd` inside a command is now the shell's own — no fold/normalization step
// exists to test; a `cd X && …` composes (or fails) exactly as it would in a
// real terminal, dir doubling included. See run_command_shell_test.go for the
// shell-semantics coverage (redirects, `$(…)`, `&&`, globs) this replaces.

func TestRunCommandStderrMergeIsARealRedirect(t *testing.T) {
	// `2>&1` is now a genuine shell redirect (not a stripped idiom): it still
	// merges stderr into the piped stream, and a quoted "2>&1" still prints
	// literally — the observable behavior is unchanged even though the
	// mechanism (a real shell) is not the argv-only one that used to require it.
	b := newTestBinding(t, "u1")
	ensureUserRoot(t, b)
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
	// Single-stage: stderr really is merged into the output.
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
	// A quoted "2>&1" is a literal argument — echo prints it verbatim.
	res, err = b.runCommand(runCommandArgs{Command: `echo "2>&1"`})
	if err != nil {
		t.Fatalf(`runCommand(echo "2>&1"): %v`, err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "2>&1") {
		t.Errorf("quoted 2>&1 should print literally: exit=%d out=%q", res.ExitCode, res.Output)
	}
}
