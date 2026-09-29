//go:build !linux

package workspace

import (
	"fmt"
	"os"
)

// probeLandlock: Landlock is a Linux LSM - there is nothing to probe.
func probeLandlock() error {
	return fmt.Errorf("landlock is only supported on Linux (kernel 6.2+, Landlock ABI >= 3)")
}

// SandboxExecMain: unreachable in practice - ResolveSandbox refuses
// `workspace.sandbox: landlock` on this platform before any child ever
// carries SandboxExecArg, but a stub keeps the argv[0] dispatch (which any binary answers unconditionally - see RunSandboxExecIfInvoked) from being build-tagged itself.
func SandboxExecMain(args []string) error {
	return fmt.Errorf("sandbox-exec: landlock is only supported on Linux")
}

// ReapMain: unreachable in practice - withReaper only emits __reap argv on Linux.
func ReapMain(args []string) error {
	return fmt.Errorf("reap: subreaper mode is only supported on Linux")
}

// openNoFollow: unreachable in practice - ConfineGit's Landlock probe fails off Linux.
func openNoFollow(p string, _ bool) (*os.File, error) {
	return nil, fmt.Errorf("git: %s: no-symlink open is only supported on Linux", p)
}

// kernelLandlockABI: Landlock is Linux-only.
func kernelLandlockABI() int { return 0 }
