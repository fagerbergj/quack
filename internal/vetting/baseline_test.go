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

// git runs a git command in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// clonedRepoConfig builds a Config whose Workdir is a real CLONE of a source
// repo - the same shape the worker's git_clone leaves behind, so the baseline
// (the clone's original HEAD) is genuinely distinct from whatever the worker
// commits on top. seed writes the source repo's committed content.
func clonedRepoConfig(t *testing.T, checks []string, seed map[string]string) (Config, string) {
	t.Helper()
	origin := t.TempDir()
	git(t, origin, "init", "-q", "-b", "main")
	for name, content := range seed {
		if err := os.WriteFile(filepath.Join(origin, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, origin, "add", "-A")
	git(t, origin, "commit", "-qm", "base")

	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := j.UserRoot("u1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	git(t, root, "clone", "-q", "--depth", "1", "file://"+origin, repo)

	return Config{
		Checks: checks, Workdir: "repo",
		Workspace: j, WorkspaceUserID: "u1", WorkspaceCaps: workspace.DefaultCaps(),
		NodeID: "n1",
	}, repo
}

// The bug (live dogfood 2026-07-27, quack 0.16.0): the baseline worktree was
// created under the SERVER's os.TempDir(), but the git that populates it runs as
// a sandboxed child whose grants cover the node dir, its $HOME and the sandbox's
// own tmp - not /tmp. So `git worktree add` died with "could not create leading
// directories of '/tmp/quack-base-*/wt/.git': Permission denied", failsAtBase
// reported "does not fail at base", and a Go-only change was gated three rounds
// running on a frontend build failure it never caused. The baseline dir has to
// live where the sandbox already lets the child write.
func TestRunAtBaseUsesTheSandboxTmpDir(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, []string{"true"}, map[string]string{"a.txt": "x"})
	caps := cfg.WorkspaceCaps
	caps.Sandbox = workspace.SandboxLandlock
	caps.HomeDir = t.TempDir()
	granted := workspace.SandboxTmpDir(caps)
	if granted == os.TempDir() {
		t.Fatalf("SandboxTmpDir returned the server's own tmp %q - the grant set does not cover it", granted)
	}

	base, err := baseCommit(repo, caps)
	if err != nil {
		t.Fatalf("baseCommit: %v", err)
	}
	// runAtBase deletes its scratch dir before returning, so the check itself is
	// the witness: it runs INSIDE the worktree, so its cwd is that dir. The record
	// goes under the granted $HOME - anywhere else and the sandbox denies the
	// write, which is the point of the mode.
	record := filepath.Join(caps.HomeDir, "cwd")
	if _, err := runAtBase(repo, base, "pwd | tee "+record, caps, nil); err != nil {
		t.Fatalf("runAtBase: %v", err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the base check never ran: %v", err)
	}
	wt := strings.TrimSpace(string(got))
	if !strings.HasPrefix(wt, granted) {
		t.Errorf("baseline worktree ran at %q, want it under the granted tmp %q", wt, granted)
	}
}

// The bug (live e2e 2026-07-13): the target repo's `lint` already failed on its
// base commit (pre-existing eslint errors in a game the worker never touched).
// The worker's own code was clean, yet the gate failed it 5 rounds running on a
// check it could not possibly win. A check that already fails at base is repo
// debt, not a regression - it must not gate the node.
func TestChecksPassPreExistingFailureDoesNotGate(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, []string{"ls broken"}, map[string]string{"a.txt": "a"})
	// The worker's own (unrelated) change.
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("worker"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (the check already failed at the base commit - pre-existing, not the node's fault): %s", got.Score, got.Reason)
	}
}

// The other half: a check that PASSED at base and fails now is a real
// regression the worker caused - it must still fail the node. The worker even
// COMMITTED its change here, so the base can't be read off the current HEAD.
func TestChecksPassRegressionStillGates(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, []string{"ls marker"}, map[string]string{"marker": "here"})
	if err := os.Remove(filepath.Join(repo, "marker")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-qam", "worker breaks it")

	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 0 {
		t.Errorf("Score = %v, want 0 (the check passed at base and the worker's change broke it)", got.Score)
	}
	if !strings.Contains(got.Reason, "ls marker") {
		t.Errorf("Reason = %q, want it to name the failing check", got.Reason)
	}
}

func TestChecksPassPassingCheckNeedsNoBaseline(t *testing.T) {
	cfg, _ := clonedRepoConfig(t, []string{"ls marker"}, map[string]string{"marker": "here"})
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (%s)", got.Score, got.Reason)
	}
}

// The dangerous failure mode: baselining must NEVER disturb the worker's tree.
// Losing its uncommitted work would be catastrophic, so assert the tree is
// byte-for-byte intact - and that git still sees the worker's changes - after
// the baseline ran.
func TestChecksPassBaselineLeavesWorkerTreeIntact(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, []string{"ls broken"}, map[string]string{"a.txt": "a"})
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("worker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("worker edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok := checksPassCriterion(context.Background(), cfg); !ok {
		t.Fatal("checks_pass should apply")
	}

	for name, want := range map[string]string{"new.txt": "worker", "a.txt": "worker edited"} {
		got, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			t.Fatalf("worker's %s is gone after baselining: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q (baselining clobbered the worker's tree)", name, got, want)
		}
	}
	out, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "new.txt") || !strings.Contains(string(out), "a.txt") {
		t.Errorf("git status = %q, want the worker's uncommitted changes still there", out)
	}
}

// #839: quack's own repo self-disarms nearly every derived check because they
// all fail in a fresh clone (embed.go needs `make plugins`, mermaid tests need
// `npm ci`), so a missing bootstrap gets waived exactly like real repo debt
// and the gate loses its teeth. check_setup runs a repo-declared bootstrap
// once, in BOTH the worker's tree and the baseline worktree (runAtBase), so a
// check that only fails for lack of bootstrapping regains real teeth: the
// check here can never pass without check_setup (generated.txt never exists
// anywhere), and the worker's own regression is that its content diverges
// from the source setup projects it from.
func TestCheckSetupMakesABaseFailingCheckGateAgain(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, []string{"grep -q hello generated.txt"}, map[string]string{"src.txt": "hello"})
	cfg.CheckSetup = []string{"cp src.txt generated.txt"}
	if err := os.WriteFile(filepath.Join(repo, "src.txt"), []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 0 {
		t.Errorf("Score = %v, want 0 - check_setup ran at base too, so the base check passes and the worker's regression is real: %s", got.Score, got.Reason)
	}
	if strings.Contains(got.Reason, "ignored, not your fault") {
		t.Errorf("Reason = %q, the check must not have been waived as pre-existing", got.Reason)
	}
}

// The other half: leave the worker's change alone, so the only thing
// check_setup changes is whether the check can pass at all.
func TestCheckSetupPassesWhenWorkerLeavesSourceAlone(t *testing.T) {
	cfg, _ := clonedRepoConfig(t, []string{"grep -q hello generated.txt"}, map[string]string{"src.txt": "hello"})
	cfg.CheckSetup = []string{"cp src.txt generated.txt"}

	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (setup ran, check passes, nothing waived): %s", got.Score, got.Reason)
	}
}

// A broken bootstrap must never become a new way to fail a node - the
// existing base-failure self-disarm (TestChecksPassPreExistingFailureDoesNotGate)
// keeps protecting the worker exactly as it did before check_setup existed.
func TestCheckSetupFailureFallsBackToBaseFailureSelfDisarm(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, []string{"ls broken"}, map[string]string{"a.txt": "a"})
	cfg.CheckSetup = []string{"false"} // deliberately broken bootstrap command
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("worker"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (a broken check_setup must not fail the worker for the repo's own sins): %s", got.Score, got.Reason)
	}
}

// No check_setup configured (the zero value every pre-#839 test in this file
// leaves it at) must behave byte-identically to before the feature existed.
func TestCheckSetupUnconfiguredIsUnchanged(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, []string{"ls marker"}, map[string]string{"marker": "here"})
	if cfg.CheckSetup != nil {
		t.Fatal("clonedRepoConfig must not set CheckSetup - this test pins the no-config default")
	}
	if err := os.Remove(filepath.Join(repo, "marker")); err != nil {
		t.Fatal(err)
	}
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 0 {
		t.Errorf("Score = %v, want 0 (unchanged regression-gates-without-setup behavior)", got.Score)
	}
}
