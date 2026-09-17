package acp

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// updateEnvGolden regenerates testdata/prompts. The goldens were captured
// before the artifact-resolver change (#1420) and must stay byte-identical.
var updateEnvGolden = flag.Bool("update-golden", false, "rewrite the environment block golden files")

func checkEnvGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "prompts", name)
	if *updateEnvGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run go test -run Golden -update-golden)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s: environment block changed\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// TestGoldenEnvironmentBlock pins five fixtures so each new line renders only where its
// condition holds; preseed presence is injected, not read from the real host - a native install has none.
func TestGoldenEnvironmentBlock(t *testing.T) {
	populated := t.TempDir()
	for _, n := range []string{"go.mod", "README.md"} {
		if err := os.WriteFile(filepath.Join(populated, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(populated, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	missingModCache := filepath.Join(t.TempDir(), "no-preseed-here")

	// The cwd and TMPDIR are machine-specific; the block's shape is not.
	norm := func(cwd, s string) string {
		s = strings.ReplaceAll(s, cwd, "<CWD>")
		return strings.ReplaceAll(s, os.TempDir(), "<TMP>")
	}
	ctx := context.Background()
	checkEnvGolden(t, "environment.txt", norm(populated, environmentBlock(ctx, nil, populated, workspace.Caps{})))
	checkEnvGolden(t, "environment.empty.txt", norm(empty, environmentBlock(ctx, nil, empty, workspace.Caps{})))
	ro := workspace.Caps{ReadOnly: true, HomeDir: "/home/agent", Sandbox: workspace.SandboxBwrap, Env: map[string]string{"GOMODCACHE": missingModCache}}
	checkEnvGolden(t, "environment.readonly.txt", norm(populated, environmentBlock(ctx, nil, populated, ro)))

	noPreseed := workspace.Caps{Sandbox: workspace.SandboxBwrap, HomeDir: t.TempDir(), Env: map[string]string{"GOMODCACHE": missingModCache}}
	checkEnvGolden(t, "environment.sandboxed-no-preseed.txt", norm(populated, environmentBlock(ctx, nil, populated, noPreseed)))

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	runGitIsolated(t, repo, "init", "-q")
	// symbolic-ref, not `git init -b` (needs git 2.28+) or a later `checkout -b` (fails on an
	// unborn HEAD on some git versions): sets the initial branch name on every git version.
	runGitIsolated(t, repo, "symbolic-ref", "HEAD", "refs/heads/quack/work")
	runGitIsolated(t, repo, "add", "-A")
	runGitIsolated(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "init")
	// A real, existing (with-preseed) dir at a fixed name under os.TempDir() - not t.TempDir(),
	// whose random per-run root would make the rendered path (and the golden) non-deterministic.
	presentModCache := filepath.Join(os.TempDir(), "quack-golden-test-gomodcache-preseed")
	if err := os.MkdirAll(presentModCache, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(presentModCache) })
	// HomeDir distinct from repo: childEnv farms a writable GOMODCACHE under HOME, which would
	// otherwise pollute repo's own entries.
	sandboxed := workspace.Caps{Sandbox: workspace.SandboxBwrap, HomeDir: t.TempDir(), Env: map[string]string{"GOMODCACHE": presentModCache}}
	block := environmentBlock(ctx, nil, repo, sandboxed)
	if !strings.Contains(block, "git: yes (branch quack/work") {
		// The fixture repo, not the golden, is what failed - gitInfo degrades to "git: no" on
		// ANY probe failure (environment.go), so dump the repo's own state instead of guessing.
		status, _ := exec.Command("git", "-C", repo, "status").CombinedOutput()
		branch, _ := exec.Command("git", "-C", repo, "branch", "--show-current").CombinedOutput()
		t.Fatalf("environmentBlock did not detect the fixture git repo; block=%q\ngit status:\n%s\ngit branch:\n%s", block, status, branch)
	}
	got := norm(repo, block)
	got = normHexSha.ReplaceAllString(got, "<SHA>")
	checkEnvGolden(t, "environment.sandboxed-git.txt", got)
}

var normHexSha = regexp.MustCompile(`HEAD [0-9a-f]{7,40}`)

// runGitIsolated runs git isolated from the runner's own global/system config (a missing
// user.name/email, an unexpected safe.directory or hooksPath entry) so fixture setup is
// self-contained; unlike runGit, no config from outside this call can make it flaky.
func runGitIsolated(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
