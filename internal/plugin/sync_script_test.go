package plugin

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The vendored plugin trees under .agents/vendor are the build input and carry
// local edits that exist nowhere else, so scripts/sync-plugins.sh must refuse
// to overwrite a drifted tree. Its own test drives a throwaway local upstream
// (no network): clean tree syncs, dirty tree refuses, FORCE=1 discards.
func TestSyncPluginsScript(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	script := filepath.Join(repoRoot(t), "scripts", "sync-plugins_test.sh")
	out, err := exec.Command("bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sync-plugins_test.sh failed: %v\n%s", err, out)
	}
}
