package tools

import (
	"os"
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
	root, err := b.jail.Resolve(b.userID, "")
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

func TestRunCommandRejectsShellMetachars(t *testing.T) {
	b := newTestBinding(t, "u1")
	for _, cmd := range []string{
		"echo hi | grep h",
		"echo hi; rm -rf /",
		"echo $HOME",
		"echo `whoami`",
		"echo hi > out.txt",
		"echo hi && echo bye",
		"(echo hi)",
	} {
		if _, err := b.runCommand(runCommandArgs{Dir: "", Command: cmd}); err == nil {
			t.Errorf("runCommand(%q): want error (shell metacharacter), got nil", cmd)
		}
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

// sanity: workspace.ContainsShellMetachar/SplitArgv are exercised directly in
// internal/workspace/exec_test.go; this just confirms run_command wires them up.
func TestRunCommandUsesWorkspaceValidation(t *testing.T) {
	if !workspace.ContainsShellMetachar("a|b") {
		t.Fatal("sanity: ContainsShellMetachar broken")
	}
}
