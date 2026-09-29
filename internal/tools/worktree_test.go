package tools

import (
	"context"
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
		return strings.Contains(string(out), filepath.Base(filepath.Dir(wt)))
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
