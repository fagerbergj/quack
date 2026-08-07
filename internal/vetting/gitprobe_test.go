package vetting

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	augmentFromRepo(context.Background(), &act, cfg)

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
	augmentFromRepo(context.Background(), &act, cfg)
	if act.committed || len(act.written) != 0 || len(act.stagedDelivery) != 0 {
		t.Fatalf("clean branch must change nothing: %+v", act)
	}
}

func TestAugmentFromRepo_SkipsNonSetupNodes(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.Setup = nil
	act := workerActivity{fetched: map[string]fetchRecord{}, seen: map[string]string{}, paths: map[string]bool{}}
	augmentFromRepo(context.Background(), &act, cfg)
	if act.committed {
		t.Fatal("the probe must only fire for setup-provisioned nodes")
	}
}

// #710: chained nodes share one clone, so diffing from the reflog's oldest
// entry showed every sibling's commits too - the change-shape criteria then
// failed a node for work it never did and could not remove. NodeBaseSHA scopes
// the diff to this node's own contribution.
func TestDiffSinceScopesToNodeBaseSHA(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		res, err := workspace.RunArgv(context.Background(), dir, append([]string{"git"}, args...), workspace.DefaultCaps())
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("git %v: %v (exit %d): %s", args, err, res.ExitCode, res.Output)
		}
		return strings.TrimSpace(res.Output)
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("base.txt", "base\n")
	run("add", "-A")
	run("commit", "-qm", "base")

	// Sibling node's commit - present in the clone before this node starts.
	write("sibling.kt", "sibling work\n")
	run("add", "-A")
	run("commit", "-qm", "sibling")
	nodeBase := run("rev-parse", "HEAD")

	// This node's own commit.
	write("mine.kt", "my work\n")
	run("add", "-A")
	run("commit", "-qm", "mine")

	caps := workspace.DefaultCaps()
	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "diff", nodeBase + "...HEAD"}, caps)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("diff: %v", err)
	}
	if strings.Contains(res.Output, "sibling.kt") {
		t.Errorf("node-scoped diff leaked a sibling's file:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "mine.kt") {
		t.Errorf("node-scoped diff missing this node's own file:\n%s", res.Output)
	}

	// The old behaviour, for contrast: from the reflog base both appear.
	b, err := baseCommit(dir, caps)
	if err != nil {
		t.Fatalf("baseCommit: %v", err)
	}
	res2, _ := workspace.RunArgv(context.Background(), dir, []string{"git", "diff", b + "...HEAD"}, caps)
	if !strings.Contains(res2.Output, "sibling.kt") {
		t.Skip("reflog base did not include the sibling commit; environment-dependent, nothing to contrast")
	}
}

// #762 test case 3: resetCloneToNodeBase is computed purely from
// cfg.NodeBaseSHA - it must undo a rejected round's commit(s) regardless of
// what they contain, never by reading commit messages or diffs for topicality.
func TestResetCloneToNodeBase_RewindsToStampedSHA(t *testing.T) {
	cfg := probeRepo(t, false) // base commit + empty work branch, nothing committed yet
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil {
		t.Fatal(err)
	}
	cfg.NodeBaseSHA = cloneHeadSHA(cfg)

	// The rejected round's own commit - resetCloneToNodeBase never looks at
	// its message or diff, only at cfg.NodeBaseSHA.
	writeFile(t, filepath.Join(dir, "unrelated.go"), "package whatever")
	run := func(args ...string) {
		t.Helper()
		res, err := workspace.RunArgv(context.Background(), dir, append([]string{"git"}, args...), workspace.DefaultCaps())
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("git %v: %v (exit %d): %s", args, err, res.ExitCode, res.Output)
		}
	}
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("add", "-A")
	run("commit", "-q", "-m", "totally unrelated work")

	resetCloneToNodeBase(cfg)

	head := gitLine(dir, workspace.DefaultCaps(), "rev-parse", "HEAD")
	if head != cfg.NodeBaseSHA {
		t.Fatalf("HEAD = %s, want cfg.NodeBaseSHA %s (reset must land exactly on the stamped base)", head, cfg.NodeBaseSHA)
	}
	if fileExists(dir, "unrelated.go") {
		t.Fatal("the rejected round's file survived the reset")
	}
}

// resetCloneToNodeBase must no-op (never touch the clone) when there's nothing
// safe to reset against: a read-only node, no clone, or no stamped base.
func TestResetCloneToNodeBase_NoopsWithoutABase(t *testing.T) {
	cfg := probeRepo(t, true)
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil {
		t.Fatal(err)
	}
	cfg.NodeBaseSHA = cloneHeadSHA(cfg) // stamped AFTER the one commit - i.e. HEAD already
	headBefore := gitLine(dir, workspace.DefaultCaps(), "rev-parse", "HEAD")

	for name, c := range map[string]Config{
		"read-only":   {ReadOnly: true, Setup: cfg.Setup, Workspace: cfg.Workspace, WorkspaceUserID: cfg.WorkspaceUserID, ChatID: cfg.ChatID, NodeID: cfg.NodeID, NodeBaseSHA: cfg.NodeBaseSHA},
		"no setup":    {Setup: nil, Workspace: cfg.Workspace, WorkspaceUserID: cfg.WorkspaceUserID, ChatID: cfg.ChatID, NodeID: cfg.NodeID, NodeBaseSHA: cfg.NodeBaseSHA},
		"no base sha": {Setup: cfg.Setup, Workspace: cfg.Workspace, WorkspaceUserID: cfg.WorkspaceUserID, ChatID: cfg.ChatID, NodeID: cfg.NodeID, NodeBaseSHA: ""},
	} {
		t.Run(name, func(t *testing.T) {
			resetCloneToNodeBase(c)
			if head := gitLine(dir, workspace.DefaultCaps(), "rev-parse", "HEAD"); head != headBefore {
				t.Fatalf("HEAD moved from %s to %s - resetCloneToNodeBase must no-op here", headBefore, head)
			}
		})
	}
}
