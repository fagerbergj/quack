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
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, reapStopSignal)
	for _, sig := range forwardedSignals {
		// Notify would un-ignore an inherited SIG_IGN (nohup, a background job) before the target inherits it.
		if !signal.Ignored(sig) {
			signal.Notify(sigs, sig)
		}
	}
	defer signal.Stop(sigs)
	// os.Process signals through a pidfd, so a late SIGTERM can't hit a reused PID once reaped below.
	p, err := os.StartProcess(bin, target, &os.ProcAttr{Env: os.Environ(), Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	if err != nil {
		return 0, fmt.Errorf("reap: start %q: %w", target[0], err)
	}
	done := make(chan struct{})
	defer close(done)
	go forwardSignals(p, sigs, done)
	ws, werr := waitForPID(p.Pid)
	if left := sweepDescendants(reapSweepBudget); left > 0 {
		fmt.Fprintf(os.Stderr, "quack-reap: %d descendant(s) of %s still alive after a %s sweep\n",
			left, filepath.Base(bin), reapSweepBudget)
	}
	return ws, werr
}

// forwardedSignals reach the target unchanged; its exit (trap or not) is what triggers the sweep.
var forwardedSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT}

// forwardSignals relays sigs to the target, turning StopChild's reapStopSignal into SIGKILL.
func forwardSignals(p *os.Process, sigs <-chan os.Signal, done <-chan struct{}) {
	for {
		select {
		case sig := <-sigs:
			if sig == reapStopSignal {
				sig = syscall.SIGKILL
			}
			_ = p.Signal(sig)
		case <-done:
			return
		}
	}
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
	for {
		if reapExited() {
			return 0
		}
		kids := childPIDs()
		if time.Now().After(deadline) {
			return max(len(kids), 1) // reapExited saw at least one, even if /proc didn't
		}
		for _, k := range kids {
			_ = syscall.Kill(k, syscall.SIGKILL)
		}
		// Back off only on an empty round: a fork-and-die chain must not get a free pause per link.
		if len(kids) == 0 {
			time.Sleep(time.Millisecond)
		}
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

// childPIDs lists this process's children from each thread's children file (CONFIG_PROC_CHILDREN),
// else by scanning /proc. Unreaped children can't have their PID reused, so killing them is safe.
func childPIDs() []int {
	self := os.Getpid()
	taskDir := "/proc/" + strconv.Itoa(self) + "/task/"
	if _, err := os.Stat(taskDir + strconv.Itoa(self) + "/children"); err != nil {
		return scanChildPIDs(self)
	}
	tasks, _ := os.ReadDir(taskDir)
	var out []int
	for _, t := range tasks {
		b, _ := os.ReadFile(taskDir + t.Name() + "/children")
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				out = append(out, pid)
			}
		}
	}
	return out
}

// scanChildPIDs lists processes whose parent is ppid by reading every /proc/<pid>/stat.
func scanChildPIDs(ppid int) []int {
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
