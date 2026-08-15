package workspace

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestReadOnlyCapsBlocksWriteBwrap is spec test case 1 (#754), bwrap half: a
// node whose Caps.ReadOnly is set cannot write to its own working directory -
// the write fails at the OS level (a non-zero exit, nothing landing on disk),
// not because a prompt asked it not to. Before this fix childArgv bound the
// node dir read-write unconditionally; this must have failed against main.
func TestReadOnlyCapsBlocksWriteBwrap(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	caps := sandboxCaps(t, SandboxBwrap)
	caps.ReadOnly = true

	// A relative path, not the host's absolute dir - bwrap remaps the node dir
	// onto a fixed mount point (SandboxWorkRoot), so a literal host path is
	// simply absent inside the namespace regardless of RO/RW. Denial must come
	// from the read-only bind, not from the path not existing at all.
	res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", "echo pwned > f"}, caps)
	if err != nil {
		t.Fatalf("run errored (want a clean non-zero exit): %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("SANDBOX GAP: a read_only node wrote to its own working directory (exit 0): %q", res.Output)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "f")); statErr == nil {
		t.Fatal("the blocked write landed on disk despite the non-zero exit")
	}
}

// TestReadOnlyCapsBlocksWriteLandlock is test case 1's landlock half - the
// mode the container deploy runs (config/quack.yaml), where bwrap can't nest.
func TestReadOnlyCapsBlocksWriteLandlock(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	caps := sandboxCaps(t, SandboxLandlock)
	caps.WorkRoot = dir
	caps.ReadOnly = true

	res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", "echo pwned > f"}, caps)
	if err != nil {
		t.Fatalf("run errored (want a clean non-zero exit): %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("SANDBOX GAP: a read_only node wrote to its own working directory (exit 0): %q", res.Output)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "f")); statErr == nil {
		t.Fatal("the blocked write landed on disk despite the non-zero exit")
	}
}

// TestReadOnlyCapsAllowsReadAndGrep is test case 2: a read-only grant still
// lets the node read and grep its own directory - only writing is denied.
func TestReadOnlyCapsAllowsReadAndGrep(t *testing.T) {
	for _, mode := range []SandboxMode{SandboxBwrap, SandboxLandlock} {
		t.Run(string(mode), func(t *testing.T) {
			if mode == SandboxBwrap {
				requireBwrap(t)
			} else {
				requireLandlock(t)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			caps := sandboxCaps(t, mode)
			caps.WorkRoot = dir
			caps.ReadOnly = true

			res, err := RunArgv(context.Background(), dir, []string{"cat", "hello.txt"}, caps)
			if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "beta") {
				t.Fatalf("read of a read-only node's own file failed: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
			}

			res, err = RunArgv(context.Background(), dir, []string{"grep", "beta", "hello.txt"}, caps)
			if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "beta") {
				t.Fatalf("grep of a read-only node's own file failed: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
			}
		})
	}
}

// TestWritableCapsStillWrites is test case 3: a node without ReadOnly set
// writes normally - this fix must not regress the writer path.
func TestWritableCapsStillWrites(t *testing.T) {
	for _, mode := range []SandboxMode{SandboxBwrap, SandboxLandlock} {
		t.Run(string(mode), func(t *testing.T) {
			if mode == SandboxBwrap {
				requireBwrap(t)
			} else {
				requireLandlock(t)
			}
			dir := t.TempDir()
			caps := sandboxCaps(t, mode)
			caps.WorkRoot = dir

			res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", "echo built > f"}, caps)
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("a writer node could not write its own directory: err=%v exit=%d output=%q", err, res.ExitCode, res.Output)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "f")); statErr != nil {
				t.Fatalf("the write did not land on disk: %v", statErr)
			}
		})
	}
}

// TestChildArgvBwrapReadOnlyUsesRoBind is a pure argv-assembly check (no
// bwrap install needed): Caps.ReadOnly turns the node's own work/dir binds
// into --ro-bind, and leaves them --bind when unset.
func TestChildArgvBwrapReadOnlyUsesRoBind(t *testing.T) {
	dir := t.TempDir()
	ro := childArgv(dir, "/bin/echo", []string{"/bin/echo", "hi"}, Caps{Sandbox: SandboxBwrap, ReadOnly: true})
	joined := strings.Join(ro, "\x00")
	if !strings.Contains(joined, "--ro-bind\x00"+dir+"\x00"+SandboxWorkRoot) {
		t.Errorf("childArgv(ReadOnly) = %v, want a --ro-bind of the node dir", ro)
	}
	if strings.Contains(joined, "--bind\x00"+dir+"\x00"+SandboxWorkRoot) {
		t.Errorf("childArgv(ReadOnly) = %v, must not ALSO bind the node dir read-write", ro)
	}

	rw := childArgv(dir, "/bin/echo", []string{"/bin/echo", "hi"}, Caps{Sandbox: SandboxBwrap})
	joined = strings.Join(rw, "\x00")
	if !strings.Contains(joined, "--bind\x00"+dir+"\x00"+SandboxWorkRoot) {
		t.Errorf("childArgv(writer) = %v, want a --bind of the node dir", rw)
	}
}

// TestLandlockGrantsReadOnlyGrantsRO is landlockGrants' pure-computation
// counterpart: ReadOnly moves the node's own work dir from rw to ro, without
// touching HOME or tmp (an agent that can't write its content should still
// have a writable HOME/tmp scratch).
func TestLandlockGrantsReadOnlyGrantsRO(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	caps := Caps{WorkRoot: dir, HomeDir: home, ReadOnly: true}
	rw, ro := landlockGrants(dir, caps)

	found := false
	for _, p := range ro {
		if p == dir {
			found = true
		}
	}
	if !found {
		t.Errorf("landlockGrants(ReadOnly) ro = %v, missing the node dir %q", ro, dir)
	}
	for _, p := range rw {
		if p == dir {
			t.Errorf("landlockGrants(ReadOnly) rw = %v, the node dir %q must not ALSO be read-write", rw, dir)
		}
	}
	homeFound := false
	for _, p := range rw {
		if p == home {
			homeFound = true
		}
	}
	if !homeFound {
		t.Errorf("landlockGrants(ReadOnly) rw = %v, HOME %q must stay read-write", rw, home)
	}
}

// TestWrapArgvReadOnlyDegradesAndLogsUnderNone is test case 4: with sandboxing
// unable to enforce read_only (sandbox: none has no boundary at all), the node
// must still run (argv unchanged, no error) rather than refuse to start, and
// the degradation must be LOGGED, not silent. bwrap enforces it since #921, so
// it is no longer in this set - see TestWrapArgvBwrapEnforcesGrantsEndToEnd.
func TestWrapArgvReadOnlyDegradesAndLogsUnderNone(t *testing.T) {
	for _, mode := range []SandboxMode{SandboxNone, ""} {
		t.Run(string(mode), func(t *testing.T) {
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			warnReadOnlyUnenforcedOnce = sync.Once{}
			argv := []string{"opencode", "acp"}
			got := WrapArgv(t.TempDir(), argv, Caps{Sandbox: mode, ReadOnly: true}, nil, nil)
			slog.SetDefault(restore)

			if len(got) != len(argv) || got[0] != argv[0] || got[1] != argv[1] {
				t.Errorf("WrapArgv(ReadOnly) = %v, want the node to still run with argv unchanged", got)
			}
			if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "read_only") {
				t.Errorf("expected a WARN log naming read_only degradation, got: %s", out)
			}
		})
	}
}
