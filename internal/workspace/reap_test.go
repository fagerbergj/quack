package workspace

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
		cmd := startWrapped(t, dir, caps, escapeScript+"; exec sleep 300", "gc.pid")
		if err := StopChild(cmd); err != nil {
			t.Fatalf("StopChild: %v", err)
		}
		_ = cmd.Wait()
		requireGrandchildGone(t, dir)
	})
}

// startWrapped starts script the way ACP and plugin children start (WrapArgv, own group) and
// waits until it has written dir/ready.
func startWrapped(t *testing.T, dir string, caps Caps, script, ready string) *exec.Cmd {
	t.Helper()
	argv := WrapArgv(dir, []string{"sh", "-c", script}, caps, nil, nil)
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
		if b, _ := os.ReadFile(filepath.Join(dir, ready)); len(b) > 0 {
			return cmd
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("%s never appeared", ready)
		}
	}
}

// TestReapForwardsGracefulSignals: SIGINT/SIGTERM sent to the wrapper (a Ctrl-C, the MCP SDK's
// stop) reach the target's trap instead of becoming a SIGKILL.
func TestReapForwardsGracefulSignals(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{{syscall.SIGINT, 5}, {syscall.SIGTERM, 6}} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			reapModes(t, func(t *testing.T, dir string, caps Caps) {
				script := fmt.Sprintf(`trap "exit %d" %d; echo x > ready; sleep 300 & wait`, tc.code, tc.sig)
				cmd := startWrapped(t, dir, caps, script, "ready")
				_ = cmd.Process.Signal(tc.sig)
				_ = cmd.Wait()
				if got := cmd.ProcessState.ExitCode(); got != tc.code {
					t.Fatalf("exit = %d (%v), want the trap's %d", got, cmd.ProcessState, tc.code)
				}
			})
		})
	}
}

// TestReapSurvivesGroupSIGTERM: `kill 0` also hits the wrapper; a target ignoring SIGTERM
// keeps running and its own exit status comes back.
func TestReapSurvivesGroupSIGTERM(t *testing.T) {
	res, err := RunArgv(context.Background(), t.TempDir(), []string{"sh", "-c", `trap "" TERM; kill 0; exit 7`}, sandboxCaps(t, SandboxNone))
	if err != nil || res.ExitCode != 7 {
		t.Fatalf("RunArgv: err=%v exit=%d output=%q, want exit 7", err, res.ExitCode, res.Output)
	}
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

// chainScript re-forks itself into a new session and exits, forever: each link lives only long
// enough to spawn the next, the shape that outran a sweep that paused between rounds. A stop file ends it.
const chainScript = `[ -e "$(dirname "$0")/stop" ] && exit 0; exec setsid -f sh "$0"`

// liveChainLinks counts running processes whose argv names script.
func liveChainLinks(script string) int {
	n := 0
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err == nil && strings.Contains(string(b), script) {
			n++
		}
	}
	return n
}

// TestRunArgvReapsForkAndDieChain: a chain that keeps re-parenting to the reaper is still swept.
func TestRunArgvReapsForkAndDieChain(t *testing.T) {
	reapModes(t, func(t *testing.T, dir string, caps Caps) {
		script := filepath.Join(dir, "chain.sh")
		if err := os.WriteFile(script, []byte(chainScript+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(filepath.Join(dir, "stop"), nil, 0o644) })
		res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", "setsid -f sh " + script + "; sleep 0.3"}, caps)
		if err != nil || res.ExitCode != 0 || strings.Contains(res.Output, "quack-reap") {
			t.Fatalf("RunArgv: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
		}
		time.Sleep(200 * time.Millisecond)
		if n := liveChainLinks(script); n > 0 {
			t.Fatalf("fork-and-die chain outlived its command (%d live links)", n)
		}
	})
}

// TestStopChildEscalatesPastStuckReaper: a wrapper that ignores the stop signal still dies with
// its group once reapGrace runs out, and the server says so.
func TestStopChildEscalatesPastStuckReaper(t *testing.T) {
	old := reapGrace
	reapGrace = 100 * time.Millisecond
	t.Cleanup(func() { reapGrace = old })
	buf := &lockedBuffer{}
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	dir := t.TempDir()
	// Args[1] == ReapArg marks it as a wrapper; sh runs the file of that name, which ignores the stop.
	if err := os.WriteFile(filepath.Join(dir, ReapArg), []byte("trap '' USR2\necho x > ready\nexec sleep 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{Path: "/bin/sh", Args: []string{"sh", ReapArg}, Dir: dir,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(filepath.Join(dir, "ready")); len(b) > 0 {
			break
		}
	}
	if err := StopChild(cmd); err != nil {
		t.Fatalf("StopChild: %v", err)
	}
	_ = cmd.Wait()
	if ws := cmd.ProcessState.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("stuck wrapper ended %v, want SIGKILL from the escalation", cmd.ProcessState)
	}
	// The timer goroutine logs after its kill lands, so Wait can return first.
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(buf.String(), "sandbox reaper still running"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("escalation not logged: %s", buf.String())
		}
	}
}

// lockedBuffer is a bytes.Buffer safe for a logging goroutine and the test to share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStopChildWithoutReaperKillsChild: bwrap-style children get the group kill, and a child
// that leads no group (the sandbox CLI) is killed directly.
func TestStopChildWithoutReaperKillsChild(t *testing.T) {
	for _, setpgid := range []bool{true, false} {
		cmd := exec.Command("sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: setpgid}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := StopChild(cmd); err != nil {
			t.Fatalf("setpgid=%v: StopChild: %v", setpgid, err)
		}
		_ = cmd.Wait()
		if ws := cmd.ProcessState.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Errorf("setpgid=%v: child ended %v, want SIGKILL", setpgid, cmd.ProcessState)
		}
	}
	if StopChild(&exec.Cmd{}) != nil {
		t.Error("StopChild on an unstarted command should be a no-op")
	}
}
