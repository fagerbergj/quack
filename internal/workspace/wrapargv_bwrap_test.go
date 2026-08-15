package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// acpCaps is the caps shape internal/acp hands WrapArgv for a real round: an
// isolated jail HOME, a per-node scratch dir under it, and the node's own work
// tree elsewhere in the jail (Jail.HomeDir vs Jail.EnsureDir - siblings).
func acpCaps(t *testing.T, mode SandboxMode, dir string, readOnly bool) Caps {
	t.Helper()
	home := t.TempDir()
	return Caps{
		Sandbox:    mode,
		WorkRoot:   dir,
		HomeDir:    home,
		ScratchDir: filepath.Join(home, "tmp", "chat__node"),
		ReadOnly:   readOnly,
	}
}

// TestWrapArgvBwrapEnforcesGrantsEndToEnd (#921) is the whole point of wrapping
// the ACP child under bwrap: the grants are enforced by the OS, not asserted by
// an argv shape. A read_only node's own work tree rejects a write, its $HOME and
// $TMPDIR accept one, and a file outside every grant is not even readable.
func TestWrapArgvBwrapEnforcesGrantsEndToEnd(t *testing.T) {
	requireBwrap(t)
	// A credential outside every grant - the class of file (~/.ssh/id_*,
	// ~/.aws/credentials, .env) an unwrapped ACP child could read before this.
	secret := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(secret, []byte("PRIVATE-KEY-MATERIAL"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, readOnly := range []bool{true, false} {
		name := "writer"
		if readOnly {
			name = "read_only"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			caps := acpCaps(t, SandboxBwrap, dir, readOnly)

			out, code := runWrapArgv(t, dir, []string{"sh", "-c", "echo x > f"}, caps)
			_, onDisk := os.Stat(filepath.Join(dir, "f"))
			if readOnly {
				if code == 0 {
					t.Errorf("SANDBOX GAP: a read_only ACP child wrote its own work tree (exit 0): %q", out)
				}
				if onDisk == nil {
					t.Error("the blocked write landed on disk despite the non-zero exit")
				}
			} else {
				if code != 0 {
					t.Errorf("a writer ACP child could not write its own work tree: exit=%d output=%q", code, out)
				}
				if onDisk != nil {
					t.Errorf("the write did not land on disk: %v", onDisk)
				}
			}

			// $HOME and $TMPDIR stay writable for BOTH classes - the half
			// acp.allow_clone rests on (it clones into $TMPDIR).
			tmpdir := SandboxTmpDir(caps)
			if tmpdir != caps.ScratchDir {
				t.Fatalf("SandboxTmpDir = %q, want the scratch dir %q", tmpdir, caps.ScratchDir)
			}
			for _, probe := range []string{filepath.Join(caps.HomeDir, "h"), filepath.Join(tmpdir, "t")} {
				out, code := runWrapArgv(t, dir, []string{"sh", "-c", "echo x > " + probe}, caps)
				if code != 0 {
					t.Errorf("write to %q denied: exit=%d output=%q", probe, code, out)
				}
				if _, err := os.Stat(probe); err != nil {
					t.Errorf("write to %q did not land on disk: %v", probe, err)
				}
			}

			out, code = runWrapArgv(t, dir, []string{"sh", "-c", "cat " + secret}, caps)
			if code == 0 || strings.Contains(out, "PRIVATE-KEY-MATERIAL") {
				t.Errorf("SANDBOX ESCAPE: the ACP child read a file outside every grant: exit=%d output=%q", code, out)
			}
		})
	}
}

// TestWrapArgvBwrapMostSpecificGrantWins pins the bind ORDER invariant: bwrap
// applies binds in argv order and a later mount on a subpath overlays the
// earlier one, so a read-only work tree nested inside a writable HOME is only
// read-only if the deeper bind comes last. Landlock's rule union has no such
// ordering, which is exactly why it needs its own test.
func TestWrapArgvBwrapMostSpecificGrantWins(t *testing.T) {
	requireBwrap(t)
	home := t.TempDir()
	dir := filepath.Join(home, "node")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	caps := Caps{Sandbox: SandboxBwrap, WorkRoot: dir, HomeDir: home, ReadOnly: true}

	out, code := runWrapArgv(t, dir, []string{"sh", "-c", "echo x > f"}, caps)
	if code == 0 {
		t.Errorf("SANDBOX GAP: the writable HOME bind overlaid the read-only work tree (exit 0): %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "f")); err == nil {
		t.Error("the blocked write landed on disk despite the non-zero exit")
	}
	if out, code := runWrapArgv(t, dir, []string{"sh", "-c", "echo x > " + filepath.Join(home, "h")}, caps); code != 0 {
		t.Errorf("HOME itself must stay writable: exit=%d output=%q", code, out)
	}
}

// TestWrapArgvBwrapShape is the argv-assembly half (no bwrap install needed):
// bwrap leads, the work tree is bound at its IDENTITY path (never childArgv's
// SandboxWorkRoot remap - the ACP child trades absolute paths with quack over
// JSON-RPC), read-only per caps.ReadOnly, and the command survives past "--"
// with no rlimit ceiling (#798).
func TestWrapArgvBwrapShape(t *testing.T) {
	dir := t.TempDir()
	argv := []string{"opencode", "acp"}
	caps := acpCaps(t, SandboxBwrap, dir, true)
	caps.Limits = Limits{AddressSpaceMB: 8192, FileSizeMB: 1024}

	got := WrapArgv(dir, argv, caps, []string{"/skills"}, []string{"/extra-rw"})
	if filepath.Base(got[0]) != bwrapBinary {
		t.Fatalf("WrapArgv(bwrap)[0] = %q, want the bwrap binary", got[0])
	}
	joined := strings.Join(got, "\x00")
	for _, want := range []string{
		"--ro-bind-try\x00" + dir + "\x00" + dir,
		"--bind-try\x00" + caps.HomeDir + "\x00" + caps.HomeDir,
		"--ro-bind-try\x00/skills\x00/skills",
		"--bind-try\x00/extra-rw\x00/extra-rw",
		"--chdir\x00" + dir,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("WrapArgv(bwrap) missing %q\ngot: %v", strings.ReplaceAll(want, "\x00", " "), got)
		}
	}
	if strings.Contains(joined, SandboxWorkRoot) {
		t.Errorf("WrapArgv(bwrap) remapped the work tree onto %s; ACP paths must stay identity\ngot: %v", SandboxWorkRoot, got)
	}
	if strings.Contains(joined, "--bind-try\x00"+dir) {
		t.Errorf("WrapArgv(bwrap, ReadOnly) also bound the work tree read-write: %v", got)
	}
	for _, bad := range []string{"prlimit", "--as=", "--fsize="} {
		if strings.Contains(joined, bad) {
			t.Errorf("WrapArgv(bwrap) carries %q; the agent subprocess must run with no rlimit ceiling (#798): %v", bad, got)
		}
	}
	if !strings.HasSuffix(strings.Join(got, " "), "-- opencode acp") {
		t.Errorf("WrapArgv(bwrap) dropped the command: %v", got)
	}
}

// TestWrapArgvBwrapKeepsOwnMounts: a grant that names a path bwrapSystemArgs
// (or tmpArgs) already mounts must NOT be bound over it - re-binding the host's
// /proc under --unshare-pid would leak the real PID table back in, and the host
// /dev its device nodes. landlockGrants names both (/dev RW, /proc RO).
func TestWrapArgvBwrapKeepsOwnMounts(t *testing.T) {
	dir := t.TempDir()
	got := WrapArgv(dir, []string{"opencode", "acp"}, Caps{Sandbox: SandboxBwrap, WorkRoot: dir}, nil, nil)
	joined := strings.Join(got, "\x00")
	for _, owned := range []string{"/proc", "/dev", "/tmp"} {
		for _, flag := range []string{"--bind-try", "--ro-bind-try"} {
			if strings.Contains(joined, flag+"\x00"+owned+"\x00") {
				t.Errorf("WrapArgv(bwrap) bound %s over bwrap's own mount (%s): %v", owned, flag, got)
			}
		}
	}
	if !strings.Contains(joined, "--proc\x00/proc") || !strings.Contains(joined, "--dev\x00/dev") {
		t.Errorf("WrapArgv(bwrap) lost bwrap's own /proc or /dev mount: %v", got)
	}
}

// TestWrapArgvNoneStaysUnwrapped: `none` has no boundary to wrap into, so the
// ACP child is spawned exactly as configured - and the gate on capabilities
// that rest on a boundary (EnforcesBoundary) must say so.
func TestWrapArgvNoneStaysUnwrapped(t *testing.T) {
	dir := t.TempDir()
	argv := []string{"opencode", "acp"}
	for _, mode := range []SandboxMode{SandboxNone, ""} {
		got := WrapArgv(dir, argv, Caps{Sandbox: mode, HomeDir: t.TempDir()}, []string{"/skills"}, nil)
		if len(got) != 2 || got[0] != "opencode" || got[1] != "acp" {
			t.Errorf("mode %q: WrapArgv = %v, want argv unchanged", mode, got)
		}
		if EnforcesBoundary(mode) {
			t.Errorf("EnforcesBoundary(%q) = true, want false - no OS boundary exists there", mode)
		}
	}
	for _, mode := range []SandboxMode{SandboxLandlock, SandboxBwrap} {
		if !EnforcesBoundary(mode) {
			t.Errorf("EnforcesBoundary(%q) = false, want true - WrapArgv wraps it", mode)
		}
	}
}
