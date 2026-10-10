//go:build linux

package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/landlock-lsm/go-landlock/landlock"
	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// landlockABI is the minimum SandboxExecMain requires; V3 adds file truncation.
var landlockABI = landlock.V3

const landlockABIVersion = 3

// probeLandlock applies a strict ruleset in a throwaway child, never the server, which a successful
// RestrictPaths would confine for the rest of its life.
func probeLandlock() error {
	cmd := exec.Command(landlockSelfExe(), SandboxExecArg, "--probe")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("landlock probe failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SandboxExecMain applies a strict (no BestEffort) ruleset, then execs the target, so only --probe returns nil.
// Missing grant paths are skipped, like bwrap's --*-bind-try.
func SandboxExecMain(args []string) error {
	rw, ro, probe, target, err := parseSandboxExecArgs(args)
	if err != nil {
		return err
	}

	if probe {
		// Discarded with this throwaway process; proves the syscalls work without granting anything.
		return landlockABI.RestrictPaths(landlock.RODirs("/").IgnoreIfMissing())
	}
	if len(target) == 0 {
		return fmt.Errorf("sandbox-exec: no target command (missing --)")
	}
	// Resolve before restricting: LookPath reads PATH dirs the ruleset may not cover.
	bin, err := exec.LookPath(target[0])
	if err != nil {
		return fmt.Errorf("sandbox-exec: %q not found: %w", target[0], err)
	}

	if err := claimGrantFDs(append(rw, ro...)); err != nil {
		return err
	}
	rwDirs, rwFiles := splitFiles(rw)
	roDirs, roFiles := splitFiles(ro)
	var rules []landlock.Rule
	if len(rwDirs) > 0 {
		// Without REFER, cross-directory link()/rename() fails with EXDEV, breaking git's object writes.
		// Always safe: REFER needs ABI >= 2 and landlockABI is V3.
		rules = append(rules, landlock.RWDirs(rwDirs...).WithRefer().IgnoreIfMissing())
	}
	if len(roDirs) > 0 {
		rules = append(rules, landlock.RODirs(roDirs...).IgnoreIfMissing())
	}
	if len(rwFiles) > 0 {
		rules = append(rules, landlock.RWFiles(rwFiles...).IgnoreIfMissing())
	}
	if len(roFiles) > 0 {
		rules = append(rules, landlock.ROFiles(roFiles...).IgnoreIfMissing())
	}
	if err := withSignalScope(landlockABI).Restrict(rules...); err != nil {
		return fmt.Errorf("sandbox-exec: restrict: %w", err)
	}
	execArgv := append([]string{bin}, target[1:]...)
	// For operators only: kernels through 6.8 show no Landlock state in /proc, and the child can
	// overwrite this, so it is never an enforcement signal.
	env := append(os.Environ(), fmt.Sprintf("%s=landlock:abi%d:rw%d:ro%d",
		SandboxEnvMarker, landlockABIVersion, len(rw), len(ro)))
	return syscall.Exec(bin, execArgv, env)
}

// claimGrantFDs refuses a /proc/self/fd grant with no open handle behind it (IgnoreIfMissing would drop it
// silently) and keeps each handle from reaching the target.
func claimGrantFDs(paths []string) error {
	for _, p := range paths {
		n, ok := strings.CutPrefix(p, "/proc/self/fd/")
		if !ok {
			continue
		}
		fd, err := strconv.Atoi(n)
		if _, serr := os.Stat(p); err != nil || serr != nil {
			return fmt.Errorf("sandbox-exec: grant %s has no open handle: %w", p, errors.Join(err, serr))
		}
		syscall.CloseOnExec(fd)
	}
	return nil
}

// openNoFollow opens p as an O_PATH handle, refusing a symlink in any component; create makes missing dirs the
// same way, so neither the grant nor the mkdir can be steered through a planted link.
func openNoFollow(p string, create bool) (*os.File, error) {
	how := &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS}
	fd, err := unix.Openat2(unix.AT_FDCWD, p, how)
	if errors.Is(err, unix.ENOENT) && create && filepath.Dir(p) != p {
		parent, perr := openNoFollow(filepath.Dir(p), true)
		if perr != nil {
			return nil, perr
		}
		defer func() { _ = parent.Close() }()
		if err = unix.Mkdirat(int(parent.Fd()), filepath.Base(p), 0o755); err == nil || errors.Is(err, unix.EEXIST) {
			fd, err = unix.Openat2(int(parent.Fd()), filepath.Base(p), how)
		}
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), p), nil
}

// splitFiles separates non-directory grants: a directory right on a file rule is EINVAL. Missing paths stay with
// dirs, where IgnoreIfMissing skips them.
func splitFiles(paths []string) (dirs, files []string) {
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			files = append(files, p)
		} else {
			dirs = append(dirs, p)
		}
	}
	return dirs, files
}

// kernelLandlockABI is the running kernel's Landlock ABI, 0 when unknown.
func kernelLandlockABI() int {
	v, _ := ll.LandlockGetABIVersion()
	return v
}

// withSignalScope adds signal scoping where the kernel has it (ABI 6+, 6.12+): the child can't kill
// the __reap wrapper above it (orphaning past the sweep) or the server. One ruleset: a second layer denies REFER.
func withSignalScope(cfg landlock.Config) landlock.Config {
	if v, err := ll.LandlockGetABIVersion(); err == nil && v >= 6 {
		cfg.Scoped = landlock.ScopedSet(ll.ScopeSignal)
	}
	return cfg
}

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
