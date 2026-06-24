package serve

import (
	"context"
	"os"
	"testing"

	"google.golang.org/adk/tool/skilltoolset/skill"
)

// TestSkillsLoad guards against a shipped skill whose SKILL.md frontmatter fails
// the skilltoolset's validation (e.g. an invalid name) — which crashes startup.
func TestSkillsLoad(t *testing.T) {
	const dir = "../../skills"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read skills dir: %v", err)
	}
	src := skill.NewFileSystemSource(os.DirFS(dir))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := src.LoadFrontmatter(context.Background(), e.Name()); err != nil {
			t.Errorf("skill %q frontmatter failed to load: %v", e.Name(), err)
		}
	}
}
