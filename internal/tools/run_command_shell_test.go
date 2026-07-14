package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// sandboxedBinding is a binding whose children run inside the real bubblewrap
// namespace — the deployment default (workspace.sandbox: bwrap), and therefore
// the mode run_command's shell path exists for. The user root is created up
// front because run_command never auto-creates its dir.
func sandboxedBinding(t *testing.T) fsBinding {
	t.Helper()
	requireSandbox(t)
	b := newTestBinding(t, "u1")
	b.caps.Sandbox = workspace.SandboxBwrap
	b.caps.HomeDir = t.TempDir()
	ensureUserRoot(t, b)
	return b
}

// TestRunCommandShellWhenSandboxed pins the live failure this change exists to
// remove. A code-explorer node could not run the ONE command that answered its
// question — `python3 -c "import sys; print(sys.path)"` — because `(` and `)`
// tripped the metachar guard, so it wrote a script file to disk to route around
// us and burned turns. Inside the sandbox the command line goes to a real shell:
// substitution, redirects, subshells and chaining all just work.
func TestRunCommandShellWhenSandboxed(t *testing.T) {
	b := sandboxedBinding(t)
	root, err := b.resolve("")
	if err != nil {
		t.Fatal(err)
	}

	// 1. The actual thing the explorer needed: an inline interpreter, `(` `)`.
	res, err := b.runCommand(runCommandArgs{Command: `python3 -c "import sys; print(sys.path)"`})
	if err != nil {
		t.Fatalf("run_command(python3 -c …): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "[") {
		t.Fatalf("python3 -c: exit=%d output=%q — want the printed sys.path", res.ExitCode, res.Output)
	}

	// 2. A redirect lands in the child's cwd.
	if res, err := b.runCommand(runCommandArgs{Command: "ls > out.txt"}); err != nil || res.ExitCode != 0 {
		t.Fatalf("run_command(ls > out.txt): err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); err != nil {
		t.Fatalf("the redirect did not land in the cwd (%s): %v", root, err)
	}

	// 3. Command substitution, `&&`, a subshell and a pipe in one line.
	res, err = b.runCommand(runCommandArgs{Command: `echo $(echo one) && (echo two; echo three) | wc -l`})
	if err != nil {
		t.Fatalf("run_command(substitution + && + subshell): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "one") || !strings.Contains(res.Output, "2") {
		t.Fatalf("exit=%d output=%q — want 'one', then a subshell line count of 2", res.ExitCode, res.Output)
	}

	// 4. A plain pipeline still works on the shell path.
	res, err = b.runCommand(runCommandArgs{Command: "printf 'b\\na\\nc\\n' | sort | head -2"})
	if err != nil {
		t.Fatalf("run_command(pipeline): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "a") || strings.Contains(res.Output, "c") {
		t.Fatalf("exit=%d output=%q — want the sorted head only", res.ExitCode, res.Output)
	}

	// 5. A failing command is still a clean non-zero exit, not a tool error.
	res, err = b.runCommand(runCommandArgs{Command: "false"})
	if err != nil {
		t.Fatalf("run_command(false): %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("run_command(false): want a non-zero exit code")
	}
}

// TestShellCannotWiden proves the sandbox — not the metachar guard — is the
// wall: with a FULL shell, a child still cannot write outside its own cwd and
// $HOME, because nothing else exists in its mount namespace.
func TestShellCannotWiden(t *testing.T) {
	b := sandboxedBinding(t)

	// A path outside the sandbox's two writable binds, named outright (this is
	// how a sibling node's directory would be reached — see sandbox_cwd_test.go).
	sibling := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(sibling, []byte("another node's own file"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, cmd := range []string{
		"echo x > /etc/passwd",
		"echo pwn > " + sibling,
		"sh -c 'cat /etc/shadow'",
	} {
		res, err := b.runCommand(runCommandArgs{Command: cmd})
		if err != nil {
			t.Fatalf("run_command(%q) errored (want a clean non-zero exit): %v", cmd, err)
		}
		if res.ExitCode == 0 {
			t.Fatalf("SANDBOX ESCAPE: %q succeeded with a real shell: %q", cmd, res.Output)
		}
	}
	if got, err := os.ReadFile(sibling); err != nil || string(got) != "another node's own file" {
		t.Fatalf("the outside file was modified through the shell: %q (err=%v)", got, err)
	}
}

// TestNoSandboxKeepsTheMetacharGuard is the asymmetry, stated as a test: the
// shell is available BECAUSE the sandbox is real, so with `sandbox: none` the
// habit guard is all there is and it stays exactly as it was.
func TestNoSandboxKeepsTheMetacharGuard(t *testing.T) {
	b := newTestBinding(t, "u1")
	b.caps.Sandbox = workspace.SandboxNone
	ensureUserRoot(t, b)

	for _, cmd := range []string{
		`python3 -c "import sys; print(sys.path)"`,
		"ls > out.txt",
		"echo hi; rm -rf /",
		"echo $HOME",
	} {
		if _, err := b.runCommand(runCommandArgs{Command: cmd}); err == nil {
			t.Errorf("run_command(%q) with sandbox: none — want a metachar rejection, got nil", cmd)
		}
	}
	// …and native pipelines still work there.
	res, err := b.runCommand(runCommandArgs{Command: "printf 'b\\na\\n' | sort | head -1"})
	if err != nil {
		t.Fatalf("run_command(pipeline, sandbox: none): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "a") {
		t.Fatalf("exit=%d output=%q — want the sorted head", res.ExitCode, res.Output)
	}
}
