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

// TestReadOnlyCapsBlocksWriteBwrap: a ReadOnly node's write to its own dir fails at the OS level,
// not because a prompt asked it not to.
func TestReadOnlyCapsBlocksWriteBwrap(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	caps := sandboxCaps(t, SandboxBwrap)
	caps.ReadOnly = true

	// Relative: bwrap remaps the dir, so a host path would be absent and deny for the wrong reason.
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

// TestReadOnlyCapsBlocksWriteLandlock: the container deploy's mode, where bwrap can't nest.
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

// TestReadOnlyCapsAllowsReadAndGrep: a read-only node can still read and grep its own directory.
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

// TestWritableCapsStillWrites: a node without ReadOnly writes normally.
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

// TestChildArgvBwrapReadOnlyUsesRoBind: ReadOnly turns the node's own binds into --ro-bind.
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

// TestLandlockGrantsReadOnlyGrantsRO: ReadOnly moves the work dir to ro; HOME and tmp stay writable.
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

// TestLandlockGrantsReadOnlyAddsGitignoredBuildDirs: gitignored build dirs join rw on top of the tree's
// ro grant; Landlock is additive, so the ro rule holds everywhere else.
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

// TestChildArgvBwrapReadOnlyBuildDirsGetWritableOverlay: a gitignored build dir gets a writable
// --bind-try over the RO work-tree bind.
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

// TestWrapArgvReadOnlyDegradesAndLogsUnderNone: sandbox none can't enforce read_only, so the node
// still runs unchanged and the degradation is logged.
func TestWrapArgvReadOnlyDegradesAndLogsUnderNone(t *testing.T) {
	for _, mode := range []SandboxMode{SandboxNone, ""} {
		t.Run(string(mode), func(t *testing.T) {
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			warnReadOnlyUnenforcedOnce = sync.Once{}
			argv := []string{"pi-acp", "run"}
			got := unreaped(WrapArgv(t.TempDir(), argv, Caps{Sandbox: mode, ReadOnly: true}, nil, nil))
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

// buildDirFixture: a repo ignoring node_modules/ and dist/, with tracked files at the root and in internal/.
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

// TestReadOnlyBuildDirsWritable: only configured build dirs the repo gitignores are writable;
// an ungitignored one ("build") gets no grant and no pre-creation.
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

// TestReadOnlyBuildDirSymlinkCannotEscape: a symlink planted inside node_modules pointing at a tracked
// file must not be writable through, under landlock or bwrap.
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

// TestBuildDirGrantsRejectsSymlinkedBuildDir: a build dir that is itself a symlink (committed or swapped in)
// would grant its real target, so it gets no grant.
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

// TestBuildDirGrantsSkipsUngitignoredEntries: ungitignored entries drop, nested matches stay,
// and unconfigured top-level .gitignore names are added.
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

// TestBuildDirGrantsMatchesQuacksOwnGitignoreShape: patterns without a trailing slash, as in quack's
// own .gitignore, still match directories.
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

// TestBuildDirGrantsNoGitignoreGrantsNothing: never falls back to granting every configured entry.
func TestBuildDirGrantsNoGitignoreGrantsNothing(t *testing.T) {
	dir := t.TempDir()
	if got := buildDirGrants(dir, []string{"node_modules", "dist"}); len(got) != 0 {
		t.Errorf("buildDirGrants(no .gitignore) = %v, want none", got)
	}
}

// TestBuildDirGrantsRejectsDotDotEscape: a bare ".." line in an untrusted .gitignore would grant RW
// on the work dir's parent; nothing returned may name a path outside work.
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

// TestBuildDirGrantsHonoursNegation: a name a later "!name" line takes back is tracked,
// so granting it RW would expose tracked content.
func TestBuildDirGrantsHonoursNegation(t *testing.T) {
	dir := t.TempDir()
	gitignore := "node_modules\n!node_modules\ndist\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildDirGrants(dir, []string{"node_modules", "dist"})
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}
	if gotSet["node_modules"] {
		t.Errorf("buildDirGrants(%v) must not include %q - negated by !node_modules", got, "node_modules")
	}
	if !gotSet["dist"] {
		t.Errorf("buildDirGrants(%v) missing %q", got, "dist")
	}
}

// TestBuildDirGrantsSkipsDotGit: granting ".git" would expose the gitdir pointer or shared metadata.
func TestBuildDirGrantsSkipsDotGit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".git\ndist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildDirGrants(dir, nil)
	for _, rel := range got {
		if rel == ".git" {
			t.Fatalf("buildDirGrants(%v) must never grant %q", got, ".git")
		}
	}
}

// TestBuildDirGrantsSkipsConfiguredDotGit: a configured entry naming ".git" (bare or "sub/.git") is skipped too.
func TestBuildDirGrantsSkipsConfiguredDotGit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := buildDirGrants(dir, []string{".git", "sub/.git"})
	for _, rel := range got {
		if rel == ".git" || rel == filepath.Join("sub", ".git") {
			t.Fatalf("buildDirGrants(%v) must never grant a configured %q entry", got, rel)
		}
	}
}

// TestBuildDirGrantsRejectsTrackedRegularFile: a tracked plain file named like a build dir
// must not get the RW grant.
func TestBuildDirGrantsRejectsTrackedRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules"), []byte("tracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildDirGrants(dir, []string{"node_modules"})
	for _, rel := range got {
		if rel == "node_modules" {
			t.Fatalf("buildDirGrants(%v) must not grant a tracked regular file: %q", got, rel)
		}
	}
}

// TestBuildDirGrantsRejectsEscapingConfiguredEntry: operator config, but "../../etc" is still rejected.
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
