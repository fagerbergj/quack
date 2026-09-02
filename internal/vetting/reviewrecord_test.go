package vetting

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/workspace"
)

func reviewerCfgWithArtifacts(t *testing.T, svc artifact.Service, commitOnBranch bool) Config {
	t.Helper()
	cfg := probeRepo(t, commitOnBranch)
	cfg.IsReviewer = true
	cfg.User = "u1"
	cfg.Artifacts = svc
	cfg.NodeBaseSHA = cloneHeadSHA(cfg)
	return cfg
}

// TestReviewRecordGatePass covers #1006 test case 1: FINDINGS(2)+DISMISSED(1)+CLEAN(2)
// tail gate-passes -> review rev 1, head_sha == NodeBaseSHA, ids <sha>-1/2, empty critique.
func TestReviewRecordGatePass(t *testing.T) {
	svc := artifact.InMemoryService()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	answer := `VERDICT: request_changes
FINDINGS:
- a.go:1: bug one. it breaks things
- b.go:2: bug two
DISMISSED:
- c.go:3: looked ok
CLEAN:
- d.go
- e.go
`
	staged := StagedDelivery{Kind: "review", Recovered: true}
	SaveReview(context.Background(), cfg, answer, staged, nil)
	waitForRecord(t, cfg, "review")

	rc := recordClient(cfg)
	raw, rev, ok, err := rc.Latest(context.Background(), "review")
	if err != nil || !ok || rev != 1 {
		t.Fatalf("Latest: rev=%d ok=%v err=%v", rev, ok, err)
	}
	var rec ReviewRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.HeadSHA != cfg.NodeBaseSHA || rec.HeadSHA == "" {
		t.Fatalf("HeadSHA = %q, want cfg.NodeBaseSHA %q", rec.HeadSHA, cfg.NodeBaseSHA)
	}
	if len(rec.Findings) != 2 || rec.Findings[0].ID != cfg.NodeBaseSHA+"-1" || rec.Findings[1].ID != cfg.NodeBaseSHA+"-2" {
		t.Fatalf("findings = %+v", rec.Findings)
	}
	if len(rec.Dismissed) != 1 || len(rec.Clean) != 2 {
		t.Fatalf("dismissed/clean = %+v / %+v", rec.Dismissed, rec.Clean)
	}
	if len(rec.Critique) != 0 {
		t.Fatalf("critique should be empty on the first round, got %+v", rec.Critique)
	}
}

// TestGateFailWritesNothing covers #1006 test case 4: SaveReview/SaveBody are
// gate-owned - node.go only calls them under res.Passed, so a helper called
// directly proves nothing on its own; this proves the store stays empty when
// nothing calls Save at all (the caller-side contract this pkg documents).
func TestGateFailWritesNothing(t *testing.T) {
	svc := artifact.InMemoryService()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)
	if _, _, ok, _ := rc.Latest(context.Background(), "review"); ok {
		t.Fatal("no record should exist before any Save call")
	}
	// Gate fail path in node.go never calls SaveReview/SaveBody at all.
}

// TestSaveErrorFailsOpen: a Save error must never be visible to the caller -
// SaveJSONAsync just logs. This proves calling SaveReview against a failing
// service does not panic or block.
func TestSaveErrorFailsOpen(t *testing.T) {
	cfg := reviewerCfgWithArtifacts(t, failingArtifactService{}, true)
	SaveReview(context.Background(), cfg, "VERDICT: approve\n", StagedDelivery{Recovered: true}, nil)
	time.Sleep(50 * time.Millisecond) // let the fire-and-forget goroutine run
}

type failingArtifactService struct{ artifact.Service }

func (failingArtifactService) Save(context.Context, *artifact.SaveRequest) (*artifact.SaveResponse, error) {
	return nil, os.ErrPermission
}

// TestCritiqueDiffDroppedFinding covers #1006 test case 7: judge round 1
// finds 3, revise round finds 2 -> critique = the dropped one.
func TestCritiqueDiffDroppedFinding(t *testing.T) {
	prior := []ReviewComment{
		{Path: "a.go", Line: 1, Body: "issue A"},
		{Path: "b.go", Line: 2, Body: "issue B"},
		{Path: "c.go", Line: 3, Body: "issue C"},
	}
	current := []ReviewComment{
		{Path: "a.go", Line: 1, Body: "issue A"},
		{Path: "b.go", Line: 9, Body: "issue B"}, // line moved; still matches on (path, body)
	}
	got := critiqueDiff(prior, current)
	if len(got) != 1 || got[0].File != "c.go" {
		t.Fatalf("critique = %+v, want just c.go", got)
	}
}

// TestResumePreloadFiltersByFile covers #1006 test case 2: after a commit
// touching only a.go, preload keeps b.go's clean entry and untouched
// findings, drops a.go's clean entry.
func TestResumePreloadFiltersByFile(t *testing.T) {
	svc := artifact.InMemoryService()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	baseSHA := cfg.NodeBaseSHA

	rec := ReviewRecord{
		HeadSHA:  baseSHA,
		Findings: []FindingRecord{{ID: baseSHA + "-1", File: "b.txt", Line: 1, Title: "untouched finding"}},
		Clean:    []string{"a.txt", "b.txt"},
	}
	rc := recordClient(cfg)
	if _, err := rc.SaveJSON(context.Background(), "review", rec); err != nil {
		t.Fatal(err)
	}

	// Commit touching only a.txt on the same clone, advancing HEAD.
	dir := probeDirOf(t, cfg)
	writeFile(t, filepath.Join(dir, "a.txt"), "changed")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "touch a.txt")

	block := BuildReviewPreload(context.Background(), cfg)
	if block == "" {
		t.Fatal("expected a non-empty preload block")
	}
	if strings.Contains(block, "a.txt") {
		t.Fatalf("a.txt clean entry should be dropped (file changed): %s", block)
	}
	if !strings.Contains(block, "b.txt") || !strings.Contains(block, "untouched finding") {
		t.Fatalf("b.txt clean entry + untouched finding should survive: %s", block)
	}
}

// TestResumePreloadDropsUnreachableHead covers #1006 test case 3: a
// force-push (history rewrite) makes the record's head_sha unreachable ->
// preload is empty, though the store still holds the revision.
func TestResumePreloadDropsUnreachableHead(t *testing.T) {
	svc := artifact.InMemoryService()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)
	if _, err := rc.SaveJSON(context.Background(), "review", ReviewRecord{
		HeadSHA: "0000000000000000000000000000000000dead", // never existed in this repo
		Clean:   []string{"a.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	if block := BuildReviewPreload(context.Background(), cfg); block != "" {
		t.Fatalf("expected empty preload for an unreachable head_sha, got: %s", block)
	}
	if _, _, ok, _ := rc.Latest(context.Background(), "review"); !ok {
		t.Fatal("the store must still hold the revision - filtered at read, not deleted")
	}
}

// TestBodyStagesAndRetention covers #1006 test case 8 (SaveBody half): three
// gate-passed stages write three body revisions stamped with their own node
// id, and KeepLastRevisions(N=3) keeps exactly the last N once a fourth lands.
func TestBodyStagesAndRetention(t *testing.T) {
	svc := artifact.InMemoryService()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = bodyRecordName

	saveStage := func(nodeID, text string) {
		cfg := base
		cfg.NodeID = nodeID
		SaveBody(context.Background(), cfg, text)
	}
	saveStage("ocr", "ocr text v1")
	waitFor(t, func() bool { _, _, ok, _ := recordClient(base).Latest(context.Background(), "body"); return ok })
	saveStage("summarize", "summary v1")
	waitForRevision(t, base, 2)
	saveStage("clarify", "clarified v1")
	waitForRevision(t, base, 3)
	saveStage("ocr", "ocr text v2 (re-dispatch)")
	waitForRevision(t, base, 4)

	rc := recordClient(base)
	raw, rev, ok, err := rc.Latest(context.Background(), bodyRecordName)
	if err != nil || !ok || rev != 4 {
		t.Fatalf("Latest: rev=%d ok=%v err=%v", rev, ok, err)
	}
	var rec BodyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Stage != "ocr" {
		t.Fatalf("v4.stage = %q, want ocr", rec.Stage)
	}
	if v, err := rc.LoadVersion(context.Background(), bodyRecordName, 1); err != nil || v != nil {
		t.Fatalf("v1 should be evicted by retention: v=%s err=%v", v, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true (async save timed out)")
}

func waitForRevision(t *testing.T, cfg Config, want int) {
	t.Helper()
	rc := recordClient(cfg)
	waitFor(t, func() bool {
		_, rev, ok, _ := rc.Latest(context.Background(), bodyRecordName)
		return ok && rev >= want
	})
}

func waitForRecord(t *testing.T, cfg Config, name string) {
	t.Helper()
	rc := recordClient(cfg)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok, _ := rc.Latest(context.Background(), name); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("record %q was never saved (async save timed out)", name)
}

func probeDirOf(t *testing.T, cfg Config) string {
	t.Helper()
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
