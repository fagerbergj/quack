package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// TestScanChildPIDsFindsChild: the /proc fallback (no CONFIG_PROC_CHILDREN) sees a direct child.
func TestScanChildPIDsFindsChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if got := scanChildPIDs(os.Getpid()); !slices.Contains(got, cmd.Process.Pid) {
		t.Errorf("scanChildPIDs = %v, want it to include %d", got, cmd.Process.Pid)
	}
	if got := childPIDs(); !slices.Contains(got, cmd.Process.Pid) {
		t.Errorf("childPIDs = %v, want it to include %d", got, cmd.Process.Pid)
	}
	if parentPID(-1) != -1 {
		t.Error("parentPID of a missing process should be -1")
	}
}

// TestForwardSignals: graceful signals pass through as-is; reapStopSignal becomes SIGKILL.
func TestForwardSignals(t *testing.T) {
	for _, tc := range []struct{ in, want syscall.Signal }{
		{syscall.SIGTERM, syscall.SIGTERM},
		{reapStopSignal, syscall.SIGKILL},
	} {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		sigs, done := make(chan os.Signal, 1), make(chan struct{})
		go forwardSignals(cmd.Process, sigs, done)
		sigs <- tc.in
		_ = cmd.Wait()
		close(done)
		if ws := cmd.ProcessState.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != tc.want {
			t.Errorf("%v forwarded as %v, want %v", tc.in, cmd.ProcessState, tc.want)
		}
	}
}
