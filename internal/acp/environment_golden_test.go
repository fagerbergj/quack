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

// TestGoldenEnvironmentBlock pins four fixtures (plain, empty, read-only+sandboxed, sandboxed
// git repo) so each new line renders only where its condition actually holds.
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

	// The cwd and TMPDIR are machine-specific; the block's shape is not.
	norm := func(cwd, s string) string {
		s = strings.ReplaceAll(s, cwd, "<CWD>")
		return strings.ReplaceAll(s, os.TempDir(), "<TMP>")
	}
	ctx := context.Background()
	checkEnvGolden(t, "environment.txt", norm(populated, environmentBlock(ctx, nil, populated, workspace.Caps{})))
	checkEnvGolden(t, "environment.empty.txt", norm(empty, environmentBlock(ctx, nil, empty, workspace.Caps{})))
	ro := workspace.Caps{ReadOnly: true, HomeDir: "/home/agent", Sandbox: workspace.SandboxBwrap, Env: map[string]string{"GOMODCACHE": "/usr/local/go/pkg/mod"}}
	checkEnvGolden(t, "environment.readonly.txt", norm(populated, environmentBlock(ctx, nil, populated, ro)))

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "quack/work")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "init")
	// HomeDir set (and distinct from repo): childEnv farms a writable GOMODCACHE under HOME
	// (EnsureWritableGoModCache), which would otherwise land inside repo and pollute its own entries.
	sandboxed := workspace.Caps{Sandbox: workspace.SandboxBwrap, HomeDir: t.TempDir(), Env: map[string]string{"GOMODCACHE": "/usr/local/go/pkg/mod"}}
	got := norm(repo, environmentBlock(ctx, nil, repo, sandboxed))
	got = normHexSha.ReplaceAllString(got, "<SHA>")
	checkEnvGolden(t, "environment.sandboxed-git.txt", got)
}

var normHexSha = regexp.MustCompile(`HEAD [0-9a-f]{7,40}`)
