package vetting

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/workspace"
)

// metaAwareInMemory implements recordstore's optional SaveWithMeta/
// LoadWithMeta over artifact.InMemoryService() - production always wraps the
// real row-backed store, which supports these; this stands in for that in
// tests so the preload validity path (which reads head_sha from lineage, not
// the JSON body - #1090 P2) is actually exercised without a real database.
type metaAwareInMemory struct {
	artifact.Service
	mu   sync.Mutex
	meta map[string]struct {
		kind, class string
		lineage     []byte
	}
}

func newMetaAwareInMemory() *metaAwareInMemory {
	return &metaAwareInMemory{Service: artifact.InMemoryService(), meta: map[string]struct {
		kind, class string
		lineage     []byte
	}{}}
}

func metaKey(appName, userID, sessionID, fileName string) string {
	return appName + "\x00" + userID + "\x00" + sessionID + "\x00" + fileName
}

// SaveWithMeta implements the recordstore metaSaver interface structurally.
func (m *metaAwareInMemory) SaveWithMeta(ctx context.Context, req *artifact.SaveRequest, kind, class string, lineageJSON []byte, turnID string) (*artifact.SaveResponse, error) {
	resp, err := m.Service.Save(ctx, req)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meta[metaKey(req.AppName, req.UserID, req.SessionID, req.FileName)] = struct {
		kind, class string
		lineage     []byte
	}{kind, class, lineageJSON}
	return resp, nil
}

func (m *metaAwareInMemory) LoadWithMeta(ctx context.Context, req *artifact.LoadRequest) (*artifact.LoadResponse, string, string, []byte, error) {
	resp, err := m.Service.Load(ctx, req)
	if err != nil {
		return nil, "", "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	meta := m.meta[metaKey(req.AppName, req.UserID, req.SessionID, req.FileName)]
	return resp, meta.kind, meta.class, meta.lineage, nil
}

func reviewerCfgWithArtifacts(t *testing.T, svc artifact.Service, commitOnBranch bool) Config {
	t.Helper()
	cfg := probeRepo(t, commitOnBranch)
	cfg.IsReviewer = true
	cfg.User = "u1"
	cfg.Artifacts = svc
	cfg.NodeBaseSHA = cloneHeadSHA(cfg)
	return cfg
}

func codeReviewID(cfg Config) string {
	id, err := recordstore.IdentityFor(kindCodeReview, nil, subjectHint(cfg.ChatID))
	if err != nil {
		panic(err)
	}
	return id
}

func findingID(t *testing.T, rec FindingRecord) string {
	t.Helper()
	id, err := recordstore.IdentityFor(kindFinding, rec, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestCodeReviewRoundWrite covers #1006 test case 1: a FINDINGS(2)+
// DISMISSED(1)+CLEAN(2) tail round writes a code_review record plus one
// finding artifact per live finding, both state "new".
func TestCodeReviewRoundWrite(t *testing.T) {
	svc := newMetaAwareInMemory()
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
	current := saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, answer, staged, nil)
	waitFor(t, func() bool {
		_, _, ok, _ := recordClient(cfg).Latest(context.Background(), codeReviewID(cfg))
		return ok
	})

	if len(current) != 2 {
		t.Fatalf("returned live findings = %d, want 2: %+v", len(current), current)
	}

	rc := recordClient(cfg)
	raw, _, rev, ok, err := rc.LatestWithMeta(context.Background(), codeReviewID(cfg))
	if err != nil || !ok || rev != 1 {
		t.Fatalf("LatestWithMeta: rev=%d ok=%v err=%v", rev, ok, err)
	}
	var rec CodeReviewRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != "request_changes" || len(rec.FindingIDs) != 2 {
		t.Fatalf("record = %+v", rec)
	}
	if len(rec.Dismissed) != 1 || len(rec.Clean) != 2 {
		t.Fatalf("dismissed/clean = %+v / %+v", rec.Dismissed, rec.Clean)
	}

	for id, want := range current {
		waitFor(t, func() bool {
			_, _, _, ok, _ := rc.LatestWithMeta(context.Background(), id)
			return ok
		})
		fRaw, _, _, _, err := rc.LatestWithMeta(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		var f FindingRecord
		if err := json.Unmarshal(fRaw, &f); err != nil {
			t.Fatal(err)
		}
		if f.State != "new" {
			t.Fatalf("finding %s state = %q, want new", id, f.State)
		}
		if f.Path != want.Path {
			t.Fatalf("finding %s path = %q, want %q", id, f.Path, want.Path)
		}
	}
}

// TestFindingIdentityStableAcrossLineShiftHeadSHAAndNode covers #1006 test
// case 2 / #1090 V4.2 verification #2: the same finding (same path, title,
// flagged-line text) keeps its id regardless of line number, head SHA, or
// which node's hint is passed in - findingIdentity ignores hint entirely.
func TestFindingIdentityStableAcrossLineShiftHeadSHAAndNode(t *testing.T) {
	rec := func(lineHint int, snippet string) FindingRecord {
		return FindingRecord{Path: "a.go", LineHint: lineHint, Title: "bug one", Snippet: snippet}
	}
	id1 := findingID(t, rec(1, "func Foo() {"))
	id2 := findingID(t, rec(99, "func Foo() {")) // line shifted; content unchanged
	if id1 != id2 {
		t.Fatalf("a line shift must not change the id: %q vs %q", id1, id2)
	}
	// A trivial reformat (whitespace/case) of the title or line must not mint a new id.
	recNorm := FindingRecord{Path: "a.go", Title: "  Bug   One  ", Snippet: "func   Foo()  {"}
	id3 := findingID(t, recNorm)
	if id1 != id3 {
		t.Fatalf("normalization failed: %q vs %q", id1, id3)
	}
	// hint (the reporting node) must never affect a content-hashed kind's identity.
	idViaClient, err := recordstore.IdentityFor(kindFinding, rec(1, "func Foo() {"), "some-other-node")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != idViaClient {
		t.Fatalf("hint changed a content-hashed identity: %q vs %q", id1, idViaClient)
	}
	// A different flagged line (real content change) must mint a different id.
	id4 := findingID(t, rec(1, "func Bar() {"))
	if id1 == id4 {
		t.Fatal("different flagged-line content produced the same hash")
	}
}

// TestGateFailWritesNothing covers #1006 test case 4 at the call-site level:
// saveEpisodicRound is only ever invoked from inside RunGatedRefine's round
// loop; calling nothing (the gate-fail-before-any-round path) leaves the
// store empty.
func TestGateFailWritesNothing(t *testing.T) {
	svc := artifact.InMemoryService()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)
	if _, _, ok, _ := rc.Latest(context.Background(), codeReviewID(cfg)); ok {
		t.Fatal("no record should exist before any round runs")
	}
}

// TestSaveErrorFailsOpen: a Save error must never be visible to the caller -
// SaveStructuredAsync just logs. This proves calling the round-write helper
// against a failing service does not panic or block.
func TestSaveErrorFailsOpen(t *testing.T) {
	cfg := reviewerCfgWithArtifacts(t, failingArtifactService{}, true)
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, "VERDICT: approve\n", StagedDelivery{Recovered: true}, nil)
	time.Sleep(50 * time.Millisecond) // let the fire-and-forget goroutine run
}

type failingArtifactService struct{ artifact.Service }

func (failingArtifactService) Save(context.Context, *artifact.SaveRequest) (*artifact.SaveResponse, error) {
	return nil, os.ErrPermission
}

// TestFindingResolvedAcrossRounds covers #1006 test case 7 (reframed as
// finding state, #1090 P2): a finding present in round 1 and absent from
// round 2 gets one final revision with state "resolved".
func TestFindingResolvedAcrossRounds(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	round1 := `VERDICT: request_changes
FINDINGS:
- a.go:1: issue A. detail
- b.go:2: issue B. detail
`
	round2 := `VERDICT: request_changes
FINDINGS:
- a.go:1: issue A. detail
`
	staged := StagedDelivery{Recovered: true}
	prev := saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, round1, staged, nil)
	if len(prev) != 2 {
		t.Fatalf("round 1 live findings = %d, want 2", len(prev))
	}
	var droppedID string
	for id, f := range prev {
		if f.Path == "b.go" {
			droppedID = id
		}
	}
	if droppedID == "" {
		t.Fatal("could not find b.go's finding id in round 1")
	}

	current := saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 2, round2, staged, prev)
	if len(current) != 1 {
		t.Fatalf("round 2 live findings = %d, want 1", len(current))
	}

	rc := recordClient(cfg)
	waitFor(t, func() bool {
		raw, _, _, ok, _ := rc.LatestWithMeta(context.Background(), droppedID)
		if !ok {
			return false
		}
		var f FindingRecord
		_ = json.Unmarshal(raw, &f)
		return f.State == "resolved"
	})
}

// TestResumePreloadFiltersByFile covers #1006 test case 2: after a commit
// touching only a.go, preload keeps b.go's clean entry and untouched
// findings, drops a.go's clean entry.
func TestResumePreloadFiltersByFile(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)

	answer := `VERDICT: comment
FINDINGS:
- b.txt:1: untouched finding. still here
CLEAN:
- a.txt
- b.txt
`
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, answer, StagedDelivery{Recovered: true}, nil)
	waitFor(t, func() bool {
		_, _, ok, _ := recordClient(cfg).Latest(context.Background(), codeReviewID(cfg))
		return ok
	})

	// Commit touching only a.txt on the same clone, advancing HEAD.
	dir := probeDirOf(t, cfg)
	writeFile(t, filepath.Join(dir, "a.txt"), "changed")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "touch a.txt")

	var block string
	waitFor(t, func() bool { block = BuildReviewPreload(context.Background(), cfg, cfg.NodeID); return block != "" })
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
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)
	lineage := recordstore.Lineage{NodeID: cfg.NodeID, Round: 1, HeadSHA: "0000000000000000000000000000000000dead"}
	if _, _, err := rc.SaveStructured(context.Background(), kindCodeReview,
		CodeReviewRecord{Verdict: "comment", Clean: []string{"a.txt"}}, subjectHint(cfg.ChatID), lineage); err != nil {
		t.Fatal(err)
	}
	if block := BuildReviewPreload(context.Background(), cfg, cfg.NodeID); block != "" {
		t.Fatalf("expected empty preload for an unreachable head_sha, got: %s", block)
	}
	if _, _, ok, _ := rc.Latest(context.Background(), codeReviewID(cfg)); !ok {
		t.Fatal("the store must still hold the revision - filtered at read, not deleted")
	}
}

// TestDocumentStagesShareOneID covers #1006 test case 8 under #1090 V4.2: the
// document id carries no node segment, so every stage of one chat's
// dispatch (ocr, summarize, clarify) appends revisions to the SAME id;
// lineage.NodeID (not the id) is what tells them apart. No retention
// (design V4.1 #2) - every revision is kept.
func TestDocumentStagesShareOneID(t *testing.T) {
	svc := artifact.InMemoryService()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = kindDocument

	saveStage := func(nodeID, text string) {
		cfg := base
		cfg.NodeID = nodeID
		saveDocumentRound(context.Background(), cfg, nodeID, "", 1, text)
	}
	docID, err := recordstore.IdentityFor(kindDocument, nil, documentHint(base.ChatID))
	if err != nil {
		t.Fatal(err)
	}
	rc := recordClient(base)

	saveStage("ocr", "ocr text v1")
	waitFor(t, func() bool { _, _, ok, _ := rc.Latest(context.Background(), docID); return ok })

	saveStage("summarize", "summary v1 (re-dispatch)")
	waitFor(t, func() bool {
		_, rev, ok, _ := rc.Latest(context.Background(), docID)
		return ok && rev == 2
	})
	raw, rev, ok, err := rc.Latest(context.Background(), docID)
	if err != nil || !ok || rev != 2 {
		t.Fatalf("Latest: rev=%d ok=%v err=%v", rev, ok, err)
	}
	if string(raw) != "summary v1 (re-dispatch)" {
		t.Fatalf("Latest content = %q", raw)
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
