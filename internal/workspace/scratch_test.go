package workspace

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHomeTmpDirPrefersScratchDir: every sandboxed tmp resolves through homeTmpDir, which prefers
// ScratchDir over HomeDir/tmp and creates it.
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

// TestHomeTmpDirFallsBackWithoutScratchDir: a caller without ScratchDir keeps the shared HomeDir/tmp.
func TestHomeTmpDirFallsBackWithoutScratchDir(t *testing.T) {
	home := t.TempDir()
	caps := Caps{HomeDir: home}
	got := homeTmpDir(caps)
	want := filepath.Join(home, "tmp")
	if got != want {
		t.Errorf("homeTmpDir(no ScratchDir) = %q, want the shared %q", got, want)
	}
}

// TestSandboxTmpDirReflectsScratchDirUnderLandlock: the ACP TMPDIR is ScratchDir once a caller scopes one.
func TestSandboxTmpDirReflectsScratchDirUnderLandlock(t *testing.T) {
	home := t.TempDir()
	scratch := t.TempDir()
	caps := Caps{Sandbox: SandboxLandlock, HomeDir: home, ScratchDir: scratch}
	if got := SandboxTmpDir(caps); got != scratch {
		t.Errorf("SandboxTmpDir = %q, want caps.ScratchDir %q", got, scratch)
	}
}

// TestLandlockGrantsIncludesScratchDirRW: scratch is RW even for ReadOnly; a reviewer needs TMPDIR too.
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

// runWrapArgv executes through WrapArgv, the real seam the ACP child uses, not the raw shim.
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

// TestWrapArgvRWWorkerWritesOwnNodeDirEndToEnd: an RW worker's write inside its own node dir succeeds
// through WrapArgv under landlock.
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

// TestWrapArgvScratchDirWritableForBothWorkerClasses: ScratchDir is writable for reviewers and implementers,
// so a denied tree still leaves somewhere for temp files.
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

// TestWrapArgvTmpdirEnvPointsAtScratchDir: TMPDIR must name the same dir the sandbox grants.
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

// TestWrapArgvScratchDirGrantedUnderBwrap: under bwrap the scratch dir is bound RW at its identity path,
// the same path SandboxTmpDir names.
func TestWrapArgvScratchDirGrantedUnderBwrap(t *testing.T) {
	dir := t.TempDir()
	scratch := t.TempDir()
	caps := Caps{Sandbox: SandboxBwrap, ScratchDir: scratch, HomeDir: t.TempDir()}
	if got := SandboxTmpDir(caps); got != scratch {
		t.Errorf("SandboxTmpDir(bwrap) = %q, want the scratch dir %q", got, scratch)
	}
	got := WrapArgv(dir, []string{"pi-acp", "run"}, caps, nil, nil)
	if !strings.Contains(strings.Join(got, "\x00"), "--bind-try\x00"+scratch+"\x00"+scratch) {
		t.Errorf("WrapArgv(bwrap) = %v, missing the scratch dir RW bind %q", got, scratch)
	}
}

// TestWrapArgvScratchDirUnwrappedUnderNone: none grants nothing; TMPDIR names the process tmp dir.
func TestWrapArgvScratchDirUnwrappedUnderNone(t *testing.T) {
	dir := t.TempDir()
	scratch := t.TempDir()
	for _, mode := range []SandboxMode{SandboxNone, ""} {
		caps := Caps{Sandbox: mode, ScratchDir: scratch}
		got := unreaped(WrapArgv(dir, []string{"pi-acp", "run"}, caps, nil, nil))
		if len(got) != 2 || got[0] != "pi-acp" || got[1] != "run" {
			t.Errorf("mode %q: WrapArgv = %v, want argv unchanged", mode, got)
		}
		if strings.Contains(strings.Join(got, " "), scratch) {
			t.Errorf("mode %q: WrapArgv leaked the scratch path into argv", mode)
		}
	}
}

// TestLandlockTmpDirWorkRootFallbackIsSilent: a WorkRoot-derived dir emits no shared-/tmp warning.
func TestLandlockTmpDirWorkRootFallbackIsSilent(t *testing.T) {
	var buf strings.Builder
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	workRoot := t.TempDir()
	got := landlockTmpDir(Caps{WorkRoot: workRoot})
	slog.SetDefault(restore)

	if want := filepath.Join(workRoot, workRootTmpDirName); got != want {
		t.Errorf("landlockTmpDir() = %q, want %q", got, want)
	}
	if strings.Contains(buf.String(), "no workspace-scoped scratch dir available") {
		t.Errorf("boot warning fired despite a WorkRoot-derived scratch dir: %s", buf.String())
	}
}
