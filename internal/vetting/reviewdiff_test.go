package vetting

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestChangedFilesSection_ReviewNodeGetsDiff: a review node's act.written is always empty, so with
// cfg.IsReviewer set the judge gets the actual base..HEAD diff off the clone instead.
func TestChangedFilesSection_ReviewNodeGetsDiff(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.IsReviewer = true
	// A reviewer is read-only: it wrote nothing itself.
	act := workerActivity{}

	got, _ := changedFilesSection(cfg, act)
	if !strings.Contains(got, "DIFF UNDER REVIEW") {
		t.Fatalf("missing diff header:\n%s", got)
	}
	if !strings.Contains(got, "x.go") || !strings.Contains(got, "package x") {
		t.Fatalf("missing the actual diff content:\n%s", got)
	}
}

// TestChangedFilesSection_ImplementNodeNeverGetsReviewHeader: a non-review node never gets the
// review-only header, even with a setup clone present.
func TestChangedFilesSection_ImplementNodeNeverGetsReviewHeader(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.IsReviewer = false
	act := workerActivity{written: []string{workspace.NodeDir("n1") + "/x.go"}}

	got, _ := changedFilesSection(cfg, act)
	if strings.Contains(got, "DIFF UNDER REVIEW") {
		t.Fatalf("implement node must not get the review-framed diff header:\n%s", got)
	}
	if !strings.Contains(got, "ACTUAL CURRENT CONTENT") {
		t.Fatalf("implement node must keep the act.written-based section:\n%s", got)
	}
}

// TestChangedFilesSection_ImplementNodeGetsDiffAndContent: an implement node with a clone gets BOTH the
// base..HEAD diff (for change-shape criteria) and the full re-read file content (for whole-file criteria).
func TestChangedFilesSection_ImplementNodeGetsDiffAndContent(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.IsReviewer = false
	act := workerActivity{written: []string{workspace.NodeDir("n1") + "/x.go"}}

	got, _ := changedFilesSection(cfg, act)
	if !strings.Contains(got, "ACTUAL DIFF THIS NODE PRODUCED") || !strings.Contains(got, "package x") {
		t.Fatalf("missing the actual diff section:\n%s", got)
	}
	if !strings.Contains(got, "ACTUAL CURRENT CONTENT") {
		t.Fatalf("missing the full-content section:\n%s", got)
	}
}

// TestChangedFilesSection_ImplementNodeNoCloneKeepsWrittenOnly: with no Setup clone, an empty
// buildImplementDiffSection must not blank out the act.written section.
func TestChangedFilesSection_ImplementNodeNoCloneKeepsWrittenOnly(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.IsReviewer = false
	cfg.Setup = nil
	act := workerActivity{written: []string{workspace.NodeDir("n1") + "/x.go"}}

	got, _ := changedFilesSection(cfg, act)
	if strings.Contains(got, "ACTUAL DIFF THIS NODE PRODUCED") {
		t.Fatalf("no clone must yield no diff section:\n%s", got)
	}
	if !strings.Contains(got, "ACTUAL CURRENT CONTENT") {
		t.Fatalf("missing the full-content section:\n%s", got)
	}
}

// TestChangedFilesSection_CarriesStagedVerdict: the judge sees the reviewer's structured verdict
// (act.stagedDelivery["review"]) as a fact, not something to infer from prose.
func TestChangedFilesSection_CarriesStagedVerdict(t *testing.T) {
	for _, event := range []string{"approve", "request_changes"} {
		t.Run(event, func(t *testing.T) {
			cfg := probeRepo(t, true)
			cfg.IsReviewer = true
			act := workerActivity{stagedDelivery: map[string]StagedDelivery{
				"review": {Kind: "review", Event: event, Body: "looks good"},
			}}

			got, _ := changedFilesSection(cfg, act)
			want := "Staged review verdict: " + event
			if !strings.Contains(got, want) {
				t.Fatalf("missing %q in:\n%s", want, got)
			}

			prompt := buildJudgePrompt("", "rubric text", "", "", questionContent("review this"), "looks good", got, act, "")
			if !strings.Contains(prompt, want) {
				t.Fatalf("judge prompt missing %q:\n%s", want, prompt)
			}
		})
	}

	// No staged review yet (e.g. a fallback comment with no VERDICT: tail) ⇒
	// no fabricated verdict line.
	t.Run("unstaged", func(t *testing.T) {
		cfg := probeRepo(t, true)
		cfg.IsReviewer = true
		got, _ := changedFilesSection(cfg, workerActivity{})
		if strings.Contains(got, "Staged review verdict:") {
			t.Fatalf("must not fabricate a verdict line when none is staged:\n%s", got)
		}
	})
}

// TestBuildReviewDiffSection_NoClone: no Setup/Workspace or no .git returns "" rather than an
// error, so a judge round never fails over a missing clone.
func TestBuildReviewDiffSection_NoClone(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.Setup = nil
	if got := buildReviewDiffSection(cfg); got != "" {
		t.Fatalf("no Setup must yield no section, got:\n%s", got)
	}

	cfg2 := probeRepo(t, true)
	cfg2.Workspace = nil
	if got := buildReviewDiffSection(cfg2); got != "" {
		t.Fatalf("no Workspace must yield no section, got:\n%s", got)
	}
}

// TestBuildReviewDiffSection_GitError: a dir that isn't a git repo (or has no resolvable base)
// degrades to "", never failing the judge round.
func TestBuildReviewDiffSection_GitError(t *testing.T) {
	cfg := probeRepo(t, true)
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the clone: it exists and has a .git dir, but no reflog/HEAD
	// history baseCommit can read.
	if out, err := exec.Command("rm", "-rf", dir+"/.git").CombinedOutput(); err != nil {
		t.Fatalf("rm .git: %v\n%s", err, out)
	}
	if got := buildReviewDiffSection(cfg); got != "" {
		t.Fatalf("a non-git dir must yield no section, got:\n%s", got)
	}
}

// TestBuildReviewDiffSection_Truncates: a diff over changedFilesBudget bytes is cut with the
// truncation marker, never handed to the judge whole.
func TestBuildReviewDiffSection_Truncates(t *testing.T) {
	cfg := probeRepo(t, false) // no commit yet - we add a big one ourselves
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
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
	writeFile(t, dir+"/big.txt", strings.Repeat("line of content\n", changedFilesBudget/8))
	git("add", "-A")
	git("commit", "-q", "-m", "add a large file")

	got := buildReviewDiffSection(cfg)
	if !strings.Contains(got, diffTruncatedMarker) {
		t.Fatalf("large diff must carry the truncation marker:\n%s", got[:200])
	}
	// changedFilesBudget bytes of diff, plus the header/marker overhead - bound
	// it generously rather than exactly, since the header text isn't budgeted.
	if len(got) > changedFilesBudget+1024 {
		t.Fatalf("diff section not bounded: %d bytes", len(got))
	}
}

// TestBuildImplementDiffSection_NoCloneOrGitError: the implement-node formatter shares diffSince,
// so it degrades to "" the same way.
func TestBuildImplementDiffSection_NoCloneOrGitError(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.Setup = nil
	if got := buildImplementDiffSection(cfg); got != "" {
		t.Fatalf("no Setup must yield no section, got:\n%s", got)
	}

	cfg2 := probeRepo(t, false) // no commit beyond base
	if got := buildImplementDiffSection(cfg2); got != "" {
		t.Fatalf("no commits beyond base must yield no section, got:\n%s", got)
	}
}
