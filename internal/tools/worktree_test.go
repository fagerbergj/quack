package tools

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestSetupWorktreeCreatesDistinctDirsAndBranches pins the core of worktree-per-node
// isolation: two read-only qualifying nodes (reviewer, explorer) sharing one
// plan.Setup clone must each get their OWN directory AND their own branch - git refuses to check the same branch out in two worktrees at once, so a shared branch name would break the second node outright.
func TestSetupWorktreeCreatesDistinctDirsAndBranches(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	dir1, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, nil)
	if err != nil {
		t.Fatalf("SetupWorktree(review1): %v", err)
	}
	dir2, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("explore1"), workspace.WorktreeBranch("explore1"), b.caps, nil)
	if err != nil {
		t.Fatalf("SetupWorktree(explore1): %v", err)
	}

	if dir1 == dir2 {
		t.Fatalf("both nodes resolved to the SAME dir %q, want distinct worktrees", dir1)
	}
	if _, err := os.Stat(filepath.Join(dir1, "README.md")); err != nil {
		t.Errorf("worktree 1 missing the parent clone's content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "README.md")); err != nil {
		t.Errorf("worktree 2 missing the parent clone's content: %v", err)
	}

	branch1 := strings.TrimSpace(runGitInT(t, parentDir, dir1, "rev-parse", "--abbrev-ref", "HEAD"))
	branch2 := strings.TrimSpace(runGitInT(t, parentDir, dir2, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch1 == branch2 {
		t.Fatalf("both worktrees checked out the SAME branch %q, want distinct (git would refuse this for real)", branch1)
	}
	if branch1 != workspace.WorktreeBranch("review1") {
		t.Errorf("worktree 1 branch = %q, want %q", branch1, workspace.WorktreeBranch("review1"))
	}
	if branch2 != workspace.WorktreeBranch("explore1") {
		t.Errorf("worktree 2 branch = %q, want %q", branch2, workspace.WorktreeBranch("explore1"))
	}

	// Both worktrees are registered against the SAME parent clone.
	list := runGitT(t, parentDir, "worktree", "list", "--porcelain")
	if !strings.Contains(list, dir1) || !strings.Contains(list, dir2) {
		t.Errorf("git worktree list at the parent clone = %q, want both worktree dirs listed", list)
	}
}

// TestSetupWorktreeIsIdempotent pins the resumed-run requirement: re-entering
// the same node calls SetupWorktree again with the same arguments, and that
// must be a cheap no-op (the SAME worktree, still valid) rather than a disruptive re-link that could clobber files the worker already wrote.
func TestSetupWorktreeIsIdempotent(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	nodeRel := workspace.NodeDir("review1")
	branch := workspace.WorktreeBranch("review1")
	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil)
	if err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	marker := filepath.Join(dir, "worker-wrote-this.txt")
	if err := os.WriteFile(marker, []byte("in progress"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir2, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil)
	if err != nil {
		t.Fatalf("second SetupWorktree (resume) must succeed, got: %v", err)
	}
	if dir2 != dir {
		t.Fatalf("second SetupWorktree resolved to a different dir: %q, want %q", dir2, dir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("idempotent re-entry must NOT clobber the worker's own files: %v", err)
	}
}

// TestSetupWorktreeFollowsMovedParentHead: a re-review after a push reuses the node's
// worktree; it must land on the shared clone's new head, not keep reviewing the old files.
func TestSetupWorktreeFollowsMovedParentHead(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}
	nodeRel, branch := workspace.NodeDir("review1"), workspace.WorktreeBranch("review1")
	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil)
	if err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parentDir, "README.md"), []byte("pushed after the first review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, parentDir, "commit", "--quiet", "-am", "new head")
	want := strings.TrimSpace(runGitT(t, parentDir, "rev-parse", "HEAD"))

	if _, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil); err != nil {
		t.Fatalf("second SetupWorktree: %v", err)
	}
	if got := strings.TrimSpace(runGitInT(t, parentDir, dir, "rev-parse", "HEAD")); got != want {
		t.Errorf("worktree HEAD = %s, want the shared clone's new head %s", got, want)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(body) != "pushed after the first review\n" {
		t.Errorf("worktree README.md = %q, want the new head's content", body)
	}
	if st := runGitInT(t, parentDir, dir, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(st) != "" {
		t.Errorf("worktree left dirty after sync:\n%s", st)
	}
}

// TestSetupWorktreeRunsCheckSetup pins the #856 follow-up: a read-only
// worktree (reviewer/explorer) can never bootstrap itself, so check_setup
// must run quack-side, in the worktree, before the worker's first round - not only later at gate-check time.
func TestSetupWorktreeRunsCheckSetup(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, []string{"touch generated.txt"})
	if err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated.txt")); err != nil {
		t.Errorf("check_setup did not run in the worktree before returning: %v", err)
	}
}

// TestSetupWorktreeRunsCheckSetupAfterSharedCloneAlreadyDid pins the per-dir
// cache key: workspace.RunCheckSetup's cache is shared across every caller
// (SetupClone and SetupWorktree both call into it), so a naive key (e.g. the parent clone's dir, or the node ID alone) would make the shared clone's bootstrap poison a worktree's own - exactly the live failure (a worktree missing scripts/node_modules). The key must be the worktree's OWN resolved dir.
func TestSetupWorktreeRunsCheckSetupAfterSharedCloneAlreadyDid(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	checkSetup := []string{"touch generated.txt"}

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}
	// Warm the cache for the SHARED clone's own dir first, mirroring
	// SetupClone's provisioning-time call.
	workspace.RunCheckSetup(parentDir, checkSetup, b.caps)
	if _, err := os.Stat(filepath.Join(parentDir, "generated.txt")); err != nil {
		t.Fatalf("check_setup did not run in the shared clone: %v", err)
	}

	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, checkSetup)
	if err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated.txt")); err != nil {
		t.Errorf("check_setup did not run in the worktree - the shared clone's dir already being cached must not skip the worktree's own bootstrap: %v", err)
	}
}

// TestSetupWorktreeNoCheckSetupUnchanged pins that an unset check_setup
// leaves worktree provisioning byte-identical to before this call site existed.
func TestSetupWorktreeNoCheckSetupUnchanged(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, nil)
	if err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated.txt")); err == nil {
		t.Error("no check_setup configured, but a bootstrap artifact appeared anyway")
	}
}

// TestSetupWorktreeCheckSetupFailureWarnsAndProceeds pins the shared failure
// semantics with the gate's own check_setup call (checks.go): a broken
// bootstrap command must not fail node worktree provisioning.
func TestSetupWorktreeCheckSetupFailureWarnsAndProceeds(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, []string{"false"})
	if err != nil {
		t.Fatalf("SetupWorktree must succeed despite a broken check_setup command, got: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("worktree missing after a failed check_setup: %v", err)
	}
}

// TestPruneWorktreeStaysInsideRoot: a worktree registered to a clone outside root is never pruned from it; one
// whose clone lies inside root is.
func TestPruneWorktreeStaysInsideRoot(t *testing.T) {
	requireGit(t)
	newClone := func(parent string) (clone, wt string) {
		clone, wt = filepath.Join(parent, "clone"), filepath.Join(t.TempDir(), "wt")
		rawGit(t, parent, "init", "--quiet", "--initial-branch=main", clone)
		rawGit(t, clone, "-c", "user.name=t", "-c", "user.email=t@x.local", "commit", "--quiet", "--allow-empty", "-m", "init")
		rawGit(t, clone, "worktree", "add", "--quiet", "--detach", wt)
		return clone, wt
	}
	registered := func(clone, wt string) bool {
		out, err := exec.Command("git", "-C", clone, "worktree", "list", "--porcelain").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(string(out), "worktree "+wt+"\n")
	}
	root := t.TempDir()
	inClone, inWT := newClone(root)
	outClone, outWT := newClone(t.TempDir())

	if err := PruneWorktree(context.Background(), root, outWT, workspace.DefaultCaps()); err == nil {
		t.Error("PruneWorktree of a worktree owned by a clone outside root: want an error")
	}
	if !registered(outClone, outWT) {
		t.Error("the outside clone's worktree bookkeeping was touched")
	}
	if err := PruneWorktree(context.Background(), root, inWT, workspace.DefaultCaps()); err != nil {
		t.Fatalf("PruneWorktree inside root: %v", err)
	}
	if registered(inClone, inWT) {
		t.Error("the inside clone still registers the pruned worktree")
	}
}

// TestSetupWorktreeSyncIgnoresRetargetedHead: a worktree HEAD rewritten to name a shared branch doesn't make the
// re-sync move that branch; the worktree lands on its own branch at the clone's head.
func TestSetupWorktreeSyncIgnoresRetargetedHead(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}
	nodeRel, branch := workspace.NodeDir("review1"), workspace.WorktreeBranch("review1")
	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil)
	if err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parentDir, "README.md"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, parentDir, "commit", "--quiet", "-am", "work")
	head, mainBefore := runGitT(t, parentDir, "rev-parse", "HEAD"), runGitT(t, parentDir, "rev-parse", "main")
	ptr, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	adminHead := filepath.Join(strings.TrimPrefix(strings.TrimSpace(string(ptr)), "gitdir: "), "HEAD")
	if err := os.WriteFile(adminHead, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil); err != nil {
		t.Fatalf("second SetupWorktree: %v", err)
	}
	if got := runGitT(t, parentDir, "rev-parse", "main"); got != mainBefore {
		t.Errorf("shared main moved to %s, want it left at %s", got, mainBefore)
	}
	if got := strings.TrimSpace(runGitInT(t, parentDir, dir, "symbolic-ref", "HEAD")); got != "refs/heads/"+branch {
		t.Errorf("worktree HEAD = %s, want refs/heads/%s", got, branch)
	}
	if got := runGitInT(t, parentDir, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("worktree at %s, want the clone's head %s", got, head)
	}
}

// TestRedirectRecoveryWarns: a shared clone or worktree whose .git was aimed at another repo is still discarded and
// recreated, but now with a WARN naming the dir and why; a first-time setup logs none.
func TestRedirectRecoveryWarns(t *testing.T) {
	requireGit(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	bare, other := newBareRepoFixture(t), t.TempDir()
	rawGit(t, other, "init", "--quiet")
	b := newTestGitBinding(t)
	setup := func() string {
		dir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope), "file://"+bare, "main", "quack/work", false)
		if err != nil {
			t.Fatalf("setup the shared clone: %v", err)
		}
		return dir
	}
	worktree := func(parent string) string {
		dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parent, workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, nil)
		if err != nil {
			t.Fatalf("SetupWorktree: %v", err)
		}
		return dir
	}
	parent := setup()
	wt := worktree(parent)
	if logs.Len() != 0 {
		t.Fatalf("first-time setup warned: %s", logs.String())
	}

	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree(parent)
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "dir="+wt) || !strings.Contains(logs.String(), "not a repository quack created") {
		t.Errorf("redirected worktree recovered without a WARN naming it:\n%s", logs.String())
	}

	logs.Reset()
	if err := os.RemoveAll(filepath.Join(parent, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, ".git"), filepath.Join(parent, ".git")); err != nil {
		t.Fatal(err)
	}
	setup()
	if !strings.Contains(logs.String(), "dir="+parent) || !strings.Contains(logs.String(), "not a repository quack created") {
		t.Errorf("redirected shared clone recovered without a WARN naming it:\n%s", logs.String())
	}
	if fi, err := os.Lstat(filepath.Join(parent, ".git")); err != nil || !fi.IsDir() {
		t.Errorf("shared clone not recloned: %v %v", fi, err)
	}
}

// TestConfinedWorktreeOps: worktree add, the reuse sync and GC's prune still work with quack's git under Landlock.
func TestConfinedWorktreeOps(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	b := newTestGitBinding(t)
	parentDir, err := setupCloneAndBranch(ctx, b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+newBareRepoFixture(t), "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}
	t.Cleanup(func() { workspace.ConfineGit(false) })
	if !workspace.ConfineGit(true) {
		t.Skip("SKIPPING: landlock unavailable")
	}

	nodeRel, branch := workspace.NodeDir("review1"), workspace.WorktreeBranch("review1")
	dir, err := SetupWorktree(ctx, b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil)
	if err != nil {
		t.Fatalf("confined worktree add: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parentDir, "README.md"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, parentDir, "-c", "user.name=t", "-c", "user.email=t@x.local", "commit", "--quiet", "-am", "move")
	if _, err := SetupWorktree(ctx, b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil); err != nil {
		t.Fatalf("confined worktree sync: %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(body) != "moved\n" {
		t.Errorf("synced worktree README.md = %q, want the clone's new head", body)
	}
	if err := PruneWorktree(ctx, b.jail.Root(), dir, b.caps); err != nil {
		t.Fatalf("confined worktree remove: %v", err)
	}
	if list := runGitT(t, parentDir, "worktree", "list", "--porcelain"); strings.Contains(list, dir) {
		t.Errorf("pruned worktree still registered:\n%s", list)
	}
}

// warnLog captures WARN and above for one test.
func warnLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// snapshot names every file under dir with its size and mtime, to prove a tree untouched.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %d\n", p, fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func sharedClone(t *testing.T, b gitBinding, bare string) (string, error) {
	t.Helper()
	return setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope), "file://"+bare, "main", "quack/work", false)
}

// TestSymlinkedRepoDirsAreReplacedNotFollowed: a shared clone dir or worktree dir replaced by a symlink to a decoy
// is removed with a WARN and recreated as a real dir; the decoy is never touched.
func TestSymlinkedRepoDirsAreReplacedNotFollowed(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	parent, err := sharedClone(t, b, bare)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.resolve("")
	if err != nil {
		t.Fatal(err)
	}
	// A healthy clone of the same remote, which reuse would adopt if it followed the link.
	decoy := filepath.Join(scope, "decoy")
	rawGit(t, scope, "clone", "--quiet", bare, decoy)
	wtDecoy := filepath.Join(scope, "wt-decoy")
	if err := os.MkdirAll(wtDecoy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDecoy, "keep.txt"), []byte("decoy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(scope, workspace.NodeDir("review1"))
	if err := os.RemoveAll(parent); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{parent: decoy, node: wtDecoy} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	before, beforeWT := snapshot(t, decoy), snapshot(t, wtDecoy)
	logs := warnLog(t)

	if got, err := sharedClone(t, b, bare); err != nil || got != parent {
		t.Fatalf("setup = %q %v, want a fresh clone at %s", got, err, parent)
	}
	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parent, workspace.NodeDir("review1"),
		workspace.WorktreeBranch("review1"), b.caps, nil)
	if err != nil || dir != node {
		t.Fatalf("SetupWorktree = %q %v, want %s", dir, err, node)
	}
	for _, p := range []string{parent, node} {
		if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
			t.Errorf("%s is not a real dir: %v %v", p, fi, err)
		}
		if !strings.Contains(logs.String(), "link="+p) {
			t.Errorf("no WARN naming the replaced link %s:\n%s", p, logs.String())
		}
	}
	if snapshot(t, decoy) != before || snapshot(t, wtDecoy) != beforeWT {
		t.Error("a decoy behind a replaced link was written")
	}
}

// TestUnreadableCloneSaysSo: a reused clone whose objects git cannot read is recloned with a WARN saying so, or, when
// its checkout fails too, moved aside with that reason, never as uncommitted changes.
func TestUnreadableCloneSaysSo(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	t.Run("recloned", func(t *testing.T) {
		b := newTestGitBinding(t)
		parent, err := sharedClone(t, b, bare)
		if err != nil {
			t.Fatal(err)
		}
		pack := filepath.Join(parent, ".git", "objects", "pack")
		if err := os.Rename(pack, filepath.Join(t.TempDir(), "pack")); err != nil {
			t.Fatal(err)
		}
		logs := warnLog(t)
		if _, err := sharedClone(t, b, bare); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "re-cloning a clone git cannot read") || !strings.Contains(logs.String(), "dir="+parent) {
			t.Errorf("no WARN saying git cannot read the clone:\n%s", logs.String())
		}
	})
	t.Run("moved aside", func(t *testing.T) {
		b := newTestGitBinding(t)
		parent, err := sharedClone(t, b, bare)
		if err != nil {
			t.Fatal(err)
		}
		unpackObjects(t, parent)
		blob := strings.TrimSpace(runGitT(t, parent, "rev-parse", "HEAD:README.md"))
		if err := os.Remove(filepath.Join(parent, ".git", "objects", blob[:2], blob[2:])); err != nil {
			t.Fatal(err)
		}
		writeFile(t, parent, ".git/index.lock", "") // the reuse checkout fails too
		logs := warnLog(t)
		_, err = setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope), "file://"+bare, "main", "quack/other", false)
		if err == nil || !strings.Contains(err.Error(), "git cannot read its repository") || strings.Contains(err.Error(), "uncommitted") {
			t.Errorf("setup err = %v, want the tree moved aside because git cannot read it", err)
		}
		if !strings.Contains(logs.String(), "cannot read a clone's repository") {
			t.Errorf("no WARN with the read error:\n%s", logs.String())
		}
	})
}

// unpackObjects turns dir's packs into loose objects, so one can be removed.
func unpackObjects(t *testing.T, dir string) {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packs {
		moved := filepath.Join(t.TempDir(), "p.pack")
		if err := os.Rename(p, moved); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(strings.TrimSuffix(p, ".pack") + ".idx")
		f, err := os.Open(moved)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "-C", dir, "unpack-objects", "-q")
		cmd.Stdin = f
		out, err := cmd.CombinedOutput()
		_ = f.Close()
		if err != nil {
			t.Fatalf("unpack-objects: %v %s", err, out)
		}
	}
}
