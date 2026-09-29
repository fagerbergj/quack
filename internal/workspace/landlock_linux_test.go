package workspace

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestSplitFiles: file grants get file rules; dirs and missing paths keep dir rules.
func TestSplitFiles(t *testing.T) {
	dir := t.TempDir()
	file, missing := filepath.Join(dir, "f"), filepath.Join(dir, "gone")
	writeFile(t, file, "")
	dirs, files := splitFiles([]string{dir, file, missing})
	if !slices.Equal(dirs, []string{dir, missing}) || !slices.Equal(files, []string{file}) {
		t.Errorf("dirs %v files %v", dirs, files)
	}
}
