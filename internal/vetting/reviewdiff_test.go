package vetting

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestChangedFilesSection_ReviewNodeGetsDiff pins #498 step 1: a review
// node's act.written is always empty (read-only), so before this fix the
// judge's changedFiles slot was empty too and it could only score the
// review's internal consistency. With cfg.IsReviewer set, changedFilesSection
// must source the actual base..HEAD diff off the clone instead.
func TestChangedFilesSection_ReviewNodeGetsDiff(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.IsReviewer = true
	// A reviewer is read-only: it wrote nothing itself.
	act := workerActivity{}

	got := changedFilesSection(cfg, act)
	if !strings.Contains(got, "DIFF UNDER REVIEW") {
		t.Fatalf("missing diff header:\n%s", got)
	}
	if !strings.Contains(got, "x.go") || !strings.Contains(got, "package x") {
		t.Fatalf("missing the actual diff content:\n%s", got)
	}
}

// TestChangedFilesSection_ImplementNodeUnchanged pins that a non-review node
// keeps sourcing changedFiles from act.written - the review-diff path must
// never fire for it, even when a setup clone is present.
func TestChangedFilesSection_ImplementNodeUnchanged(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.IsReviewer = false
	act := workerActivity{written: []string{workspace.NodeDir("n1") + "/x.go"}}

	got := changedFilesSection(cfg, act)
	if strings.Contains(got, "DIFF UNDER REVIEW") {
		t.Fatalf("implement node must not get the review-diff section:\n%s", got)
	}
	if !strings.Contains(got, "ACTUAL CURRENT CONTENT") {
		t.Fatalf("implement node must keep the act.written-based section:\n%s", got)
	}
}

// TestChangedFilesSection_CarriesStagedVerdict pins #520: the judge must see
// the reviewer's STRUCTURED verdict (stage_review's event / the answer's
// VERDICT: tail, already resolved into act.stagedDelivery["review"]) as a
// fact, not have to infer it from summary prose the reviewer is told never to
// restate it in.
func TestChangedFilesSection_CarriesStagedVerdict(t *testing.T) {
	for _, event := range []string{"approve", "request_changes"} {
		t.Run(event, func(t *testing.T) {
			cfg := probeRepo(t, true)
			cfg.IsReviewer = true
			act := workerActivity{stagedDelivery: map[string]StagedDelivery{
				"review": {Kind: "review", Event: event, Body: "looks good"},
			}}

			got := changedFilesSection(cfg, act)
			want := "Staged review verdict: " + event
			if !strings.Contains(got, want) {
				t.Fatalf("missing %q in:\n%s", want, got)
			}

			prompt := buildJudgePrompt("", "rubric text", "", questionContent("review this"), "looks good", got, act)
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
		got := changedFilesSection(cfg, workerActivity{})
		if strings.Contains(got, "Staged review verdict:") {
			t.Fatalf("must not fabricate a verdict line when none is staged:\n%s", got)
		}
	})
}

// TestBuildReviewDiffSection_NoClone pins the fallback: no Setup/Workspace or
// no .git in the resolved dir must return "" rather than error, so a judge
// round never fails over a missing clone.
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

// TestBuildReviewDiffSection_GitError pins the fallback when the resolved dir
// exists but isn't a git repo (or the base commit can't be determined) - a
// git error must degrade to "", never fail the judge round.
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

// TestBuildReviewDiffSection_Truncates pins the budget cap: a diff larger than
// changedFilesBudget bytes must be cut down to size with the truncation
// marker, never handed to the judge whole.
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
	if !strings.Contains(got, reviewDiffTruncatedMarker) {
		t.Fatalf("large diff must carry the truncation marker:\n%s", got[:200])
	}
	// changedFilesBudget bytes of diff, plus the header/marker overhead - bound
	// it generously rather than exactly, since the header text isn't budgeted.
	if len(got) > changedFilesBudget+1024 {
		t.Fatalf("diff section not bounded: %d bytes", len(got))
	}
}
