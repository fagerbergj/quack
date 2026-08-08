package workspace

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func never(string) bool { return false }

// touch creates an empty file (and its parent dirs) then sets its mtime.
func touch(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestSweepChatScopesTTLBoundary(t *testing.T) {
	jail := newTestJail(t)
	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now()

	oldScope := filepath.Join(jail.Root(), "alice", "old-chat")
	newScope := filepath.Join(jail.Root(), "alice", "new-chat")
	touch(t, filepath.Join(oldScope, "repo", "README.md"), old)
	touch(t, filepath.Join(newScope, "repo", "README.md"), fresh)
	// The scope dir's own mtime is what the TTL check reads - the file writes
	// above already set it via the MkdirAll, so age it explicitly to be sure.
	if err := os.Chtimes(oldScope, old, old); err != nil {
		t.Fatal(err)
	}

	res := Sweep(context.Background(), jail, GCConfig{ChatTTL: 24 * time.Hour}, never, nil)
	if res.ChatsRemoved != 1 {
		t.Fatalf("ChatsRemoved = %d, want 1", res.ChatsRemoved)
	}
	if _, err := os.Stat(oldScope); !os.IsNotExist(err) {
		t.Errorf("old scope should have been reaped, stat err = %v", err)
	}
	if _, err := os.Stat(newScope); err != nil {
		t.Errorf("new scope should have survived: %v", err)
	}
}

func TestSweepChatScopesSkipsActiveChat(t *testing.T) {
	jail := newTestJail(t)
	old := time.Now().Add(-48 * time.Hour)
	scope := filepath.Join(jail.Root(), "alice", "live-chat")
	touch(t, filepath.Join(scope, "repo", "README.md"), old)
	if err := os.Chtimes(scope, old, old); err != nil {
		t.Fatal(err)
	}

	isActive := func(chatID string) bool { return chatID == "live-chat" }
	res := Sweep(context.Background(), jail, GCConfig{ChatTTL: 24 * time.Hour}, isActive, nil)
	if res.ChatsRemoved != 0 {
		t.Fatalf("ChatsRemoved = %d, want 0 (chat has a run in flight)", res.ChatsRemoved)
	}
	if _, err := os.Stat(scope); err != nil {
		t.Errorf("a live chat's scope must never be reaped: %v", err)
	}
}

// TestSweepChatScopesNilActiveSkipsAll: with no way to prove a chat inactive,
// the reaper must fail closed rather than reap everything.
func TestSweepChatScopesNilActiveSkipsAll(t *testing.T) {
	jail := newTestJail(t)
	old := time.Now().Add(-48 * time.Hour)
	scope := filepath.Join(jail.Root(), "alice", "some-chat")
	touch(t, filepath.Join(scope, "repo", "README.md"), old)
	if err := os.Chtimes(scope, old, old); err != nil {
		t.Fatal(err)
	}

	res := Sweep(context.Background(), jail, GCConfig{ChatTTL: 24 * time.Hour}, nil, nil)
	if res.ChatsRemoved != 0 {
		t.Fatalf("ChatsRemoved = %d, want 0 (nil isActive must fail closed)", res.ChatsRemoved)
	}
}

func TestSweepNeverEscapesJailRoot(t *testing.T) {
	parent := t.TempDir()
	jailRoot := filepath.Join(parent, "jail")
	jail, err := NewJail(jailRoot)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)

	outsideMarker := filepath.Join(parent, "outside", "marker.txt")
	touch(t, outsideMarker, old)

	insideScope := filepath.Join(jail.Root(), "alice", "old-chat")
	touch(t, filepath.Join(insideScope, "repo", "README.md"), old)
	if err := os.Chtimes(insideScope, old, old); err != nil {
		t.Fatal(err)
	}

	res := Sweep(context.Background(), jail, GCConfig{ChatTTL: time.Hour, ScratchTTL: time.Hour}, never, nil)
	if res.ChatsRemoved != 1 {
		t.Fatalf("ChatsRemoved = %d, want 1", res.ChatsRemoved)
	}
	if _, err := os.Stat(outsideMarker); err != nil {
		t.Errorf("a sweep must never touch anything outside the jail root: %v", err)
	}
}

func TestSweepHomeTmpTTLBoundaryLeavesCachesAlone(t *testing.T) {
	jail := newTestJail(t)
	homeDir, err := jail.HomeDir("alice")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-12 * time.Hour)
	fresh := time.Now()

	oldScratch := filepath.Join(homeDir, "tmp", "old-build")
	newScratch := filepath.Join(homeDir, "tmp", "new-build")
	cache := filepath.Join(homeDir, "npm-cache", "pkg")
	touch(t, filepath.Join(oldScratch, "f"), old)
	touch(t, filepath.Join(newScratch, "f"), fresh)
	touch(t, cache, old) // a cache entry, NOT under tmp/ - GC never sweeps caches
	if err := os.Chtimes(oldScratch, old, old); err != nil {
		t.Fatal(err)
	}

	res := Sweep(context.Background(), jail, GCConfig{ScratchTTL: time.Hour}, never, nil)
	if res.ScratchRemoved != 1 {
		t.Fatalf("ScratchRemoved = %d, want 1", res.ScratchRemoved)
	}
	if _, err := os.Stat(oldScratch); !os.IsNotExist(err) {
		t.Errorf("old scratch entry should have been reaped, stat err = %v", err)
	}
	if _, err := os.Stat(newScratch); err != nil {
		t.Errorf("new scratch entry should have survived: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("a cache entry must never be swept: %v", err)
	}
}

// growHome writes an n-byte file into the user's agent home, simulating
// opencode.db/snapshot/tool-output growth from a completed round.
func growHome(t *testing.T, jail *Jail, userID, name string, n int) {
	t.Helper()
	home, err := jail.HomeDir(userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, name), make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSweepAgentHomeResetsPastQuotaWhenIdle is issue #800 test case 1 (the
// reclaimed half): a home over HomeMaxBytes with no chat in flight is reset
// whole - never edited, the directory is gone and recreated empty.
func TestSweepAgentHomeResetsPastQuotaWhenIdle(t *testing.T) {
	jail := newTestJail(t)
	growHome(t, jail, "alice", "opencode.db", 100)

	res := Sweep(context.Background(), jail, GCConfig{HomeMaxBytes: 50}, never, nil)
	if res.HomeReset != 1 {
		t.Fatalf("HomeReset = %d, want 1", res.HomeReset)
	}
	if res.BytesReclaimed != 100 {
		t.Fatalf("BytesReclaimed = %d, want 100", res.BytesReclaimed)
	}
	home, err := jail.HomeDir("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "opencode.db")); !os.IsNotExist(err) {
		t.Errorf("opencode.db should have been reclaimed, stat err = %v", err)
	}
}

// TestSweepAgentHomeSkipsLiveChat is issue #800 test case 1 (the live half):
// the SAME isActive signal that protects a live chat's clone in
// sweepChatScopes must also keep the shared home untouched while that chat
// has a round in flight, even though it's well past HomeMaxBytes.
func TestSweepAgentHomeSkipsLiveChat(t *testing.T) {
	jail := newTestJail(t)
	growHome(t, jail, "alice", "opencode.db", 100)
	touch(t, filepath.Join(jail.Root(), "alice", "live-chat", "repo", "README.md"), time.Now())

	isActive := func(chatID string) bool { return chatID == "live-chat" }
	res := Sweep(context.Background(), jail, GCConfig{HomeMaxBytes: 50}, isActive, nil)
	if res.HomeReset != 0 {
		t.Fatalf("HomeReset = %d, want 0 (a chat has a run in flight)", res.HomeReset)
	}
	home, err := jail.HomeDir("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "opencode.db")); err != nil {
		t.Errorf("a live user's agent home must never be reaped: %v", err)
	}
}

// TestSweepAgentHomeNilActiveFailsClosed mirrors
// TestSweepChatScopesNilActiveSkipsAll: with no way to prove any chat
// inactive, the reaper must not touch the shared home either.
func TestSweepAgentHomeNilActiveFailsClosed(t *testing.T) {
	jail := newTestJail(t)
	growHome(t, jail, "alice", "opencode.db", 100)

	res := Sweep(context.Background(), jail, GCConfig{HomeMaxBytes: 50}, nil, nil)
	if res.HomeReset != 0 {
		t.Fatalf("HomeReset = %d, want 0 (nil isActive must fail closed)", res.HomeReset)
	}
}

// TestSweepAgentHomeBelowQuotaLeavesItAlone is the TTL-boundary analogue for
// a quota basis: a home under HomeMaxBytes survives untouched.
func TestSweepAgentHomeBelowQuotaLeavesItAlone(t *testing.T) {
	jail := newTestJail(t)
	growHome(t, jail, "alice", "opencode.db", 10)

	res := Sweep(context.Background(), jail, GCConfig{HomeMaxBytes: 50}, never, nil)
	if res.HomeReset != 0 {
		t.Fatalf("HomeReset = %d, want 0 (home is under quota)", res.HomeReset)
	}
}

// TestSweepAgentHomeResetStaysUsable is issue #800 test case 2: a run whose
// agent home was reclaimed must still start and complete. HomeDir's own
// MkdirAll makes this true by construction - prove it survives a reset.
func TestSweepAgentHomeResetStaysUsable(t *testing.T) {
	jail := newTestJail(t)
	growHome(t, jail, "alice", "opencode.db", 100)

	res := Sweep(context.Background(), jail, GCConfig{HomeMaxBytes: 50}, never, nil)
	if res.HomeReset != 1 {
		t.Fatalf("HomeReset = %d, want 1", res.HomeReset)
	}
	home, err := jail.HomeDir("alice")
	if err != nil {
		t.Fatalf("HomeDir after reset: %v", err)
	}
	probe := filepath.Join(home, "opencode.db")
	if err := os.WriteFile(probe, []byte("fresh round"), 0o600); err != nil {
		t.Fatalf("a run reusing the reclaimed home could not write to it: %v", err)
	}
}

// TestSweepAgentHomeBoundsGrowthAcrossManySweeps is issue #800 test case 3:
// however many idle rounds accumulate state, the home is never left more than
// one round's worth of growth (deltaBytes) over HomeMaxBytes, unattended,
// across many sweep cycles - not just a single before/after snapshot.
func TestSweepAgentHomeBoundsGrowthAcrossManySweeps(t *testing.T) {
	jail := newTestJail(t)
	const maxBytes = 1000
	const deltaBytes = 200
	cfg := GCConfig{HomeMaxBytes: maxBytes}

	for round := 0; round < 50; round++ {
		growHome(t, jail, "alice", fmt.Sprintf("round-file-%d", round), deltaBytes)
		res := Sweep(context.Background(), jail, cfg, never, nil)
		home, err := jail.HomeDir("alice")
		if err != nil {
			t.Fatal(err)
		}
		sz := dirSize(home)
		if sz > maxBytes+deltaBytes {
			t.Fatalf("round %d: agent home grew to %d bytes (reset this pass: %v), want <= %d",
				round, sz, res.HomeReset == 1, maxBytes+deltaBytes)
		}
	}
}

// TestSweepAgentHomeLogsWhatWasFreed is issue #800 test case 4: a reset must
// be distinguishable in the logs from a run failure - Info level, naming the
// user and the bytes freed, not just "something happened".
func TestSweepAgentHomeLogsWhatWasFreed(t *testing.T) {
	jail := newTestJail(t)
	growHome(t, jail, "alice", "opencode.db", 100)

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	sweepAndLog(context.Background(), jail, GCConfig{HomeMaxBytes: 50}, never, nil)
	slog.SetDefault(restore)

	out := buf.String()
	if !strings.Contains(out, "level=INFO") {
		t.Errorf("expected an INFO log for a reclaim, got: %s", out)
	}
	if !strings.Contains(out, "home_reset=1") || !strings.Contains(out, "bytes_reclaimed=100") {
		t.Errorf("expected the log to name what was freed (home_reset, bytes_reclaimed), got: %s", out)
	}
}

func TestSweepDisabledConfigSweepsNothing(t *testing.T) {
	jail := newTestJail(t)
	old := time.Now().Add(-48 * time.Hour)
	scope := filepath.Join(jail.Root(), "alice", "old-chat")
	touch(t, filepath.Join(scope, "repo", "README.md"), old)
	if err := os.Chtimes(scope, old, old); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		// Enabled: false must return before ever sweeping - a cancelled ctx
		// isn't even needed for it to stop.
		RunGC(context.Background(), jail, GCConfig{Enabled: false, ChatTTL: time.Hour}, never, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunGC(Enabled: false) did not return promptly")
	}
	if _, err := os.Stat(scope); err != nil {
		t.Errorf("a disabled GC must not remove anything: %v", err)
	}
}

// TestRunGCStopsOnContextCancel drives the ticker loop with a short interval
// and proves shutdown is clean - no real sleeps, just a bounded select.
func TestRunGCStopsOnContextCancel(t *testing.T) {
	jail := newTestJail(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunGC(ctx, jail, GCConfig{Enabled: true, Interval: time.Millisecond}, never, nil)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunGC did not stop after context cancellation")
	}
}

// initGitRepo makes dir a real git repo with one commit - the minimum a
// `git worktree add` needs.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.local",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.local")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "--quiet", "-m", "init")
}

// TestSweepBaselineWorktreeReapingKeepsParentConsistent proves the CRITICAL
// worktree case: an orphaned baseline worktree (internal/vetting/baseline.go's
// os.MkdirTemp("", "quack-base-") scratch, never cleaned up because the
// process crashed mid-check) is reaped WITHOUT leaving the parent clone's
// `git worktree` bookkeeping wedged - the parent can add a worktree at the
// same path again afterward, proving nothing stale survived.
func TestSweepBaselineWorktreeReapingKeepsParentConsistent(t *testing.T) {
	jail := newTestJail(t)
	parent := filepath.Join(jail.Root(), "alice", "chat1", "quack-shared-repo")
	initGitRepo(t, parent)

	scratch, err := os.MkdirTemp("", baselineTempPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	wt := filepath.Join(scratch, "wt")
	add := exec.Command("git", "worktree", "add", "--detach", wt, "HEAD")
	add.Dir = parent
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	// Creating the worktree just bumped scratch's own mtime - age it past the
	// TTL now, exactly as a crash-orphaned scratch dir would look.
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(scratch, old, old); err != nil {
		t.Fatal(err)
	}

	prune := func(ctx context.Context, dir string) error {
		common := WorktreeCommonGitDir(dir)
		if common == "" {
			return nil
		}
		p := filepath.Dir(common)
		if _, err := os.Stat(p); err != nil {
			return nil
		}
		cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", dir)
		cmd.Dir = p
		return cmd.Run()
	}

	res := Sweep(context.Background(), jail, GCConfig{ScratchTTL: time.Hour}, never, prune)
	if res.ScratchRemoved != 1 {
		t.Fatalf("ScratchRemoved = %d, want 1", res.ScratchRemoved)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch dir should be gone, stat err = %v", err)
	}

	list := exec.Command("git", "worktree", "list", "--porcelain")
	list.Dir = parent
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v\n%s", err, out)
	}
	if strings.Contains(string(out), wt) {
		t.Errorf("parent's worktree bookkeeping still references the reaped worktree:\n%s", out)
	}

	// The real proof: the parent can register a NEW worktree at the same path,
	// which a wedged parent (stale .git/worktrees/<name> metadata) would refuse.
	readd := exec.Command("git", "worktree", "add", "--detach", wt, "HEAD")
	readd.Dir = parent
	if out, err := readd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add after reap: %v\n%s", err, out)
	}
}
