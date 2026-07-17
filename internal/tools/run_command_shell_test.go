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

// TestRunCommandShellWithoutSandbox pins the #277 fix itself: with
// `sandbox: none` run_command still hands its command line to a REAL shell
// (workspace.RunShell just skips the bwrap wrapper — see childArgv), so
// `&&`, globs, redirects and `$(…)` all behave exactly as they do under
// sandbox: bwrap. Only the boundary differs (the server user's own filesystem
// authority, not a namespace) — see TestRunCommandUnsandboxedHasHostAuthority.
func TestRunCommandShellWithoutSandbox(t *testing.T) {
	b := newTestBinding(t, "u1")
	b.caps.Sandbox = workspace.SandboxNone
	root := ensureUserRoot(t, b)

	// 1. `&&` chaining.
	res, err := b.runCommand(runCommandArgs{Command: "echo one && echo two"})
	if err != nil {
		t.Fatalf("run_command(&&): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "one") || !strings.Contains(res.Output, "two") {
		t.Fatalf("&&: exit=%d output=%q", res.ExitCode, res.Output)
	}

	// 2. A glob the shell expands (not passed through as a literal "*.txt").
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err = b.runCommand(runCommandArgs{Command: "cat *.txt"})
	if err != nil {
		t.Fatalf("run_command(glob): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "a.txt") || !strings.Contains(res.Output, "b.txt") {
		t.Fatalf("glob: exit=%d output=%q, want both files' contents", res.ExitCode, res.Output)
	}

	// 3. A redirect lands in the child's cwd.
	if res, err := b.runCommand(runCommandArgs{Command: "echo redirected > out.txt"}); err != nil || res.ExitCode != 0 {
		t.Fatalf("run_command(redirect): err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
	}
	got, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil || strings.TrimSpace(string(got)) != "redirected" {
		t.Fatalf("the redirect did not land in the cwd (%s): %q, err=%v", root, got, err)
	}

	// 4. Command substitution.
	res, err = b.runCommand(runCommandArgs{Command: `echo $(echo nested)`})
	if err != nil {
		t.Fatalf("run_command($(…)): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "nested") {
		t.Fatalf("substitution: exit=%d output=%q", res.ExitCode, res.Output)
	}

	// 5. A non-zero exit surfaces the shell's own exit code, stderr included.
	res, err = b.runCommand(runCommandArgs{Command: "ls --definitely-bogus-flag"})
	if err != nil {
		t.Fatalf("run_command(bad flag): %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("run_command(ls --bogus-flag): want a non-zero exit code")
	}
	if res.Output == "" {
		t.Fatal("run_command(ls --bogus-flag): want ls's stderr surfaced in the output")
	}
}

// TestRunCommandUnsandboxedHasHostAuthority: with no OS sandbox, run_command's
// shell has the server user's own filesystem authority — nothing here confines
// a command to `dir` the way bwrap's mount namespace does (contrast
// TestShellCannotWiden). This is the documented, deliberate cost of
// `sandbox: none` (see runCommandDescription and workspace.ResolveSandbox's
// startup WARN), not a regression this change introduces.
func TestRunCommandUnsandboxedHasHostAuthority(t *testing.T) {
	b := newTestBinding(t, "u1")
	b.caps.Sandbox = workspace.SandboxNone
	ensureUserRoot(t, b)

	outside := filepath.Join(t.TempDir(), "reachable.txt")
	res, err := b.runCommand(runCommandArgs{Command: "echo host-authority > " + outside})
	if err != nil {
		t.Fatalf("run_command(write outside dir): %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("run_command(write outside dir): exit=%d output=%q", res.ExitCode, res.Output)
	}
	if got, err := os.ReadFile(outside); err != nil || strings.TrimSpace(string(got)) != "host-authority" {
		t.Fatalf("want the write to land outside dir (no OS boundary here): %q, err=%v", got, err)
	}
}
