package workspace

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// SandboxMode selects the OS boundary every RunArgv/RunPipeline child process
// runs inside. The workspace Jail is a PATH check on the TOOLS; it never
// constrained a child process - this is the security boundary. run_command
// hands its command line to a real shell (RunShell) either way (#277);
// SandboxMode only decides whether childArgv wraps that shell in a bwrap
// namespace (SandboxBwrap) or runs it with the server user's own filesystem
// authority (SandboxNone).
type SandboxMode string

const (
	// SandboxBwrap wraps each child in a bubblewrap (bwrap) mount/pid/ipc/user
	// namespace: the host filesystem is replaced by a read-only view of the
	// system directories the toolchains need, plus exactly two writable paths -
	// the child's own working directory and its isolated $HOME. Everything else
	// (~/.ssh, ~/.aws, ~/.config/gh, /etc/shadow, other users' workspaces, the
	// server's own .env) is not merely un-suggested: it does not exist inside
	// the child's mount namespace. Needs no daemon and no root.
	SandboxBwrap SandboxMode = "bwrap"
	// SandboxNone runs children directly, exactly as quack did before: with the
	// server user's full filesystem authority. Loudly warned about at startup.
	SandboxNone SandboxMode = "none"
	// SandboxLandlock confines each child with a self-applied Landlock ruleset
	// (see landlock_linux.go) instead of a constructed mount namespace: no new
	// PID/mount/user namespace, so paths are NOT remapped (SandboxWorkRoot is
	// bwrap-only) and /proc stays the server's own (see childArgv's landlock
	// branch). Chosen for the container deploy, where bwrap cannot nest inside
	// an unprivileged Docker container but Landlock - a self-restriction, not a
	// namespace construction - still applies.
	SandboxLandlock SandboxMode = "landlock"
)

// SandboxExecArg is the hidden argv[1] dispatch mode any binary that can
// resolve its own path (os.Executable) answers: [self, SandboxExecArg, --rw
// <path>..., --ro <path>..., [--probe], --, <target argv>...]. It is the
// Landlock analogue of the GIT_ASKPASS self-exec (internal/tools/git.go's
// GitAskpassLinkName) - never a documented CLI command, dispatched on raw
// argv before cobra/testing.Main ever runs (see RunSandboxExecIfInvoked and
// cmd/quack/main.go).
const SandboxExecArg = "__sandbox-exec"

// bwrapBinary / prlimitBinary are looked up on the SERVER's ambient PATH (like
// every other binary RunArgv resolves - see RunArgv's LookPath rationale).
const (
	bwrapBinary   = "bwrap"
	prlimitBinary = "prlimit"
)

// SandboxWorkRoot is the ONE path a sandboxed child's workspace appears at,
// whatever the host calls it: Caps.WorkRoot is bind-mounted here and the child
// chdir'd relative to it. Never varies by host, chat, or node - the shell half
// of the one-namespace invariant (a `pwd` that prints the host path hands the
// model two names for one place).
//
// Not "/" itself because the child still needs /usr, /proc, /dev to exist, and
// making the node dir the root would create those mountpoints inside it where
// the fs tools would show them. Cost: a top-level entry literally named
// "workspace" is shadowed in the ABSOLUTE spelling only.
const SandboxWorkRoot = "/workspace"

// Limits are the per-child-PROCESS resource limits (setrlimit), applied via
// prlimit(1) as the INNERMOST wrapper - Go's os/exec has no setrlimit hook, and
// setting them in the server process would limit the server itself. Zero means
// "leave the inherited limit alone". Motivation: a runaway build (`npm ci` on a
// hostile repo, a `go test` that allocates without bound) can OOM the machine
// the server runs on; nothing stopped it.
type Limits struct {
	// AddressSpaceMB is RLIMIT_AS - per process, not per build. Keep it
	// generous: Node's V8 reserves a very large VIRTUAL region at startup, so a
	// too-tight limit does not slim a build down, it makes `node` refuse to
	// start at all.
	AddressSpaceMB int
	// Procs is RLIMIT_NPROC. Applied ONLY under SandboxBwrap: RLIMIT_NPROC is
	// counted per-UID across the whole system, so outside the sandbox's user
	// namespace a limit below the server user's existing process count fails
	// every fork - including bwrap's own (observed: "Creating new namespace
	// failed: Resource temporarily unavailable"). Inside the namespace the
	// count starts at ~0 and the limit means what it says. --unshare-pid
	// already contains a fork bomb's blast radius; this bounds it.
	Procs int
	// FileSizeMB is RLIMIT_FSIZE: a child that writes past it is killed
	// (SIGXFSZ), so no agent can fill the server's disk with one command.
	FileSizeMB int
}

// ResolveSandbox validates the configured mode and PROVES it works on this host
// before the server starts serving. It never falls back: a deployment that asks
// for a boundary and doesn't get one is exactly the failure this whole change
// exists to remove, so a missing/broken bwrap is a startup error, and running
// without a boundary is a WARN that says plainly what it costs.
func ResolveSandbox(mode SandboxMode) (SandboxMode, error) {
	switch mode {
	case SandboxNone:
		slog.Warn("workspace sandbox is OFF (workspace.sandbox: none): every run_command and gate-check child process "+
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

// probeBwrap checks that bwrap is installed AND that it can actually create a
// namespace here - presence is not proof: a container runtime whose seccomp
// profile blocks unshare(CLONE_NEWUSER) has bwrap on disk and cannot use it,
// and that must fail at startup, not on the first agent command. The program it
// runs inside the probe sandbox is bwrap itself (`bwrap --version`): /usr is
// bound read-only, so it is always present in there, with no dependency on
// coreutils existing in the image.
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

// bwrapSystemArgs is the host-independent half of the sandbox: the namespaces
// and the read-only system view. Every bind is here because something a coding
// agent routinely runs needs it - discovered by running `go build`, `go test`,
// `npm install`, `npm test`, `npx`, and `git` inside the sandbox until they all
// passed:
//
//   - --unshare-user: the whole reason this needs no root or daemon.
//   - --unshare-pid: the child can neither see nor signal the server's own
//     processes, and a fork bomb dies with its namespace.
//   - --unshare-ipc / --unshare-uts: no shared SysV IPC, no hostname games.
//   - --die-with-parent: a killed/timed-out run leaves nothing behind.
//   - --new-session: no controlling terminal to inject keystrokes into (TIOCSTI).
//   - /usr, /bin, /lib, /lib64, /sbin (ro): the toolchains and their shared
//     libraries. git links against libcurl/libssl/libpcre2/zlib; node and go
//     live here too. Read-only: a child cannot patch the system it runs on.
//   - /etc/ssl + /etc/ca-certificates (ro): TLS trust - `npm install` and
//     `git clone` over HTTPS fail without it.
//   - /etc/resolv.conf, /etc/hosts, /etc/nsswitch.conf (ro): DNS. The network
//     namespace is deliberately NOT unshared - agents legitimately fetch
//     dependencies (npm ci, go mod download) - so name resolution must work.
//   - /etc/passwd, /etc/group (ro): getpwuid()/getgrgid() - npm and git both
//     look the running user up and misbehave when it doesn't exist.
//   - /etc/alternatives (ro): on Debian, /usr/bin/<tool> is often a symlink
//     into here (java, editor, …).
//   - /etc/localtime (ro): sane timestamps in build/test output.
//   - --proc /proc: required with --unshare-pid; every language runtime reads it.
//   - --dev /dev: a minimal device set (/dev/null, /dev/urandom, /dev/tty) -
//     NOT the host's /dev.
//
// Everything NOT listed is absent from the child's filesystem - including all
// of $HOME, /root, /etc/shadow, and the rest of /etc.
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

// childArgv is the ONE place a child's real argv is assembled: rlimits wrap the
// program, and the sandbox wraps that. bin is the already-resolved absolute
// path of argv[0] (see RunArgv), dir the jail-resolved working directory.
//
//	bwrap <system view> <toolchains ro> <workspace + HOME rw> <root aliases>
//	      --remount-ro / --chdir <workspace path of dir> -- prlimit … -- <bin> <args>
//
// prlimit goes INSIDE bwrap on purpose: RLIMIT_NPROC is counted per-UID
// system-wide, so it is only meaningful (and only safe) inside the sandbox's
// own user namespace.
func childArgv(dir, bin string, argv []string, caps Caps) []string {
	inner := append([]string{bin}, argv[1:]...)
	switch caps.Sandbox {
	case SandboxLandlock:
		return landlockArgv(dir, inner, caps)
	case SandboxBwrap:
		// falls through to the bwrap assembly below
	default:
		// No OS boundary: still bound the damage a runaway build can do, but
		// leave RLIMIT_NPROC alone (see Limits.Procs).
		return withLimits(inner, caps.Limits, false)
	}
	args := bwrapSystemArgs()
	args = append(args, tmpArgs(caps)...)
	args = append(args, toolchainArgs(caps)...)
	// The only writable paths: the node's own directory (Caps.WorkRoot) and its
	// isolated $HOME. NOT the whole workspace root - a node's child still cannot
	// reach another node's clone, another chat's tree, or another user's jail.
	// Bind the WHOLE node dir, not just the cwd: tmpArgs replaces /tmp wholesale,
	// so anything the child wrote in its workspace outside the bind would land in
	// the throwaway mount and evaporate (a live clone vanished exactly that way).
	// Mounted at SandboxWorkRoot, a FIXED path, so the host path / chat id /
	// node id never enter the child's view.
	work := caps.WorkRoot
	if work == "" || !isDir(work) {
		work = dir // no node scope (a direct/un-gated call): the cwd is all there is
	}
	args = append(args, "--bind", work, SandboxWorkRoot)
	chdir := SandboxWorkRoot
	if rel, ok := relUnder(work, dir); ok {
		chdir = filepath.Join(SandboxWorkRoot, rel)
	} else {
		// A cwd outside the node's own workspace - the gate's baseline worktree
		// (internal/vetting/baseline.go) is the only one, and no model ever sees
		// its path. Bind it where it is and run there, exactly as before.
		args = append(args, "--bind", dir, dir)
		chdir = dir
	}
	if caps.HomeDir != "" && caps.HomeDir != dir {
		args = append(args, "--bind", caps.HomeDir, caps.HomeDir)
	}
	// A linked git worktree's parent .git store lies OUTSIDE work entirely -
	// bind it at its own real path, same exception as the outside-cwd branch
	// above, since bwrap cannot remap two disjoint host trees into one mount
	// without giving the child two names for one place. Read-only for the
	// shared store, read-write for the worktree's own gitdir nested inside it
	// (see worktreeGrants) - and in that order, because a later bwrap bind
	// layers over an earlier one.
	wtRW, wtRO := worktreeGrants(work, dir)
	for _, common := range wtRO {
		args = append(args, "--ro-bind", common, common)
	}
	for _, gitdir := range wtRW {
		args = append(args, "--bind", gitdir, gitdir)
	}
	args = append(args, rootAliasArgs(work, dir, caps)...)
	// The sandbox's own root is a throwaway tmpfs: anything written there is gone
	// when the command exits. That is how we ate the agent's files once already
	// (#214), and the root aliases below make a stray `cd /repo && cd ..` land
	// there. Read-only, so a write to the fake root FAILS instead of vanishing;
	// every real mount (/workspace, $HOME, /tmp, /dev, /proc) keeps its own flags.
	args = append(args, "--remount-ro", "/")
	args = append(args, "--chdir", chdir, "--")
	args = append(args, withLimits(inner, caps.Limits, true)...)
	return append([]string{bwrapPath()}, args...)
}

// rootAliasArgs symlinks each top-level entry of the node's workspace to the
// same name at the sandbox root (/quack → /workspace/quack), so the "/quack"
// paths the fs tools hand back also work in the shell - the last inch of the one
// namespace. A symlink holds no data, so a write through the alias lands in the
// real bind and survives. Entries colliding with a real mount are skipped: the
// mount must win.
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

// maxRootAliases bounds the symlink farm: the node dir holds clones and a handful
// of files, so this is never reached in practice - it just keeps a pathological
// directory (an agent that wrote 5,000 files at its root) from building an absurd
// argv. Beyond it the entries are still reachable at /workspace/<name>.
const maxRootAliases = 100

// reservedRoots are the top-level names inside the sandbox that a workspace entry
// may NOT shadow: the system view's mountpoints, the workspace mount itself, and
// the first component of every host path we bind (the isolated $HOME, an outside
// cwd, the exec_path toolchains, a linked worktree's parent .git store - bwrap
// creates those parent dirs at the root).
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
	for _, p := range extra {
		add(p)
	}
	return m
}

// firstComponent is the first path element of an absolute path ("/home/j/x" →
// "home"); "" for a relative or empty path.
func firstComponent(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(p), "/"), "/")
	return parts[0]
}

// relUnder reports dir's path RELATIVE to base when dir is base or lies inside it
// - the mapping from a host path to its place under SandboxWorkRoot. Both paths
// come from Jail.Resolve (already cleaned and symlink-resolved), so a lexical
// answer is the true one.
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

// isDir guards the WorkRoot bind: bwrap fails outright on a bind source that does
// not exist, and a caller can hand us a node dir that was never created (a gate
// check on a node whose worker never wrote anything). Falling back to the cwd -
// which by then has been stat'd by the caller - keeps that a normal run.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// tmpArgs gives the child a private /tmp. It is backed by a real directory
// under the isolated $HOME rather than a tmpfs when one is available: a tmpfs
// lives in RAM, and a `go build`'s temporary objects are exactly the kind of
// multi-gigabyte write that would then become memory pressure on the server.
func tmpArgs(caps Caps) []string {
	if tmp := homeTmpDir(caps); tmp != "" {
		return []string{"--bind", tmp, "/tmp"}
	}
	return []string{"--tmpfs", "/tmp"}
}

// homeTmpDir is caps.HomeDir/tmp, created on demand - "" when HomeDir is
// unset or the dir can't be created, so callers fall back to whatever /tmp
// isolation their sandbox mode offers (bwrap: a tmpfs; landlock: the real
// /tmp, since it has no mount namespace to privatize one - see
// landlockTmpDir).
func homeTmpDir(caps Caps) string {
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

// toolchainArgs read-only-binds the operator's configured exec_path entries
// (workspace.exec_path - the real toolchain dirs, e.g. nvm's node bin), which
// live outside the system directories bwrapSystemArgs covers.
//
// A bin/ entry gets its FHS siblings (lib, libexec, share) bound too: a prefix
// toolchain keeps its libraries next to its binaries and its bin entries are
// symlinks into them (nvm's `npm` is a symlink to ../lib/node_modules/npm/…),
// so binding bin/ alone yields a working `node` and a broken `npm`. Only those
// three siblings - never the parent directory itself, which for a `~/bin` entry
// would be the operator's whole home directory.
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

// withLimits prefixes argv with prlimit(1) when any limit is set. prlimit is
// util-linux - present on any Linux userland, including the debian-slim runtime
// image - but limits are DoS hygiene, not the security boundary, so a host
// without it gets a one-time WARN and unlimited children rather than a refusal
// to run (unlike the sandbox itself, which fails closed at startup).
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

// bwrapPath resolves bwrap once. ResolveSandbox already proved it is there and
// works before any child runs, so a lookup failure here is unreachable in a
// running server; returning the bare name keeps the failure a clear "bwrap not
// found" from exec rather than a panic.
func bwrapPath() string {
	if p, err := exec.LookPath(bwrapBinary); err == nil {
		return p
	}
	return bwrapBinary
}

// ---------------------------------------------------------------------------
// Landlock mode: no mount namespace, so paths keep their real host names -
// the child self-restricts (via the __sandbox-exec re-exec) instead of
// running inside a constructed filesystem view. See landlock_linux.go for the
// actual ruleset (build-tagged: Landlock is Linux-only).
// ---------------------------------------------------------------------------

// probeLandlockHook lets a test simulate an unsupported kernel (ABI < 3)
// without one - ResolveSandbox's fail-closed path needs exercising even on a
// host (like CI) that DOES have working Landlock. probeLandlock itself is
// build-tag-selected (landlock_linux.go / landlock_other.go).
var probeLandlockHook = probeLandlock

// landlockArgv builds the [self, __sandbox-exec, --rw …, --ro …, --, inner…]
// argv childArgv's landlock branch execs - the RunArgv/RunPipeline path,
// which has no extra grants beyond the node's own scope (WrapArgv is the
// seam for a caller that needs more, e.g. internal/acp's skill paths).
func landlockArgv(dir string, inner []string, caps Caps) []string {
	rw, ro := landlockGrants(dir, caps)
	return assembleSandboxExec(rw, ro, withLimits(inner, caps.Limits, false))
}

// assembleSandboxExec is the ONE place a __sandbox-exec argv is built from a
// grant set and a final command - shared by landlockArgv and WrapArgv so the
// two callers can't drift on flag order or the self-exec path.
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

// landlockGrants computes the read-write and read-only path grants for a
// child rooted at dir: RW is the node's own scope (mirrors childArgv's bwrap
// branch - its own WorkRoot/cwd, the isolated HOME, and tmp), RO is the
// system view plus the operator's toolchain extras. Landlock can't remap
// paths (no mount namespace - see SandboxWorkRoot's doc), so unlike bwrap
// these are the child's REAL host paths, not a fixed mount.
func landlockGrants(dir string, caps Caps) (rw, ro []string) {
	work := caps.WorkRoot
	if work == "" || !isDir(work) {
		work = dir
	}
	rw = append(rw, work)
	if _, ok := relUnder(work, dir); !ok {
		// dir lies outside the node's own workspace (the gate's baseline
		// worktree is the only caller this happens for) - grant it directly,
		// mirroring bwrap's childArgv "outside cwd" branch.
		rw = append(rw, dir)
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

// landlockSelfExe resolves this binary's own path for the __sandbox-exec
// self-spawn (os.Executable() - askpass already relies on this working).
// ResolveSandbox's probe already proved this exact lookup succeeds before any
// child runs, so a failure here is unreachable in a running server;
// falling back to argv[0] keeps a would-be failure a clear exec error rather
// than a panic (mirrors bwrapPath's rationale).
func landlockSelfExe() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return os.Args[0]
}

// RunSandboxExecIfInvoked is the Landlock analogue of the GIT_ASKPASS argv[0]
// dispatch (internal/tools/git.go's isGitAskpassInvocation / cmd/quack's
// main): when this process was spawned as [self, SandboxExecArg, …] by
// landlockArgv/WrapArgv, apply the ruleset and exec the target - never
// returns on success. Call this at the very top of main() (see
// cmd/quack/main.go), before cobra/testing.Main ever run; this package's own
// tests mirror the call in a TestMain for the same reason askpass's do.
func RunSandboxExecIfInvoked() {
	if len(os.Args) < 2 || os.Args[1] != SandboxExecArg {
		return
	}
	// SandboxExecMain only ever RETURNS for --probe (or an error) - a real
	// target exec's success path replaces this process and never comes back.
	// Always os.Exit here regardless, so the caller (main, or a test
	// binary's TestMain) can never fall through into its own normal
	// execution - which, in a test binary, would silently re-run the whole
	// suite inside what was meant to be a throwaway probe/shim child.
	if err := SandboxExecMain(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// SandboxTmpDir is the TMPDIR a subprocess built OUTSIDE RunArgv/RunPipeline
// (internal/acp) should be given: the granted tmp dir under landlock (see
// childEnv's doc - Landlock can't remap /tmp, so a tool must be TOLD where
// its writable tmp lives), the server's real /tmp otherwise (bwrap remaps it
// wholesale; none has no grant to honor).
func SandboxTmpDir(caps Caps) string {
	if caps.Sandbox == SandboxLandlock {
		return landlockTmpDir(caps)
	}
	return os.TempDir()
}

// ChildPath is the hermetic PATH any subprocess built OUTSIDE RunArgv/
// RunPipeline should run with (internal/acp's ACP agent, which constructs its
// own exec.Cmd) - caps.ExtraPath first, then the same fixed system
// directories every other child gets. See childPath.
func ChildPath(caps Caps) string {
	return childPath(caps)
}

// WrapArgv wraps argv (already command[0]+args, resolved by the caller) to
// run confined at dir under caps.Sandbox, with extraRO/extraRW grants added on
// top of the caller's own scope (internal/acp's skill paths and exec_path) -
// a linked worktree's parent .git store is granted automatically
// (worktreeCommonGitDirs via landlockGrants), with no extraRW needed for it.
// The ONE seam a caller outside RunArgv/newChildCmd routes through.
//
// landlock: fully confines, same mechanism as childArgv's landlock branch.
// none: returns argv unchanged by design - there is no boundary to wrap into.
// bwrap: ALSO returns argv unchanged for now - ponytail: wiring an ACP
// subprocess into the SAME bind/remount/chdir machinery childArgv assembles
// for RunArgv (fixed /workspace, root aliases, the outside-cwd exception) is
// real work with its own edge cases the ACP spawn shape doesn't share with a
// gate check; landlock is what the container deploy this spec exists for
// actually uses, and bwrap's host deploys already ran the ACP agent bare.
// Ceiling: an ACP round under `sandbox: bwrap` stays unconfined until this is
// wired.
func WrapArgv(dir string, argv []string, caps Caps, extraRO, extraRW []string) []string {
	if len(argv) == 0 || caps.Sandbox != SandboxLandlock {
		return argv
	}
	rw, ro := landlockGrants(dir, caps)
	rw = append(rw, extraRW...)
	ro = append(ro, extraRO...)
	return assembleSandboxExec(rw, ro, argv)
}

// ---------------------------------------------------------------------------
// Linked-worktree grants: dag's read-only-node isolation (worktree per
// reviewer/explorer node) gives each such node its own `git worktree`, linked
// off the plan's ONE shared setup clone rather than an independent clone. A
// linked worktree's OWN ".git" is a pointer FILE (not a directory) reading
// "gitdir: <parent>/.git/worktrees/<name>" - the actual object database,
// refs, and per-worktree index/HEAD/logs all live under the PARENT clone's
// ".git", found via that gitdir's own "commondir" file. Granting only the
// worktree's own directory leaves every git command inside it unable to see
// its own history ("not a git repository" or worse, a silent empty log) -
// this resolves the one extra grant that fixes it, for both sandbox modes.
// ---------------------------------------------------------------------------

// WorktreeCommonGitDir resolves the shared ".git" directory a linked git
// worktree at dir points at ("" when dir is not a linked worktree at all - a
// plain clone's own ".git" is a directory, not a pointer file, and this
// returns "" for it too since a plain clone needs no extra grant).
func WorktreeCommonGitDir(dir string) string {
	_, common := worktreeGitDirs(dir)
	return common
}

// worktreeGitDirs resolves both halves a linked worktree needs: its OWN gitdir
// (the "gitdir:" pointer target, .git/worktrees/<name> - HEAD and the index
// live here, so git writes it on an ordinary `status`) and the shared common
// dir it nests inside (objects, refs, packed-refs, logs). Both "" when dir is
// not a linked worktree.
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

// worktreeGrants splits the linked-worktree grants for paths (typically (work,
// dir), often the same directory) into the WRITABLE per-worktree gitdirs and
// the READ-ONLY shared common dirs, deduped.
//
// The split is the point: granting the common dir read-write would let a
// read-only node (a reviewer or explorer in its own worktree) rewrite the
// IMPLEMENTER's refs and object store - the very sharing the worktree
// isolation exists to end. Measured: with the common dir read-only and only
// the per-worktree gitdir writable, `git status`, `git log` and `git diff` all
// still work inside the worktree, while writing <common>/refs/heads/X is
// denied. Landlock applies the most specific enclosing rule, and bwrap's later
// binds layer over earlier ones, so the writable gitdir nested inside the
// read-only common dir works in both modes.
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

// worktreeCommonGitDirs is worktreeGrants' read-only half, for callers that
// only need the paths (rootAliasArgs' reserved-name set).
func worktreeCommonGitDirs(paths ...string) []string {
	_, ro := worktreeGrants(paths...)
	return ro
}
