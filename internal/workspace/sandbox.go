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
// runs inside.
//
// Why this exists: the workspace Jail is a PATH check — it confines the paths
// the filesystem/git TOOLS resolve. It never constrained a child process, only
// its cwd: RunArgv set cmd.Dir and scrubbed the env, but a child's ARGUMENTS
// were never path-checked, argv[0] resolved against the server's ambient PATH,
// and there is no binary allowlist. `run_command: cat ~/.ssh/id_ed25519` — or
// any `sh -c "…"`, which contains none of the rejected shell metacharacters —
// therefore ran as the server's own OS user with that user's full filesystem
// authority. The metachar wall (shellMetachars) is an LLM-habit guard, not a
// security boundary; this is the security boundary. That is also why, once this
// exists, run_command hands its command line to a REAL SHELL inside the
// namespace (RunShell): the guard blocked no attack, it only blocked the model.
// With SandboxNone there is no boundary but the habit guard, so there the
// argv-only rule — and the metachar rejection — stays exactly as it was.
type SandboxMode string

const (
	// SandboxBwrap wraps each child in a bubblewrap (bwrap) mount/pid/ipc/user
	// namespace: the host filesystem is replaced by a read-only view of the
	// system directories the toolchains need, plus exactly two writable paths —
	// the child's own working directory and its isolated $HOME. Everything else
	// (~/.ssh, ~/.aws, ~/.config/gh, /etc/shadow, other users' workspaces, the
	// server's own .env) is not merely un-suggested: it does not exist inside
	// the child's mount namespace. Needs no daemon and no root.
	SandboxBwrap SandboxMode = "bwrap"
	// SandboxNone runs children directly, exactly as quack did before: with the
	// server user's full filesystem authority. Loudly warned about at startup.
	SandboxNone SandboxMode = "none"
)

// bwrapBinary / prlimitBinary are looked up on the SERVER's ambient PATH (like
// every other binary RunArgv resolves — see RunArgv's LookPath rationale).
const (
	bwrapBinary   = "bwrap"
	prlimitBinary = "prlimit"
)

// SandboxWorkRoot is the ONE path a sandboxed child's workspace appears at,
// whatever the host calls it: Caps.WorkRoot (the calling node's own directory —
// the INVISIBLE ROOT every model-supplied path already resolves under, see
// internal/tools/cwd.go) is bind-mounted here, and the child is chdir'd relative
// to it. It never varies — not by host, not by chat, not by node.
//
// This is the shell half of the one-namespace invariant (#204, #209): the fs
// tools call the node's directory "/", so a path the model reads out of any tool
// result must be usable in any tool, INCLUDING the shell. Give the shell's child
// the host path instead and the model is handed two names for one place — which
// is exactly what happened when the shell landed (#213). Live, a code-implementer
// ran `pwd`, got
//
//	/tmp/claude-1000/-home-jason-…/workspace/local/<chatID>/<nodeID>/quack
//
// for the directory every other tool called `/quack`, and spent its next three
// turns running `find /tmp -name quack` over the host filesystem before giving up
// and cloning the repo a SECOND time. `pwd` now prints /workspace/quack.
//
// Why not "/" itself: the child still needs /usr, /bin, /proc and /dev to exist,
// and making the node dir the root would have bwrap create those mountpoints
// INSIDE the node's own directory, where the fs tools would show them to the
// model. A named mountpoint under a read-only root is the closest honest thing.
// "/workspace" is the name because it is what it is; the cost is that a top-level
// entry literally named "workspace" cannot be addressed through the ABSOLUTE
// alias (jailPath maps "/workspace/…" back onto the node root) — it is still
// addressable relatively, and no clone has ever been called that.
const SandboxWorkRoot = "/workspace"

// Limits are the per-child-PROCESS resource limits (setrlimit), applied via
// prlimit(1) as the INNERMOST wrapper — Go's os/exec has no setrlimit hook, and
// setting them in the server process would limit the server itself. Zero means
// "leave the inherited limit alone". Motivation: a runaway build (`npm ci` on a
// hostile repo, a `go test` that allocates without bound) can OOM the machine
// the server runs on; nothing stopped it.
type Limits struct {
	// AddressSpaceMB is RLIMIT_AS — per process, not per build. Keep it
	// generous: Node's V8 reserves a very large VIRTUAL region at startup, so a
	// too-tight limit does not slim a build down, it makes `node` refuse to
	// start at all.
	AddressSpaceMB int
	// Procs is RLIMIT_NPROC. Applied ONLY under SandboxBwrap: RLIMIT_NPROC is
	// counted per-UID across the whole system, so outside the sandbox's user
	// namespace a limit below the server user's existing process count fails
	// every fork — including bwrap's own (observed: "Creating new namespace
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
			"runs as the server's OS user with that user's FULL filesystem authority — it can read ~/.ssh, ~/.aws, "+
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
	default:
		return "", fmt.Errorf("workspace.sandbox: unknown mode %q (want %q or %q)", mode, SandboxBwrap, SandboxNone)
	}
}

// probeBwrap checks that bwrap is installed AND that it can actually create a
// namespace here — presence is not proof: a container runtime whose seccomp
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
// agent routinely runs needs it — discovered by running `go build`, `go test`,
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
//   - /etc/ssl + /etc/ca-certificates (ro): TLS trust — `npm install` and
//     `git clone` over HTTPS fail without it.
//   - /etc/resolv.conf, /etc/hosts, /etc/nsswitch.conf (ro): DNS. The network
//     namespace is deliberately NOT unshared — agents legitimately fetch
//     dependencies (npm ci, go mod download) — so name resolution must work.
//   - /etc/passwd, /etc/group (ro): getpwuid()/getgrgid() — npm and git both
//     look the running user up and misbehave when it doesn't exist.
//   - /etc/alternatives (ro): on Debian, /usr/bin/<tool> is often a symlink
//     into here (java, editor, …).
//   - /etc/localtime (ro): sane timestamps in build/test output.
//   - --proc /proc: required with --unshare-pid; every language runtime reads it.
//   - --dev /dev: a minimal device set (/dev/null, /dev/urandom, /dev/tty) —
//     NOT the host's /dev.
//
// Everything NOT listed is absent from the child's filesystem — including all
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
	if caps.Sandbox != SandboxBwrap {
		// No OS boundary: still bound the damage a runaway build can do, but
		// leave RLIMIT_NPROC alone (see Limits.Procs).
		return withLimits(inner, caps.Limits, false)
	}
	args := bwrapSystemArgs()
	args = append(args, tmpArgs(caps)...)
	args = append(args, toolchainArgs(caps)...)
	// The only writable paths: the child's own WORKSPACE — the calling node's own
	// directory (Caps.WorkRoot), which contains its cwd — and its isolated $HOME
	// (npm's _cacache, GOCACHE, ~/.gitconfig — see Jail.HomeDir). NOT the whole
	// workspace root: a node's child still cannot reach another node's clone,
	// another chat's tree, or another user's jail.
	//
	// Binding only the CWD was silent data loss. tmpArgs replaces /tmp wholesale,
	// and a workspace root that lives under /tmp (ours does) is therefore GONE
	// inside the sandbox except for the one path bound back on top of it. Anything
	// the child wrote elsewhere in its own workspace — a `git clone` into the node
	// dir, a file one directory up — landed in the throwaway mount and evaporated
	// when the command exited. The Go fs tools are not sandboxed and saw the real
	// tree, so the model was handed two contradictory views of its own workspace.
	// Live (2026-07-13), a code-explorer, after cloning a repo with the shell:
	//
	//	"The software-agent-sdk clone is missing from this workspace… I see the
	//	 workspace has changed between turns… The cd tool seems to have lost its
	//	 state or the path resolution is broken."
	//
	// It hadn't. We ate its files. The node's own dir is what the fs tools already
	// treat as writable (it is the invisible root every model path resolves under),
	// so binding it here removes an inconsistency rather than widening anything.
	//
	// And it is mounted at SandboxWorkRoot — a FIXED path — so the child sees the
	// workspace by the same name the model does (see SandboxWorkRoot): the host
	// path, the chat id and the node id never enter the child's view at all.
	work := caps.WorkRoot
	if work == "" || !isDir(work) {
		work = dir // no node scope (a direct/un-gated call): the cwd is all there is
	}
	args = append(args, "--bind", work, SandboxWorkRoot)
	chdir := SandboxWorkRoot
	if rel, ok := relUnder(work, dir); ok {
		chdir = filepath.Join(SandboxWorkRoot, rel)
	} else {
		// A cwd outside the node's own workspace — the gate's baseline worktree
		// (internal/vetting/baseline.go) is the only one, and no model ever sees
		// its path. Bind it where it is and run there, exactly as before.
		args = append(args, "--bind", dir, dir)
		chdir = dir
	}
	if caps.HomeDir != "" && caps.HomeDir != dir {
		args = append(args, "--bind", caps.HomeDir, caps.HomeDir)
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

// rootAliasArgs symlinks each top-level entry of the node's workspace to the same
// name at the sandbox root: /quack → /workspace/quack.
//
// This is the last inch of the one namespace. The model's tools call the node's
// directory "/", so the paths they hand back are "/quack", "/quack/main.go" — and
// the model feeds those straight into the next tool, which is now sometimes a
// shell. Without the aliases, `cat /quack/README.md` in a shell is a "No such
// file or directory" the model has to translate its way out of; with them, the
// path it just read WORKS, which is the whole invariant (#204, #209).
//
// A symlink holds no data: the file is only ever at /workspace/…, so a write
// through the alias lands in the real bind and survives (and the read-only root
// above means a write to the alias's *parent* cannot silently vanish). Entries
// whose names collide with a real mount are skipped — bwrap would refuse to
// create the symlink, and the mount is what must win.
func rootAliasArgs(work, dir string, caps Caps) []string {
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil // unreadable node dir: the fixed mount alone is still correct
	}
	taken := reservedRoots(dir, caps)
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
// of files, so this is never reached in practice — it just keeps a pathological
// directory (an agent that wrote 5,000 files at its root) from building an absurd
// argv. Beyond it the entries are still reachable at /workspace/<name>.
const maxRootAliases = 100

// reservedRoots are the top-level names inside the sandbox that a workspace entry
// may NOT shadow: the system view's mountpoints, the workspace mount itself, and
// the first component of every host path we bind (the isolated $HOME, an outside
// cwd, the exec_path toolchains — bwrap creates those parent dirs at the root).
func reservedRoots(dir string, caps Caps) map[string]bool {
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
// — the mapping from a host path to its place under SandboxWorkRoot. Both paths
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
// check on a node whose worker never wrote anything). Falling back to the cwd —
// which by then has been stat'd by the caller — keeps that a normal run.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// tmpArgs gives the child a private /tmp. It is backed by a real directory
// under the isolated $HOME rather than a tmpfs when one is available: a tmpfs
// lives in RAM, and a `go build`'s temporary objects are exactly the kind of
// multi-gigabyte write that would then become memory pressure on the server.
func tmpArgs(caps Caps) []string {
	if caps.HomeDir == "" {
		return []string{"--tmpfs", "/tmp"}
	}
	tmp := filepath.Join(caps.HomeDir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		slog.Warn("could not create the sandbox tmp dir; falling back to an in-memory /tmp",
			"component", "workspace", "dir", tmp, "err", err)
		return []string{"--tmpfs", "/tmp"}
	}
	return []string{"--bind", tmp, "/tmp"}
}

// toolchainArgs read-only-binds the operator's configured exec_path entries
// (workspace.exec_path — the real toolchain dirs, e.g. nvm's node bin), which
// live outside the system directories bwrapSystemArgs covers.
//
// A bin/ entry gets its FHS siblings (lib, libexec, share) bound too: a prefix
// toolchain keeps its libraries next to its binaries and its bin entries are
// symlinks into them (nvm's `npm` is a symlink to ../lib/node_modules/npm/…),
// so binding bin/ alone yields a working `node` and a broken `npm`. Only those
// three siblings — never the parent directory itself, which for a `~/bin` entry
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
// util-linux — present on any Linux userland, including the debian-slim runtime
// image — but limits are DoS hygiene, not the security boundary, so a host
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
