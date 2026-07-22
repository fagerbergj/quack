package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/tools"
)

// TestMain mirrors main()'s argv[0] dispatch so the askpass-exec test below
// can run the REAL mechanism: it symlinks the test binary under the askpass
// link name and execs it exactly the way git execs $GIT_ASKPASS - direct
// program path, prompt as the single argument, no shell. Without this, tests
// could only call the answer function in-process and would never catch an
// unexecutable GIT_ASKPASS value (the exact live failure this guards:
// GIT_ASKPASS="<binary> git-askpass" made git look for a file literally
// named "quack git-askpass").
func TestMain(m *testing.M) {
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
// argv[1] - precisely git's invocation) through a symlink named
// tools.GitAskpassLinkName, and asserts BOTH halves of git's two-call
// protocol: the Username prompt answers with the configured username, the
// Password prompt with the token.
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
	}
	if got := execAskpass(t, link, "Username for 'https://github.com': ", env); got != "x-access-token\n" {
		t.Errorf("Username prompt answered %q, want %q", got, "x-access-token\n")
	}
	if got := execAskpass(t, link, "Password for 'https://x-access-token@github.com': ", env); got != "sekret-token\n" {
		t.Errorf("Password prompt answered %q, want %q", got, "sekret-token\n")
	}
}

// TestGitAskpassAnswerTwoPrompts covers the in-process answer logic: username
// prompts (any case) get the username, everything else gets the token.
func TestGitAskpassAnswerTwoPrompts(t *testing.T) {
	t.Setenv(tools.GitAskpassUserEnv, "x-access-token")
	t.Setenv(tools.GitAskpassTokenEnv, "sekret-token")
	cases := []struct {
		prompt, want string
	}{
		{"Username for 'https://github.com': ", "x-access-token"},
		{"username: ", "x-access-token"},
		{"Password for 'https://github.com': ", "sekret-token"},
		{"", "sekret-token"},
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
