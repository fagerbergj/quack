package vetting

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// EnsureMermaidValidatorDeps runs npm ci in scripts/ for the validator tests. vetting and tools tests
// share it under a cross-process lockfile because go test runs their binaries in parallel.
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
		return fmt.Errorf("npm ci in %s failed: %w\n%s", dir, err, out)
	}
	return nil
}

// mermaidDepsPresent checks both validator packages are installed, not just a leftover node_modules.
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

// acquireLock is an O_EXCL mutex across processes (separate go test binaries can't share a Go lock).
// A lock file older than lockStaleAfter is reclaimed so a crashed holder can't wedge the suite.
func acquireLock(path string, timeout time.Duration) (release func(), err error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create lock %s: %w", path, err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			_ = os.Remove(path) // holder crashed mid-install; reclaim rather than wedge forever
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting %s for lock %s", timeout, path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
