package vetting

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// probeRepo builds a setup-style clone under the jail: base commit, work
// branch, optionally one commit on it. Returns the cfg pointing at it.
func probeRepo(t *testing.T, commitOnBranch bool) Config {
	t.Helper()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := jail.EnsureDir("u1", "c1", workspace.SetupCloneDir("n1"))
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	writeFile(t, filepath.Join(dir, "base.txt"), "base")
	git("add", "-A")
	git("commit", "-q", "-m", "base commit")
	git("checkout", "-q", "-b", "quack/work")
	if commitOnBranch {
		writeFile(t, filepath.Join(dir, "x.go"), "package x")
		git("add", "-A")
		git("commit", "-q", "-m", "add x package")
	}
	return Config{
		Workspace:       jail,
		WorkspaceUserID: "u1",
		ChatID:          "c1",
		NodeID:          "n1",
		WorkspaceCaps:   workspace.DefaultCaps(),
		Setup:           &SetupBranch{Repo: "https://example.com/r.git", WorkBranch: "quack/work"},
		Task:            "Implement the feature, commit, and open a pull request",
		Deliver:         func(ctx context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) { return nil, nil },
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAugmentFromRepo_ReadsCommitsOffDisk(t *testing.T) {
	cfg := probeRepo(t, true)
	act := workerActivity{fetched: map[string]fetchRecord{}, seen: map[string]string{}, paths: map[string]bool{}}
	augmentFromRepo(&act, cfg)

	if !act.committed {
		t.Fatal("a commit on the work branch must mark committed")
	}
	if act.currentBranch != "quack/work" {
		t.Fatalf("branch: got %q", act.currentBranch)
	}
	wantPath := joinWritten(workspace.NodeDir("n1"), "x.go")
	if len(act.written) != 1 || act.written[0] != wantPath {
		t.Fatalf("written: got %v, want [%s]", act.written, wantPath)
	}
	pr, ok := act.stagedDelivery["pr"]
	if !ok {
		t.Fatal("a PR-demanding task with commits must synthesize a staged PR")
	}
	// Kind must be the delivery discriminator github's deliverOne switches on
	// ("pull_request"), NOT the staging-slot key - a live delivery failed on
	// kind "pr" while the judge had already passed the node.
	if pr.Kind != "pull_request" || pr.Title != "add x package" || pr.Branch != "quack/work" {
		t.Fatalf("staged PR: %+v", pr)
	}
}

func TestAugmentFromRepo_NoCommitsNoChange(t *testing.T) {
	cfg := probeRepo(t, false)
	act := workerActivity{fetched: map[string]fetchRecord{}, seen: map[string]string{}, paths: map[string]bool{}}
	augmentFromRepo(&act, cfg)
	if act.committed || len(act.written) != 0 || len(act.stagedDelivery) != 0 {
		t.Fatalf("clean branch must change nothing: %+v", act)
	}
}

func TestAugmentFromRepo_SkipsNonSetupNodes(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.Setup = nil
	act := workerActivity{fetched: map[string]fetchRecord{}, seen: map[string]string{}, paths: map[string]bool{}}
	augmentFromRepo(&act, cfg)
	if act.committed {
		t.Fatal("the probe must only fire for setup-provisioned nodes")
	}
}
