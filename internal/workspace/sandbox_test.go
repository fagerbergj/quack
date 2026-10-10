package workspace

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireBwrap skips loudly with the reason: a sandbox test that silently no-ops is worse than none.
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

// TestSandboxBlocksReadsOutsideTheJail: even `sh -c` can't read outside its dir; the same command
// succeeding under none proves it's the OS boundary, not a path check.
func TestSandboxBlocksReadsOutsideTheJail(t *testing.T) {
	requireBwrap(t)

	// A "credential" outside the jail, like ~/.ssh/id_* or .env.
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

// TestSandboxAllowsWorkInsideTheJail: in-jail reads, real pipelines and the isolated HOME still work.
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

// TestSandboxMountsTheWorkRootAtOneFixedPath: WorkRoot appears only at SandboxWorkRoot, so `pwd`
// matches the model's view with no host prefix.
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

	// The write lands on the real host tree: the fixed path is a mount, not a copy.
	if res, err := RunArgv(context.Background(), repo, []string{"sh", "-c", "echo built > ../built.txt"}, caps); err != nil || res.ExitCode != 0 {
		t.Fatalf("write into the node's own workspace: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(work, "built.txt")); err != nil {
		t.Fatalf("the sandboxed write did not survive on the host: %v", err)
	}

	// The throwaway root is read-only, so a stray write fails loudly instead of silently evaporating.
	res, err = RunArgv(context.Background(), repo, []string{"sh", "-c", "echo x > /stray.txt"}, caps)
	if err != nil {
		t.Fatalf("stray write errored (want a clean non-zero exit): %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("a write to the sandbox root succeeded (%q) - it would vanish with the mount", res.Output)
	}
}

// TestSandboxToolchainAndHomeSurviveTheFixedMount: the fake toolchain only runs if exec_path reached
// the namespace, and prints the $HOME it got.
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

// TestSandboxBwrapLinkedWorktreeGitWorks: the parent clone lives outside work, so without the extra
// bind every git command in the worktree would fail.
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

// TestChildArgvBwrapGrantsExtraROReadOnly: Caps.ExtraRO lands as a read-only bwrap bind.
func TestChildArgvBwrapGrantsExtraROReadOnly(t *testing.T) {
	dir := t.TempDir()
	ctxDir := t.TempDir()
	caps := Caps{Sandbox: SandboxBwrap, ExtraRO: []string{ctxDir}}
	got := childArgv(dir, "/bin/echo", []string{"/bin/echo", "hi"}, caps)
	joined := strings.Join(got, "\x00")
	if !strings.Contains(joined, "--ro-bind-try\x00"+ctxDir+"\x00"+ctxDir) {
		t.Errorf("childArgv(bwrap) = %v, missing a read-only bind of %q", got, ctxDir)
	}
}

// TestResolveSandbox: an unknown mode or a missing bwrap refuses to start rather than run unconfined.
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

// TestLimitsApplyToChildren: RLIMIT_FSIZE has a visible, non-destructive effect, so it stands in for the set.
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

// TestSandboxJavaToolOptionsBoundsAddressSpace: the JVM ignores RLIMIT_AS in every mode,
// so the bound applies whenever AddressSpaceMB is set.
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

// TestSandboxJavaToolOptionsCombinesLandlockTmpdirAndMemoryBound: both land in one string, since a second
// JAVA_TOOL_OPTIONS entry replaces the first.
func TestSandboxJavaToolOptionsCombinesLandlockTmpdirAndMemoryBound(t *testing.T) {
	home := t.TempDir()
	caps := Caps{Sandbox: SandboxLandlock, Limits: Limits{AddressSpaceMB: 1024}, HomeDir: home}
	got := SandboxJavaToolOptions(caps)
	if !strings.HasPrefix(got, "-Djava.io.tmpdir="+SandboxTmpDir(caps)+" -Xmx") {
		t.Errorf("SandboxJavaToolOptions() = %q, want the tmpdir pin first and the memory bound appended", got)
	}
}

// TestSameDeviceHoldsForPathsOnTheSameFilesystem: the normal-path invariant -
// two dirs under the same parent are the same device.
func TestSameDeviceHoldsForPathsOnTheSameFilesystem(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.MkdirAll(a, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b, 0o700); err != nil {
		t.Fatal(err)
	}
	same, err := sameDevice(a, b)
	if err != nil {
		t.Fatalf("sameDevice(%q, %q) errored: %v", a, b, err)
	}
	if !same {
		t.Errorf("sameDevice(%q, %q) = false, want true: both under %q", a, b, root)
	}
}

// TestSameDeviceErrorsOnAMissingPath: can't verify what can't be stat'd -
// callers must treat an error as "unknown", not "different device".
func TestSameDeviceErrorsOnAMissingPath(t *testing.T) {
	if _, err := sameDevice(filepath.Join(t.TempDir(), "does-not-exist"), t.TempDir()); err == nil {
		t.Fatal("sameDevice on a missing path must error, not silently report a verdict")
	}
}

// TestHomeTmpDirRejectsACrossDeviceHomeDir drives sameDeviceHook directly: live stats would pass
// vacuously when both dirs share a device.
func TestHomeTmpDirRejectsACrossDeviceHomeDir(t *testing.T) {
	restore := sameDeviceHook
	sameDeviceHook = func(a, b string) (bool, error) { return false, nil }
	defer func() { sameDeviceHook = restore }()

	caps := Caps{HomeDir: t.TempDir(), WorkRoot: t.TempDir()}
	want := filepath.Join(caps.WorkRoot, workRootTmpDirName)
	if got := homeTmpDir(caps); got != want {
		t.Errorf("homeTmpDir() = %q, want the WorkRoot-derived %q when HOME/tmp is reported cross-device", got, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("workspace-derived scratch dir %q was not created: %v", want, err)
	}
}

// TestHomeTmpDirCrossDeviceReadOnlySkipsWorkRoot: a ReadOnly tree stays immutable, so the WorkRoot
// last resort is skipped.
func TestHomeTmpDirCrossDeviceReadOnlySkipsWorkRoot(t *testing.T) {
	restore := sameDeviceHook
	sameDeviceHook = func(a, b string) (bool, error) { return false, nil }
	defer func() { sameDeviceHook = restore }()

	caps := Caps{HomeDir: t.TempDir(), WorkRoot: t.TempDir(), ReadOnly: true}
	if got := homeTmpDir(caps); got != "" {
		t.Errorf("homeTmpDir() = %q, want \"\" for a ReadOnly node with cross-device HOME", got)
	}
}

// TestHomeTmpDirNoHomeFallsBackToWorkRoot: with no HomeDir, a WorkRoot still yields a scratch dir.
func TestHomeTmpDirNoHomeFallsBackToWorkRoot(t *testing.T) {
	caps := Caps{WorkRoot: t.TempDir()}
	want := filepath.Join(caps.WorkRoot, workRootTmpDirName)
	if got := homeTmpDir(caps); got != want {
		t.Errorf("homeTmpDir() = %q, want %q", got, want)
	}
}

// TestHomeTmpDirTrustsHomeDirWhenDeviceIsUnknown: with no WorkRoot to compare, HOME/tmp is trusted.
func TestHomeTmpDirTrustsHomeDirWhenDeviceIsUnknown(t *testing.T) {
	home := t.TempDir()
	got := homeTmpDir(Caps{HomeDir: home})
	want := filepath.Join(home, "tmp")
	if got != want {
		t.Errorf("homeTmpDir() = %q, want %q (WorkRoot unset, nothing to verify against)", got, want)
	}
}

// TestHomeTmpDirScratchDirIgnoresDeviceCheck: ScratchDir is already workspace-scoped, so it is never
// re-verified.
func TestHomeTmpDirScratchDirIgnoresDeviceCheck(t *testing.T) {
	restore := sameDeviceHook
	sameDeviceHook = func(a, b string) (bool, error) { return false, nil }
	defer func() { sameDeviceHook = restore }()

	scratch := filepath.Join(t.TempDir(), "scratch")
	caps := Caps{ScratchDir: scratch, WorkRoot: t.TempDir()}
	if got := homeTmpDir(caps); got != scratch {
		t.Errorf("homeTmpDir() = %q, want %q: ScratchDir resolving normally must be unaffected by the device check", got, scratch)
	}
}

// TestLandlockTmpDirWarnsBeforeFallingBackToSharedTmp: the os.TempDir() fallback must warn, since a
// cross-device TMPDIR fails confusingly much later.
func TestLandlockTmpDirWarnsBeforeFallingBackToSharedTmp(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	got := landlockTmpDir(Caps{})
	slog.SetDefault(restore)

	if got != os.TempDir() {
		t.Errorf("landlockTmpDir() = %q, want os.TempDir() (%q) with nothing else configured", got, os.TempDir())
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a WARN log for the silent-fallback case, got: %s", out)
	}
}

// TestResolveLandlockSelfExePrefersSidecar: self when the sidecar is absent or not a regular file.
func TestResolveLandlockSelfExePrefersSidecar(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "quack")
	if err := os.WriteFile(self, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := resolveLandlockSelfExe(self); got != self {
		t.Errorf("resolveLandlockSelfExe() = %q, want self %q with no sidecar installed", got, self)
	}

	sidecar := filepath.Join(dir, "quack-sandbox")
	if err := os.WriteFile(sidecar, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveLandlockSelfExe(self); got != sidecar {
		t.Errorf("resolveLandlockSelfExe() = %q, want sidecar %q once it exists beside self", got, sidecar)
	}

	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveLandlockSelfExe(self); got != self {
		t.Errorf("resolveLandlockSelfExe() = %q, want self %q when the sidecar path is a directory, not a file", got, self)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}

	// A non-executable sidecar must not be chosen: it would turn every
	// sandboxed exec into a confusing "permission denied".
	if err := os.WriteFile(sidecar, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveLandlockSelfExe(self); got != self {
		t.Errorf("resolveLandlockSelfExe() = %q, want self %q for a non-executable sidecar", got, self)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}

	// A symlink must not be trusted either - it could point anywhere.
	if err := os.Symlink(self, sidecar); err != nil {
		t.Fatal(err)
	}
	if got := resolveLandlockSelfExe(self); got != self {
		t.Errorf("resolveLandlockSelfExe() = %q, want self %q for a symlinked sidecar", got, self)
	}
}
