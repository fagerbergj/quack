package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireBwrap skips a test when bubblewrap isn't usable on this host - LOUDLY
// (with the reason), never by quietly passing: a sandbox test that silently
// no-ops is worse than no test.
func requireBwrap(t *testing.T) {
	t.Helper()
	if err := probeBwrap(); err != nil {
		t.Skipf("SKIPPING sandbox test: bubblewrap is not usable here (%v). Install it (Debian: apt-get install bubblewrap) to exercise the OS boundary.", err)
	}
}

// sandboxCaps is the caps a jailed child gets in these tests: an isolated HOME
// under the jail (as internal/serve wires it) and the given sandbox mode.
func sandboxCaps(t *testing.T, mode SandboxMode) Caps {
	t.Helper()
	c := DefaultCaps()
	c.Sandbox = mode
	c.HomeDir = t.TempDir()
	c.Timeout = 60 * time.Second
	return c
}

// TestSandboxBlocksReadsOutsideTheJail is the whole point of the sandbox: a
// child process - even one the metachar wall happily allows, and even `sh -c` -
// cannot READ a file outside its working directory. The same command with
// sandbox: none succeeds, which is what proves this test exercises the OS
// boundary rather than some path check.
func TestSandboxBlocksReadsOutsideTheJail(t *testing.T) {
	requireBwrap(t)

	// A "credential" outside the jail: the exact class of file (~/.ssh/id_*,
	// ~/.aws/credentials, .env) a child could read before this existed.
	outside := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(outside, []byte("PRIVATE-KEY-MATERIAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	for _, argv := range [][]string{
		{"cat", outside},
		{"sh", "-c", "cat " + outside}, // the interpreter the metachar wall never blocked
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			sandboxed, err := RunArgv(context.Background(), dir, argv, sandboxCaps(t, SandboxBwrap))
			if err != nil {
				t.Fatalf("sandboxed run errored (want a clean non-zero exit): %v", err)
			}
			if sandboxed.ExitCode == 0 || strings.Contains(sandboxed.Output, "PRIVATE-KEY-MATERIAL") {
				t.Fatalf("SANDBOX ESCAPE: %v read a file outside the jail: exit=%d output=%q",
					argv, sandboxed.ExitCode, sandboxed.Output)
			}

			// Control: with the sandbox off the SAME command reads the file. If
			// this ever stops passing, the assertion above proves nothing.
			plain, err := RunArgv(context.Background(), dir, argv, sandboxCaps(t, SandboxNone))
			if err != nil {
				t.Fatalf("unsandboxed control run errored: %v", err)
			}
			if plain.ExitCode != 0 || !strings.Contains(plain.Output, "PRIVATE-KEY-MATERIAL") {
				t.Fatalf("control run should have read the file (the test would otherwise prove nothing): exit=%d output=%q",
					plain.ExitCode, plain.Output)
			}
		})
	}
}

// TestSandboxAllowsWorkInsideTheJail: the boundary must not break the job. A
// normal in-jail command reads its own working directory, a pipeline still
// chains real processes across it, and the child's isolated HOME (where npm/go
// caches land) stays writable.
func TestSandboxAllowsWorkInsideTheJail(t *testing.T) {
	requireBwrap(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	caps := sandboxCaps(t, SandboxBwrap)

	res, err := RunArgv(context.Background(), dir, []string{"cat", "hello.txt"}, caps)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "beta") {
		t.Fatalf("in-jail read failed: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
	}

	stages, err := SplitPipeline("cat hello.txt | grep beta")
	if err != nil {
		t.Fatal(err)
	}
	res, err = RunPipeline(context.Background(), dir, stages, caps)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "beta") {
		t.Fatalf("in-jail pipeline failed: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
	}
	if strings.Contains(res.Output, "alpha") {
		t.Fatalf("pipeline did not actually filter: %q", res.Output)
	}

	res, err = RunArgv(context.Background(), dir, []string{"touch", filepath.Join(caps.HomeDir, "cache-probe")}, caps)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("child could not write its own HOME: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(caps.HomeDir, "cache-probe")); err != nil {
		t.Fatalf("HOME write did not land on the host: %v", err)
	}
}

// TestSandboxMountsTheWorkRootAtOneFixedPath: the child's view of its own
// workspace must be the MODEL's view of it. Caps.WorkRoot (the node's own
// directory - the invisible root every fs tool resolves against) appears inside
// the namespace at SandboxWorkRoot and nowhere else, so `pwd` in a subdirectory
// prints the same trailing path the tools use, with no host prefix at all.
//
// Before this, the child was chdir'd to the HOST path, so `pwd` printed the
// server's workspace root, the chat id and the node id - and the model, handed a
// second name for the one place it already knew as "/quack", went looking for
// its workspace on the host filesystem (see internal/tools/sandbox_namespace_test.go).
func TestSandboxMountsTheWorkRootAtOneFixedPath(t *testing.T) {
	requireBwrap(t)

	work := t.TempDir() // stands in for <root>/<user>/<chat>/<node>/
	repo := filepath.Join(work, "quack")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	caps := sandboxCaps(t, SandboxBwrap)
	caps.WorkRoot = work

	res, err := RunArgv(context.Background(), repo, []string{"pwd"}, caps)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("pwd: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
	}
	got := strings.TrimSpace(res.Output)
	if want := SandboxWorkRoot + "/quack"; got != want {
		t.Errorf("pwd = %q, want %q - the child must see the workspace at the fixed mount, not on the host", got, want)
	}
	if strings.Contains(got, work) {
		t.Errorf("pwd = %q leaks the host path %q into the model's context", got, work)
	}

	// The write still lands on the real host tree (the fixed path is a MOUNT, not
	// a copy) - the guarantee #214 exists for.
	if res, err := RunArgv(context.Background(), repo, []string{"sh", "-c", "echo built > ../built.txt"}, caps); err != nil || res.ExitCode != 0 {
		t.Fatalf("write into the node's own workspace: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(work, "built.txt")); err != nil {
		t.Fatalf("the sandboxed write did not survive on the host: %v", err)
	}

	// A stray write to the sandbox's own root can no longer EVAPORATE silently:
	// the throwaway root is read-only, so it fails loudly instead of landing in a
	// mount that disappears when the command exits.
	res, err = RunArgv(context.Background(), repo, []string{"sh", "-c", "echo x > /stray.txt"}, caps)
	if err != nil {
		t.Fatalf("stray write errored (want a clean non-zero exit): %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("a write to the sandbox root succeeded (%q) - it would vanish with the mount", res.Output)
	}
}

// TestSandboxToolchainAndHomeSurviveTheFixedMount: remapping the WORKSPACE must not
// disturb the other two things a real build needs - a toolchain from exec_path
// (bound read-only at its OWN host path, because RunArgv resolves argv[0] on the
// host and hands the child that absolute path) and the isolated $HOME the caches
// land in (npm's _cacache, GOCACHE, ~/.gitconfig).
//
// The "toolchain" here is a script in a temp dir on PATH, which is exactly what
// workspace.exec_path binds (nvm's node bin, an asdf shim dir) minus any dependence
// on what happens to be installed on the test host. It answers both questions at
// once: it only runs at all if exec_path reached inside the namespace, and what it
// prints is the $HOME the child actually got.
func TestSandboxToolchainAndHomeSurviveTheFixedMount(t *testing.T) {
	requireBwrap(t)

	toolBin := t.TempDir()
	script := "#!/bin/sh\necho \"$HOME\"\nmkdir -p \"$HOME/.cache\" && echo cached > \"$HOME/.cache/probe\"\n"
	if err := os.WriteFile(filepath.Join(toolBin, "faketool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolBin+string(os.PathListSeparator)+os.Getenv("PATH")) // RunArgv resolves argv[0] on the ambient PATH

	work := t.TempDir()
	caps := sandboxCaps(t, SandboxBwrap)
	caps.WorkRoot = work
	caps.ExtraPath = []string{toolBin} // as workspace.exec_path would

	res, err := RunArgv(context.Background(), work, []string{"faketool"}, caps)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("an exec_path toolchain could not run inside the sandbox: err=%v exit=%d output=%q",
			err, res.ExitCode, res.Output)
	}
	if got := strings.TrimSpace(res.Output); got != caps.HomeDir {
		t.Errorf("child HOME = %q, want the isolated home %q - a toolchain's cache must not land in the workspace",
			got, caps.HomeDir)
	}
	if _, err := os.Stat(filepath.Join(caps.HomeDir, ".cache", "probe")); err != nil {
		t.Fatalf("the toolchain's cache write did not survive in the isolated HOME: %v", err)
	}
}

// TestSandboxBwrapLinkedWorktreeGitWorks is the bwrap-mode counterpart of
// TestSandboxLandlockLinkedWorktreeGitWorks: a linked git worktree's parent
// clone lives OUTSIDE work entirely (a sibling under the same chat scope, see
// dag.worktreeParentID), so without the extra bind (worktreeCommonGitDirs,
// wired into childArgv's bwrap branch) it would simply be absent from the
// child's mount namespace and every git command inside would fail.
func TestSandboxBwrapLinkedWorktreeGitWorks(t *testing.T) {
	requireBwrap(t)
	worktreeDir := setupLinkedWorktreeFixture(t)

	caps := sandboxCaps(t, SandboxBwrap)
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
		t.Fatalf("git status inside a bwrap-wrapped worktree: exit=%d %q", res.ExitCode, res.Output)
	}
	if res := run("git", "log", "-1", "--format=%H"); res.ExitCode != 0 {
		t.Fatalf("git log inside a bwrap-wrapped worktree: exit=%d %q", res.ExitCode, res.Output)
	} else if strings.TrimSpace(res.Output) == "" {
		t.Fatal("git log returned no output - the worktree can't see its own history")
	}
}

// TestSandboxBlocksWritesOutsideTheJail: read containment is half of it - a
// child must not be able to WRITE outside its working dir either.
func TestSandboxBlocksWritesOutsideTheJail(t *testing.T) {
	requireBwrap(t)

	outside := filepath.Join(t.TempDir(), "clobbered")
	if err := os.WriteFile(outside, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RunArgv(context.Background(), t.TempDir(),
		[]string{"sh", "-c", "echo pwned > " + outside}, sandboxCaps(t, SandboxBwrap))
	if err != nil {
		t.Fatalf("run errored: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("SANDBOX ESCAPE: a child wrote outside the jail (exit 0): %q", res.Output)
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "original" {
		t.Fatalf("a file outside the jail was modified: %q (err=%v)", got, err)
	}
}

// TestResolveSandbox covers the loud-failure contract: an unknown mode is a
// startup error, and `bwrap` with no bwrap on PATH refuses to start rather than
// silently running children unconfined.
func TestResolveSandbox(t *testing.T) {
	if _, err := ResolveSandbox("chroot"); err == nil {
		t.Fatal("an unknown sandbox mode must be a startup error")
	}
	if mode, err := ResolveSandbox(SandboxNone); err != nil || mode != SandboxNone {
		t.Fatalf("ResolveSandbox(none) = %q, %v; want none, nil (with a WARN)", mode, err)
	}

	t.Setenv("PATH", t.TempDir()) // no bwrap anywhere on it
	_, err := ResolveSandbox(SandboxBwrap)
	if err == nil {
		t.Fatal("sandbox: bwrap with no bwrap installed must REFUSE TO START, not fall back")
	}
	for _, want := range []string{"bubblewrap", "workspace.sandbox: none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must tell the operator what to do; %q missing from: %v", want, err)
		}
	}
}

// TestLimitsApplyToChildren proves the rlimits reach the child: RLIMIT_FSIZE is
// the one with a visible, non-destructive effect (a write past the limit kills
// the writer), so it stands in for the set.
func TestLimitsApplyToChildren(t *testing.T) {
	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Skipf("SKIPPING rlimit test: prlimit(1) not installed (%v)", err)
	}
	caps := DefaultCaps()
	caps.Limits = Limits{FileSizeMB: 1}
	res, err := RunArgv(context.Background(), t.TempDir(),
		[]string{"dd", "if=/dev/zero", "of=big", "bs=1M", "count=8"}, caps)
	if err != nil {
		t.Fatalf("run errored: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("RLIMIT_FSIZE did not reach the child: an 8MB write succeeded under a 1MB limit: %q", res.Output)
	}
}

// TestSandboxJavaToolOptionsBoundsAddressSpace: the JVM memory bound (#647)
// applies whenever AddressSpaceMB is set, in EVERY sandbox mode - unlike the
// tmpdir pin (landlock-only), a JVM's ergonomics ignore RLIMIT_AS regardless
// of which OS boundary wraps it.
func TestSandboxJavaToolOptionsBoundsAddressSpace(t *testing.T) {
	for _, mode := range []SandboxMode{SandboxNone, SandboxBwrap, SandboxLandlock} {
		caps := Caps{Sandbox: mode, Limits: Limits{AddressSpaceMB: 8192}, HomeDir: t.TempDir()}
		got := SandboxJavaToolOptions(caps)
		for _, want := range []string{
			"-Xmx2867m", "-XX:MaxMetaspaceSize=819m",
			"-XX:CompressedClassSpaceSize=655m", "-XX:ReservedCodeCacheSize=655m",
			"-XX:ActiveProcessorCount=4",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("mode %q: SandboxJavaToolOptions() = %q, want it to contain %q", mode, got, want)
			}
		}
	}
}

// TestSandboxJavaToolOptionsNoLimitIsEmpty: AddressSpaceMB unset must not grow
// JAVA_TOOL_OPTIONS at all outside landlock (whose own tmpdir pin still fires).
func TestSandboxJavaToolOptionsNoLimitIsEmpty(t *testing.T) {
	if got := SandboxJavaToolOptions(Caps{Sandbox: SandboxBwrap}); got != "" {
		t.Errorf("SandboxJavaToolOptions() = %q, want empty with no AddressSpaceMB set", got)
	}
}

// TestSandboxJavaToolOptionsCombinesLandlockTmpdirAndMemoryBound: under
// landlock with a limit set, both concerns must land in the SAME string - a
// second JAVA_TOOL_OPTIONS entry would replace the first rather than merge
// (the JVM honours only the last occurrence in envp), so childEnv/spawnEnv can
// only ever append one.
func TestSandboxJavaToolOptionsCombinesLandlockTmpdirAndMemoryBound(t *testing.T) {
	home := t.TempDir()
	caps := Caps{Sandbox: SandboxLandlock, Limits: Limits{AddressSpaceMB: 1024}, HomeDir: home}
	got := SandboxJavaToolOptions(caps)
	if !strings.HasPrefix(got, "-Djava.io.tmpdir="+SandboxTmpDir(caps)+" -Xmx") {
		t.Errorf("SandboxJavaToolOptions() = %q, want the tmpdir pin first and the memory bound appended", got)
	}
}
