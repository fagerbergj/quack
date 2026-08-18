//go:build linux

package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// landlockABI is the minimum ABI SandboxExecMain requires. V3 adds file truncation.
var landlockABI = landlock.V3

// landlockABIVersion is landlockABI's number for the env marker.
const landlockABIVersion = 3

// probeLandlock proves Landlock ABI >= V3 actually works HERE by applying a
// trivial strict ruleset in a THROWAWAY child process (self-spawned in
// --probe mode) - never in the server process, which a successful
// RestrictPaths call would otherwise confine for the rest of its life.
func probeLandlock() error {
	cmd := exec.Command(landlockSelfExe(), SandboxExecArg, "--probe")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("landlock probe failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SandboxExecMain implements the __sandbox-exec argv mode: parse repeatable
// --rw/--ro grants and an optional --probe up to "--", apply a STRICT (no
// BestEffort) Landlock ruleset at landlockABI, then syscall.Exec the target -
// which REPLACES this process, so a nil return only ever happens for --probe.
// A path in --rw/--ro that doesn't exist is skipped (IgnoreIfMissing),
// mirroring bwrap's --*-bind-try tolerance for an optional grant.
func SandboxExecMain(args []string) error {
	rw, ro, probe, target, err := parseSandboxExecArgs(args)
	if err != nil {
		return err
	}

	if probe {
		// A trivial strict ruleset, applied and immediately discarded with
		// this throwaway process - proves the syscalls work at this ABI
		// without granting anything a real child would use.
		return landlockABI.RestrictPaths(landlock.RODirs("/").IgnoreIfMissing())
	}
	if len(target) == 0 {
		return fmt.Errorf("sandbox-exec: no target command (missing --)")
	}
	// Resolve the target BEFORE restricting: LookPath needs to read PATH's
	// directories, which the ruleset below may or may not cover, and this
	// mirrors newChildCmd's own "resolve first, restrict after" order.
	bin, err := exec.LookPath(target[0])
	if err != nil {
		return fmt.Errorf("sandbox-exec: %q not found: %w", target[0], err)
	}

	var rules []landlock.Rule
	if len(rw) > 0 {
		// WithRefer grants LANDLOCK_ACCESS_FS_REFER: without it, cross-directory
		// link()/rename() within these RW dirs is denied and reported as EXDEV even
		// on one filesystem - breaking git's object writes and `git clone --local`.
		// Safe to request unconditionally: landlockABI is fixed at V3 (REFER needs
		// only ABI>=2), so any kernel this ruleset applies on already supports it.
		rules = append(rules, landlock.RWDirs(rw...).WithRefer().IgnoreIfMissing())
	}
	if len(ro) > 0 {
		rules = append(rules, landlock.RODirs(ro...).IgnoreIfMissing())
	}
	if err := landlockABI.RestrictPaths(rules...); err != nil {
		return fmt.Errorf("sandbox-exec: restrict: %w", err)
	}
	execArgv := append([]string{bin}, target[1:]...)
	// Stamp the ruleset into the environment we exec into. syscall.Exec replaces
	// this process image, so a confined child is indistinguishable from a bare one
	// in `ps`, and kernels through 6.8 expose no Landlock field in
	// /proc/<pid>/status - without this there is no way to answer "is this
	// confined?" from outside. NOT a security control: the child can overwrite it
	// (same caveat Codex documents for CODEX_PERMISSION_PROFILE), so it is for
	// operators, not for enforcement decisions.
	env := append(os.Environ(), fmt.Sprintf("%s=landlock:abi%d:rw%d:ro%d",
		SandboxEnvMarker, landlockABIVersion, len(rw), len(ro)))
	return syscall.Exec(bin, execArgv, env)
}

// parseSandboxExecArgs splits the __sandbox-exec argv into its repeatable
// grants, the --probe flag, and the target command past "--".
func parseSandboxExecArgs(args []string) (rw, ro []string, probe bool, target []string, err error) {
	i := 0
	for ; i < len(args); i++ {
		switch args[i] {
		case "--probe":
			probe = true
		case "--rw", "--ro":
			flag := args[i]
			i++
			if i >= len(args) {
				return nil, nil, false, nil, fmt.Errorf("sandbox-exec: %s needs a path", flag)
			}
			if flag == "--rw" {
				rw = append(rw, args[i])
			} else {
				ro = append(ro, args[i])
			}
		case "--":
			i++
			return rw, ro, probe, args[i:], nil
		default:
			return nil, nil, false, nil, fmt.Errorf("sandbox-exec: unknown flag %q", args[i])
		}
	}
	return rw, ro, probe, nil, nil
}
