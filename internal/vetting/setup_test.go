package vetting

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

func TestRunCheckSetup_EmptyIsNoop(t *testing.T) {
	dir := t.TempDir()
	runCheckSetup(dir, nil, workspace.DefaultCaps())
	if _, done := setupCache.Load(dir); done {
		t.Error("unconfigured check_setup must not mark dir as done - byte-identical to the no-config baseline")
	}
}

func TestRunCheckSetup_RunsOncePerDir(t *testing.T) {
	dir := t.TempDir()
	caps := workspace.DefaultCaps()
	marker := filepath.Join(dir, "marker")
	setup := []string{"touch " + marker}

	runCheckSetup(dir, setup, caps)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("setup did not run: %v", err)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	runCheckSetup(dir, setup, caps) // same dir again: cached, must not rerun
	if _, err := os.Stat(marker); err == nil {
		t.Error("setup ran a second time for the same dir - a shared clone's bootstrap must run once, not every gate round")
	}
}
