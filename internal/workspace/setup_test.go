package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunCheckSetup_EmptyIsNoop(t *testing.T) {
	dir := t.TempDir()
	RunCheckSetup(dir, nil, DefaultCaps())
	if _, done := setupCache.Load(dir); done {
		t.Error("unconfigured check_setup must not mark dir as done - byte-identical to the no-config baseline")
	}
}

func TestRunCheckSetup_RunsOncePerDir(t *testing.T) {
	dir := t.TempDir()
	caps := DefaultCaps()
	marker := filepath.Join(dir, "marker")
	setup := []string{"touch " + marker}

	RunCheckSetup(dir, setup, caps)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("setup did not run: %v", err)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	RunCheckSetup(dir, setup, caps) // same dir again: cached, must not rerun
	if _, err := os.Stat(marker); err == nil {
		t.Error("setup ran a second time for the same dir - a shared clone's bootstrap must run once, not every gate round")
	}
}
