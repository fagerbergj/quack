package vetting

import (
	"os"
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
	got, ok := checksPassCriterion(cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (all checks passed)", got.Score)
	}
}

func TestChecksPassCriterionOneFails(t *testing.T) {
	cfg := testChecksConfig(t, []string{"true", "false"}, "")
	got, ok := checksPassCriterion(cfg)
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
	got, ok := checksPassCriterion(cfg)
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
	got, ok := checksPassCriterion(cfg)
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
	got := foldDeterministic(v, "some answer", workerActivity{}, cfg)
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
	got := foldDeterministic(v, "some answer", workerActivity{}, cfg)
	if _, ok := got.Criteria["checks_pass"]; ok {
		t.Fatal("checks_pass should not appear for a node with no Checks configured")
	}
}

func TestFoldDeterministicPassingChecksDoNotFail(t *testing.T) {
	cfg := testChecksConfig(t, []string{"true"}, "")
	v := verdict{Criteria: map[string]criterionScore{"answers_question": {Score: 0.9}}}
	got := foldDeterministic(v, "some answer", workerActivity{}, cfg)
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
