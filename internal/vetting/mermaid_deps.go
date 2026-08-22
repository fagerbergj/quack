package vetting

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// EnsureMermaidValidatorDeps provisions scripts/node_modules (npm ci) so the
// mermaid validator tests just work on a fresh clone. Both internal/vetting's
// and internal/tools' tests call this SAME function - they used to run their
// own `npm ci` independently, which raced when go test ran both packages'
// binaries in parallel against the same scripts/ directory.
//
// An flock-style lockfile serializes the provisioning across the separate OS
// processes go test spawns per package (a sync.Once only dedupes within one
// process). A stale lock (holder crashed mid-install) is reclaimed after
// lockStaleAfter rather than wedging the suite forever.
func EnsureMermaidValidatorDeps() error {
	dir := filepath.Dir(mermaidValidatorPath)
	if mermaidDepsPresent(dir) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "package-lock.json")); err != nil {
		return fmt.Errorf("no package-lock.json in %s", dir)
	}

	release, err := acquireLock(filepath.Join(dir, ".npm-ci.lock"), lockWaitTimeout)
	if err != nil {
		return err
	}
	defer release()

	// Re-check: whoever held the lock before us may have just finished this.
	if mermaidDepsPresent(dir) {
		return nil
	}
	cmd := exec.Command("npm", "ci")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("npm ci in %s failed: %v\n%s", dir, err, out)
	}
	return nil
}

// mermaidDepsPresent is the cheap idempotency check: both packages the
// validator needs, actually installed - not just a node_modules directory
// left over from a partial/interrupted install.
func mermaidDepsPresent(dir string) bool {
	for _, pkg := range [...]string{"mermaid", "jsdom"} {
		if _, err := os.Stat(filepath.Join(dir, "node_modules", pkg, "package.json")); err != nil {
			return false
		}
	}
	return true
}

const (
	lockWaitTimeout = 3 * time.Minute
	lockStaleAfter  = 90 * time.Second
)

// acquireLock is an O_EXCL-based mutex across processes (not goroutines -
// separate `go test` binaries per package can't share a Go-level lock). A
// lock file older than lockStaleAfter is assumed abandoned by a crashed
// holder and reclaimed, so a dead process can't wedge the suite forever.
func acquireLock(path string, timeout time.Duration) (release func(), err error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create lock %s: %w", path, err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			os.Remove(path) // holder crashed mid-install; reclaim rather than wedge forever
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting %s for lock %s", timeout, path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
