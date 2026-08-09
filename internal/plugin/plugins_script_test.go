package plugin

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The plugin trees are not in git, so scripts/plugins.sh is the only thing that
// puts them on disk - and it must never delete a tree it cannot re-fetch.
func TestPluginsScript(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	script := filepath.Join(repoRoot(t), "scripts", "plugins_test.sh")
	out, err := exec.Command("bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("plugins_test.sh failed: %v\n%s", err, out)
	}
}
