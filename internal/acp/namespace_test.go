package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoStaleMemoryPrefixedToolNames pins #558's "done when" requirement that
// this cannot silently drift: nothing in the agent bundles or skill library
// may hardcode the old "quack_memory_<tool>"-prefixed form of the review/PR
// tools opencode derives client-side from the MCP server's advertised Name
// (memorymcp.go). Bundles reference the bare tool name (e.g. stage_review),
// never the prefixed one - a prefixed reference means either a stale doc or a
// server rename nobody updated the prompts for.
func TestNoStaleMemoryPrefixedToolNames(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dirs := []string{"agents", "skills"}
	const badPrefix = "quack_memory_"
	for _, d := range dirs {
		root := filepath.Join(repoRoot, d)
		if _, err := os.Stat(root); err != nil {
			continue // optional directory
		}
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
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
