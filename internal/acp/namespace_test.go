package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoStaleMemoryPrefixedToolNames pins #558's "done when" requirement that
// this cannot silently drift: nothing in the agent bundles, skill library,
// Go source, or docs may hardcode the old "quack_memory_<tool>"-prefixed form
// of the review/PR tools opencode derives client-side from the MCP server's
// advertised Name (memorymcp.go). Bundles reference the bare tool name (e.g.
// stage_review), never the prefixed one - a prefixed reference means either a
// stale doc or a server rename nobody updated the prompts for. Vendored and
// generated trees are skipped: they never hand-carry a tool name, and walking
// them (vendored plugins, the embedded SPA client) is pure waste.
func TestNoStaleMemoryPrefixedToolNames(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	const badPrefix = "quack_memory_"
	dirs := []string{"agents", "skills", "internal", "docs"}
	skipDirs := map[string]bool{
		filepath.Join(repoRoot, ".agents", "vendor"):            true,
		filepath.Join(repoRoot, "frontend", "src", "generated"): true,
	}
	self, err := filepath.Abs("namespace_test.go")
	if err != nil {
		t.Fatal(err)
	}
	skipFiles := map[string]bool{
		filepath.Join(repoRoot, "internal", "schema", "quack.gen.go"): true,
		self: true, // this file's own badPrefix literal isn't a stale reference
	}
	for _, d := range dirs {
		root := filepath.Join(repoRoot, d)
		if _, err := os.Stat(root); err != nil {
			continue // optional directory
		}
		err := filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				if de.Name() == "node_modules" || de.Name() == ".git" || skipDirs[path] {
					return filepath.SkipDir
				}
				return nil
			}
			if skipFiles[path] {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(b), badPrefix) {
				t.Errorf("%s references the stale memory-prefixed tool namespace %q", path, badPrefix)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
