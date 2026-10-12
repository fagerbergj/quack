package github

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireGitSnapshotBinary skips a test when git isn't on PATH.
func requireGitSnapshotBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH")
	}
}

// runGitSnapshotTest runs one git command in dir, failing the test on error.
func runGitSnapshotTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// diffFor returns one commit's unified diff (as commitDiff fetches via v3.diff) to compute a real patch-id offline.
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

func writeSnapshotCommit(t *testing.T, dir, file, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitSnapshotTest(t, dir, "add", "-A")
	runGitSnapshotTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@t.co", "commit", "--quiet", "-m", msg)
	return runGitSnapshotTest(t, dir, "rev-parse", "HEAD")
}

// The same patch content keeps its patch-id after a rebase rewrites the commit's SHA and parent.
func TestGitPatchIDStableAcrossRebase(t *testing.T) {
	requireGitSnapshotBinary(t)
	dir := t.TempDir()
	runGitSnapshotTest(t, dir, "init", "-q", "--initial-branch=main")
	runGitSnapshotTest(t, dir, "config", "user.name", "t")
	runGitSnapshotTest(t, dir, "config", "user.email", "t@t.co")
	sha1 := writeSnapshotCommit(t, dir, "a.txt", "hello\n", "add a")
	pid1 := patchIDFor(t, dir, sha1)

	// Amend the committer date so the SHA changes with identical diff content, as a rebase does.
	t.Setenv("GIT_COMMITTER_DATE", "2030-01-01T00:00:00")
	runGitSnapshotTest(t, dir, "commit", "--amend", "--quiet", "--no-edit")
	sha1Rebased := runGitSnapshotTest(t, dir, "rev-parse", "HEAD")
	if sha1Rebased == sha1 {
		t.Fatal("amend did not change the SHA; test fixture is broken")
	}
	pid1Rebased := patchIDFor(t, dir, sha1Rebased)
	if pid1Rebased != pid1 {
		t.Errorf("patch-id changed across a SHA-only rewrite: %s != %s", pid1Rebased, pid1)
	}
}

// A PR reviewed at [c1,c2] then rebased (same patches, new SHAs) reports zero new commits,
// not "all new" and not an error on an unreachable SHA.
func TestDiffSnapshotsRebaseNoNewWork(t *testing.T) {
	requireGitSnapshotBinary(t)
	dir := t.TempDir()
	runGitSnapshotTest(t, dir, "init", "-q", "--initial-branch=main")
	runGitSnapshotTest(t, dir, "config", "user.name", "t")
	runGitSnapshotTest(t, dir, "config", "user.email", "t@t.co")
	c1 := writeSnapshotCommit(t, dir, "a.txt", "a\n", "add a")
	c2 := writeSnapshotCommit(t, dir, "b.txt", "b\n", "add b")
	old := Snapshot{Commits: []snapshotCommit{
		{SHA: c1, PatchID: patchIDFor(t, dir, c1)},
		{SHA: c2, PatchID: patchIDFor(t, dir, c2)},
	}}

	// Rebase: replay both commits onto a fresh base (simulated the same way
	// as TestGitPatchIDStableAcrossRebase - amend rewrites the SHA chain).
	runGitSnapshotTest(t, dir, "checkout", "--orphan", "newbase")
	runGitSnapshotTest(t, dir, "reset", "--hard")
	writeSnapshotCommit(t, dir, "base.txt", "base\n", "unrelated base commit")
	runGitSnapshotTest(t, dir, "checkout", "main")
	runGitSnapshotTest(t, dir, "rebase", "newbase")
	c1r := runGitSnapshotTest(t, dir, "rev-parse", "HEAD~1")
	c2r := runGitSnapshotTest(t, dir, "rev-parse", "HEAD")
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

// The same rebase plus a genuinely new c3 reports exactly c3, not [c1,c2,c3].
func TestDiffSnapshotsRebasePlusOneNewCommit(t *testing.T) {
	requireGitSnapshotBinary(t)
	dir := t.TempDir()
	runGitSnapshotTest(t, dir, "init", "-q", "--initial-branch=main")
	runGitSnapshotTest(t, dir, "config", "user.name", "t")
	runGitSnapshotTest(t, dir, "config", "user.email", "t@t.co")
	c1 := writeSnapshotCommit(t, dir, "a.txt", "a\n", "add a")
	c2 := writeSnapshotCommit(t, dir, "b.txt", "b\n", "add b")
	old := Snapshot{Commits: []snapshotCommit{
		{SHA: c1, PatchID: patchIDFor(t, dir, c1)},
		{SHA: c2, PatchID: patchIDFor(t, dir, c2)},
	}}

	runGitSnapshotTest(t, dir, "checkout", "--orphan", "newbase2")
	runGitSnapshotTest(t, dir, "reset", "--hard")
	writeSnapshotCommit(t, dir, "base.txt", "base\n", "unrelated base commit")
	runGitSnapshotTest(t, dir, "checkout", "main")
	runGitSnapshotTest(t, dir, "rebase", "newbase2")
	c3 := writeSnapshotCommit(t, dir, "c.txt", "c\n", "add c")
	cur := Snapshot{Commits: []snapshotCommit{
		{SHA: runGitSnapshotTest(t, dir, "rev-parse", "HEAD~2"), PatchID: patchIDFor(t, dir, runGitSnapshotTest(t, dir, "rev-parse", "HEAD~2"))},
		{SHA: runGitSnapshotTest(t, dir, "rev-parse", "HEAD~1"), PatchID: patchIDFor(t, dir, runGitSnapshotTest(t, dir, "rev-parse", "HEAD~1"))},
		{SHA: c3, PatchID: patchIDFor(t, dir, c3)},
	}}

	delta := diffSnapshots(old, cur, 0)
	if len(delta.NewCommits) != 1 || delta.NewCommits[0].SHA != c3 {
		t.Errorf("NewCommits = %+v; want exactly [%s]", delta.NewCommits, c3)
	}
}

// Reviewed at [c1,c2,c3], force-pushed to [c1,c2]: the delta must not error or re-flag c1/c2 as new.
func TestDiffSnapshotsForcePushDropsCommit(t *testing.T) {
	requireGitSnapshotBinary(t)
	dir := t.TempDir()
	runGitSnapshotTest(t, dir, "init", "-q", "--initial-branch=main")
	runGitSnapshotTest(t, dir, "config", "user.name", "t")
	runGitSnapshotTest(t, dir, "config", "user.email", "t@t.co")
	c1 := writeSnapshotCommit(t, dir, "a.txt", "a\n", "add a")
	c2 := writeSnapshotCommit(t, dir, "b.txt", "b\n", "add b")
	c3 := writeSnapshotCommit(t, dir, "c.txt", "c\n", "add c")
	old := Snapshot{Commits: []snapshotCommit{
		{SHA: c1, PatchID: patchIDFor(t, dir, c1)},
		{SHA: c2, PatchID: patchIDFor(t, dir, c2)},
		{SHA: c3, PatchID: patchIDFor(t, dir, c3)},
	}}

	runGitSnapshotTest(t, dir, "reset", "--hard", c2) // force-push equivalent: drop c3
	cur := Snapshot{Commits: []snapshotCommit{
		{SHA: c1, PatchID: patchIDFor(t, dir, c1)},
		{SHA: c2, PatchID: patchIDFor(t, dir, c2)},
	}}

	delta := diffSnapshots(old, cur, 0)
	if len(delta.NewCommits) != 0 {
		t.Errorf("NewCommits = %+v; want zero - kept commits must not be re-flagged", delta.NewCommits)
	}
}

// Comment add/edit/delete and title/state/label changes are keyed by stable id, never by position.
func TestDiffSnapshotsCommentLifecycle(t *testing.T) {
	old := Snapshot{
		Title: "Old title", State: "open", Labels: []string{"bug"},
		Comments: []snapshotComment{
			{ID: 1, User: "alice", Body: "first", CreatedAt: "t0"},
			{ID: 2, User: "bob", Body: "will be deleted", CreatedAt: "t0"},
		},
	}
	cur := Snapshot{
		Title: "New title", State: "closed", Labels: []string{"bug", "priority:high"},
		Comments: []snapshotComment{
			{ID: 1, User: "alice", Body: "first - edited", CreatedAt: "t1"},
			{ID: 3, User: "carol", Body: "brand new", CreatedAt: "t1"},
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

	// An identical resnapshot yields an empty delta, not the whole thread again.
	if noop := diffSnapshots(cur, cur, 0); !noop.Empty() {
		t.Errorf("diffSnapshots(cur, cur) = %+v; want Empty()", noop)
	}
}

func TestMarshalUnmarshalSnapshotRoundTrip(t *testing.T) {
	snap := Snapshot{
		Title: "t", Body: "b", State: "open", Labels: []string{"bug"},
		IsPR: true, HeadRef: "feat/x", HeadSHA: "abc", BaseRef: "main",
		Comments: []snapshotComment{{ID: 1, User: "alice", Body: "hi"}},
		Commits:  []snapshotCommit{{SHA: "abc", PatchID: "pid1", Message: "msg"}},
	}
	j, err := marshalJSON(snap)
	if err != nil {
		t.Fatalf("marshalJSON: %v", err)
	}
	got, err := unmarshalJSON[Snapshot](j)
	if err != nil {
		t.Fatalf("unmarshalJSON: %v", err)
	}
	if got.Title != snap.Title || got.HeadSHA != snap.HeadSHA || len(got.Comments) != 1 || len(got.Commits) != 1 {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, snap)
	}
}
