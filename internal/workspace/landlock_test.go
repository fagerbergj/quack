package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireLandlock skips a test when Landlock ABI >= 3 isn't usable here -
// LOUDLY, mirroring requireBwrap: a sandbox test that silently no-ops proves
// nothing.
func requireLandlock(t *testing.T) {
	t.Helper()
	if err := probeLandlock(); err != nil {
		t.Skipf("SKIPPING landlock test: Landlock ABI >= 3 is not usable here (%v). "+
			"Needs a Linux kernel >= 6.2.", err)
	}
}

// runSandboxExec self-spawns THIS test binary in __sandbox-exec mode (the
// real mechanism - see main_test.go's TestMain, which mirrors
// RunSandboxExecIfInvoked) and returns its combined output and exit code.
func runSandboxExec(t *testing.T, args ...string) (string, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, append([]string{SandboxExecArg}, args...)...)
	out, _ := cmd.CombinedOutput()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return string(out), code
}

// landlockBaseRO grants the real system dirs every exec (sh, cat) needs just
// to run at all under a strict ruleset - standing in for the RO grants
// landlockSystemDirs() supplies in production.
var landlockBaseRO = []string{"--ro", "/usr", "--ro", "/bin", "--ro", "/lib", "--ro", "/lib64"}

// TestSandboxExecShimConfinement is spec test case 1: the whole point of the
// shim. An rw-granted write succeeds, the SAME command targeting a sibling
// (ungranted) dir fails, a granted RO read succeeds, and a read outside every
// grant fails.
func TestSandboxExecShimConfinement(t *testing.T) {
	requireLandlock(t)

	granted := t.TempDir()
	sibling := t.TempDir()

	args := append([]string{"--rw", granted}, landlockBaseRO...)
	args = append(args, "--", "sh", "-c", "echo x > "+filepath.Join(granted, "f"))
	if out, code := runSandboxExec(t, args...); code != 0 {
		t.Fatalf("rw-granted write failed: exit=%d output=%q", code, out)
	}
	if _, err := os.Stat(filepath.Join(granted, "f")); err != nil {
		t.Fatalf("the granted write did not land: %v", err)
	}

	args = append([]string{"--rw", granted}, landlockBaseRO...)
	args = append(args, "--", "sh", "-c", "echo x > "+filepath.Join(sibling, "f"))
	if out, code := runSandboxExec(t, args...); code == 0 {
		t.Fatalf("SANDBOX ESCAPE: wrote to a sibling dir outside the RW grant: %q", out)
	}

	outside := filepath.Join(sibling, "secret")
	if err := os.WriteFile(outside, []byte("PRIVATE-KEY-MATERIAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	args = append([]string{"--ro", "/etc"}, landlockBaseRO...)
	args = append(args, "--", "cat", "/etc/passwd")
	if out, code := runSandboxExec(t, args...); code != 0 {
		t.Fatalf("RO-granted read of /etc/passwd failed: exit=%d output=%q", code, out)
	} else if !strings.Contains(out, "root") {
		t.Fatalf("RO read of /etc/passwd returned unexpected content: %q", out)
	}

	args = append([]string{"--rw", granted}, landlockBaseRO...)
	args = append(args, "--", "cat", outside)
	if out, code := runSandboxExec(t, args...); code == 0 || strings.Contains(out, "PRIVATE-KEY-MATERIAL") {
		t.Fatalf("SANDBOX ESCAPE: read a file outside every grant: exit=%d output=%q", code, out)
	}
}

// TestSandboxExecProbe: --probe applies a trivial strict ruleset and exits
// clean - the mechanism ResolveSandbox's fail-closed check is built on.
func TestSandboxExecProbe(t *testing.T) {
	requireLandlock(t)
	if out, code := runSandboxExec(t, "--probe"); code != 0 {
		t.Fatalf("--probe failed on a host with working Landlock: exit=%d output=%q", code, out)
	}
}

// TestSandboxExecRequiresTarget: no "--" / no target command is a clean
// error, not a hang or a panic.
func TestSandboxExecRequiresTarget(t *testing.T) {
	requireLandlock(t)
	if out, code := runSandboxExec(t, "--rw", t.TempDir()); code == 0 {
		t.Fatalf("sandbox-exec with no target command should fail; got exit 0: %q", out)
	}
}

// TestSandboxLandlockRealToolchains is the empirical basis for the /proc and
// /dev grant decisions (landlockGrants/landlockSystemDirs' docs), exercised
// through the REAL production path (RunArgv -> childArgv's landlock branch),
// not just the raw shim: git runs cleanly with no /proc grant at all, and
// node's os.cpus() - which silently returns an EMPTY array without /proc,
// rather than failing loudly - reports the real CPU count with it.
func TestSandboxLandlockRealToolchains(t *testing.T) {
	requireLandlock(t)

	t.Run("git_needs_no_proc", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not on PATH")
		}
		dir := t.TempDir()
		caps := sandboxCaps(t, SandboxLandlock)
		caps.WorkRoot = dir
		run := func(argv ...string) ExecResult {
			t.Helper()
			res, err := RunArgv(context.Background(), dir, argv, caps)
			if err != nil {
				t.Fatalf("%v: %v (%q)", argv, err, res.Output)
			}
			return res
		}
		if res := run("git", "init", "-q"); res.ExitCode != 0 {
			t.Fatalf("git init: exit=%d %q", res.ExitCode, res.Output)
		}
		if res := run("git", "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "init"); res.ExitCode != 0 {
			t.Fatalf("git commit: exit=%d %q", res.ExitCode, res.Output)
		}
		if res := run("git", "status"); res.ExitCode != 0 {
			t.Fatalf("git status: exit=%d %q", res.ExitCode, res.Output)
		}
	})

	t.Run("node_needs_proc_for_cpu_count", func(t *testing.T) {
		nodeBin, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node not on PATH")
		}
		dir := t.TempDir()
		caps := sandboxCaps(t, SandboxLandlock)
		caps.WorkRoot = dir
		caps.ExtraPath = []string{filepath.Dir(nodeBin)}
		res, err := RunArgv(context.Background(), dir,
			[]string{"node", "-e", "console.log(require('os').cpus().length)"}, caps)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("node under landlock: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
		}
		if got := strings.TrimSpace(res.Output); got == "0" || got == "" {
			t.Errorf("node os.cpus().length = %q under landlock - the /proc grant regressed", got)
		}
	})
}

// setupLinkedWorktreeFixture creates a plain (unsandboxed) clone at parentDir
// and links a git worktree off it at the returned dir, checked out on its own
// branch - the fixture the landlock grant (worktreeCommonGitDirs) exists
// for. Provisioning itself runs OUTSIDE the sandbox (exactly like
// tools.SetupClone/SetupWorktree do in production - the harness provisions,
// only the AGENT'S OWN commands run confined).
func setupLinkedWorktreeFixture(t *testing.T) (worktreeDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	parentDir := filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, argv ...string) string {
		t.Helper()
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", argv, err, out)
		}
		return string(out)
	}
	run(parentDir, "init", "-q")
	run(parentDir, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "init")

	worktreeDir = filepath.Join(t.TempDir(), "review-node")
	run(parentDir, "worktree", "add", "--quiet", "-B", "quack-worktree/review1", worktreeDir, "HEAD")
	return worktreeDir
}

// TestSandboxLandlockLinkedWorktreeGitWorks pins that a linked worktree's git
// commands work under landlock: its ".git" is a pointer file, with the object
// db/refs/index living under the PARENT clone's ".git" outside the worktree
// dir. Without the extra grant (worktreeCommonGitDirs) landlock's deny-by-
// default breaks `git status`/`git log` with "not a git repository".
func TestSandboxLandlockLinkedWorktreeGitWorks(t *testing.T) {
	requireLandlock(t)
	worktreeDir := setupLinkedWorktreeFixture(t)

	caps := sandboxCaps(t, SandboxLandlock)
	caps.WorkRoot = worktreeDir
	run := func(argv ...string) ExecResult {
		t.Helper()
		res, err := RunArgv(context.Background(), worktreeDir, argv, caps)
		if err != nil {
			t.Fatalf("%v: %v (%q)", argv, err, res.Output)
		}
		return res
	}
	if res := run("git", "status"); res.ExitCode != 0 {
		t.Fatalf("git status inside a landlock-wrapped worktree: exit=%d %q", res.ExitCode, res.Output)
	}
	if res := run("git", "log", "-1", "--format=%H"); res.ExitCode != 0 {
		t.Fatalf("git log inside a landlock-wrapped worktree: exit=%d %q", res.ExitCode, res.Output)
	} else if strings.TrimSpace(res.Output) == "" {
		t.Fatal("git log returned no output - the worktree can't see its own history")
	}
}

// TestSandboxLandlockWorktreeCannotWriteParentStore is the other half of the
// worktree grant: the shared store is granted READ-ONLY, so a read-only node
// (a reviewer or explorer in its own worktree) can read the writer's history
// but cannot rewrite its refs or objects. Granting the whole parent .git
// read-write - as the first cut of this did - would have left exactly the
// cross-node write the worktree isolation exists to end.
func TestSandboxLandlockWorktreeCannotWriteParentStore(t *testing.T) {
	requireLandlock(t)
	worktreeDir := setupLinkedWorktreeFixture(t)

	common := WorktreeCommonGitDir(worktreeDir)
	if common == "" {
		t.Fatal("fixture is not a linked worktree")
	}
	caps := sandboxCaps(t, SandboxLandlock)
	caps.WorkRoot = worktreeDir

	// The per-worktree gitdir stays writable - git writes HEAD/index there on
	// an ordinary status, so a read-only grant would break the case above.
	gitdir, _ := worktreeGitDirs(worktreeDir)
	res, err := RunArgv(context.Background(), worktreeDir,
		[]string{"touch", filepath.Join(gitdir, "gc-probe")}, caps)
	if err != nil {
		t.Fatalf("touch in the worktree's own gitdir: %v (%q)", err, res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("writing the worktree's OWN gitdir was denied: exit=%d %q", res.ExitCode, res.Output)
	}

	// The shared store is not writable.
	evil := filepath.Join(common, "refs", "heads", "EVIL")
	res, err = RunArgv(context.Background(), worktreeDir, []string{"touch", evil}, caps)
	if err != nil {
		t.Fatalf("touch parent ref: %v (%q)", err, res.Output)
	}
	if res.ExitCode == 0 {
		t.Errorf("wrote %s from inside a worktree - the parent store is NOT read-only", evil)
	}
	if _, statErr := os.Stat(evil); statErr == nil {
		t.Errorf("%s exists - the write landed despite the grant", evil)
	}
}

// TestWorktreeCommonGitDirResolvesParentGitDir pins the pointer-file parsing
// itself, independent of the sandbox: a linked worktree's WorktreeCommonGitDir
// resolves to the PARENT clone's real ".git" directory, and a plain (non-
// worktree) clone or an arbitrary directory resolves to "".
func TestWorktreeCommonGitDirResolvesParentGitDir(t *testing.T) {
	worktreeDir := setupLinkedWorktreeFixture(t)

	common := WorktreeCommonGitDir(worktreeDir)
	if common == "" {
		t.Fatal("WorktreeCommonGitDir returned \"\" for a real linked worktree")
	}
	if !strings.HasSuffix(common, string(filepath.Separator)+".git") {
		t.Errorf("WorktreeCommonGitDir = %q, want it to end in .git", common)
	}

	if got := WorktreeCommonGitDir(t.TempDir()); got != "" {
		t.Errorf("WorktreeCommonGitDir(plain dir) = %q, want \"\"", got)
	}
}

// TestResolveSandboxLandlockFailsClosed is spec test case 2: a probe failure
// must refuse to start, never fall back - stubbed via probeLandlockHook so
// this runs on every host regardless of kernel support.
func TestResolveSandboxLandlockFailsClosed(t *testing.T) {
	orig := probeLandlockHook
	defer func() { probeLandlockHook = orig }()
	probeLandlockHook = func() error { return errors.New("stub: kernel below Landlock ABI 3") }

	if _, err := ResolveSandbox(SandboxLandlock); err == nil {
		t.Fatal("a failed landlock probe must refuse to start, not fall back")
	} else if !strings.Contains(err.Error(), "ABI") {
		t.Errorf("error should explain the kernel requirement: %v", err)
	}
}

// TestResolveSandboxLandlockSucceeds: on a host where Landlock actually
// works, ResolveSandbox(landlock) returns cleanly - and bwrap's own behavior
// (TestResolveSandbox) stays unchanged alongside it.
func TestResolveSandboxLandlockSucceeds(t *testing.T) {
	requireLandlock(t)
	mode, err := ResolveSandbox(SandboxLandlock)
	if err != nil || mode != SandboxLandlock {
		t.Fatalf("ResolveSandbox(landlock) = %q, %v; want landlock, nil", mode, err)
	}
}

// TestWrapArgvLandlockIncludesExtraGrants: WrapArgv adds extraRO/extraRW on
// top of the caller's own scope (internal/acp's skill paths, and worktree
// isolation's future extraRW). The bwrap and none halves live in
// wrapargv_bwrap_test.go.
func TestWrapArgvLandlockIncludesExtraGrants(t *testing.T) {
	dir := t.TempDir()
	argv := []string{"opencode", "acp"}
	caps := Caps{Sandbox: SandboxLandlock}
	got := WrapArgv(dir, argv, caps, []string{"/skills"}, []string{"/extra-rw"})
	joined := strings.Join(got, " ")
	for _, want := range []string{SandboxExecArg, dir, "/skills", "/extra-rw", "opencode acp"} {
		if !strings.Contains(joined, want) {
			t.Errorf("WrapArgv(landlock) = %v, missing %q", got, want)
		}
	}
}

// TestWrapArgvLandlockCarriesNoLimits (#798, reverting #646): the ACP wrap path
// must NOT carry rlimits. On the live deployment each limit alone stopped
// opencode before its first ACP message - FSIZE 1024MB against a 1.27GB
// opencode.db, and AS 8192MB against V8's startup reservation - both reported
// as the same opaque "Failed query: PRAGMA wal_checkpoint(PASSIVE)". This
// asserts the absence so the next edit to WrapArgv can't silently restore it.
func TestWrapArgvLandlockCarriesNoLimits(t *testing.T) {
	dir := t.TempDir()
	argv := []string{"opencode", "acp"}
	caps := Caps{Sandbox: SandboxLandlock, Limits: Limits{AddressSpaceMB: 8192, FileSizeMB: 1024}}
	got := WrapArgv(dir, argv, caps, nil, nil)
	joined := strings.Join(got, " ")
	for _, bad := range []string{"prlimit", "--as=", "--fsize="} {
		if strings.Contains(joined, bad) {
			t.Errorf("WrapArgv carries %q; the agent subprocess must run with no rlimit ceiling (#798)\ngot: %v", bad, got)
		}
	}
	if !strings.Contains(joined, "opencode acp") {
		t.Errorf("WrapArgv dropped the command: %v", got)
	}
}

// TestLandlockArgvStillCarriesLimits pins the other half of #798: the gate's
// own one-shot children keep their ceilings. A per-command FSIZE is correct
// there - it bounds one build, not a process whose DB grows across every run.
func TestLandlockArgvStillCarriesLimits(t *testing.T) {
	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Skipf("SKIPPING rlimit test: prlimit(1) not installed (%v)", err)
	}
	caps := Caps{Sandbox: SandboxLandlock, Limits: Limits{AddressSpaceMB: 8192, FileSizeMB: 1024}}
	joined := strings.Join(landlockArgv(t.TempDir(), []string{"go", "build", "./..."}, caps), " ")
	for _, want := range []string{"--as=8589934592", "--fsize=1073741824", "go build ./..."} {
		if !strings.Contains(joined, want) {
			t.Errorf("landlockArgv(check) = %q, missing %q", joined, want)
		}
	}
}

// TestLandlockGrantsIncludesCapsExtraRO is a pure computation check (no
// Landlock kernel support needed): Caps.ExtraRO (#660's hook for a GitHub
// context dir sibling to the node's own WorkRoot) lands in the read-only
// grant set, never the read-write one.
func TestLandlockGrantsIncludesCapsExtraRO(t *testing.T) {
	dir := t.TempDir()
	ctxDir := t.TempDir()
	caps := Caps{WorkRoot: dir, ExtraRO: []string{ctxDir}}
	rw, ro := landlockGrants(dir, caps)
	found := false
	for _, p := range ro {
		if p == ctxDir {
			found = true
		}
	}
	if !found {
		t.Errorf("landlockGrants ro = %v, missing %q", ro, ctxDir)
	}
	for _, p := range rw {
		if p == ctxDir {
			t.Errorf("landlockGrants rw = %v, %q must be read-only, not read-write", rw, ctxDir)
		}
	}
}

// TestSandboxExecStampsTheEnvMarker pins the observability marker: a confined
// process is indistinguishable from a bare one in `ps` (syscall.Exec replaces the
// image) and kernels through 6.8 expose no Landlock field in /proc, so the marker
// is the only way to answer "is this confined?" from outside.
func TestSandboxExecStampsTheEnvMarker(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	caps := sandboxCaps(t, SandboxLandlock)
	caps.WorkRoot = dir

	res, err := RunArgv(context.Background(), dir, []string{"env"}, caps)
	if err != nil {
		t.Fatalf("env under landlock: %v (%q)", err, res.Output)
	}
	var line string
	for _, l := range strings.Split(res.Output, "\n") {
		if strings.HasPrefix(l, SandboxEnvMarker+"=") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatalf("no %s in the child's environment:\n%s", SandboxEnvMarker, res.Output)
	}
	if !strings.Contains(line, "landlock:abi3:rw") || !strings.Contains(line, ":ro") {
		t.Errorf("marker %q does not name the mode, ABI and grant counts", line)
	}
}
