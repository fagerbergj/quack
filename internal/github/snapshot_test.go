package github

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// diffFor returns one commit's unified diff (mirrors what commitDiff fetches
// from GitHub's v3.diff media type) - used to compute a real patch-id in
// tests without hitting the network.
func diffFor(t *testing.T, dir, sha string) string {
	t.Helper()
	cmd := exec.Command("git", "log", "-p", "-1", "--format=format:", sha)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log -p %s: %v\n%s", sha, err, out)
	}
	return string(out)
}

func patchIDFor(t *testing.T, dir, sha string) string {
	t.Helper()
	pid, err := gitPatchID(context.Background(), diffFor(t, dir, sha))
	if err != nil {
		t.Fatalf("gitPatchID(%s): %v", sha, err)
	}
	if pid == "" {
		t.Fatalf("gitPatchID(%s) = \"\"; want a non-empty patch id", sha)
	}
	return pid
}

func writeCommit(t *testing.T, dir, file, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, dir, "add", "-A")
	runGitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@t.co", "commit", "--quiet", "-m", msg)
	return runGitTest(t, dir, "rev-parse", "HEAD")
}

// TestGitPatchIDStableAcrossRebase pins the core rebase-safety mechanism: the
// same patch content produces the same patch-id even after a rebase rewrites
// the commit's SHA and parent.
func TestGitPatchIDStableAcrossRebase(t *testing.T) {
	requireGitBinary(t)
	dir := t.TempDir()
	runGitTest(t, dir, "init", "-q", "--initial-branch=main")
	runGitTest(t, dir, "config", "user.name", "t")
	runGitTest(t, dir, "config", "user.email", "t@t.co")
	sha1 := writeCommit(t, dir, "a.txt", "hello\n", "add a")
	pid1 := patchIDFor(t, dir, sha1)

	// Simulate "rebased onto a newer base": amend committer date/env so the
	// SHA changes while the diff content is identical (a real `git rebase`
	// does the same thing to every replayed commit).
	os.Setenv("GIT_COMMITTER_DATE", "2030-01-01T00:00:00")
	defer os.Unsetenv("GIT_COMMITTER_DATE")
	runGitTest(t, dir, "commit", "--amend", "--quiet", "--no-edit")
	sha1Rebased := runGitTest(t, dir, "rev-parse", "HEAD")
	if sha1Rebased == sha1 {
		t.Fatal("amend did not change the SHA; test fixture is broken")
	}
	pid1Rebased := patchIDFor(t, dir, sha1Rebased)
	if pid1Rebased != pid1 {
		t.Errorf("patch-id changed across a SHA-only rewrite: %s != %s", pid1Rebased, pid1)
	}
}

// TestDiffSnapshotsRebaseNoNewWork pins required test (a) from #459: a PR
// reviewed at commits [c1,c2] gets rebased onto a newer base - same patches,
// new SHAs. The delta must report ZERO new commits, not "all new" and not an
// error on a SHA that's no longer reachable the old way.
func TestDiffSnapshotsRebaseNoNewWork(t *testing.T) {
	requireGitBinary(t)
	dir := t.TempDir()
	runGitTest(t, dir, "init", "-q", "--initial-branch=main")
	runGitTest(t, dir, "config", "user.name", "t")
	runGitTest(t, dir, "config", "user.email", "t@t.co")
	c1 := writeCommit(t, dir, "a.txt", "a\n", "add a")
	c2 := writeCommit(t, dir, "b.txt", "b\n", "add b")
	old := Snapshot{Commits: []snapshotCommit{
		{SHA: c1, PatchID: patchIDFor(t, dir, c1)},
		{SHA: c2, PatchID: patchIDFor(t, dir, c2)},
	}}

	// Rebase: replay both commits onto a fresh base (simulated the same way
	// as TestGitPatchIDStableAcrossRebase - amend rewrites the SHA chain).
	runGitTest(t, dir, "checkout", "--orphan", "newbase")
	runGitTest(t, dir, "reset", "--hard")
	writeCommit(t, dir, "base.txt", "base\n", "unrelated base commit")
	runGitTest(t, dir, "checkout", "main")
	runGitTest(t, dir, "rebase", "newbase")
	c1r := runGitTest(t, dir, "rev-parse", "HEAD~1")
	c2r := runGitTest(t, dir, "rev-parse", "HEAD")
	if c1r == c1 || c2r == c2 {
		t.Fatal("rebase did not rewrite SHAs; test fixture is broken")
	}
	cur := Snapshot{Commits: []snapshotCommit{
		{SHA: c1r, PatchID: patchIDFor(t, dir, c1r)},
		{SHA: c2r, PatchID: patchIDFor(t, dir, c2r)},
	}}

	delta := diffSnapshots(old, cur, 0)
	if len(delta.NewCommits) != 0 {
		t.Errorf("NewCommits = %+v; want zero - a rebase with no new work must not read as new", delta.NewCommits)
	}
}

// TestDiffSnapshotsRebasePlusOneNewCommit pins required test (b): the same
// rebase as above, PLUS a genuinely new commit c3. The delta must be EXACTLY
// c3 - not [c1,c2,c3].
func TestDiffSnapshotsRebasePlusOneNewCommit(t *testing.T) {
	requireGitBinary(t)
	dir := t.TempDir()
	runGitTest(t, dir, "init", "-q", "--initial-branch=main")
	runGitTest(t, dir, "config", "user.name", "t")
	runGitTest(t, dir, "config", "user.email", "t@t.co")
	c1 := writeCommit(t, dir, "a.txt", "a\n", "add a")
	c2 := writeCommit(t, dir, "b.txt", "b\n", "add b")
	old := Snapshot{Commits: []snapshotCommit{
		{SHA: c1, PatchID: patchIDFor(t, dir, c1)},
		{SHA: c2, PatchID: patchIDFor(t, dir, c2)},
	}}

	runGitTest(t, dir, "checkout", "--orphan", "newbase2")
	runGitTest(t, dir, "reset", "--hard")
	writeCommit(t, dir, "base.txt", "base\n", "unrelated base commit")
	runGitTest(t, dir, "checkout", "main")
	runGitTest(t, dir, "rebase", "newbase2")
	c3 := writeCommit(t, dir, "c.txt", "c\n", "add c")
	cur := Snapshot{Commits: []snapshotCommit{
		{SHA: runGitTest(t, dir, "rev-parse", "HEAD~2"), PatchID: patchIDFor(t, dir, runGitTest(t, dir, "rev-parse", "HEAD~2"))},
		{SHA: runGitTest(t, dir, "rev-parse", "HEAD~1"), PatchID: patchIDFor(t, dir, runGitTest(t, dir, "rev-parse", "HEAD~1"))},
		{SHA: c3, PatchID: patchIDFor(t, dir, c3)},
	}}

	delta := diffSnapshots(old, cur, 0)
	if len(delta.NewCommits) != 1 || delta.NewCommits[0].SHA != c3 {
		t.Errorf("NewCommits = %+v; want exactly [%s]", delta.NewCommits, c3)
	}
}

// TestDiffSnapshotsForcePushDropsCommit pins required test (c): reviewed at
// [c1,c2,c3], force-pushed down to [c1,c2] (c3 dropped). The delta must not
// error and must not re-flag c1/c2 as new.
func TestDiffSnapshotsForcePushDropsCommit(t *testing.T) {
	requireGitBinary(t)
	dir := t.TempDir()
	runGitTest(t, dir, "init", "-q", "--initial-branch=main")
	runGitTest(t, dir, "config", "user.name", "t")
	runGitTest(t, dir, "config", "user.email", "t@t.co")
	c1 := writeCommit(t, dir, "a.txt", "a\n", "add a")
	c2 := writeCommit(t, dir, "b.txt", "b\n", "add b")
	c3 := writeCommit(t, dir, "c.txt", "c\n", "add c")
	old := Snapshot{Commits: []snapshotCommit{
		{SHA: c1, PatchID: patchIDFor(t, dir, c1)},
		{SHA: c2, PatchID: patchIDFor(t, dir, c2)},
		{SHA: c3, PatchID: patchIDFor(t, dir, c3)},
	}}

	runGitTest(t, dir, "reset", "--hard", c2) // force-push equivalent: drop c3
	cur := Snapshot{Commits: []snapshotCommit{
		{SHA: c1, PatchID: patchIDFor(t, dir, c1)},
		{SHA: c2, PatchID: patchIDFor(t, dir, c2)},
	}}

	delta := diffSnapshots(old, cur, 0)
	if len(delta.NewCommits) != 0 {
		t.Errorf("NewCommits = %+v; want zero - kept commits must not be re-flagged", delta.NewCommits)
	}
}

// TestDiffSnapshotsCommentLifecycle pins the general (non-commit) delta
// mechanics: added/edited/deleted comments, title/state/label changes - all
// keyed by stable id, never by position.
func TestDiffSnapshotsCommentLifecycle(t *testing.T) {
	old := Snapshot{
		Title: "Old title", State: "open", Labels: []string{"bug"},
		Comments: []snapshotComment{
			{ID: 1, User: "alice", Body: "first", UpdatedAt: "t0"},
			{ID: 2, User: "bob", Body: "will be deleted", UpdatedAt: "t0"},
		},
	}
	cur := Snapshot{
		Title: "New title", State: "closed", Labels: []string{"bug", "priority:high"},
		Comments: []snapshotComment{
			{ID: 1, User: "alice", Body: "first - edited", UpdatedAt: "t1"},
			{ID: 3, User: "carol", Body: "brand new", UpdatedAt: "t1"},
		},
	}
	d := diffSnapshots(old, cur, 0)
	if !d.TitleChanged || d.OldTitle != "Old title" || d.NewTitle != "New title" {
		t.Errorf("title delta = %+v", d)
	}
	if !d.StateChanged || d.OldState != "open" || d.NewState != "closed" {
		t.Errorf("state delta = %+v", d)
	}
	if len(d.LabelsAdded) != 1 || d.LabelsAdded[0] != "priority:high" {
		t.Errorf("LabelsAdded = %+v", d.LabelsAdded)
	}
	if len(d.CommentsAdded) != 1 || d.CommentsAdded[0].ID != 3 {
		t.Errorf("CommentsAdded = %+v", d.CommentsAdded)
	}
	if len(d.CommentsEdited) != 1 || d.CommentsEdited[0].ID != 1 {
		t.Errorf("CommentsEdited = %+v", d.CommentsEdited)
	}
	if len(d.CommentsDeleted) != 1 || d.CommentsDeleted[0].ID != 2 {
		t.Errorf("CommentsDeleted = %+v", d.CommentsDeleted)
	}
	if d.Empty() {
		t.Error("delta with real changes reported Empty()")
	}

	// An identical resnapshot yields an empty delta - the resume-with-nothing
	// -new case (#459's "injects an empty delta, not the whole thread again").
	if noop := diffSnapshots(cur, cur, 0); !noop.Empty() {
		t.Errorf("diffSnapshots(cur, cur) = %+v; want Empty()", noop)
	}
}

// TestRenderDeltaDetailOmitsUnchanged pins that the rendered delta text only
// ever mentions what actually changed.
func TestRenderDeltaDetailOmitsUnchanged(t *testing.T) {
	d := diffSnapshots(
		Snapshot{Comments: []snapshotComment{{ID: 1, User: "alice", Body: "old"}}},
		Snapshot{Comments: []snapshotComment{{ID: 1, User: "alice", Body: "old"}, {ID: 2, User: "bob", Body: "new one"}}},
		0,
	)
	text := renderDeltaDetail(d)
	if !strings.Contains(text, "new one") {
		t.Errorf("delta text missing the new comment:\n%s", text)
	}
	if strings.Contains(text, "old") {
		t.Errorf("delta text should not mention the unchanged comment:\n%s", text)
	}

	if got := renderDeltaDetail(Delta{}); got != "" {
		t.Errorf("renderDeltaDetail(Delta{}) = %q; want \"\"", got)
	}
}

// TestMarshalUnmarshalSnapshotRoundTrip pins the store's opaque JSON
// encode/decode.
func TestMarshalUnmarshalSnapshotRoundTrip(t *testing.T) {
	snap := Snapshot{
		Title: "t", Body: "b", State: "open", Labels: []string{"bug"},
		IsPR: true, HeadRef: "feat/x", HeadSHA: "abc", BaseRef: "main",
		Comments: []snapshotComment{{ID: 1, User: "alice", Body: "hi"}},
		Commits:  []snapshotCommit{{SHA: "abc", PatchID: "pid1", Message: "msg"}},
	}
	j, err := marshalSnapshot(snap)
	if err != nil {
		t.Fatalf("marshalSnapshot: %v", err)
	}
	got, err := unmarshalSnapshot(j)
	if err != nil {
		t.Fatalf("unmarshalSnapshot: %v", err)
	}
	if got.Title != snap.Title || got.HeadSHA != snap.HeadSHA || len(got.Comments) != 1 || len(got.Commits) != 1 {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, snap)
	}
}
