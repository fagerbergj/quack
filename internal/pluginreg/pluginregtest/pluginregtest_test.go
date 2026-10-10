package pluginregtest

import (
	"strings"
	"testing"
)

// Exercises the fixture helpers directly so this package's statements count as covered in its own package.
func TestNewFixtureRepoAndCommitAndPush(t *testing.T) {
	bare, work := NewFixtureRepo(t)
	if bare == "" || work == "" {
		t.Fatalf("NewFixtureRepo returned empty paths: bare=%q work=%q", bare, work)
	}
	sha := CommitAndPush(t, work, "v2")
	if len(strings.TrimSpace(sha)) != 40 {
		t.Fatalf("CommitAndPush sha = %q, want a 40-char sha", sha)
	}
	out := RunGit(t, work, "rev-parse", "HEAD")
	if strings.TrimSpace(out) != sha {
		t.Fatalf("HEAD = %q, want the pushed sha %q", strings.TrimSpace(out), sha)
	}
}
