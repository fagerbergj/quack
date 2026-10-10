package workspace

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// SandboxMode is the OS boundary for child processes; the Jail constrains tool paths, not processes.
type SandboxMode string

const (
	// bwrap namespaces: read-only system dirs plus writable cwd/HOME. No daemon, no root.
	SandboxBwrap SandboxMode = "bwrap"
	// No sandbox: server user's full filesystem authority. Warned at startup.
	SandboxNone SandboxMode = "none"
	// Self-applied Landlock ruleset (no new namespace), for containers where bwrap can't nest.
	SandboxLandlock SandboxMode = "landlock"
)

// SandboxExecArg is the hidden argv[1] for the Landlock self-exec, dispatched before cobra.
const SandboxExecArg = "__sandbox-exec"

// ReapArg is the hidden argv[1] for the descendant reaper (ReapMain) wrapping every non-bwrap child.
const ReapArg = "__reap"

// SandboxEnvMarker is stamped by the Landlock shim for observability; never read for safety decisions.
const SandboxEnvMarker = "QUACK_SANDBOX"

// Looked up on the server's ambient PATH, like every binary RunArgv resolves.
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
		return fmt.Errorf("bwrap is installed but cannot create a sandbox on this host (%w): %s",
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

// childArgv: rlimits wrap the program, the sandbox wraps that; prlimit runs inside bwrap since RLIMIT_NPROC is per-UID.
func childArgv(dir, bin string, argv []string, caps Caps) []string {
	inner := append([]string{bin}, argv[1:]...)
	switch caps.Sandbox {
	case SandboxLandlock:
		return withReaper(landlockArgv(dir, inner, caps))
	case SandboxBwrap:
	default:
		return withReaper(withLimits(inner, caps.Limits, false))
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
	if caps.ReadOnly {
		// A later bind on a subpath wins, so each gitignored build dir is writable and the rest stays RO.
		for _, rel := range buildDirGrants(work, caps.BuildDirs) {
			args = append(args, "--bind-try", filepath.Join(work, rel), filepath.Join(SandboxWorkRoot, rel))
		}
	}
	var chdir string
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

// relUnder reports dir relative to base; lexical is correct since both come from Jail.Resolve.
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

// sameDevice: a TMPDIR fallback must share the workspace's filesystem, since git's hardlinking
// operations (clone --local, worktree add) fail with EXDEV across devices.
func sameDevice(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	da, ok := sa.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("sameDevice: no Stat_t for %s", a)
	}
	db, ok := sb.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("sameDevice: no Stat_t for %s", b)
	}
	return da.Dev == db.Dev, nil
}

// sameDeviceHook lets tests simulate a cross-device fallback.
var sameDeviceHook = sameDevice

// Private /tmp backed by $HOME/tmp when available (tmpfs = RAM pressure for builds).
func tmpArgs(caps Caps) []string {
	if tmp := homeTmpDir(caps); tmp != "" {
		return []string{"--bind", tmp, "/tmp"}
	}
	return []string{"--tmpfs", "/tmp"}
}

// homeTmpDir: caps.ScratchDir, else HOME/tmp if on the WorkRoot's device, else workRootTmpDir; "" when
// nothing is usable. ScratchDir is not device-checked: it already lives under the workspace.
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
		return workRootTmpDir(caps)
	}
	tmp := filepath.Join(caps.HomeDir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		slog.Warn("could not create the sandbox tmp dir; falling back to a workspace-derived scratch dir",
			"component", "workspace", "dir", tmp, "err", err)
		return workRootTmpDir(caps)
	}
	// An unverifiable dir (no WorkRoot to compare) is trusted rather than rejected on no evidence.
	if same, err := sameDeviceHook(tmp, caps.WorkRoot); err == nil && !same {
		slog.Warn("HOME/tmp is on a different filesystem than the workspace; using it as TMPDIR would break "+
			"hardlinking git operations (clone --local, worktree add) with a confusing EXDEV error - deriving "+
			"a scratch dir from the workspace instead", "component", "workspace", "dir", tmp, "workroot", caps.WorkRoot)
		return workRootTmpDir(caps)
	}
	return tmp
}

// Dot-prefixed so it reads as infrastructure; constant so repeated spawns reuse one dir.
const workRootTmpDirName = ".quack-tmp"

// workRootTmpDir: a scratch dir inside the work root, on the workspace filesystem by construction.
// Skipped for ReadOnly trees; untracked files are fine since git push moves committed refs only.
func workRootTmpDir(caps Caps) string {
	if caps.WorkRoot == "" || caps.ReadOnly {
		return ""
	}
	dir := filepath.Join(caps.WorkRoot, workRootTmpDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("could not create the workspace-derived scratch dir; falling back to a shared /tmp",
			"component", "workspace", "dir", dir, "err", err)
		return ""
	}
	return dir
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

// withLimits prefixes argv with prlimit(1); a missing prlimit warns and runs unlimited.
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

// bwrapPath falls back to the bare name so the exec error stays clear.
func bwrapPath() string {
	if p, err := exec.LookPath(bwrapBinary); err == nil {
		return p
	}
	return bwrapBinary
}

// probeLandlockHook lets tests simulate an unsupported kernel.
var probeLandlockHook = probeLandlock

func landlockArgv(dir string, inner []string, caps Caps) []string {
	rw, ro := landlockGrants(dir, caps)
	return assembleSandboxExec(rw, ro, withLimits(inner, caps.Limits, false))
}

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

// landlockGrants: unlike bwrap, paths are real host paths (no mount namespace).
func landlockGrants(dir string, caps Caps) (rw, ro []string) {
	work := caps.WorkRoot
	if work == "" || !isDir(work) {
		work = dir
	}
	ownPaths := []string{work}
	if _, ok := relUnder(work, dir); !ok {
		// Only the gate's baseline worktree lies outside the node's workspace.
		ownPaths = append(ownPaths, dir)
	}
	if caps.ReadOnly {
		ro = append(ro, ownPaths...)
		// Landlock is additive: a RW rule on a subpath grants write there despite the RO parent.
		for _, rel := range buildDirGrants(work, caps.BuildDirs) {
			rw = append(rw, filepath.Join(work, rel))
		}
	} else {
		rw = append(rw, ownPaths...)
	}
	if caps.HomeDir != "" && caps.HomeDir != work {
		rw = append(rw, caps.HomeDir)
	}
	// Both work and dir: a check's workdir can be a subdirectory of the node's own root.
	wtRW, wtRO := worktreeGrants(work, dir)
	rw = append(rw, wtRW...)
	rw = append(rw, landlockTmpDir(caps))
	// /dev RW: `go vet` opens /dev/null for write, and DAC still governs what each device node allows.
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

// gitignoreDirPatterns: bare names (no inner "/") match at any depth; anchored paths match only exactly.
// Not a full matcher: globs are skipped, and "!name" only undoes an earlier exact ignore.
func gitignoreDirPatterns(dir string) (bare, anchored map[string]bool) {
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil, nil
	}
	bare, anchored = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		p := strings.TrimSuffix(strings.TrimSpace(line), "\r")
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		negate := strings.HasPrefix(p, "!")
		if negate {
			p = strings.TrimSpace(p[1:])
		}
		p = strings.Trim(p, "/")
		if p == "" || strings.ContainsAny(p, "*?[]") {
			continue
		}
		target := bare
		if strings.Contains(p, "/") {
			target = anchored
		}
		if negate {
			// An un-ignored name is tracked; granting it RW would let a malicious .gitignore overwrite tracked content.
			delete(target, p)
			continue
		}
		target[p] = true
	}
	return bare, anchored
}

// buildDirGrants: configured build dirs the repo already gitignores (others would dirty a read-only tree),
// plus every top-level dir a bare .gitignore pattern names, so repos work without build_dirs config.
func buildDirGrants(work string, configured []string) []string {
	bare, anchored := gitignoreDirPatterns(work)
	if len(bare) == 0 && len(anchored) == 0 {
		return nil
	}
	book := &dirGrantBook{work: work, seen: map[string]bool{}}
	addConfiguredDirs(book, configured, bare, anchored)
	addBareNames(book, bare)
	return book.out
}

type dirGrantBook struct {
	work string
	seen map[string]bool
	out  []string
}

func addConfiguredDirs(b *dirGrantBook, configured []string, bare, anchored map[string]bool) {
	for _, d := range configured {
		rel := filepath.Clean(d)
		// Granting ".git" would expose the gitdir pointer or the shared clone's metadata.
		if filepath.Base(rel) == ".git" {
			continue
		}
		if bare[filepath.Base(rel)] || anchored[filepath.ToSlash(rel)] {
			b.grant(d)
		}
	}
}

func addBareNames(b *dirGrantBook, bare map[string]bool) {
	names := make([]string, 0, len(bare))
	for name := range bare {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic argv/mkdir order
	for _, name := range names {
		// Granting ".git" would expose the gitdir pointer or the shared clone's metadata.
		if name == ".git" {
			continue
		}
		b.grant(name)
	}
}

func (b *dirGrantBook) grant(rel string) {
	rel = filepath.Clean(rel)
	// IsLocal is required: addBareNames passes .gitignore lines verbatim, and that file is untrusted repo content.
	if rel == "." || rel == "" || b.seen[rel] || !filepath.IsLocal(rel) {
		return
	}
	// Lstat: a symlink or non-dir would make the grant RW its real target. Missing is fine (precreated later).
	if fi, err := os.Lstat(filepath.Join(b.work, rel)); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir()) {
		return
	}
	b.seen[rel] = true
	b.out = append(b.out, rel)
}

// PrecreateBuildDirs: a RW grant only covers an existing path, and a read-only node can't mkdir once
// sandboxed. Called from setup, before any sandboxed worker starts in dir.
func PrecreateBuildDirs(dir string, buildDirs []string) {
	for _, rel := range buildDirGrants(dir, buildDirs) {
		if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
			slog.Warn("could not pre-create a writable build dir for the read-only sandbox",
				"component", "workspace", "dir", dir, "rel", rel, "err", err)
		}
	}
}

// landlockSystemDirs: all of /etc is safe since Landlock only adds to DAC (/etc/shadow stays unreadable).
// /proc is needed by Node: without it os.cpus() is empty and worker pools size to zero.
func landlockSystemDirs() []string {
	return []string{"/usr", "/bin", "/lib", "/lib64", "/sbin", "/etc", "/proc"}
}

// toolchainROPaths is toolchainArgs' landlock equivalent.
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

// landlockTmpDir falls back to the host's shared /tmp, loudly: it may be another device (git EXDEV),
// and without a mount namespace it is not private.
func landlockTmpDir(caps Caps) string {
	if tmp := homeTmpDir(caps); tmp != "" {
		return tmp
	}
	slog.Warn("no workspace-scoped scratch dir available; TMPDIR falls back to the shared /tmp, which may be a "+
		"different filesystem than the workspace - git operations that hardlink (clone --local, worktree add) "+
		"can fail with a confusing EXDEV", "component", "workspace", "tmp", os.TempDir(), "workroot", caps.WorkRoot)
	return os.TempDir()
}

// landlockSelfExe prefers the small quack-sandbox sidecar so a child re-execs a few MB, not the server;
// falls back to self-exec when the sidecar isn't installed (dev, `go test`).
var landlockSelfExe = sync.OnceValue(func() string {
	self, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return resolveLandlockSelfExe(self)
})

// resolveLandlockSelfExe: no version handshake; the sidecar is assumed to come from the same image build.
func resolveLandlockSelfExe(self string) string {
	if sidecar := filepath.Join(filepath.Dir(self), sandboxSidecar); isExecutableRegularFile(sidecar) {
		return sidecar
	}
	return self
}

const sandboxSidecar = "quack-sandbox"

// isExecutableRegularFile: Lstat so a symlink, which could point anywhere, is never the re-exec target.
func isExecutableRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

// selfExecDispatch: this binary answers __sandbox-exec/__reap. Without it a self-exec would rerun
// the host binary itself - a test binary with no TestMain dispatch recursing into its own tests.
var selfExecDispatch atomic.Bool

// withReaper runs argv under a __reap subreaper so no descendant outlives it (bwrap's PID namespace
// already does). Unchanged off Linux or when neither the sidecar nor the dispatch is available.
func withReaper(argv []string) []string {
	if len(argv) == 0 || !canSelfExec() {
		return argv
	}
	out := append([]string{landlockSelfExe(), ReapArg, "--"}, argv...)
	// Resolve on the server's PATH like exec.Command would; the reaper only sees the child's PATH.
	if p, err := exec.LookPath(argv[0]); err == nil {
		out[3] = p
	}
	return out
}

// canSelfExec: on Linux, the sidecar or this binary's own dispatch answers __sandbox-exec/__reap.
func canSelfExec() bool {
	return runtime.GOOS == "linux" && (selfExecDispatch.Load() || filepath.Base(landlockSelfExe()) == sandboxSidecar)
}

// RunSandboxExecIfInvoked dispatches the __sandbox-exec and __reap self-execs; call first in main or TestMain.
func RunSandboxExecIfInvoked() {
	var run func([]string) error
	if len(os.Args) > 1 && os.Args[1] == SandboxExecArg {
		run = SandboxExecMain
	} else if len(os.Args) > 1 && os.Args[1] == ReapArg {
		run = ReapMain
	}
	if run == nil {
		selfExecDispatch.Store(true)
		return
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// SandboxTmpDir is TMPDIR for subprocesses outside RunArgv (ACP): the scratch dir WrapArgv grants RW.
// SandboxNone keeps os.TempDir(); a WorkRoot-derived path may not exist on a dev machine.
func SandboxTmpDir(caps Caps) string {
	if EnforcesBoundary(caps.Sandbox) {
		return landlockTmpDir(caps)
	}
	return os.TempDir()
}

// preseededGoModCache: the image's read-only module cache, so builds work with no network.
const preseededGoModCache = "/usr/local/go/pkg/mod"

// EnsureWritableGoModCache symlinks each preseeded entry into a writable GOMODCACHE under home.
// Go ignores the failed cache/lock through the symlinks, so only modules the preseed lacks fail.
func EnsureWritableGoModCache(home string) string {
	dir := filepath.Join(home, "go", "pkg", "mod")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir
	}
	entries, err := os.ReadDir(preseededGoModCache)
	if err != nil {
		return dir
	}
	for _, e := range entries {
		link := filepath.Join(dir, e.Name())
		if _, err := os.Lstat(link); err == nil {
			continue // already farmed, or the caller/a build wrote something real here
		}
		_ = os.Symlink(filepath.Join(preseededGoModCache, e.Name()), link)
	}
	return dir
}

func GoModCachePreseeded(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// SandboxJavaToolOptions: the variable replaces rather than merges, and java.io.tmpdir is hardcoded to /tmp.
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

// Sizes JVM regions to fit inside asMB RLIMIT_AS with ~39% margin.
func javaAddressSpaceOptions(asMB int) string {
	if asMB <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"-Xmx%dm -XX:MaxMetaspaceSize=%dm -XX:CompressedClassSpaceSize=%dm -XX:ReservedCodeCacheSize=%dm -XX:ActiveProcessorCount=%d",
		asMB*35/100, asMB*10/100, asMB*8/100, asMB*8/100, javaBuildProcessors,
	)
}

// Core count cap for a sandboxed JVM (one gate-check build).
const javaBuildProcessors = 4

// Hermetic PATH for subprocesses outside RunArgv/RunPipeline (ACP agent).
func ChildPath(caps Caps) string {
	return childPath(caps)
}

// WrapArgv: the one seam for callers outside RunArgv (ACP). No caps.Limits: FSIZE breaks the agent's
// growing DB and RLIMIT_AS breaks V8's startup reservation; the container quota is the bound.
func WrapArgv(dir string, argv []string, caps Caps, extraRO, extraRW []string) []string {
	if len(argv) == 0 || !EnforcesBoundary(caps.Sandbox) {
		if caps.ReadOnly {
			warnReadOnlyUnenforced(caps.Sandbox)
		}
		return withReaper(argv)
	}
	rw, ro := landlockGrants(dir, caps)
	rw = append(rw, extraRW...)
	ro = append(ro, extraRO...)
	if caps.Sandbox == SandboxBwrap {
		return bwrapWrapArgv(dir, argv, caps, rw, ro)
	}
	return withReaper(assembleSandboxExec(rw, ro, argv))
}

func EnforcesBoundary(mode SandboxMode) bool {
	return mode == SandboxLandlock || mode == SandboxBwrap
}

// bwrapWrapArgv binds landlockGrants at identity paths: the ACP child exchanges absolute paths
// with quack over JSON-RPC, so childArgv's /workspace remap would break them.
func bwrapWrapArgv(dir string, argv []string, caps Caps, rw, ro []string) []string {
	args := bwrapSystemArgs()
	args = append(args, tmpArgs(caps)...)
	args = append(args, identityBinds(rw, ro)...)
	// Root RO so the parent dirs bwrap creates for the binds aren't writable scratch space.
	args = append(args, "--remount-ro", "/", "--chdir", dir, "--")
	return append([]string{bwrapPath()}, append(args, argv...)...)
}

// identityBinds sorts shallowest first: a later bind on a subpath overlays the earlier one,
// so the most specific grant wins (a read-only tree inside a writable HOME stays read-only).
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
	// RO first: a path granted both ways keeps the read-only bind.
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

// bwrapOwnedMount: paths bwrap already mounts. A host bind over /proc or /dev would leak the real
// PID table and device nodes back in.
func bwrapOwnedMount(p string) bool {
	switch p {
	case "/proc", "/dev", "/tmp":
		return true
	}
	return slices.Contains(landlockSystemDirs(), p)
}

var warnReadOnlyUnenforcedOnce sync.Once

func warnReadOnlyUnenforced(mode SandboxMode) {
	warnReadOnlyUnenforcedOnce.Do(func() {
		slog.Warn("read_only agent's own working directory is NOT read-only enforced at the OS level in this sandbox mode "+
			"(WrapArgv only mounts it RO under landlock or bwrap); the agent could write there - only its prompt says not to. "+
			"Set workspace.sandbox: landlock or bwrap to enforce it.", "component", "workspace", "sandbox", mode)
	})
}

// WorktreeCommonGitDir is the shared .git dir a linked worktree points at; "" for a plain clone.
func WorktreeCommonGitDir(dir string) string {
	_, common := worktreeGitDirs(dir)
	return common
}

// worktreeGitDirs: both "" when dir is not a linked worktree.
func worktreeGitDirs(dir string) (gitdir, common string) {
	line, err := readPointer(filepath.Join(dir, ".git"))
	if err != nil {
		return "", ""
	}
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return "", ""
	}
	gitdir = strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	if common, err = readPointer(filepath.Join(gitdir, "commondir")); err != nil {
		return "", ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	return filepath.Clean(gitdir), filepath.Clean(common)
}

// worktreeGrants: RW own gitdirs, RO shared dirs, else a read-only node could rewrite the implementer's refs.
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

func worktreeCommonGitDirs(paths ...string) []string {
	_, ro := worktreeGrants(paths...)
	return ro
}
