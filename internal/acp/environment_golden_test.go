package acp

import (
	"context"
	"flag"
	"os"
	"path/filepath"
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

// TestGoldenEnvironmentBlock pins the ACP environment block for each branch it
// renders: a non-git tree, an empty tree, and the read-only filesystem facts.
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
	checkEnvGolden(t, "environment.txt", norm(populated, environmentBlock(ctx, populated, workspace.Caps{})))
	checkEnvGolden(t, "environment.empty.txt", norm(empty, environmentBlock(ctx, empty, workspace.Caps{})))
	ro := workspace.Caps{ReadOnly: true, HomeDir: "/home/agent"}
	checkEnvGolden(t, "environment.readonly.txt", norm(populated, environmentBlock(ctx, populated, ro)))
}
