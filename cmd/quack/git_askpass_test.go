package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestMain mirrors main()'s dispatch so tests can exercise the REAL
// self-exec mechanisms rather than calling their logic in-process:
//   - __sandbox-exec (workspace.RunSandboxExecIfInvoked): the Landlock shim.
//   - GIT_ASKPASS (isGitAskpassInvocation): symlinks the test binary under the askpass link name and execs it exactly the way git execs $GIT_ASKPASS - direct program path, prompt as the single argument, no shell. Catches an unexecutable GIT_ASKPASS value that an in-process call would miss.
func TestMain(m *testing.M) {
	workspace.RunSandboxExecIfInvoked()
	if isGitAskpassInvocation() {
		gitAskpassMain(os.Args, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// execAskpass runs the askpass symlink the way git does: exec.Command on the
// $GIT_ASKPASS value itself with the prompt as one argument.
func execAskpass(t *testing.T, link, prompt string, env map[string]string) string {
	t.Helper()
	cmd := exec.Command(link, prompt)
	cmd.Env = []string{} // scrubbed, like gitEnv
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("exec %q: %v", link, err)
	}
	return out.String()
}

// TestGitAskpassSymlinkExecsBothPrompts is the test that would have caught
// the live bug: it execs the GIT_ASKPASS value directly (no shell, prompt as
// argv[1] - precisely git's invocation) through a symlink named tools.GitAskpassLinkName, and asserts BOTH halves of git's two-call protocol: the Username prompt answers with the configured username, the Password prompt with the token.
func TestGitAskpassSymlinkExecsBothPrompts(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), tools.GitAskpassLinkName)
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		tools.GitAskpassUserEnv:  "x-access-token",
		tools.GitAskpassTokenEnv: "sekret-token",
		tools.GitAskpassHostEnv:  "github.com",
	}
	if got := execAskpass(t, link, "Username for 'https://github.com': ", env); got != "x-access-token\n" {
		t.Errorf("Username prompt answered %q, want %q", got, "x-access-token\n")
	}
	if got := execAskpass(t, link, "Password for 'https://x-access-token@github.com': ", env); got != "sekret-token\n" {
		t.Errorf("Password prompt answered %q, want %q", got, "sekret-token\n")
	}
}

// TestGitAskpassAnswerTwoPrompts covers the in-process answer logic: prompts for
// the expected host get the username or token; any other host, or no host, gets nothing.
func TestGitAskpassAnswerTwoPrompts(t *testing.T) {
	t.Setenv(tools.GitAskpassUserEnv, "x-access-token")
	t.Setenv(tools.GitAskpassTokenEnv, "sekret-token")
	t.Setenv(tools.GitAskpassHostEnv, "github.com")
	cases := []struct {
		prompt, want string
	}{
		{"Username for 'https://github.com': ", "x-access-token"},
		{"Password for 'https://x-access-token@GitHub.com': ", "sekret-token"},
		{"Username for 'https://example.com': ", ""},
		{"Password for 'https://x-access-token@example.com': ", ""},
		{"Password for 'https://github.com.example.com': ", ""},
		{"Password for 'https://github.com:8443': ", ""},
		{"username: ", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := tools.GitAskpassAnswer(c.prompt); got != c.want {
			t.Errorf("GitAskpassAnswer(%q) = %q, want %q", c.prompt, got, c.want)
		}
	}
}

// TestGitAskpassSubcommandSecondaryEntry: the hidden cobra subcommand answers
// the same way (manual-debugging entry; git itself uses the symlink).
func TestGitAskpassSubcommandSecondaryEntry(t *testing.T) {
	t.Setenv(tools.GitAskpassUserEnv, "u1")
	t.Setenv(tools.GitAskpassTokenEnv, "tok")
	t.Setenv(tools.GitAskpassHostEnv, "github.com")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"git-askpass", "Username for 'https://github.com':"})
	if err := root.Execute(); err != nil {
		t.Fatalf("git-askpass errored: %v", err)
	}
	if got := out.String(); got != "u1\n" {
		t.Errorf("subcommand answered %q, want %q", got, "u1\n")
	}
}

// TestGitAskpassIsHidden: the credential mode must not surface in help output.
func TestGitAskpassIsHidden(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("help errored: %v", err)
	}
	if strings.Contains(out.String(), "git-askpass") {
		t.Error("git-askpass appears in help output; it must stay hidden")
	}
}
