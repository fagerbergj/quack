package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHomeTmpDirPrefersScratchDir is the choke-point pin: homeTmpDir (and
// therefore SandboxTmpDir/landlockTmpDir/tmpArgs, every caller that resolves
// a sandboxed child's tmp) prefers caps.ScratchDir over the shared
// caps.HomeDir/tmp when both are set, and creates it on demand.
func TestHomeTmpDirPrefersScratchDir(t *testing.T) {
	home := t.TempDir()
	scratch := filepath.Join(t.TempDir(), "not-yet-created")
	caps := Caps{HomeDir: home, ScratchDir: scratch}

	got := homeTmpDir(caps)
	if got != scratch {
		t.Errorf("homeTmpDir = %q, want caps.ScratchDir %q", got, scratch)
	}
	if info, err := os.Stat(scratch); err != nil || !info.IsDir() {
		t.Errorf("homeTmpDir did not create %q: %v", scratch, err)
	}
}

// TestHomeTmpDirFallsBackWithoutScratchDir pins the OTHER half: a caller that
// never wires ScratchDir (the gate's own one-shot check commands until #.,
// or any caller pre-dating this fix) keeps the pre-fix shared HomeDir/tmp -
// this fix must not regress a caller that hasn't opted in.
func TestHomeTmpDirFallsBackWithoutScratchDir(t *testing.T) {
	home := t.TempDir()
	caps := Caps{HomeDir: home}
	got := homeTmpDir(caps)
	want := filepath.Join(home, "tmp")
	if got != want {
		t.Errorf("homeTmpDir(no ScratchDir) = %q, want the shared %q", got, want)
	}
}

// TestSandboxTmpDirReflectsScratchDirUnderLandlock: SandboxTmpDir (the ACP
// TMPDIR seam, workspace.SandboxTmpDir) returns caps.ScratchDir under
// landlock, not the shared HomeDir/tmp, once a caller scopes one.
func TestSandboxTmpDirReflectsScratchDirUnderLandlock(t *testing.T) {
	home := t.TempDir()
	scratch := t.TempDir()
	caps := Caps{Sandbox: SandboxLandlock, HomeDir: home, ScratchDir: scratch}
	if got := SandboxTmpDir(caps); got != scratch {
		t.Errorf("SandboxTmpDir = %q, want caps.ScratchDir %q", got, scratch)
	}
}

// TestLandlockGrantsIncludesScratchDirRW: the per-node scratch dir lands in
// the RW grant set regardless of caps.ReadOnly - a read-only reviewer needs
// TMPDIR/mktemp/heredocs to work exactly as much as a writer does; only the
// node's OWN tree differs by ReadOnly.
func TestLandlockGrantsIncludesScratchDirRW(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		dir := t.TempDir()
		scratch := t.TempDir()
		caps := Caps{WorkRoot: dir, HomeDir: t.TempDir(), ScratchDir: scratch, ReadOnly: readOnly}
		rw, _ := landlockGrants(dir, caps)
		found := false
		for _, p := range rw {
			if p == scratch {
				found = true
			}
		}
		if !found {
			t.Errorf("readOnly=%v: landlockGrants rw = %v, missing the scratch dir %q", readOnly, rw, scratch)
		}
	}
}

// runWrapArgv builds argv the SAME way internal/acp's wrappedArgv does
// (workspace.WrapArgv) and actually executes it, mirroring landlock_test.go's
// runSandboxExec but going through the real production seam instead of the
// raw shim - this is the seam a caps bug in the ACP write path would surface
// through, not just an argv-assembly check.
func runWrapArgv(t *testing.T, dir string, argv []string, caps Caps) (string, int) {
	t.Helper()
	wrapped := WrapArgv(dir, argv, caps, nil, nil)
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return string(out), code
}

// TestWrapArgvRWWorkerWritesOwnNodeDirEndToEnd is test case 2 from the
// sandbox-scratch fix: does the RW implementer's own worktree actually
// accept a write through the REAL seam its subprocess runs through
// (WrapArgv), or was the live "permission denied … in the workspace root"
// report actually a caps bug rather than the worker reaching outside its
// own node dir? This proves it is NOT a caps bug - a write inside the node's
// own directory succeeds end to end under landlock.
func TestWrapArgvRWWorkerWritesOwnNodeDirEndToEnd(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	caps := Caps{Sandbox: SandboxLandlock, WorkRoot: dir, HomeDir: t.TempDir()}

	out, code := runWrapArgv(t, dir, []string{"sh", "-c", "echo built > f"}, caps)
	if code != 0 {
		t.Fatalf("RW worker could not write its own node dir through WrapArgv: exit=%d output=%q", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "f")); err != nil {
		t.Fatalf("the write did not land on disk: %v", err)
	}
}

// TestWrapArgvScratchDirWritableForBothWorkerClasses is test case 1 end to
// end, through the real WrapArgv seam: a per-node ScratchDir is writable for
// BOTH a read-only reviewer and an RW implementer, so a worker whose own
// tree is (correctly) denied still has somewhere to put a temp file instead
// of churning on write-denials with nowhere to fall back to.
func TestWrapArgvScratchDirWritableForBothWorkerClasses(t *testing.T) {
	requireLandlock(t)
	for _, tc := range []struct {
		name     string
		readOnly bool
	}{{"rw_implementer", false}, {"ro_reviewer", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scratch := t.TempDir()
			caps := Caps{Sandbox: SandboxLandlock, WorkRoot: dir, HomeDir: t.TempDir(), ScratchDir: scratch, ReadOnly: tc.readOnly}

			out, code := runWrapArgv(t, dir, []string{"sh", "-c", "echo x > " + filepath.Join(scratch, "probe")}, caps)
			if code != 0 {
				t.Fatalf("scratch write denied for %s: exit=%d output=%q", tc.name, code, out)
			}
			if _, err := os.Stat(filepath.Join(scratch, "probe")); err != nil {
				t.Fatalf("scratch write did not land for %s: %v", tc.name, err)
			}

			out, code = runWrapArgv(t, dir, []string{"sh", "-c", "echo x > f"}, caps)
			if tc.readOnly {
				if code == 0 {
					t.Fatalf("SANDBOX GAP: a read_only worker wrote to its own tree (exit 0): %q", out)
				}
				if _, err := os.Stat(filepath.Join(dir, "f")); err == nil {
					t.Fatal("the blocked write landed on disk despite the non-zero exit")
				}
			} else if code != 0 {
				t.Fatalf("RW worker could not write its own tree: exit=%d output=%q", code, out)
			}
		})
	}
}

// TestWrapArgvTmpdirEnvPointsAtScratchDir is test case 4: TMPDIR (the env var
// internal/acp's spawnEnv sets from workspace.SandboxTmpDir) both names AND
// grants the same scratch dir - a mismatch between the two would leave TMPDIR
// pointing somewhere the sandbox denies.
func TestWrapArgvTmpdirEnvPointsAtScratchDir(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	scratch := t.TempDir()
	caps := Caps{Sandbox: SandboxLandlock, WorkRoot: dir, HomeDir: t.TempDir(), ScratchDir: scratch}

	tmpdir := SandboxTmpDir(caps)
	if tmpdir != scratch {
		t.Fatalf("SandboxTmpDir = %q, want caps.ScratchDir %q", tmpdir, scratch)
	}

	wrapped := WrapArgv(dir, []string{"sh", "-c", "echo x > \"$TMPDIR/probe\""}, caps, nil, nil)
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TMPDIR="+tmpdir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("write via $TMPDIR failed: %v: %s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(scratch, "probe")); statErr != nil {
		t.Fatalf("$TMPDIR write did not land in the granted scratch dir: %v", statErr)
	}
}

// TestWrapArgvScratchDirUnwrappedOutsideLandlock: mode none/bwrap pass argv
// through unchanged (the documented ACP ceiling - see WrapArgv's own doc), so
// ScratchDir grants nothing extra there; TMPDIR still names it via the env,
// same as any other caller of SandboxTmpDir.
func TestWrapArgvScratchDirUnwrappedOutsideLandlock(t *testing.T) {
	dir := t.TempDir()
	scratch := t.TempDir()
	for _, mode := range []SandboxMode{SandboxNone, SandboxBwrap, ""} {
		caps := Caps{Sandbox: mode, ScratchDir: scratch}
		got := WrapArgv(dir, []string{"opencode", "acp"}, caps, nil, nil)
		if len(got) != 2 || got[0] != "opencode" || got[1] != "acp" {
			t.Errorf("mode %q: WrapArgv = %v, want argv unchanged", mode, got)
		}
		if strings.Contains(strings.Join(got, " "), scratch) {
			t.Errorf("mode %q: WrapArgv leaked the scratch path into argv", mode)
		}
	}
}
