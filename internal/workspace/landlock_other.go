//go:build !linux

package workspace

import (
	"fmt"
	"os"
)

func probeLandlock() error {
	return fmt.Errorf("landlock is only supported on Linux (kernel 6.2+, Landlock ABI >= 3)")
}

// SandboxExecMain is unreachable (ResolveSandbox refuses landlock here); the stub keeps the dispatch untagged.
func SandboxExecMain(args []string) error {
	return fmt.Errorf("sandbox-exec: landlock is only supported on Linux")
}

// ReapMain is unreachable: withReaper only emits __reap argv on Linux.
func ReapMain(args []string) error {
	return fmt.Errorf("reap: subreaper mode is only supported on Linux")
}

// openNoFollow is unreachable: ConfineGit's Landlock probe fails off Linux.
func openNoFollow(p string, _ bool) (*os.File, error) {
	return nil, fmt.Errorf("git: %s: no-symlink open is only supported on Linux", p)
}

func kernelLandlockABI() int { return 0 }
