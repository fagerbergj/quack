package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// unreaped strips withReaper's prefix so argv-shape tests compare the wrapped command itself.
func unreaped(argv []string) []string {
	if len(argv) > 3 && argv[1] == ReapArg {
		return argv[3:]
	}
	return argv
}

// escapeScript forks a grandchild into its own session (out of the process group quack used to
// kill), records its pid in gc.pid, and waits until it has.
const escapeScript = `setsid -f sh -c 'echo $$ > gc.pid; exec sleep 300'; while [ ! -s gc.pid ]; do sleep 0.01; done`

// reapModes runs fn per non-bwrap sandbox mode, skipping landlock where the kernel lacks it.
func reapModes(t *testing.T, fn func(t *testing.T, dir string, caps Caps)) {
	if runtime.GOOS != "linux" {
		t.Skip("subreaper and /proc are Linux-only")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid(1) not installed")
	}
	for _, mode := range []SandboxMode{SandboxNone, SandboxLandlock} {
		t.Run(string(mode), func(t *testing.T) {
			if mode == SandboxLandlock {
				requireLandlock(t)
			}
			dir := t.TempDir()
			caps := sandboxCaps(t, mode)
			caps.WorkRoot = dir
			fn(t, dir, caps)
		})
	}
}

// requireGrandchildGone fails when the pid in dir/gc.pid is still the sleeping grandchild.
func requireGrandchildGone(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "gc.pid"))
	if err != nil {
		t.Fatalf("grandchild never recorded its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("gc.pid = %q: %v", b, err)
	}
	if cmd, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil && strings.HasPrefix(string(cmd), "sleep\x00300") {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("setsid'd grandchild %d outlived its sandboxed command", pid)
	}
}

// TestRunArgvReapsSetsidGrandchild: once a command returns, nothing it forked is left running,
// even a grandchild in its own session that a process-group kill never reaches.
func TestRunArgvReapsSetsidGrandchild(t *testing.T) {
	reapModes(t, func(t *testing.T, dir string, caps Caps) {
		res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", escapeScript + "; echo done"}, caps)
		if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "done") {
			t.Fatalf("RunArgv: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
		}
		requireGrandchildGone(t, dir)
	})
}

// TestStopChildReapsSetsidGrandchild is the ACP/plugin shape: a long-lived WrapArgv child torn
// down by StopChild takes every descendant with it before Wait returns.
func TestStopChildReapsSetsidGrandchild(t *testing.T) {
	reapModes(t, func(t *testing.T, dir string, caps Caps) {
		argv := WrapArgv(dir, []string{"sh", "-c", escapeScript + "; exec sleep 300"}, caps, nil, nil)
		if argv[1] != ReapArg {
			t.Fatalf("WrapArgv(%s) = %v, want a %s wrapper", caps.Sandbox, argv, ReapArg)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if b, _ := os.ReadFile(filepath.Join(dir, "gc.pid")); len(b) > 0 {
				break
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				t.Fatal("grandchild never started")
			}
		}
		if err := StopChild(cmd); err != nil {
			t.Fatalf("StopChild: %v", err)
		}
		_ = cmd.Wait()
		requireGrandchildGone(t, dir)
	})
}

// TestLandlockChildCannotSignalReaper: signal scoping keeps a confined child from killing the
// __reap wrapper, which would orphan its descendants out of the sweep.
func TestLandlockChildCannotSignalReaper(t *testing.T) {
	requireLandlock(t)
	if v, err := ll.LandlockGetABIVersion(); err != nil || v < 6 {
		t.Skipf("Landlock ABI %d < 6: no signal scoping on this kernel", v)
	}
	dir := t.TempDir()
	caps := sandboxCaps(t, SandboxLandlock)
	caps.WorkRoot = dir
	res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", "kill -9 $PPID && echo killed || echo denied"}, caps)
	if err != nil || !strings.Contains(res.Output, "denied") {
		t.Fatalf("confined child signalled its reaper: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
	}
}
