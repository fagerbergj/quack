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

// requireBwrap skips a test when bubblewrap isn't usable on this host — LOUDLY
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
// child process — even one the metachar wall happily allows, and even `sh -c` —
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

// TestSandboxBlocksWritesOutsideTheJail: read containment is half of it — a
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
