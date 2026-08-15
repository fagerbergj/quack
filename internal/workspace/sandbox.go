package workspace

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// SandboxMode: OS boundary for child processes (Jail constrains tools, not processes). bwrap namespace or server user's own filesystem.
type SandboxMode string

const (
	// bwrap mount/pid/ipc/user namespace: host replaced by read-only system dirs + writable cwd/HOME. No daemon, no root.
	SandboxBwrap SandboxMode = "bwrap"
	// No sandbox: server user's full filesystem authority. Warned at startup.
	SandboxNone SandboxMode = "none"
	// Self-applied Landlock ruleset (no new namespace). Chosen for container deploy where bwrap can't nest.
	SandboxLandlock SandboxMode = "landlock"
)

// SandboxExecArg: hidden argv[1] dispatch for Landlock self-exec (analogue of GIT_ASKPASS). Dispatched before cobra.
const SandboxExecArg = "__sandbox-exec"

// SandboxEnvMarker: env var the Landlock shim stamps into the process. Observability only - never read for safety decisions.
const SandboxEnvMarker = "QUACK_SANDBOX"

// bwrapBinary / prlimitBinary are looked up on the SERVER's ambient PATH (like
// every other binary RunArgv resolves - see RunArgv's LookPath rationale).
const (
	bwrapBinary   = "bwrap"
	prlimitBinary = "prlimit"
)

// Fixed path inside sandbox so `pwd` matches fs tools' cwd regardless of host/chat/node.
const SandboxWorkRoot = "/workspace"

// Per-child resource limits via prlimit(1). Zero means inherited.
type Limits struct {
	// RLIMIT_AS - generous: V8 reserves large virtual regions at startup.
	AddressSpaceMB int
	// RLIMIT_NPROC - SandboxBwrap only (per-UID system-wide outside user NS).
	Procs int
	// RLIMIT_FSIZE - child killed (SIGXFSZ) on write past this.
	FileSizeMB int
}

// ResolveSandbox validates the mode and proves it works before serving. Fails closed.
func ResolveSandbox(mode SandboxMode) (SandboxMode, error) {
	switch mode {
	case SandboxNone:
		slog.Warn("workspace sandbox is OFF (workspace.sandbox: none): every child process a worker or the gate spawns "+
			"(a coding agent's own shell commands, ledgered as run_command, and the gate's own check commands) "+
			"runs as the server's OS user with that user's FULL filesystem authority - it can read ~/.ssh, ~/.aws, "+
			"~/.config/gh, .env and anything else that account can read, whatever the path jail says. The jail confines "+
			"the TOOLS' paths, not a child process. Only run agents you would trust with that account.",
			"component", "workspace")
		return SandboxNone, nil
	case SandboxBwrap:
		if err := probeBwrap(); err != nil {
			return "", fmt.Errorf("workspace.sandbox: %q: %w\n"+
				"Install bubblewrap (Debian/Ubuntu: apt-get install bubblewrap; Fedora: dnf install bubblewrap; "+
				"Alpine: apk add bubblewrap), or set `workspace.sandbox: none` to accept that child processes run with "+
				"the server user's full filesystem authority", SandboxBwrap, err)
		}
		return SandboxBwrap, nil
	case SandboxLandlock:
		if err := probeLandlockHook(); err != nil {
			return "", fmt.Errorf("workspace.sandbox: %q: %w\n"+
				"landlock requires a Linux kernel with Landlock ABI >= 3 (kernel 6.2+); "+
				"set `workspace.sandbox: none` to accept unconfined children, or `bwrap` on a host where bubblewrap works",
				SandboxLandlock, err)
		}
		slog.Info("workspace sandbox resolved", "component", "workspace", "mode", SandboxLandlock, "landlock_abi", ">=3 (probed)")
		return SandboxLandlock, nil
	default:
		return "", fmt.Errorf("workspace.sandbox: unknown mode %q (want %q, %q, or %q)", mode, SandboxBwrap, SandboxLandlock, SandboxNone)
	}
}

// probeBwrap verifies bwrap is installed AND can create a namespace here (presence != proof).
func probeBwrap() error {
	bin, err := exec.LookPath(bwrapBinary)
	if err != nil {
		return fmt.Errorf("the bwrap binary is not installed or not on PATH: %w", err)
	}
	args := append(bwrapSystemArgs(), "--tmpfs", "/tmp", "--", bin, "--version")
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("bwrap is installed but cannot create a sandbox on this host (%v): %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Host-independent sandbox: NS isolation + read-only system dirs. Network NOT unshared (agents need npm/go mod).
func bwrapSystemArgs() []string {
	return []string{
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
		"--die-with-parent", "--new-session",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/sbin", "/sbin",
		"--ro-bind-try", "/etc/ssl", "/etc/ssl",
		"--ro-bind-try", "/etc/ca-certificates", "/etc/ca-certificates",
		"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
		"--ro-bind-try", "/etc/hosts", "/etc/hosts",
		"--ro-bind-try", "/etc/nsswitch.conf", "/etc/nsswitch.conf",
		"--ro-bind-try", "/etc/passwd", "/etc/passwd",
		"--ro-bind-try", "/etc/group", "/etc/group",
		"--ro-bind-try", "/etc/alternatives", "/etc/alternatives",
		"--ro-bind-try", "/etc/localtime", "/etc/localtime",
		"--proc", "/proc",
		"--dev", "/dev",
	}
}

// Assembles child argv: rlimits wrap the program, sandbox wraps that. prlimit inside bwrap (RLIMIT_NPROC per-UID).
func childArgv(dir, bin string, argv []string, caps Caps) []string {
	inner := append([]string{bin}, argv[1:]...)
	switch caps.Sandbox {
	case SandboxLandlock:
		return landlockArgv(dir, inner, caps)
	case SandboxBwrap:
	default:
		return withLimits(inner, caps.Limits, false)
	}
	args := bwrapSystemArgs()
	args = append(args, tmpArgs(caps)...)
	args = append(args, toolchainArgs(caps)...)
	args = append(args, extraROArgs(caps)...)
	work := caps.WorkRoot
	if work == "" || !isDir(work) {
		work = dir
	}
	bindFlag := "--bind"
	if caps.ReadOnly {
		bindFlag = "--ro-bind"
	}
	args = append(args, bindFlag, work, SandboxWorkRoot)
	chdir := SandboxWorkRoot
	if rel, ok := relUnder(work, dir); ok {
		chdir = filepath.Join(SandboxWorkRoot, rel)
	} else {
		args = append(args, bindFlag, dir, dir)
		chdir = dir
	}
	if caps.HomeDir != "" && caps.HomeDir != dir {
		args = append(args, "--bind", caps.HomeDir, caps.HomeDir)
	}
	// Linked worktree: parent .git RO, own gitdir RW (later binds overlay earlier).
	wtRW, wtRO := worktreeGrants(work, dir)
	for _, common := range wtRO {
		args = append(args, "--ro-bind", common, common)
	}
	for _, gitdir := range wtRW {
		args = append(args, "--bind", gitdir, gitdir)
	}
	args = append(args, rootAliasArgs(work, dir, caps)...)
	// Sandbox root RO so a stray `cd /repo && cd ..` fails instead of writing to tmpfs.
	args = append(args, "--remount-ro", "/")
	args = append(args, "--chdir", chdir, "--")
	args = append(args, withLimits(inner, caps.Limits, true)...)
	return append([]string{bwrapPath()}, args...)
}

// Symlinks workspace entries at sandbox root so fs tool paths work in the shell.
func rootAliasArgs(work, dir string, caps Caps) []string {
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil // unreadable node dir: the fixed mount alone is still correct
	}
	taken := reservedRoots(dir, caps, worktreeCommonGitDirs(work, dir)...)
	var args []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || taken[name] || len(args) >= 2*maxRootAliases {
			continue
		}
		args = append(args, "--symlink", SandboxWorkRoot+"/"+name, "/"+name)
	}
	return args
}

// Bounds the symlink farm: prevents a pathological root dir from building absurd argv.
const maxRootAliases = 100

// Top-level names inside the sandbox a workspace entry may NOT shadow (mountpoints, binds).
func reservedRoots(dir string, caps Caps, extra ...string) map[string]bool {
	m := map[string]bool{
		"usr": true, "bin": true, "lib": true, "lib64": true, "sbin": true,
		"etc": true, "proc": true, "dev": true, "tmp": true,
		strings.TrimPrefix(SandboxWorkRoot, "/"): true,
	}
	add := func(p string) {
		if c := firstComponent(p); c != "" {
			m[c] = true
		}
	}
	add(caps.HomeDir)
	add(dir)
	for _, p := range caps.ExtraPath {
		add(p)
	}
	for _, p := range caps.ExtraRO {
		add(p)
	}
	for _, p := range extra {
		add(p)
	}
	return m
}

// firstComponent returns the first path element of an absolute path, "" otherwise.
func firstComponent(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(p), "/"), "/")
	return parts[0]
}

// relUnder reports dir relative to base (for SandboxWorkRoot mapping). Both from Jail.Resolve so lexical is correct.
func relUnder(base, dir string) (string, bool) {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return rel, true
}

// isDir guards the WorkRoot bind: bwrap fails on a missing bind source.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Private /tmp backed by $HOME/tmp when available (tmpfs = RAM pressure for builds).
func tmpArgs(caps Caps) []string {
	if tmp := homeTmpDir(caps); tmp != "" {
		return []string{"--bind", tmp, "/tmp"}
	}
	return []string{"--tmpfs", "/tmp"}
}

// homeTmpDir returns caps.ScratchDir (created on demand) when the caller has
// scoped one - a sandboxed worker's own per-node tmp, see Jail.ScratchDir -
// else falls back to the shared caps.HomeDir/tmp (created on demand), ""
// when neither is set.
func homeTmpDir(caps Caps) string {
	if caps.ScratchDir != "" {
		if err := os.MkdirAll(caps.ScratchDir, 0o700); err != nil {
			slog.Warn("could not create the sandbox scratch dir; falling back to shared HOME/tmp",
				"component", "workspace", "dir", caps.ScratchDir, "err", err)
		} else {
			return caps.ScratchDir
		}
	}
	if caps.HomeDir == "" {
		return ""
	}
	tmp := filepath.Join(caps.HomeDir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		slog.Warn("could not create the sandbox tmp dir; falling back to a shared /tmp",
			"component", "workspace", "dir", tmp, "err", err)
		return ""
	}
	return tmp
}

// RO-binds operator exec_path entries + a bin/'s FHS siblings (lib, libexec, share).
func toolchainArgs(caps Caps) []string {
	var args []string
	for _, p := range caps.ExtraPath {
		if strings.TrimSpace(p) == "" {
			continue
		}
		args = append(args, "--ro-bind-try", p, p)
		if filepath.Base(p) != "bin" {
			continue
		}
		prefix := filepath.Dir(p)
		for _, sib := range []string{"lib", "libexec", "share"} {
			sibling := filepath.Join(prefix, sib)
			args = append(args, "--ro-bind-try", sibling, sibling)
		}
	}
	return args
}

// RO-binds Caps.ExtraRO entries (--ro-bind-try: skips missing paths).
func extraROArgs(caps Caps) []string {
	var args []string
	for _, p := range caps.ExtraRO {
		if strings.TrimSpace(p) == "" {
			continue
		}
		args = append(args, "--ro-bind-try", p, p)
	}
	return args
}

// Prefixes argv with prlimit(1) when limits are set. Missing prlimit = WARN + unlimited (not a startup error).
func withLimits(argv []string, lim Limits, inUserNS bool) []string {
	var flags []string
	if lim.AddressSpaceMB > 0 {
		flags = append(flags, "--as="+strconv.Itoa(lim.AddressSpaceMB*1024*1024))
	}
	if lim.FileSizeMB > 0 {
		flags = append(flags, "--fsize="+strconv.Itoa(lim.FileSizeMB*1024*1024))
	}
	if lim.Procs > 0 && inUserNS {
		flags = append(flags, "--nproc="+strconv.Itoa(lim.Procs))
	}
	if len(flags) == 0 {
		return argv
	}
	bin, err := exec.LookPath(prlimitBinary)
	if err != nil {
		warnNoPrlimit.Do(func() {
			slog.Warn("prlimit(1) is not installed; child processes run with NO resource limits "+
				"(a runaway build can exhaust the host's memory or disk). Install util-linux, or ignore this if the "+
				"deployment limits the whole container instead.", "component", "workspace", "err", err)
		})
		return argv
	}
	out := append([]string{bin}, flags...)
	out = append(out, "--")
	return append(out, argv...)
}

var warnNoPrlimit sync.Once

// bwrapPath: ResolveSandbox already proved it exists. Bare name fallback keeps the exec error clear.
func bwrapPath() string {
	if p, err := exec.LookPath(bwrapBinary); err == nil {
		return p
	}
	return bwrapBinary
}

// Landlock mode: self-restricts via __sandbox-exec re-exec (Linux-only, see landlock_linux.go).

// probeLandlockHook: test seam for simulating unsupported kernel.
var probeLandlockHook = probeLandlock

// landlockArgv builds [self, __sandbox-exec, …] argv for childArgv's landlock branch.
func landlockArgv(dir string, inner []string, caps Caps) []string {
	rw, ro := landlockGrants(dir, caps)
	return assembleSandboxExec(rw, ro, withLimits(inner, caps.Limits, false))
}

// assembleSandboxExec: shared by landlockArgv and WrapArgv to keep flag order consistent.
func assembleSandboxExec(rw, ro, inner []string) []string {
	args := []string{landlockSelfExe(), SandboxExecArg}
	for _, p := range rw {
		args = append(args, "--rw", p)
	}
	for _, p := range ro {
		args = append(args, "--ro", p)
	}
	args = append(args, "--")
	return append(args, inner...)
}

// RW/RO path grants for a child. Unlike bwrap, paths are real host paths (no mount namespace).
func landlockGrants(dir string, caps Caps) (rw, ro []string) {
	work := caps.WorkRoot
	if work == "" || !isDir(work) {
		work = dir
	}
	// #754: a read-only agent's own working tree is granted RO, not RW - the
	// enforcement a prompt claim alone can't give.
	ownPaths := []string{work}
	if _, ok := relUnder(work, dir); !ok {
		// dir lies outside the node's own workspace (the gate's baseline
		// worktree is the only caller this happens for) - grant it directly,
		// mirroring bwrap's childArgv "outside cwd" branch.
		ownPaths = append(ownPaths, dir)
	}
	if caps.ReadOnly {
		ro = append(ro, ownPaths...)
	} else {
		rw = append(rw, ownPaths...)
	}
	if caps.HomeDir != "" && caps.HomeDir != work {
		rw = append(rw, caps.HomeDir)
	}
	// A linked git worktree (dag's read-only-node isolation) also needs its
	// parent clone's .git - writable only for its OWN gitdir, read-only for the
	// shared store (see worktreeGrants). Checked on both work and dir since a
	// check's workdir can be a subdirectory of the node's own root.
	wtRW, wtRO := worktreeGrants(work, dir)
	rw = append(rw, wtRW...)
	rw = append(rw, landlockTmpDir(caps))
	// /dev RW (not RO): DAC still governs which device nodes actually do
	// anything (an agent gains no reach a world-writable /dev/null didn't
	// already offer), and `go vet` empirically OPENS /dev/null for WRITE
	// (observed: "go: ... open /dev/null: permission denied" under an RO
	// grant) - see the shim confinement test.
	rw = append(rw, "/dev")

	ro = append(ro, wtRO...)
	ro = append(ro, landlockSystemDirs()...)
	ro = append(ro, toolchainROPaths(caps)...)
	for _, p := range caps.ExtraRO {
		if strings.TrimSpace(p) != "" {
			ro = append(ro, p)
		}
	}
	return rw, ro
}

// landlockSystemDirs mirrors bwrapSystemArgs' read-only system view. Unlike
// bwrap's per-file /etc allowlist, the whole of /etc is granted: Landlock adds
// restrictions on TOP of ordinary DAC permissions, never loosens them, so
// /etc/shadow stays unreadable by UID regardless. /proc is granted RO too -
// empirically, `go build`/`git` don't need it, but Node does: without it
// `os.cpus()` silently returns an empty array (libuv reads /proc/cpuinfo),
// which would size npm/webpack/jest's worker pools at zero.
func landlockSystemDirs() []string {
	return []string{"/usr", "/bin", "/lib", "/lib64", "/sbin", "/etc", "/proc"}
}

// toolchainROPaths mirrors toolchainArgs (the bwrap equivalent): the
// operator's workspace.exec_path entries, plus a bin/ entry's FHS siblings
// (lib, libexec, share) so a prefix toolchain's binaries and their linked
// libraries both resolve.
func toolchainROPaths(caps Caps) []string {
	var out []string
	for _, p := range caps.ExtraPath {
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, p)
		if filepath.Base(p) != "bin" {
			continue
		}
		prefix := filepath.Dir(p)
		for _, sib := range []string{"lib", "libexec", "share"} {
			out = append(out, filepath.Join(prefix, sib))
		}
	}
	return out
}

// landlockTmpDir is the RW tmp grant: caps.HomeDir/tmp when available
// (homeTmpDir - shared with bwrap's tmpArgs), else the real /tmp. Landlock has
// no mount namespace, so unlike bwrap's private tmpfs fallback this is the
// SAME /tmp every other process on the host sees - no worse than SandboxNone
// without an isolated HOME configured, just not private.
func landlockTmpDir(caps Caps) string {
	if tmp := homeTmpDir(caps); tmp != "" {
		return tmp
	}
	return os.TempDir()
}

// landlockSelfExe: ResolveSandbox already proved this succeeds. Bare-name fallback keeps the error clear.
func landlockSelfExe() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return os.Args[0]
}

// RunSandboxExecIfInvoked: argv[0] dispatch for Landlock self-exec. Call at top of main() before cobra.
func RunSandboxExecIfInvoked() {
	if len(os.Args) < 2 || os.Args[1] != SandboxExecArg {
		return
	}
	// os.Exit always so main/test can't fall through into its own execution.
	if err := SandboxExecMain(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TMPDIR for subprocesses outside RunArgv/RunPipeline (ACP). Landlock can't
// remap /tmp so tools must be told; under bwrap the same identity-bound
// scratch dir is what WrapArgv grants RW, and the server's own ambient TMPDIR
// would name a path that doesn't exist inside the namespace.
func SandboxTmpDir(caps Caps) string {
	if EnforcesBoundary(caps.Sandbox) {
		return landlockTmpDir(caps)
	}
	return os.TempDir()
}

// JAVA_TOOL_OPTIONS a sandboxed child needs. JAVA_TOOL_OPTIONS replaces not merges; java.io.tmpdir is hardcoded to /tmp.
func SandboxJavaToolOptions(caps Caps) string {
	var parts []string
	if caps.Sandbox == SandboxLandlock {
		parts = append(parts, "-Djava.io.tmpdir="+landlockTmpDir(caps))
	}
	if opts := javaAddressSpaceOptions(caps.Limits.AddressSpaceMB); opts != "" {
		parts = append(parts, opts)
	}
	return strings.Join(parts, " ")
}

// JVM heap/metaspace/class-space/code-cache to fit inside asMB RLIMIT_AS. ~39% margin.
func javaAddressSpaceOptions(asMB int) string {
	if asMB <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"-Xmx%dm -XX:MaxMetaspaceSize=%dm -XX:CompressedClassSpaceSize=%dm -XX:ReservedCodeCacheSize=%dm -XX:ActiveProcessorCount=%d",
		asMB*35/100, asMB*10/100, asMB*8/100, asMB*8/100, javaBuildProcessors,
	)
}

// Core count cap for sandboxed JVM (one gate-check build).
const javaBuildProcessors = 4

// Hermetic PATH for subprocesses outside RunArgv/RunPipeline (ACP agent).
func ChildPath(caps Caps) string {
	return childPath(caps)
}

// WrapArgv: the ONE seam for callers outside RunArgv/newChildCmd (e.g. ACP).
// Deliberately does NOT apply caps.Limits (#798, reverting #646): a one-shot
// check's ceilings do not transfer to a long-lived agent process. Measured on
// the live deployment, EACH limit alone stopped opencode reaching its first
// ACP message - RLIMIT_FSIZE 1024MB against an opencode.db already at 1.27GB
// (a WAL checkpoint extends the file, so EFBIG), and RLIMIT_AS 8192MB against
// a V8 process that reserves a huge virtual region before it runs anything.
// Both surfaced as the same opaque SQLite error. Any fixed FSIZE is a date
// rather than a bound while the agent's DB grows, so this seam grants no
// ceiling at all and the container's own quota is the boundary here.
func WrapArgv(dir string, argv []string, caps Caps, extraRO, extraRW []string) []string {
	if len(argv) == 0 || !EnforcesBoundary(caps.Sandbox) {
		if caps.ReadOnly {
			warnReadOnlyUnenforced(caps.Sandbox)
		}
		return argv
	}
	rw, ro := landlockGrants(dir, caps)
	rw = append(rw, extraRW...)
	ro = append(ro, extraRO...)
	if caps.Sandbox == SandboxBwrap {
		return bwrapWrapArgv(dir, argv, caps, rw, ro)
	}
	return assembleSandboxExec(rw, ro, argv)
}

// EnforcesBoundary reports whether mode gives a WrapArgv'd child an
// OS-enforced path boundary (work tree per caps.ReadOnly, $HOME/$TMPDIR
// writable, nothing else reachable). The gate on capabilities that REST on
// that boundary - acp.allow_clone and the wide external_directory it needs,
// see serve.opencodeEnv. SandboxNone never qualifies.
func EnforcesBoundary(mode SandboxMode) bool {
	return mode == SandboxLandlock || mode == SandboxBwrap
}

// bwrapWrapArgv gives the ACP child the SAME grants landlockGrants computes,
// as bwrap mounts: rw as --bind-try, ro as --ro-bind-try, both at IDENTITY
// paths. Not childArgv's SandboxWorkRoot remap - the ACP child exchanges
// absolute paths with quack over JSON-RPC (session cwd out, tool-call paths
// back), so a remapped work tree would make every path either side names
// meaningless to the other.
func bwrapWrapArgv(dir string, argv []string, caps Caps, rw, ro []string) []string {
	args := bwrapSystemArgs()
	args = append(args, tmpArgs(caps)...)
	args = append(args, identityBinds(rw, ro)...)
	// Sandbox root RO so the empty parent dirs bwrap creates for the binds
	// below aren't a writable scratch space (mirrors childArgv).
	args = append(args, "--remount-ro", "/", "--chdir", dir, "--")
	return append([]string{bwrapPath()}, append(args, argv...)...)
}

// identityBinds renders grants as bwrap binds onto their own host paths,
// SHALLOWEST FIRST: bwrap applies binds in argv order and a later mount on a
// subpath overlays the earlier one, so ordering by depth is what makes the
// most specific grant win (a read-only work tree nested inside a writable
// HOME must stay read-only). -try mirrors landlock's IgnoreIfMissing.
func identityBinds(rw, ro []string) []string {
	type bind struct{ flag, path string }
	var binds []bind
	seen := map[string]bool{}
	add := func(flag string, paths []string) {
		for _, p := range paths {
			p = strings.TrimSpace(p)
			if p == "" || !filepath.IsAbs(p) {
				continue
			}
			p = filepath.Clean(p)
			if bwrapOwnedMount(p) || seen[p] {
				continue
			}
			seen[p] = true
			binds = append(binds, bind{flag, p})
		}
	}
	// RO first: a path a caller granted both ways keeps the read-only bind,
	// matching landlockGrants' own RO-wins intent for a read_only node.
	add("--ro-bind-try", ro)
	add("--bind-try", rw)
	slices.SortStableFunc(binds, func(a, b bind) int {
		return strings.Count(a.path, "/") - strings.Count(b.path, "/")
	})
	var args []string
	for _, b := range binds {
		args = append(args, b.flag, b.path, b.path)
	}
	return args
}

// bwrapOwnedMount reports paths bwrapSystemArgs/tmpArgs already mount, which a
// grant must NOT bind over: /proc and /dev are their own filesystem types
// there (a host bind would leak the real PID table and device nodes back in),
// /tmp is the private tmpfs or scratch bind, and the rest are the same
// read-only system view landlockSystemDirs grants - bwrap's is narrower for
// /etc (a per-file allowlist), which is stricter, not weaker.
func bwrapOwnedMount(p string) bool {
	switch p {
	case "/proc", "/dev", "/tmp":
		return true
	}
	return slices.Contains(landlockSystemDirs(), p)
}

var warnReadOnlyUnenforcedOnce sync.Once

// warnReadOnlyUnenforced (#754): a read_only agent's own subprocess (the ACP
// path WrapArgv wraps) gets a real RO mount under landlock and bwrap (#921) -
// sandbox: none has no boundary at all. Degrade and say so once, rather than
// silently leaving the flag as a prompt-only claim.
func warnReadOnlyUnenforced(mode SandboxMode) {
	warnReadOnlyUnenforcedOnce.Do(func() {
		slog.Warn("read_only agent's own working directory is NOT read-only enforced at the OS level in this sandbox mode "+
			"(WrapArgv only mounts it RO under landlock or bwrap); the agent could write there - only its prompt says not to. "+
			"Set workspace.sandbox: landlock or bwrap to enforce it.", "component", "workspace", "sandbox", mode)
	})
}

// Linked-worktree grants: a linked worktree's .git is a pointer file; objects/refs live under the parent clone.

// WorktreeCommonGitDir: shared .git dir a linked worktree points at. "" for a plain clone (no extra grant needed).
func WorktreeCommonGitDir(dir string) string {
	_, common := worktreeGitDirs(dir)
	return common
}

// Own gitdir + shared common dir for a linked worktree. Both "" when not a linked worktree.
func worktreeGitDirs(dir string) (gitdir, common string) {
	data, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", ""
	}
	const prefix = "gitdir: "
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, prefix) {
		return "", ""
	}
	gitdir = strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	commonBytes, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return "", ""
	}
	common = strings.TrimSpace(string(commonBytes))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	return filepath.Clean(gitdir), filepath.Clean(common)
}

// RW per-worktree gitdirs, RO shared common dirs. Without the split a read-only node could rewrite the implementer's refs.
func worktreeGrants(paths ...string) (rw, ro []string) {
	seenRW, seenRO := map[string]bool{}, map[string]bool{}
	for _, p := range paths {
		gitdir, common := worktreeGitDirs(p)
		if common == "" {
			continue
		}
		if !seenRO[common] {
			seenRO[common] = true
			ro = append(ro, common)
		}
		if gitdir != "" && !seenRW[gitdir] {
			seenRW[gitdir] = true
			rw = append(rw, gitdir)
		}
	}
	return rw, ro
}

// worktreeCommonGitDirs: worktreeGrants' RO half for callers that only need the paths.
func worktreeCommonGitDirs(paths ...string) []string {
	_, ro := worktreeGrants(paths...)
	return ro
}
