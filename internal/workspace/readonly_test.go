package workspace

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
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

// TestLandlockGrantsReadOnlyAddsGitignoredBuildDirs is landlockGrants' pure-
// computation half of the build-dirs fix: a ReadOnly node's rw set gains the
// configured build dirs the repo's own .gitignore names, on top of (not
// instead of) the ro grant for the whole tree - Landlock is additive, so the
// more specific rw rule wins for paths under it without weakening the ro
// rule anywhere else.
func TestLandlockGrantsReadOnlyAddsGitignoredBuildDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	caps := Caps{WorkRoot: dir, ReadOnly: true, BuildDirs: []string{"node_modules", "build"}}
	rw, ro := landlockGrants(dir, caps)

	want := filepath.Join(dir, "node_modules")
	found := false
	for _, p := range rw {
		if p == want {
			found = true
		}
	}
	if !found {
		t.Errorf("landlockGrants(ReadOnly, BuildDirs) rw = %v, missing the gitignored build dir %q", rw, want)
	}
	skip := filepath.Join(dir, "build")
	for _, p := range rw {
		if p == skip {
			t.Errorf("landlockGrants(ReadOnly, BuildDirs) rw = %v, %q is not gitignored and must not be granted", rw, skip)
		}
	}
	roFound := false
	for _, p := range ro {
		if p == dir {
			roFound = true
		}
	}
	if !roFound {
		t.Errorf("landlockGrants(ReadOnly, BuildDirs) ro = %v, the node dir %q must still be read-only overall", ro, dir)
	}
}

// TestChildArgvBwrapReadOnlyBuildDirsGetWritableOverlay is childArgv's bwrap
// half of the same fix: a gitignored build dir gets a --bind-try overlay
// (writable) laid on top of the RO work-tree bind, mapped through the
// SandboxWorkRoot remap like every other bwrap path in childArgv.
func TestChildArgvBwrapReadOnlyBuildDirsGetWritableOverlay(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	caps := Caps{Sandbox: SandboxBwrap, ReadOnly: true, BuildDirs: []string{"node_modules", "build"}}
	got := childArgv(dir, "/bin/echo", []string{"/bin/echo", "hi"}, caps)
	joined := strings.Join(got, "\x00")

	wantSrc := filepath.Join(dir, "node_modules")
	wantDst := filepath.Join(SandboxWorkRoot, "node_modules")
	if !strings.Contains(joined, "--bind-try\x00"+wantSrc+"\x00"+wantDst) {
		t.Errorf("childArgv(ReadOnly, BuildDirs) = %v, missing a writable overlay for %q", got, wantSrc)
	}
	if strings.Contains(joined, filepath.Join(dir, "build")) {
		t.Errorf("childArgv(ReadOnly, BuildDirs) = %v, %q is not gitignored and must not be bound", got, filepath.Join(dir, "build"))
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

// buildDirFixture sets up a git repo at a fresh t.TempDir() with a
// .gitignore that ignores node_modules/ and dist/ (so "frontend/dist"
// qualifies by its top-level component too), a tracked root file, and a
// tracked internal/ dir - the shape TestReadOnlyBuildDirsWritable exercises.
func buildDirFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\ndist/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(argv ...string) {
		t.Helper()
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", argv, err, out)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "-q", "-m", "init")
	return dir
}

// TestReadOnlyBuildDirsWritable is this fix's spec test: a read_only node's
// own tree stays immutable at the OS level EXCEPT the configured build dirs
// the repo already gitignores (node_modules/, frontend/dist/ via a bare
// "dist/" line) - those are writable, in place, no TMPDIR copy needed. A
// configured dir the repo does NOT gitignore ("build") gets no grant and no
// pre-creation at all, same as any other path in the read-only tree.
func TestReadOnlyBuildDirsWritable(t *testing.T) {
	for _, mode := range []SandboxMode{SandboxBwrap, SandboxLandlock} {
		t.Run(string(mode), func(t *testing.T) {
			if mode == SandboxBwrap {
				requireBwrap(t)
			} else {
				requireLandlock(t)
			}
			dir := buildDirFixture(t)

			caps := sandboxCaps(t, mode)
			caps.WorkRoot = dir
			caps.ReadOnly = true
			caps.BuildDirs = []string{"node_modules", "frontend/dist", "build"}
			PrecreateBuildDirs(dir, caps.BuildDirs)

			write := func(rel string) int {
				t.Helper()
				res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", "echo x > " + rel}, caps)
				if err != nil {
					t.Fatalf("run %q errored (want a clean exit either way): %v", rel, err)
				}
				return res.ExitCode
			}

			if code := write("node_modules/f"); code != 0 {
				t.Errorf("write inside gitignored node_modules/ denied: exit=%d", code)
			}
			if code := write("frontend/dist/f"); code != 0 {
				t.Errorf("write inside gitignored frontend/dist/ (matched via bare dist/) denied: exit=%d", code)
			}

			if _, statErr := os.Stat(filepath.Join(dir, "build")); statErr == nil {
				t.Error("a configured build dir the repo does not gitignore must not be pre-created")
			}
			if code := write("main.go"); code == 0 {
				t.Error("SANDBOX GAP: overwrote a tracked source file at the root of a read-only node")
			}
			if code := write("internal/f"); code == 0 {
				t.Error("SANDBOX GAP: wrote inside internal/ on a read-only node")
			}

			out, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput()
			if err != nil {
				t.Fatalf("git status: %v: %s", err, out)
			}
			if strings.TrimSpace(string(out)) != "" {
				t.Errorf("git status --porcelain not empty after build-dir writes (pre-created dirs must stay invisible to git): %q", out)
			}
		})
	}
}

// TestReadOnlyBuildDirSymlinkCannotEscape is this fix's real attack case: the
// agent itself controls what lands inside a granted build dir (that's the
// whole point - `npm install` writes there), so it can plant a symlink INSIDE
// node_modules pointing OUT at a tracked file ("../main.go") and then write
// through the symlink name. Both landlock (a DAC rule keyed to the resolved
// target inode/path, not the syntactic name used to reach it) and bwrap (the
// symlink's ".." traversal, resolved inside the mount namespace, walks back
// onto the RO-bound parent mount, not some host path outside the sandbox)
// must deny the write and leave main.go untouched.
func TestReadOnlyBuildDirSymlinkCannotEscape(t *testing.T) {
	for _, mode := range []SandboxMode{SandboxBwrap, SandboxLandlock} {
		t.Run(string(mode), func(t *testing.T) {
			if mode == SandboxBwrap {
				requireBwrap(t)
			} else {
				requireLandlock(t)
			}
			dir := buildDirFixture(t)
			caps := sandboxCaps(t, mode)
			caps.WorkRoot = dir
			caps.ReadOnly = true
			caps.BuildDirs = []string{"node_modules"}
			PrecreateBuildDirs(dir, caps.BuildDirs)

			before, err := os.ReadFile(filepath.Join(dir, "main.go"))
			if err != nil {
				t.Fatal(err)
			}

			res, err := RunArgv(context.Background(), dir,
				[]string{"sh", "-c", "ln -s ../main.go node_modules/escape && echo pwned > node_modules/escape"}, caps)
			if err != nil {
				t.Fatalf("run errored (want a clean exit either way): %v", err)
			}
			if res.ExitCode == 0 {
				t.Errorf("SANDBOX ESCAPE: a symlink inside a granted build dir wrote through to the read-only tree (exit 0): %q", res.Output)
			}

			after, err := os.ReadFile(filepath.Join(dir, "main.go"))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("SANDBOX ESCAPE: main.go changed via a symlink planted inside a granted build dir: got %q, want %q", after, before)
			}
		})
	}
}

// TestBuildDirGrantsRejectsSymlinkedBuildDir is the OTHER symlink shape (the
// one that was actually exploitable): the build dir NAME ITSELF is a symlink
// pointing outside the work tree at pre-create time - e.g. a malicious PR
// commits a symlink literally named "node_modules" -> an arbitrary host path,
// which a normal git checkout materializes before PrecreateBuildDirs ever
// runs, or the agent later deletes its granted node_modules and replaces it
// with one (landlockGrants/childArgv recompute grants on every RunArgv call,
// so a mid-session swap is re-evaluated too). Landlock adds its rule against
// the resolved target (proven: without the Lstat guard this test landed a
// file OUTSIDE the work tree under landlock), and bwrap's --bind-try would
// bind-mount the target directory itself. Neither may be granted.
func TestBuildDirGrantsRejectsSymlinkedBuildDir(t *testing.T) {
	for _, mode := range []SandboxMode{SandboxBwrap, SandboxLandlock} {
		t.Run(string(mode), func(t *testing.T) {
			if mode == SandboxBwrap {
				requireBwrap(t)
			} else {
				requireLandlock(t)
			}
			dir := t.TempDir()
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("evil_dir\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "evil_dir")); err != nil {
				t.Fatal(err)
			}

			caps := sandboxCaps(t, mode)
			caps.WorkRoot = dir
			caps.ReadOnly = true
			caps.BuildDirs = []string{"evil_dir"}
			PrecreateBuildDirs(dir, caps.BuildDirs)

			res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", "echo pwned > evil_dir/newfile"}, caps)
			if err != nil {
				t.Fatalf("run errored (want a clean exit either way): %v", err)
			}
			if res.ExitCode == 0 {
				t.Errorf("SANDBOX GAP: write through a symlinked build dir exited 0: %q", res.Output)
			}
			if _, statErr := os.Stat(filepath.Join(outside, "newfile")); statErr == nil {
				t.Fatalf("SANDBOX ESCAPE: write via a symlinked build dir landed OUTSIDE the work tree, at %s", outside)
			}
		})
	}
}

// TestBuildDirGrantsSkipsUngitignoredEntries is buildDirGrants' pure-
// computation counterpart (no sandbox kernel support needed): a configured
// entry not named in the repo's .gitignore is dropped, an entry the
// .gitignore names by a nested path's top-level component is kept, and a
// plain top-level .gitignore line NOT in the configured list is added too.
func TestBuildDirGrantsSkipsUngitignoredEntries(t *testing.T) {
	dir := t.TempDir()
	gitignore := "node_modules/\ndist/\n.venv/\n*.log\n/build/\nnested/skip/\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildDirGrants(dir, []string{"node_modules", "frontend/dist", "coverage"})
	want := map[string]bool{"node_modules": true, "frontend/dist": true, ".venv": true, "build": true}
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}
	for w := range want {
		if !gotSet[w] {
			t.Errorf("buildDirGrants(%v) = %v, missing %q", got, got, w)
		}
	}
	for _, bad := range []string{"coverage", "nested/skip"} {
		if gotSet[bad] {
			t.Errorf("buildDirGrants(%v) must not include %q (coverage isn't gitignored; nested/skip isn't a plain top-level line)", got, bad)
		}
	}
}

// TestBuildDirGrantsMatchesQuacksOwnGitignoreShape pins the real-world shape
// this repo's OWN .gitignore uses - "node_modules" and "frontend/dist" with
// NO trailing slash (gitignore never requires one; a bare no-slash pattern
// still matches the directory) - since a parser that only accepted
// "node_modules/"-with-slash would silently grant nothing on quack's own
// tree.
func TestBuildDirGrantsMatchesQuacksOwnGitignoreShape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules\nfrontend/dist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildDirGrants(dir, []string{"node_modules", "frontend/dist", "frontend/node_modules"})
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}
	// "node_modules" is a bare pattern - matches at any depth, so it also
	// covers the configured "frontend/node_modules" entry.
	for _, want := range []string{"node_modules", "frontend/dist", "frontend/node_modules"} {
		if !gotSet[want] {
			t.Errorf("buildDirGrants(%v) = %v, missing %q (quack's own .gitignore has no trailing slash)", got, got, want)
		}
	}
}

// TestBuildDirGrantsNoGitignoreGrantsNothing: a work dir with no .gitignore
// at all (or none of its lines match) grants nothing - never falls back to
// "grant every configured entry anyway".
func TestBuildDirGrantsNoGitignoreGrantsNothing(t *testing.T) {
	dir := t.TempDir()
	if got := buildDirGrants(dir, []string{"node_modules", "dist"}); len(got) != 0 {
		t.Errorf("buildDirGrants(no .gitignore) = %v, want none", got)
	}
}

// TestBuildDirGrantsRejectsDotDotEscape: the reviewed repo's OWN .gitignore
// is untrusted content (a malicious PR branch can write anything into it),
// and the "any other top-level dir the .gitignore names" bonus grant
// (buildDirGrants' second loop) uses a bare gitignore line's text AS the
// granted path with no containment check. A bare ".." line has no "/" and no
// glob char, so it parses as a bare pattern and, unguarded, resolves through
// filepath.Join(work, "..") to the WORK DIR'S PARENT - landlockGrants would
// then grant real RW there, and childArgv would --bind-try the host parent
// dir into the sandbox: a full escape from a repo the agent is meant to only
// review. Nothing returned may name a path outside work.
func TestBuildDirGrantsRejectsDotDotEscape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildDirGrants(dir, nil)
	for _, rel := range got {
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
			t.Fatalf("SANDBOX ESCAPE: buildDirGrants(%q) = %v, %q resolves outside work via filepath.Join(work, %q) = %q",
				dir, got, rel, rel, filepath.Join(dir, rel))
		}
	}
}

// TestBuildDirGrantsRejectsEscapingConfiguredEntry: workspace.build_dirs is
// operator config, not agent input, but a "../../etc"-shaped entry should
// still be rejected defensively rather than trusted to resolve inside work -
// same containment guarantee as the gitignore-driven bonus grant above.
func TestBuildDirGrantsRejectsEscapingConfiguredEntry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("etc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildDirGrants(dir, []string{"../../etc", "/etc"})
	for _, rel := range got {
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
			t.Fatalf("SANDBOX ESCAPE: buildDirGrants(%q, [../../etc, /etc]) = %v, %q resolves outside work", dir, got, rel)
		}
	}
}
