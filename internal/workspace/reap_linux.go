//go:build linux

package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// prSetChildSubreaper is PR_SET_CHILD_SUBREAPER (linux/prctl.h); syscall has no name for it.
const prSetChildSubreaper = 36

// reapSweepBudget bounds ReapMain's kill-and-reap sweep; StopChild's reapGrace must exceed it.
var reapSweepBudget = 5 * time.Second

// ReapMain implements the __reap argv mode: become a child subreaper, run target, and once it
// exits or SIGTERM/SIGINT/SIGHUP arrives, SIGKILL and reap every descendant, then exit as target did.
func ReapMain(args []string) error {
	if len(args) < 2 || args[0] != "--" {
		return fmt.Errorf("reap: no target command (want -- CMD ARGS)")
	}
	ws, err := reapRun(args[1:])
	if err != nil {
		return err
	}
	exitLike(ws)
	return nil
}

// reapRun is ReapMain short of exiting: it returns target's wait status once every descendant is gone.
func reapRun(target []string) (syscall.WaitStatus, error) {
	bin, err := exec.LookPath(target[0])
	if err != nil {
		return 0, fmt.Errorf("reap: %q not found: %w", target[0], err)
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return 0, fmt.Errorf("reap: PR_SET_CHILD_SUBREAPER: %w", errno)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	// os.Process signals through a pidfd, so a late SIGTERM can't hit a reused PID once reaped below.
	p, err := os.StartProcess(bin, target, &os.ProcAttr{Env: os.Environ(), Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	if err != nil {
		return 0, fmt.Errorf("reap: start %q: %w", target[0], err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigs:
			_ = p.Signal(syscall.SIGKILL)
		case <-done:
		}
	}()
	ws, werr := waitForPID(p.Pid)
	if left := sweepDescendants(reapSweepBudget); left > 0 {
		fmt.Fprintf(os.Stderr, "quack-reap: %d descendant(s) of %s still alive after a %s sweep\n",
			left, filepath.Base(bin), reapSweepBudget)
	}
	return ws, werr
}

// waitForPID reaps children (orphans adopted meanwhile included) until pid itself exits.
// An error fails closed: a lost status must never read as exit 0.
func waitForPID(pid int) (syscall.WaitStatus, error) {
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
		case err != nil:
			return 0, fmt.Errorf("reap: wait for %d: %w", pid, err)
		case got == pid:
			return ws, nil
		}
	}
}

// sweepDescendants SIGKILLs and reaps children until none are left; each round reaches one level
// deeper as orphans reparent here. Returns how many are still alive when budget runs out.
func sweepDescendants(budget time.Duration) int {
	deadline := time.Now().Add(budget)
	self := os.Getpid()
	for {
		if reapExited() {
			return 0
		}
		kids := childPIDs(self)
		if time.Now().After(deadline) {
			return max(len(kids), 1) // reapExited saw at least one, even if /proc didn't
		}
		for _, k := range kids {
			_ = syscall.Kill(k, syscall.SIGKILL)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reapExited collects every already-exited child; true once there are no children at all.
func reapExited() bool {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG|syscall.WALL, nil)
		switch {
		case errors.Is(err, syscall.ECHILD):
			return true
		case errors.Is(err, syscall.EINTR):
		case err != nil || pid == 0:
			return false
		}
	}
}

// childPIDs lists processes whose parent is ppid. Unreaped children can't have their PID reused,
// so killing what this returns never hits an unrelated process.
func childPIDs(ppid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if parentPID(pid) == ppid {
			out = append(out, pid)
		}
	}
	return out
}

// parentPID reads field 4 of /proc/<pid>/stat, parsed after the last ')' since comm may hold any byte.
func parentPID(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return -1
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return -1
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 2 {
		return -1
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return -1
	}
	return ppid
}

// exitLike ends this process the way ws says the target ended, so a caller's wait status (e.g.
// SIGXFSZ from workspace.limits) reads the same as if it had waited on the target directly.
func exitLike(ws syscall.WaitStatus) {
	if !ws.Signaled() {
		exitNow(ws.ExitStatus())
	}
	sig := ws.Signal()
	// Non-dumpable: no core of this wrapper (RLIMIT_CORE doesn't stop a piped core_pattern).
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0)
	// The Go runtime catches most signals (and ignores SIGXFSZ); a zeroed kernel sigaction is SIG_DFL.
	var act [4]uint64
	_, _, _ = syscall.RawSyscall6(syscall.SYS_RT_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&act)), 0, 8, 0, 0)
	_ = syscall.Kill(os.Getpid(), sig)
	time.Sleep(time.Second)
	exitNow(128 + int(sig))
}

// exitNow skips os.Exit's hooks: nothing here needs flushing, and under -race os.Exit sleeps 1s.
func exitNow(code int) {
	_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT_GROUP, uintptr(code), 0, 0)
}
