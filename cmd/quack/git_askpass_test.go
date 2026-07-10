package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/tools"
)

// TestGitAskpassPrintsOnlyItsEnvVar: the hidden git-askpass mode prints
// exactly the token env var's value (plus a newline for git to strip) —
// nothing else, regardless of the prompt git passes as an argument.
func TestGitAskpassPrintsOnlyItsEnvVar(t *testing.T) {
	t.Setenv(tools.GitAskpassTokenEnv, "sekret-token")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"git-askpass", "Password for 'https://github.com':"})
	if err := root.Execute(); err != nil {
		t.Fatalf("git-askpass errored: %v", err)
	}
	if got := out.String(); got != "sekret-token\n" {
		t.Errorf("git-askpass printed %q, want %q", got, "sekret-token\n")
	}
}

// TestGitAskpassEmptyEnvPrintsEmptyLine: with the env var unset, output is an
// empty line (git then treats the credential as empty) — never an error, so a
// misconfigured invocation can't hang a git child on a non-zero askpass exit.
func TestGitAskpassEmptyEnvPrintsEmptyLine(t *testing.T) {
	t.Setenv(tools.GitAskpassTokenEnv, "")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"git-askpass"})
	if err := root.Execute(); err != nil {
		t.Fatalf("git-askpass errored: %v", err)
	}
	if got := out.String(); got != "\n" {
		t.Errorf("git-askpass printed %q, want just a newline", got)
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
