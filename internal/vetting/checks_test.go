package vetting

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

func testChecksConfig(t *testing.T, checks []string, workdir string) Config {
	t.Helper()
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
	return Config{
		Checks: checks, Workdir: workdir,
		Workspace: j, WorkspaceUserID: "u1", WorkspaceCaps: workspace.DefaultCaps(),
	}
}

func TestChecksPassCriterionAllPass(t *testing.T) {
	cfg := testChecksConfig(t, []string{"true", "true"}, "")
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (all checks passed)", got.Score)
	}
}

func TestChecksPassCriterionOneFails(t *testing.T) {
	cfg := testChecksConfig(t, []string{"true", "false"}, "")
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (a check failed)", got.Score)
	}
	if !strings.Contains(got.Reason, "false") {
		t.Errorf("Reason = %q, want it to name the failing command", got.Reason)
	}
}

func TestChecksPassCriterionNoWorkspaceFailsClosed(t *testing.T) {
	cfg := Config{Checks: []string{"true"}} // Workspace deliberately nil
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply (explicit Checks, fail closed)")
	}
	if got.Score != 0 {
		t.Errorf("Score = %v, want 0 (fail closed with no workspace wired up)", got.Score)
	}
}

func TestChecksPassCriterionOutputInReason(t *testing.T) {
	// A real failing command with distinctive stderr output — ls on a path
	// that doesn't exist reliably prints a recognizable error and exits
	// non-zero, without needing a shell (RunArgv never invokes one).
	cfg := testChecksConfig(t, []string{"ls /quack-checks-test-does-not-exist-xyz"}, "")
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0", got.Score)
	}
	if !strings.Contains(got.Reason, "quack-checks-test-does-not-exist-xyz") {
		t.Errorf("Reason = %q, want the command's output tail", got.Reason)
	}
}

func TestFoldDeterministicFoldsChecksPass(t *testing.T) {
	cfg := testChecksConfig(t, []string{"false"}, "")
	v := verdict{Criteria: map[string]criterionScore{"answers_question": {Score: 1}}}
	got := foldDeterministic(context.Background(), v, "some answer", workerActivity{}, cfg)
	c, ok := got.Criteria["checks_pass"]
	if !ok {
		t.Fatal("checks_pass criterion missing")
	}
	if c.Score != 0 {
		t.Errorf("checks_pass score = %v, want 0", c.Score)
	}
	if got.Score != 0 {
		t.Errorf("overall Score = %v, want 0 (weakest-link on checks_pass)", got.Score)
	}
}

func TestFoldDeterministicNodeWithoutChecksUntouched(t *testing.T) {
	cfg := Config{} // no Checks configured
	v := verdict{Criteria: map[string]criterionScore{"answers_question": {Score: 1}}}
	got := foldDeterministic(context.Background(), v, "some answer", workerActivity{}, cfg)
	if _, ok := got.Criteria["checks_pass"]; ok {
		t.Fatal("checks_pass should not appear for a node with no Checks configured")
	}
}

func TestFoldDeterministicPassingChecksDoNotFail(t *testing.T) {
	cfg := testChecksConfig(t, []string{"true"}, "")
	v := verdict{Criteria: map[string]criterionScore{"answers_question": {Score: 0.9}}}
	got := foldDeterministic(context.Background(), v, "some answer", workerActivity{}, cfg)
	c, ok := got.Criteria["checks_pass"]
	if !ok {
		t.Fatal("checks_pass criterion missing")
	}
	if c.Score != 1 {
		t.Errorf("checks_pass score = %v, want 1 (all checks passed)", c.Score)
	}
	if got.Score != 0.9 {
		t.Errorf("overall Score = %v, want 0.9 (checks_pass shouldn't drag it down)", got.Score)
	}
}

func TestComposeFeedbackAppendsFailingCriteriaReasons(t *testing.T) {
	v := verdict{
		Feedback: "judge's own narrative",
		Criteria: map[string]criterionScore{
			"answers_question": {Score: 0.9, Reason: "fine"},
			"checks_pass":      {Score: 0, Reason: "check \"go test ./...\" failed (exit 1): some failure"},
		},
	}
	got := composeFeedback(v, 0.7)
	if !strings.Contains(got, "judge's own narrative") {
		t.Errorf("composeFeedback dropped the judge's own feedback: %q", got)
	}
	if !strings.Contains(got, "go test ./...") {
		t.Errorf("composeFeedback = %q, want the failing check's reason included", got)
	}
	if strings.Contains(got, "fine") {
		t.Errorf("composeFeedback = %q, should not include a PASSING criterion's reason", got)
	}
}

func TestComposeFeedbackNoFailuresReturnsJudgeFeedbackUnchanged(t *testing.T) {
	v := verdict{
		Feedback: "all good",
		Criteria: map[string]criterionScore{"answers_question": {Score: 1, Reason: "great"}},
	}
	got := composeFeedback(v, 0.7)
	if got != "all good" {
		t.Errorf("composeFeedback = %q, want unchanged judge feedback %q", got, "all good")
	}
}

// TestChecksDirFindsTheNodesOwnRepo: two concurrent nodes each cloned a repo
// into the same chat scope. Derived checks must find THIS node's repo — a
// search from the chat root sees two repos, gives up ("no single repo"), and
// nothing gets gated.
func TestChecksDirFindsTheNodesOwnRepo(t *testing.T) {
	cfg := testChecksConfig(t, nil, "")
	cfg.ChatID = "chat-1"
	cfg.NodeID = "impl_node"
	cfg.DeriveChecks = true
	for _, rel := range []string{"impl_node/mine/.git", "other_node/theirs/.git"} {
		dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, rel)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir, ok, err := checksDir(cfg)
	if err != nil {
		t.Fatalf("checksDir: %v", err)
	}
	if !ok {
		t.Fatal("checksDir found no repo: a search from the chat root sees BOTH nodes' clones and gives up")
	}
	if !strings.HasSuffix(dir, "impl_node/mine") {
		t.Errorf("checksDir = %q, want the node's own clone (…/impl_node/mine)", dir)
	}
}

// A default-ON check_commands allowlist must be safe on a host WITHOUT the
// toolchain: deriveChecks additionally gates each candidate on its binary
// existing (toolchainPresent), so a missing `go` derives nothing instead of
// failing every node with exit 127.
func TestDeriveChecks_SkipsMissingToolchain(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := []string{"go build", "go vet", "go test"}

	if got := deriveChecks(dir, allow); len(got) != 3 {
		t.Fatalf("with go on PATH, want 3 derived checks, got %v", got)
	}

	t.Setenv("PATH", t.TempDir()) // a PATH with no binaries at all
	if got := deriveChecks(dir, allow); len(got) != 0 {
		t.Fatalf("without the toolchain, want no derived checks, got %v", got)
	}
}

// A repo with more than one toolchain present (e.g. package.json + go.mod, as
// quack's own repo root has) must derive checks from ALL of them, not just the
// first match — see #349.
func TestDeriveChecks_UnionsMultipleToolchains(t *testing.T) {
	dir := t.TempDir()
	pkg := `{"scripts": {"build": "tsc", "test": "vitest"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := []string{"npm run", "go build", "go vet", "go test"}

	got := deriveChecks(dir, allow)
	want := []string{"npm run build", "npm run test", "go build ./...", "go vet ./...", "go test ./..."}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("want checks from both npm and go in order %v, got %v", want, got)
	}
}

// A single-ecosystem repo is unaffected: still just its own toolchain's checks.
func TestDeriveChecks_SingleEcosystemUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := []string{"go build", "go vet", "go test"}

	got := deriveChecks(dir, allow)
	want := []string{"go build ./...", "go vet ./...", "go test ./..."}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("want only go checks %v, got %v", want, got)
	}
}
