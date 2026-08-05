package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// grep must be bounded in BYTES, not just in match count (a live grep
// returned 48 MB against a match cap - bound the bytes, not just the count).
func TestGrepIsBoundedInBytes(t *testing.T) {
	b, root := testBinding(t)

	// A minified bundle: one enormous line, matching the pattern.
	minified := "var x=1;" + strings.Repeat("function f(){return 'needle';};", 40_000)
	writeFile(t, root, "bundle.min.js", minified)

	res, err := b.grep(grepArgs{Pattern: "needle", Path: "."})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}

	total := 0
	for _, m := range res.Matches {
		if len(m.Text) > grepMatchMaxChars {
			t.Errorf("a single match is %d chars (cap %d) - one minified line is enough to blow the context window",
				len(m.Text), grepMatchMaxChars)
		}
		total += len(m.Text)
	}
	if total > grepTotalMaxBytes {
		t.Fatalf("grep returned %d bytes of matches (cap %d) - this is the 48 MB result that 400'd the node",
			total, grepTotalMaxBytes)
	}
}

// grep must not descend into vendored/generated trees. This is where the monster
// results live, and a hit there is never what anyone asked for.
func TestGrepSkipsGeneratedTrees(t *testing.T) {
	b, root := testBinding(t)

	writeFile(t, root, "src/app.ts", "const needle = 1")
	writeFile(t, root, ".next/build/chunks/runtime.js.map", "needle")
	writeFile(t, root, "node_modules/left-pad/index.js", "needle")
	writeFile(t, root, "dist/bundle.js", "needle")

	res, err := b.grep(grepArgs{Pattern: "needle", Path: "."})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(res.Matches) != 1 {
		var got []string
		for _, m := range res.Matches {
			got = append(got, m.Path)
		}
		t.Fatalf("grep returned %v; want only src/app.ts (.next, node_modules and dist must never be searched)", got)
	}
	if res.Matches[0].Path != "src/app.ts" {
		t.Fatalf("matched %q, want src/app.ts", res.Matches[0].Path)
	}
}

// ...but pointing `path` straight AT a generated tree only ever means it. An agent
// that deliberately reads a dependency's source must still be able to.
func TestGrepSearchesAGeneratedTreeWhenAskedExplicitly(t *testing.T) {
	b, root := testBinding(t)
	writeFile(t, root, "node_modules/left-pad/index.js", "needle")

	res, err := b.grep(grepArgs{Pattern: "needle", Path: "node_modules/left-pad"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("got %d matches; an explicit path into node_modules must still be searched", len(res.Matches))
	}
}

// grep must never slurp a huge file into memory: it reads whole files, so an
// unbounded read is an OOM. (This machine has been OOM-killed by less.)
func TestGrepSkipsOversizedFiles(t *testing.T) {
	b, root := testBinding(t)
	writeFile(t, root, "huge.txt", strings.Repeat("needle\n", (grepFileMaxBytes/7)+10))

	res, err := b.grep(grepArgs{Pattern: "needle", Path: "."})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(res.Matches) != 0 {
		t.Fatalf("grep scanned a file over the %d-byte limit (%d matches)", int64(grepFileMaxBytes), len(res.Matches))
	}
}

// glob must not hand back thousands of paths from generated trees - the agent
// then wastes its turns reading them.
func TestGlobSkipsGeneratedTrees(t *testing.T) {
	b, root := testBinding(t)
	writeFile(t, root, "src/app.ts", "x")
	writeFile(t, root, "node_modules/left-pad/index.ts", "x")

	res, err := b.glob(globArgs{Pattern: "**/*.ts", Path: "."})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(res.Paths) != 1 || !strings.HasSuffix(res.Paths[0], "src/app.ts") {
		t.Fatalf("glob returned %v; want only src/app.ts", res.Paths)
	}
}

// testBinding returns an fsBinding rooted at a fresh temp jail, plus that root.
func testBinding(t *testing.T) (fsBinding, string) {
	t.Helper()
	root := t.TempDir()
	jail, err := workspace.NewJail(root)
	if err != nil {
		t.Fatalf("jail: %v", err)
	}
	return fsBinding{userID: "local", jail: jail, caps: workspace.DefaultCaps()}, filepath.Join(root, "local")
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
