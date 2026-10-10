package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoStaleMemoryPrefixedToolNames: no bundle, skill, Go source or doc may hardcode the "quack_memory_<tool>"
// prefixed names; bundles use bare names. Vendored and generated trees are skipped.
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
