package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestMain mirrors main()'s argv dispatch (__sandbox-exec and GIT_ASKPASS) so tests exercise the real
// self-exec paths, catching an unexecutable GIT_ASKPASS value an in-process call would miss.
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

// TestGitAskpassSymlinkExecsBothPrompts execs GIT_ASKPASS exactly as git does (symlink, no shell, prompt as
// argv[1]): the Username prompt gets the username, the Password prompt the token.
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
	t.Setenv(tools.GitAskpassHostEnv, "ks.example")
	if got := tools.GitAskpassAnswer("Password for 'https://\u212a\u017f.example': "); got != "" {
		t.Errorf("Unicode-folded host (Kelvin sign, long s) got %q, want nothing", got)
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

// TestGitAskpassUnderConfinedGit: git confined by Landlock still execs the askpass link, and the token reaches
// only a server whose host the askpass expects.
func TestGitAskpassUnderConfinedGit(t *testing.T) {
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	t.Cleanup(func() { workspace.ConfineGit(false) })
	if !workspace.ConfineGit(true) {
		t.Skip("SKIPPING: landlock unavailable")
	}
	creds := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); ok {
			creds <- user + ":" + pass
			http.NotFound(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="q"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), tools.GitAskpassLinkName)
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	prev := workspace.GitProtocol
	workspace.GitProtocol = "http"
	t.Cleanup(func() { workspace.GitProtocol = prev })

	lsRemote := func(host string) string {
		env := []string{"PATH=/usr/bin:/bin", "GIT_ASKPASS=" + link, tools.GitAskpassUserEnv + "=x-access-token",
			tools.GitAskpassTokenEnv + "=sekret-token", tools.GitAskpassHostEnv + "=" + host}
		cmd, done, err := workspace.GitCmd(context.Background(), bin, "", "", []string{"ls-remote", srv.URL + "/r.git"}, env)
		if err != nil {
			t.Fatal(err)
		}
		defer done()
		_ = cmd.Run() // the server 404s even an authenticated request
		select {
		case c := <-creds:
			return c
		default:
			return ""
		}
	}
	if got := lsRemote(strings.TrimPrefix(srv.URL, "http://")); got != "x-access-token:sekret-token" {
		t.Errorf("confined git sent credentials %q, want the askpass answer", got)
	}
	if got := lsRemote("github.com"); strings.Contains(got, "sekret-token") {
		t.Errorf("token reached a host askpass does not expect: %q", got)
	}
}
