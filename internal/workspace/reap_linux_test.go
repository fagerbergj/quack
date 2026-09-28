package workspace

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestReapRunInProcess drives the reaper in this process (the subprocess runs escape coverage):
// the orphaned setsid grandchild is swept and the target's own exit status comes back.
func TestReapRunInProcess(t *testing.T) {
	t.Cleanup(func() { _, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0) })
	dir := t.TempDir()
	script := strings.ReplaceAll(escapeScript, "gc.pid", filepath.Join(dir, "gc.pid")) + "; exit 3"
	ws, err := reapRun([]string{"sh", "-c", script})
	if err != nil || ws.ExitStatus() != 3 {
		t.Fatalf("reapRun: status=%v err=%v, want exit 3", ws, err)
	}
	requireGrandchildGone(t, dir)
}

// TestReapMainRejectsBadArgv: a missing "--" or an unknown binary fails before anything runs.
func TestReapMainRejectsBadArgv(t *testing.T) {
	for _, args := range [][]string{nil, {"sh"}, {"--", "/nonexistent/quack-reap-target"}} {
		if err := ReapMain(args); err == nil {
			t.Errorf("ReapMain(%q) = nil, want an error", args)
		}
	}
}
