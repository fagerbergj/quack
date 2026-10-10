package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/workspace"
)

// metaAwareInMemory adds recordstore's optional SaveWithMeta/LoadWithMeta over InMemoryService, so the preload
// validity path (which reads head_sha from lineage) runs without a real database.
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
	id, err := recordstore.IdentityFor(kindCodeReview, nil, SubjectHint(cfg.ChatID))
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

// TestCodeReviewRoundWrite: a FINDINGS(2)+DISMISSED(1)+CLEAN(2) tail round writes a code_review
// record plus one finding artifact per live finding, both state "new".
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
	st := newEpisodicRoundState()
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, answer, staged, st)
	current := st.findings

	if len(current) != 2 {
		t.Fatalf("returned live findings = %d, want 2: %+v", len(current), current)
	}

	rc := recordClient(cfg)
	raw, _, _, rev, ok, err := rc.LatestWithMeta(context.Background(), codeReviewID(cfg))
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
			_, _, _, _, ok, _ := rc.LatestWithMeta(context.Background(), id)
			return ok
		})
		fRaw, _, _, _, _, err := rc.LatestWithMeta(context.Background(), id)
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

// TestSaveCodeReviewRound_ToolWriteSkipsTailFallback: when write_code_review already wrote round 1's record, the gate
// adopts it instead of parsing the tail. st starts fresh, so detection must go through toolWritten, not a baseline.
func TestSaveCodeReviewRound_ToolWriteSkipsTailFallback(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolStage := NewToolWrittenStage()
	RegisterMemSession("sec-1108-b2", MemSession{ToolWritten: toolStage})
	MarkMemSessionConnected("sec-1108-b2")
	defer UnregisterMemSession("sec-1108-b2")
	token := "tok-1108-b2"
	RegisterAdvisorThread(token, AdvisorTask{MemSecret: "sec-1108-b2"})
	defer UnregisterAdvisorThread(token)
	cfg.AdvisorToken = token

	toolWritten := CodeReviewRecord{Verdict: "approve", Takeaway: "written directly via write_code_review"}
	crID, toolRev, err := rc.SaveStructured(context.Background(), kindCodeReview, toolWritten, SubjectHint(cfg.ChatID), recordstore.Lineage{NodeID: cfg.NodeID, Author: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	toolStage.Add(crID)

	// A malformed/contradictory tail: if this were parsed, verdict would
	// become request_changes - proving the fallback path never ran.
	answer := "VERDICT: request_changes\nFINDINGS:\n- a.go:1: should never be recorded\n"
	staged := StagedDelivery{Kind: "review", Recovered: true}
	st := newEpisodicRoundState() // round 1's real baseline: fresh, not seeded
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, answer, staged, st)

	raw, _, _, rev, ok, err := rc.LatestWithMeta(context.Background(), codeReviewID(cfg))
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if rev != toolRev {
		t.Fatalf("revision = %d, want the tool's write (%d) - tail fallback ran when it shouldn't have", rev, toolRev)
	}
	var rec CodeReviewRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != "approve" {
		t.Fatalf("verdict = %q, want the tool-written %q (tail fallback must not overwrite it)", rec.Verdict, "approve")
	}
	if st.reviewRev != toolRev {
		t.Fatalf("st.reviewRev = %d, want %d so the next round's ParentRevision is correct", st.reviewRev, toolRev)
	}
}

// TestSaveCodeReviewRound_ToolWrittenFindingsSkipTailDuplicate: 3 write_finding calls aren't re-staged by the tail
// parse; only the tail's new 4th finding gets a revision, and finding_ids lists all 4.
func TestSaveCodeReviewRound_ToolWrittenFindingsSkipTailDuplicate(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolStage := NewToolWrittenStage()
	RegisterMemSession("sec-1091-f1", MemSession{ToolWritten: toolStage})
	MarkMemSessionConnected("sec-1091-f1")
	defer UnregisterMemSession("sec-1091-f1")
	token := "tok-1091-f1"
	RegisterAdvisorThread(token, AdvisorTask{MemSecret: "sec-1091-f1"})
	defer UnregisterAdvisorThread(token)
	cfg.AdvisorToken = token

	// Tool-written findings, as write_finding's MCP handler saves them. Snippet "" matches what fileLineAtForCfg
	// resolves for the missing file, so the hash-derived ids line up with the tail parse.
	toolFindings := []FindingRecord{
		{Path: "a.go", Title: "bug one", State: "new"},
		{Path: "b.go", Title: "bug two", State: "new"},
		{Path: "c.go", Title: "bug three", State: "new"},
	}
	toolIDs := make(map[string]int) // id -> revision written
	for _, rec := range toolFindings {
		id, rev, err := rc.SaveStructured(context.Background(), kindFinding, rec, "", recordstore.Lineage{NodeID: cfg.NodeID, Round: 1, Author: "worker"})
		if err != nil {
			t.Fatal(err)
		}
		toolIDs[id] = rev
		toolStage.Add(id)
	}

	// The answer tail describes the same 3 findings (same path/title/snippet) plus one new one.
	answer := `VERDICT: request_changes
FINDINGS:
- a.go:1: bug one.
- b.go:2: bug two.
- c.go:3: bug three.
- d.go:4: bug four.
`
	staged := StagedDelivery{Kind: "review", Recovered: true}
	st := newEpisodicRoundState()
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 1, answer, staged, st)

	// Exactly one NEW finding revision (d.go); the 3 tool-written ones stay
	// at their original revision - no duplicate, no ParentRevision-0 rewrite.
	for id, wantRev := range toolIDs {
		_, _, lineage, rev, ok, err := rc.LatestWithMeta(context.Background(), id)
		if err != nil || !ok {
			t.Fatalf("tool-written finding %s missing: ok=%v err=%v", id, ok, err)
		}
		if rev != wantRev {
			t.Fatalf("tool-written finding %s revision = %d, want unchanged %d (fallback duplicated it)", id, rev, wantRev)
		}
		if rev > 1 && lineage.ParentRevision == 0 {
			t.Fatalf("tool-written finding %s got a bogus ParentRevision 0 rewrite", id)
		}
	}

	raw, _, _, rev, ok, err := rc.LatestWithMeta(context.Background(), codeReviewID(cfg))
	if err != nil || !ok || rev != 1 {
		t.Fatalf("code_review LatestWithMeta: rev=%d ok=%v err=%v", rev, ok, err)
	}
	var rec CodeReviewRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.FindingIDs) != 4 {
		t.Fatalf("code_review.finding_ids = %v, want all 4 (3 tool-written + 1 new)", rec.FindingIDs)
	}
	for id := range toolIDs {
		found := false
		for _, fid := range rec.FindingIDs {
			if fid == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("code_review.finding_ids missing tool-written id %s: %v", id, rec.FindingIDs)
		}
	}

	// The one new finding (d.go) got exactly revision 1 - a real new write,
	// not zero and not a duplicate of anything.
	var newID string
	for _, fid := range rec.FindingIDs {
		if _, isTool := toolIDs[fid]; !isTool {
			newID = fid
		}
	}
	if newID == "" {
		t.Fatal("expected exactly one non-tool-written new finding id")
	}
	_, _, newLineage, newRev, ok, err := rc.LatestWithMeta(context.Background(), newID)
	if err != nil || !ok || newRev != 1 {
		t.Fatalf("new finding %s: rev=%d ok=%v err=%v, want rev=1", newID, newRev, ok, err)
	}
	if newLineage.ParentRevision != 0 {
		t.Fatalf("new finding %s parent_revision = %d, want 0 (genuinely new)", newID, newLineage.ParentRevision)
	}
}

// TestToolWrittenStageResetsPerRound: an id written via write_finding in round 1 must not suppress
// round N's tail-parse write of the same id; the stage is drained between rounds.
func TestToolWrittenStageResetsPerRound(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolStage := NewToolWrittenStage()
	RegisterMemSession("sec-1108-f2", MemSession{ToolWritten: toolStage})
	MarkMemSessionConnected("sec-1108-f2")
	defer UnregisterMemSession("sec-1108-f2")
	token := "tok-1108-f2"
	RegisterAdvisorThread(token, AdvisorTask{MemSecret: "sec-1108-f2"})
	defer UnregisterAdvisorThread(token)
	cfg.AdvisorToken = token

	rec := FindingRecord{Path: "a.go", Title: "bug one", State: "new"}
	id, rev1, err := rc.SaveStructured(context.Background(), kindFinding, rec, "", recordstore.Lineage{NodeID: cfg.NodeID, Round: 1, Author: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	toolStage.Add(id)

	// Round 1: seeded from the tool write, no tail-parse write for it.
	st := newEpisodicRoundState()
	staged := StagedDelivery{Kind: "review", Recovered: true}
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 1, "VERDICT: request_changes\nFINDINGS:\n", staged, st)
	if _, ok := toolStage.Snapshot()[id]; ok {
		t.Fatalf("ToolWrittenStage still holds %s after round 1 - not drained", id)
	}

	// Round 2: the same id, now only in the answer tail, is a normal write, not "already written this round".
	answer2 := "VERDICT: request_changes\nFINDINGS:\n- a.go:1: bug one.\n"
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t2", 2, answer2, staged, st)

	_, _, lineage, rev2, ok, err := rc.LatestWithMeta(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if rev2 <= rev1 {
		t.Fatalf("round 2 revision = %d, want > round 1's %d - the tail-parse write for round 2 never happened (stale suppression)", rev2, rev1)
	}
	if lineage.Round != 2 {
		t.Fatalf("lineage.round = %d, want 2", lineage.Round)
	}
}

// TestSaveCodeReviewRoundLogsAndSkipsOnSeedReadFailure: when a tool-written id can't be re-read while seeding,
// the finding doesn't silently vanish and the tail parse doesn't stamp a fabricated ParentRevision.
func TestSaveCodeReviewRoundLogsAndSkipsOnSeedReadFailure(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolStage := NewToolWrittenStage()
	RegisterMemSession("sec-1108-f3a", MemSession{ToolWritten: toolStage})
	MarkMemSessionConnected("sec-1108-f3a")
	defer UnregisterMemSession("sec-1108-f3a")
	token := "tok-1108-f3a"
	RegisterAdvisorThread(token, AdvisorTask{MemSecret: "sec-1108-f3a"})
	defer UnregisterAdvisorThread(token)
	cfg.AdvisorToken = token

	rec := FindingRecord{Path: "a.go", Title: "bug one", State: "new"}
	id, _, err := rc.SaveStructured(context.Background(), kindFinding, rec, "", recordstore.Lineage{NodeID: cfg.NodeID, Round: 1, Author: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	toolStage.Add(id)

	// Every Load now fails; the early toolRev short-circuit is unaffected since no code_review record exists yet.
	cfg.Artifacts = &alwaysFailLoadService{}

	st := newEpisodicRoundState()
	staged := StagedDelivery{Kind: "review", Recovered: true}
	answer := "VERDICT: request_changes\nFINDINGS:\n- a.go:1: bug one.\n"
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 1, answer, staged, st)

	if _, live := st.findings[id]; live {
		t.Fatalf("finding %s should not be recorded live this round when its seed-read failed", id)
	}
}

// TestSaveCodeReviewRound_ToolWroteCodeReviewStillSeedsFindings: a revise round that tool-writes a finding and
// code_review must still refresh st.findingRev before short-circuiting, or a later write stamps a stale parent.
func TestSaveCodeReviewRound_ToolWroteCodeReviewStillSeedsFindings(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolStage := NewToolWrittenStage()
	RegisterMemSession("sec-1108-b3", MemSession{ToolWritten: toolStage})
	MarkMemSessionConnected("sec-1108-b3")
	defer UnregisterMemSession("sec-1108-b3")
	token := "tok-1108-b3"
	RegisterAdvisorThread(token, AdvisorTask{MemSecret: "sec-1108-b3"})
	defer UnregisterAdvisorThread(token)
	cfg.AdvisorToken = token

	// Round 1: tail-parse creates finding F at revision 1 (no tool writes).
	st := newEpisodicRoundState()
	staged := StagedDelivery{Kind: "review", Recovered: true}
	round1Answer := "VERDICT: request_changes\nFINDINGS:\n- a.go:1: bug one.\n"
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 1, round1Answer, staged, st)
	var findingID string
	for id := range st.findings {
		findingID = id
	}
	if findingID == "" {
		t.Fatal("round 1 did not record a live finding")
	}
	rev1 := st.findingRev[findingID]

	// Round 2: the worker tool-writes a new revision of the same finding, then write_code_review;
	// the seed loop must still refresh st.findingRev[findingID] before the early return.
	fRec := FindingRecord{Path: "a.go", Title: "bug one", State: "new"}
	_, rev2, err := rc.SaveStructured(context.Background(), kindFinding, fRec, "", recordstore.Lineage{NodeID: cfg.NodeID, Round: 2, ParentRevision: rev1, Author: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if rev2 <= rev1 {
		t.Fatalf("round 2 tool write revision = %d, want > round 1's %d", rev2, rev1)
	}
	toolStage.Add(findingID)
	crRec := CodeReviewRecord{Verdict: "request_changes", FindingIDs: []string{findingID}}
	crID, _, err := rc.SaveStructured(context.Background(), kindCodeReview, crRec, SubjectHint(cfg.ChatID), recordstore.Lineage{NodeID: cfg.NodeID, Round: 2, ParentRevision: st.reviewRev, Author: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	toolStage.Add(crID)
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t2", 2, "irrelevant - short-circuits on the tool write", staged, st)

	if st.findingRev[findingID] != rev2 {
		t.Fatalf("st.findingRev[%s] = %d after round 2, want %d (the seed loop must run before the toolWroteCodeReview return, #1108 B3)", findingID, st.findingRev[findingID], rev2)
	}

	// Round 3: the tail drops the finding, so the gate writes "resolved", which must chain off
	// round 2's tool-written revision, not round 1's.
	round3Answer := "VERDICT: approve\nFINDINGS:\n"
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t3", 3, round3Answer, staged, st)
	_, _, lineage, rev3, ok, err := rc.LatestWithMeta(context.Background(), findingID)
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta(%s): ok=%v err=%v", findingID, ok, err)
	}
	if rev3 <= rev2 {
		t.Fatalf("round 3 did not write a new resolved revision: rev3=%d, rev2=%d", rev3, rev2)
	}
	if lineage.ParentRevision != rev2 {
		t.Fatalf("round 3 finding parent_revision = %d, want %d (round 2's tool-written revision) - got a bogus/stale parent (#1108 B3)", lineage.ParentRevision, rev2)
	}
}

// alwaysFailLoadService makes every Load fail, simulating a seed re-read failure.
type alwaysFailLoadService struct{ artifact.Service }

func (alwaysFailLoadService) Load(context.Context, *artifact.LoadRequest) (*artifact.LoadResponse, error) {
	return nil, os.ErrNotExist
}
func (alwaysFailLoadService) Save(ctx context.Context, req *artifact.SaveRequest) (*artifact.SaveResponse, error) {
	return &artifact.SaveResponse{Version: 1}, nil
}
func (alwaysFailLoadService) Versions(context.Context, *artifact.VersionsRequest) (*artifact.VersionsResponse, error) {
	return nil, os.ErrNotExist
}

// TestFindingIdentityMatchesAcrossTailParseAndToolWrite: a tail-parsed finding (split via splitFirstSentence) and
// the same finding from write_finding must hash to the same id, or the dedup between them breaks.
func TestFindingIdentityMatchesAcrossTailParseAndToolWrite(t *testing.T) {
	title, rationale := splitFirstSentence("bug one. it breaks things")
	tailParsed := FindingRecord{Path: "a.go", LineHint: 1, Snippet: "func Foo() {", Title: title, Rationale: rationale, State: "new"}
	toolWritten := FindingRecord{Path: "a.go", LineHint: 1, Snippet: "func Foo() {", Title: "bug one", Rationale: "it breaks things", State: "new"}

	id1, err := recordstore.IdentityFor(kindFinding, tailParsed, "")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := recordstore.IdentityFor(kindFinding, toolWritten, "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("tail-parsed id %q != tool-written id %q for the same logical finding - dedup would fail", id1, id2)
	}
}

// TestFindingIdentityStableAcrossLineShiftHeadSHAAndNode: the same finding keeps its id regardless of
// line number, head SHA, or the node hint (findingIdentity ignores hint).
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

// TestGateFailWritesNothing: saveEpisodicRound runs only inside RunGatedRefine's round loop, so
// a gate failure before any round leaves the store empty.
func TestGateFailWritesNothing(t *testing.T) {
	svc := artifact.InMemoryService()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)
	if _, _, ok, _ := rc.Latest(context.Background(), codeReviewID(cfg)); ok {
		t.Fatal("no record should exist before any round runs")
	}
}

// TestSaveErrorFailsOpen: a Save error never reaches the caller; the round-write helper
// neither panics nor blocks against a failing service.
func TestSaveErrorFailsOpen(t *testing.T) {
	cfg := reviewerCfgWithArtifacts(t, failingArtifactService{Service: artifact.InMemoryService()}, true)
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, "VERDICT: approve\n", StagedDelivery{Recovered: true}, newEpisodicRoundState())
}

// failingArtifactService fails only Save: the no-op-save guard reads the latest revision
// before every save, so Load/Versions must work.
type failingArtifactService struct{ artifact.Service }

func (failingArtifactService) Save(context.Context, *artifact.SaveRequest) (*artifact.SaveResponse, error) {
	return nil, os.ErrPermission
}

// TestFindingResolvedAcrossRounds: a finding present in round 1 and absent from round 2
// gets one final revision with state "resolved".
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
	st := newEpisodicRoundState()
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, round1, staged, st)
	if len(st.findings) != 2 {
		t.Fatalf("round 1 live findings = %d, want 2", len(st.findings))
	}
	var droppedID string
	for id, f := range st.findings {
		if f.Path == "b.go" {
			droppedID = id
		}
	}
	if droppedID == "" {
		t.Fatal("could not find b.go's finding id in round 1")
	}

	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 2, round2, staged, st)
	if len(st.findings) != 1 {
		t.Fatalf("round 2 live findings = %d, want 1", len(st.findings))
	}

	rc := recordClient(cfg)
	raw, _, ok, err := rc.Latest(context.Background(), droppedID)
	if err != nil || !ok {
		t.Fatalf("dropped finding record missing: ok=%v err=%v", ok, err)
	}
	var f FindingRecord
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.State != "resolved" {
		t.Fatalf("dropped finding state = %q, want resolved", f.State)
	}
}

// TestSecondInvocationSeedsFromStoreAndStampsParent: a fresh invocation on a chat with a code_review record loads it,
// so a repeat finding is "unchanged", a dropped one "resolved", and parent_revision is real, not 0.
func TestSecondInvocationSeedsFromStoreAndStampsParent(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	staged := StagedDelivery{Recovered: true}

	// Turn 1: a whole separate RunGatedRefine invocation - fresh nil state.
	turn1 := `VERDICT: request_changes
FINDINGS:
- a.go:1: issue A. detail
- b.go:2: issue B. detail
`
	saveEpisodicRound(context.Background(), cfg, cfg.NodeID, "t1", 1, turn1, staged, nil, nil)

	rc := recordClient(cfg)
	_, _, _, firstRev, ok, err := rc.LatestWithMeta(context.Background(), codeReviewID(cfg))
	if err != nil || !ok {
		t.Fatalf("turn 1 code_review missing: ok=%v err=%v", ok, err)
	}
	var aID string
	{
		raw, _, _, _, _, _ := rc.LatestWithMeta(context.Background(), codeReviewID(cfg))
		var rec CodeReviewRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		for _, fid := range rec.FindingIDs {
			fraw, _, _, _, _, _ := rc.LatestWithMeta(context.Background(), fid)
			var f FindingRecord
			if json.Unmarshal(fraw, &f) == nil && f.Path == "a.go" {
				aID = fid
			}
		}
	}
	if aID == "" {
		t.Fatal("could not find a.go's finding id in turn 1")
	}

	// Turn 2: another fresh invocation (nil state, like node.go's first
	// round of any call) - a.go repeats, b.go is dropped.
	turn2 := `VERDICT: comment
FINDINGS:
- a.go:1: issue A. detail
`
	saveEpisodicRound(context.Background(), cfg, cfg.NodeID, "t2", 1, turn2, staged, nil, nil)

	_, _, secondLineage, secondRev, ok, err := rc.LatestWithMeta(context.Background(), codeReviewID(cfg))
	if err != nil || !ok {
		t.Fatalf("turn 2 code_review missing: ok=%v err=%v", ok, err)
	}
	if secondRev != firstRev+1 {
		t.Fatalf("turn 2 revision = %d, want %d", secondRev, firstRev+1)
	}
	if secondLineage.ParentRevision != firstRev {
		t.Fatalf("turn 2 parent_revision = %d, want the real previous revision %d (not fabricated)", secondLineage.ParentRevision, firstRev)
	}

	aRaw, _, aLineage, aRev, ok, err := rc.LatestWithMeta(context.Background(), aID)
	if err != nil || !ok {
		t.Fatalf("a.go finding missing after turn 2: ok=%v err=%v", ok, err)
	}
	var aRec FindingRecord
	if err := json.Unmarshal(aRaw, &aRec); err != nil {
		t.Fatal(err)
	}
	if aRec.State != "unchanged" {
		t.Fatalf("a.go state on turn 2 = %q, want unchanged (was seeded from the store, not re-created as new)", aRec.State)
	}
	if aRev != 2 || aLineage.ParentRevision != 1 {
		t.Fatalf("a.go rev=%d parent=%d, want rev=2 parent=1", aRev, aLineage.ParentRevision)
	}
}

// TestSetAdvisorThreadRound_ToolWriteGetsRealLineageAndPreloads: a tool-initiated write gets the AdvisorTask's
// current Round/HeadSHA, not zero values, so BuildReviewPreload (which drops empty HeadSHA) picks it up.
func TestSetAdvisorThreadRound_ToolWriteGetsRealLineageAndPreloads(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)

	token := "tok-1091-f4"
	RegisterAdvisorThread(token, AdvisorTask{})
	defer UnregisterAdvisorThread(token)
	SetAdvisorThreadRound(token, 2, "turn-abc", cfg.NodeBaseSHA, "")

	task, ok := LookupAdvisorThread(token)
	if !ok {
		t.Fatal("advisor task not registered")
	}
	if task.Round != 2 || task.TurnID != "turn-abc" || task.HeadSHA == "" {
		t.Fatalf("AdvisorTask coords = %+v, want round=2 turn=turn-abc non-empty head", task)
	}

	// Mirrors internal/acp/memorymcp.go's currentRound + registerWriteKindTool:
	// a tool-initiated write stamps the round's live coords, not zero values.
	rc := recordClient(cfg)
	rec := FindingRecord{Path: "a.go", Title: "mid-round finding", State: "new"}
	lineage := recordstore.Lineage{NodeID: cfg.NodeID, Round: task.Round, TurnID: task.TurnID, HeadSHA: task.HeadSHA, Author: "worker"}
	id, _, err := rc.SaveStructured(context.Background(), kindFinding, rec, "", lineage)
	if err != nil {
		t.Fatal(err)
	}
	_, _, gotLineage, _, ok, err := rc.LatestWithMeta(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if gotLineage.Round != 2 {
		t.Fatalf("stored lineage.Round = %d, want 2 (not the old hardcoded 0)", gotLineage.Round)
	}
	if gotLineage.HeadSHA != cfg.NodeBaseSHA || gotLineage.HeadSHA == "" {
		t.Fatalf("stored lineage.HeadSHA = %q, want %q (non-empty)", gotLineage.HeadSHA, cfg.NodeBaseSHA)
	}

	// A code_review record referencing the finding, as saveCodeReviewRound
	// would write once the round completes.
	if _, _, err := rc.SaveStructured(context.Background(), kindCodeReview,
		CodeReviewRecord{Verdict: "request_changes", FindingIDs: []string{id}},
		SubjectHint(cfg.ChatID), recordstore.Lineage{NodeID: cfg.NodeID, Round: 2, HeadSHA: cfg.NodeBaseSHA}); err != nil {
		t.Fatal(err)
	}

	block := BuildReviewPreload(context.Background(), cfg, cfg.NodeID)
	if !strings.Contains(block, "mid-round finding") {
		t.Fatalf("BuildReviewPreload dropped the tool-written finding (empty HeadSHA would do this): %q", block)
	}
}

// TestSetAdvisorThreadRound_ToolWriteCarriesTriggerAnnotation: a tool-initiated write chains the same
// trigger_annotation (the prior round's judge_round id) a gate-written artifact gets.
func TestSetAdvisorThreadRound_ToolWriteCarriesTriggerAnnotation(t *testing.T) {
	token := "tok-1112-trigger"
	RegisterAdvisorThread(token, AdvisorTask{})
	defer UnregisterAdvisorThread(token)

	// Round 1: no prior judge_round yet.
	SetAdvisorThreadRound(token, 1, "turn-x", "sha1", "")
	// Round 2: node.go stamps round 2's trigger with round 1's judge_round id.
	SetAdvisorThreadRound(token, 2, "turn-x", "sha1", "jr-round1")

	task, ok := LookupAdvisorThread(token)
	if !ok {
		t.Fatal("advisor task not registered")
	}
	if task.TriggerAnnotation != "jr-round1" {
		t.Fatalf("AdvisorTask.TriggerAnnotation = %q, want %q", task.TriggerAnnotation, "jr-round1")
	}

	// Mirrors internal/acp/memorymcp.go's currentRound + registerWriteKindTool:
	// a round-2 tool write must stamp round 1's judge_round id as its trigger.
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)
	rec := FindingRecord{Path: "a.go", Title: "round-2 tool finding", State: "new"}
	lineage := recordstore.Lineage{NodeID: cfg.NodeID, Round: task.Round, TurnID: task.TurnID, HeadSHA: task.HeadSHA, TriggerAnnotation: task.TriggerAnnotation, Author: "worker"}
	id, _, err := rc.SaveStructured(context.Background(), kindFinding, rec, "", lineage)
	if err != nil {
		t.Fatal(err)
	}
	_, _, gotLineage, _, ok, err := rc.LatestWithMeta(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if gotLineage.TriggerAnnotation != "jr-round1" {
		t.Fatalf("stored lineage.TriggerAnnotation = %q, want %q", gotLineage.TriggerAnnotation, "jr-round1")
	}
}

// TestResumePreloadFiltersByFile: after a commit touching only a.go, preload keeps b.go's clean
// entry and untouched findings, and drops a.go's clean entry.
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
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, answer, StagedDelivery{Recovered: true}, newEpisodicRoundState())

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

// TestResumePreloadDropsUnreachableHead: a force-push makes the record's head_sha unreachable,
// so preload is empty though the store still holds the revision.
func TestResumePreloadDropsUnreachableHead(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)
	lineage := recordstore.Lineage{NodeID: cfg.NodeID, Round: 1, HeadSHA: "0000000000000000000000000000000000dead"}
	if _, _, err := rc.SaveStructured(context.Background(), kindCodeReview,
		CodeReviewRecord{Verdict: "comment", Clean: []string{"a.txt"}}, SubjectHint(cfg.ChatID), lineage); err != nil {
		t.Fatal(err)
	}
	if block := BuildReviewPreload(context.Background(), cfg, cfg.NodeID); block != "" {
		t.Fatalf("expected empty preload for an unreachable head_sha, got: %s", block)
	}
	if _, _, ok, _ := rc.Latest(context.Background(), codeReviewID(cfg)); !ok {
		t.Fatal("the store must still hold the revision - filtered at read, not deleted")
	}
}

// TestBuildReviewPreloadOneGitDiffSpawn: the preload runs exactly one `git diff --name-only` regardless of entry
// count, counted via a `git` shim prepended to PATH (caps.Sandbox is unset, so RunArgv never re-execs).
func TestBuildReviewPreloadOneGitDiffSpawn(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)
	lineage := recordstore.Lineage{NodeID: cfg.NodeID, Round: 1, HeadSHA: cfg.NodeBaseSHA}

	// 40-file/25-finding fixture: 25 findings + 15 clean entries = 40 files.
	var findingIDs []string
	for i := 0; i < 25; i++ {
		rec := FindingRecord{Path: fmt.Sprintf("f%d.go", i), Title: fmt.Sprintf("finding %d", i), State: "new"}
		id, _, err := rc.SaveStructured(context.Background(), kindFinding, rec, "", lineage)
		if err != nil {
			t.Fatal(err)
		}
		findingIDs = append(findingIDs, id)
	}
	var clean []string
	for i := 25; i < 40; i++ {
		clean = append(clean, fmt.Sprintf("f%d.go", i))
	}
	if _, _, err := rc.SaveStructured(context.Background(), kindCodeReview,
		CodeReviewRecord{Verdict: "request_changes", FindingIDs: findingIDs, Clean: clean},
		SubjectHint(cfg.ChatID), lineage); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "git-spawns.log")
	shimDir := installCountingGitShim(t, logPath)
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", shimDir+string(os.PathListSeparator)+oldPath)
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })

	block := BuildReviewPreload(context.Background(), cfg, cfg.NodeID)
	if block == "" {
		t.Fatal("expected a non-empty preload for the 25-finding/15-clean fixture")
	}

	spawns, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var diffCalls int
	for _, line := range strings.Split(strings.TrimSpace(string(spawns)), "\n") {
		if strings.HasPrefix(line, "diff --name-only") {
			diffCalls++
		}
	}
	if diffCalls != 1 {
		t.Fatalf("git diff --name-only spawns = %d, want 1 (65 valid() calls must collapse into one diff)", diffCalls)
	}
}

// installCountingGitShim puts a `git` on a fresh PATH dir that appends its
// argv (one line) to logPath before exec'ing the real git.
func installCountingGitShim(t *testing.T, logPath string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\nexec %q \"$@\"\n", logPath, realGit)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDocumentStagesShareOneID: the document id has no node segment, so every stage of one chat's dispatch
// appends revisions to the SAME id; lineage.NodeID tells them apart, and every revision is kept.
func TestDocumentStagesShareOneID(t *testing.T) {
	svc := artifact.InMemoryService()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = kindDocument

	st := newEpisodicRoundState()
	saveStage := func(nodeID, text string) {
		cfg := base
		cfg.NodeID = nodeID
		saveDocumentRound(context.Background(), cfg, nodeID, "", 1, text, st)
	}
	docID, err := recordstore.IdentityFor(kindDocument, nil, DocumentHint(base.ChatID))
	if err != nil {
		t.Fatal(err)
	}
	rc := recordClient(base)

	saveStage("ocr", "ocr text v1")
	saveStage("summarize", "summary v1 (re-dispatch)")
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

// TestTextRoundWrite_PlainNodeWritesTextArtifact: a gated node with no structured kind still gets one revision per
// round, id "text:<node>", with code_review's lineage shape and a correct parent_revision chain.
func TestTextRoundWrite_PlainNodeWritesTextArtifact(t *testing.T) {
	svc := newMetaAwareInMemory()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = ""
	base.NodeID = "explore-1"

	textID, err := recordstore.IdentityFor(kindText, nil, base.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if textID != "text:"+base.NodeID {
		t.Fatalf("id = %q, want %q (owning node name is the instance, #1090 V4 §4.1)", textID, "text:"+base.NodeID)
	}
	rc := recordClient(base)

	st := saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, "round one's answer", StagedDelivery{}, nil, nil)
	raw, _, lineage, rev, ok, err := rc.LatestWithMeta(context.Background(), textID)
	if err != nil || !ok {
		t.Fatalf("round 1 LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if rev != 1 || string(raw) != "round one's answer" {
		t.Fatalf("round 1: rev=%d body=%q", rev, raw)
	}
	// TurnID is excluded from the lineage JSON on purpose (it lives in the
	// store row's own turn_id column instead - Lineage.TurnID doc comment).
	if lineage.NodeID != base.NodeID || lineage.Round != 1 || lineage.ParentRevision != 0 || lineage.Author != "worker" {
		t.Fatalf("round 1 lineage = %+v, unexpected", lineage)
	}

	// Round 2: a failed judge round still writes a revision.
	saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 2, "round two's answer", StagedDelivery{}, st, nil)
	_, _, lineage2, rev2, ok, err := rc.LatestWithMeta(context.Background(), textID)
	if err != nil || !ok {
		t.Fatalf("round 2 LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if rev2 != 2 {
		t.Fatalf("round 2 revision = %d, want 2", rev2)
	}
	if lineage2.ParentRevision != 1 || lineage2.Round != 2 {
		t.Fatalf("round 2 lineage = %+v, want ParentRevision=1 Round=2", lineage2)
	}
}

// TestTextRoundWrite_SkippedWhenToolWrote: a node that tool-wrote an artifact via any loopback write tool gets no
// redundant "text:<node>" write. Exercises ToolWritten directly; acp/artifact_tools_test.go covers the MCP wiring.
func TestTextRoundWrite_SkippedWhenToolWrote(t *testing.T) {
	cases := []struct {
		name string
		id   string // id as it would be recorded by the tool being simulated
	}{
		{"write_kind_tool", "pr_body:pr:1"},              // write_<kind>
		{"write_artifact_blob_tool", "bytes:deadbeef12"}, // write_artifact (generic blob path)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newMetaAwareInMemory()
			base := reviewerCfgWithArtifacts(t, svc, true)
			base.IsReviewer = false
			base.Artifact = ""
			base.NodeID = "implement-1"

			toolStage := NewToolWrittenStage()
			secret := "sec-1095-tool-" + tc.name
			RegisterMemSession(secret, MemSession{ToolWritten: toolStage})
			MarkMemSessionConnected(secret)
			defer UnregisterMemSession(secret)
			token := "tok-1095-tool-" + tc.name
			RegisterAdvisorThread(token, AdvisorTask{MemSecret: secret})
			defer UnregisterAdvisorThread(token)
			base.AdvisorToken = token

			// Simulate the worker having called the tool this round - the real
			// handler's side effect (memorymcp.go) is ToolWritten.Add(id).
			toolStage.Add(tc.id)

			saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, "answer text", StagedDelivery{}, nil, nil)

			textID, err := recordstore.IdentityFor(kindText, nil, base.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			rc := recordClient(base)
			if _, _, ok, err := rc.Latest(context.Background(), textID); err != nil || ok {
				t.Fatalf("text artifact must not exist: ok=%v err=%v", ok, err)
			}
		})
	}
}

// TestSaveTextRound_TruncatesOversizedAnswer: content over artifactref.InlineMaxBytes is truncated
// with a trailing marker, since a worker can dump a full diff.
func TestSaveTextRound_TruncatesOversizedAnswer(t *testing.T) {
	svc := newMetaAwareInMemory()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = ""
	base.NodeID = "explore-big"

	big := strings.Repeat("x", artifactref.InlineMaxBytes+100)
	saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, big, StagedDelivery{}, nil, nil)

	textID, err := recordstore.IdentityFor(kindText, nil, base.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	rc := recordClient(base)
	raw, _, ok, err := rc.Latest(context.Background(), textID)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if len(raw) > artifactref.InlineMaxBytes+200 {
		t.Fatalf("stored content = %d bytes, want it capped near the %d byte limit", len(raw), artifactref.InlineMaxBytes)
	}
	if !strings.Contains(string(raw), "[truncated:") {
		t.Fatalf("stored content missing truncation marker: %q...", string(raw[:80]))
	}
}

// TestSaveCodeReviewRound_TakeawayFromAnswerTail: free prose ahead of VERDICT isn't captured;
// the answer-tail fallback reads only the explicit TAKEAWAY: tag.
func TestSaveCodeReviewRound_TakeawayFromAnswerTail(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	answer := "VERDICT: approve\nTAKEAWAY: This change looks solid overall.\nFINDINGS:\nCLEAN:\n- a.go\n"
	st := newEpisodicRoundState()
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 1, answer, StagedDelivery{Kind: "review", Recovered: true}, st)

	rc := recordClient(cfg)
	raw, _, ok, err := rc.Latest(context.Background(), codeReviewID(cfg))
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	var rec CodeReviewRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Takeaway != "This change looks solid overall." {
		t.Fatalf("Takeaway = %q, want the TAKEAWAY: tag's text", rec.Takeaway)
	}
}

// TestSaveCodeReviewRound_TakeawayFromToolStagedFields: on the stage_review tool path,
// Takeaway is the staged field verbatim.
func TestSaveCodeReviewRound_TakeawayFromToolStagedFields(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	st := newEpisodicRoundState()
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 1, "ignored answer text",
		StagedDelivery{Kind: "review", Event: "comment", Takeaway: "worker's own summary"}, st)

	rc := recordClient(cfg)
	raw, _, ok, err := rc.Latest(context.Background(), codeReviewID(cfg))
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	var rec CodeReviewRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Takeaway != "worker's own summary" {
		t.Fatalf("Takeaway = %q, want the tool-staged field", rec.Takeaway)
	}
}

// TestSaveCodeReviewRound_BackfillsEmptyToolWrittenTakeaway: write_code_review's takeaway is optional, so an empty
// one is backfilled from finding titles rather than leaving the record and its delivered body empty.
func TestSaveCodeReviewRound_BackfillsEmptyToolWrittenTakeaway(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolStage := NewToolWrittenStage()
	RegisterMemSession("sec-1198-backfill", MemSession{ToolWritten: toolStage})
	MarkMemSessionConnected("sec-1198-backfill")
	defer UnregisterMemSession("sec-1198-backfill")
	token := "tok-1198-backfill"
	RegisterAdvisorThread(token, AdvisorTask{MemSecret: "sec-1198-backfill"})
	defer UnregisterAdvisorThread(token)
	cfg.AdvisorToken = token

	finding := FindingRecord{Path: "a.go", LineHint: 3, Title: "off-by-one", Rationale: "loop bound is wrong", State: "new"}
	fID, _, err := rc.SaveStructured(context.Background(), kindFinding, finding, "", recordstore.Lineage{NodeID: cfg.NodeID, Author: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	toolStage.Add(fID)
	// No Takeaway - the exact gap a compliant-but-terse tool call leaves.
	crRec := CodeReviewRecord{Verdict: "request_changes", FindingIDs: []string{fID}}
	crID, _, err := rc.SaveStructured(context.Background(), kindCodeReview, crRec, SubjectHint(cfg.ChatID), recordstore.Lineage{NodeID: cfg.NodeID, Author: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	toolStage.Add(crID)

	st := newEpisodicRoundState()
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 1, "irrelevant - tool-written round", StagedDelivery{Kind: "review", Recovered: true}, st)

	raw, _, ok, err := rc.Latest(context.Background(), codeReviewID(cfg))
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	var rec CodeReviewRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Takeaway, "off-by-one") {
		t.Fatalf("Takeaway = %q, want it backfilled from the finding's title", rec.Takeaway)
	}
}

// TestSaveCodeReviewRound_TwoRoundsClampedTakeawayStillDelivers: an over-cap tail takeaway is clamped, so round 2's
// SaveStructured succeeds and the delivery reflects round 2 instead of sticking on round 1's approve.
func TestSaveCodeReviewRound_TwoRoundsClampedTakeawayStillDelivers(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)

	st := newEpisodicRoundState()
	round1 := "VERDICT: approve\nTAKEAWAY: Looks fine on the first pass.\nFINDINGS:\nCLEAN:\n"
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 1, round1, StagedDelivery{Kind: "review", Recovered: true}, st)

	round2 := "VERDICT: request_changes\nTAKEAWAY: First sentence about the new bug. Second sentence with more detail.\nFINDINGS:\n- a.go:1: blocking: nil deref on the empty path. Crashes on startup.\n"
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 2, round2, StagedDelivery{Kind: "review", Recovered: true}, st)

	rc := recordClient(cfg)
	raw, _, ok, err := rc.Latest(context.Background(), codeReviewID(cfg))
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	var rec CodeReviewRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != "request_changes" {
		t.Fatalf("verdict = %q, want request_changes - round 2's save must not silently fail and leave round 1's record standing", rec.Verdict)
	}
	if !strings.Contains(rec.Takeaway, "First sentence about the new bug") {
		t.Fatalf("Takeaway = %q, want round 2's takeaway saved (clamped, not rejected)", rec.Takeaway)
	}

	item, ok := renderReviewFromArtifact(context.Background(), cfg, cfg.NodeID)
	if !ok {
		t.Fatal("renderReviewFromArtifact: no record")
	}
	if item.Event != "request_changes" {
		t.Fatalf("delivered event = %q, want request_changes", item.Event)
	}
}

// TestSaveEpisodicRound_InvalidArtifactFallsBackToText: an unregistered artifact selector falls back to
// the generic "text:<node>" write instead of dropping the node's output.
func TestSaveEpisodicRound_InvalidArtifactFallsBackToText(t *testing.T) {
	svc := newMetaAwareInMemory()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = "the one-word answer"
	base.NodeID = "dominant-color"

	saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, "blue", StagedDelivery{}, nil, nil)

	rc := recordClient(base)
	textID, err := recordstore.IdentityFor(kindText, nil, base.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := rc.Latest(context.Background(), textID); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("saveEpisodicRound: expected a text:<node> fallback record for an invalid artifact selector")
	}
}

func TestSaveDocumentRound_TruncatesOversizedAnswer(t *testing.T) {
	svc := newMetaAwareInMemory()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = kindDocument
	base.NodeID = "doc-big"

	big := strings.Repeat("y", artifactref.InlineMaxBytes+100)
	saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, big, StagedDelivery{}, nil, nil)

	docID, err := recordstore.IdentityFor(kindDocument, nil, DocumentHint(base.ChatID))
	if err != nil {
		t.Fatal(err)
	}
	rc := recordClient(base)
	raw, _, ok, err := rc.Latest(context.Background(), docID)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(string(raw), "[truncated:") {
		t.Fatalf("stored content missing truncation marker: %q...", string(raw[:80]))
	}
}

// TestSaveEpisodicRound_ArtifactKind_SkippedWhenToolWrote: a worker that tool-wrote this round's cfg.Artifact
// document keeps it latest (no answer revision on top); one that wrote nothing still gets the answer saved.
func TestSaveEpisodicRound_ArtifactKind_SkippedWhenToolWrote(t *testing.T) {
	t.Run("tool_wrote_this_round", func(t *testing.T) {
		svc := newMetaAwareInMemory()
		base := reviewerCfgWithArtifacts(t, svc, true)
		base.IsReviewer = false
		base.Artifact = kindDocument
		base.NodeID = "lineup-analyst"

		toolStage := NewToolWrittenStage()
		secret := "sec-1501-artifact-tool"
		RegisterMemSession(secret, MemSession{ToolWritten: toolStage})
		MarkMemSessionConnected(secret)
		defer UnregisterMemSession(secret)
		token := "tok-1501-artifact-tool"
		RegisterAdvisorThread(token, AdvisorTask{MemSecret: secret})
		defer UnregisterAdvisorThread(token)
		base.AdvisorToken = token

		rc := recordClient(base)
		docID, err := recordstore.IdentityFor(kindDocument, nil, DocumentHint(base.ChatID))
		if err != nil {
			t.Fatal(err)
		}
		// The real write_artifact handler both saves the blob and records ToolWritten.Add(id).
		if _, rev, err := rc.SaveBlob(context.Background(), kindDocument, []byte(`{"week":1}`), "application/json", DocumentHint(base.ChatID), recordstore.Lineage{NodeID: base.NodeID}); err != nil || rev != 1 {
			t.Fatalf("seed SaveBlob: rev=%d err=%v", rev, err)
		}
		toolStage.Add(docID)

		saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, "| week | starters |\n|---|---|\n| 1 | ... |", StagedDelivery{}, nil, nil)

		raw, rev, ok, err := rc.Latest(context.Background(), docID)
		if err != nil || !ok {
			t.Fatalf("Latest: ok=%v err=%v", ok, err)
		}
		if rev != 1 {
			t.Fatalf("revision = %d, want 1 (no gate-added revision on top of the tool write)", rev)
		}
		if string(raw) != `{"week":1}` {
			t.Fatalf("latest content = %q, want the tool-written JSON untouched", raw)
		}
	})

	t.Run("later_round_without_a_write_keeps_the_tool_written_artifact", func(t *testing.T) {
		svc := newMetaAwareInMemory()
		base := reviewerCfgWithArtifacts(t, svc, true)
		base.IsReviewer = false
		base.Artifact = kindDocument
		base.NodeID = "lineup-analyst"

		toolStage := NewToolWrittenStage()
		secret := "sec-1501-artifact-sticky"
		RegisterMemSession(secret, MemSession{ToolWritten: toolStage})
		MarkMemSessionConnected(secret)
		defer UnregisterMemSession(secret)
		token := "tok-1501-artifact-sticky"
		RegisterAdvisorThread(token, AdvisorTask{MemSecret: secret})
		defer UnregisterAdvisorThread(token)
		base.AdvisorToken = token

		rc := recordClient(base)
		docID, err := recordstore.IdentityFor(kindDocument, nil, DocumentHint(base.ChatID))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := rc.SaveBlob(context.Background(), kindDocument, []byte(`{"week":1}`), "application/json", DocumentHint(base.ChatID), recordstore.Lineage{NodeID: base.NodeID}); err != nil {
			t.Fatalf("seed SaveBlob: %v", err)
		}
		toolStage.Add(docID)
		st := saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, "wrote the lineup artifact", StagedDelivery{}, nil, nil)

		// Round 2: judge failed, the worker only re-answers - no tool write this round.
		saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 2, "still no changes this week", StagedDelivery{}, st, nil)

		raw, rev, ok, err := rc.Latest(context.Background(), docID)
		if err != nil || !ok || rev != 1 || string(raw) != `{"week":1}` {
			t.Fatalf("doc latest = rev %d %q ok=%v err=%v, want rev 1 JSON untouched", rev, raw, ok, err)
		}
		textID, err := recordstore.IdentityFor(kindText, nil, base.NodeID)
		if err != nil {
			t.Fatal(err)
		}
		if traw, _, tok, err := rc.Latest(context.Background(), textID); err != nil || !tok || string(traw) != "still no changes this week" {
			t.Fatalf("text latest = %q ok=%v err=%v, want the round-2 answer", traw, tok, err)
		}
	})

	t.Run("native_tool_write_seen_by_the_session_scan", func(t *testing.T) {
		svc := newMetaAwareInMemory()
		base := reviewerCfgWithArtifacts(t, svc, true)
		base.IsReviewer = false
		base.Artifact = kindDocument
		base.NodeID = "lineup-analyst"
		rc := recordClient(base)
		docID, err := recordstore.IdentityFor(kindDocument, nil, DocumentHint(base.ChatID))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := rc.SaveBlob(context.Background(), kindDocument, []byte(`{"week":6}`), "application/json", DocumentHint(base.ChatID), recordstore.Lineage{NodeID: base.NodeID}); err != nil {
			t.Fatalf("seed SaveBlob: %v", err)
		}
		// No MCP session at all (native worker): the id arrives from the session scan.
		st := saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, "wrote it", StagedDelivery{}, nil, []string{docID})
		saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 2, "no changes", StagedDelivery{}, st, []string{docID})
		raw, rev, ok, err := rc.Latest(context.Background(), docID)
		if err != nil || !ok || rev != 1 || string(raw) != `{"week":6}` {
			t.Fatalf("doc latest = rev %d %q ok=%v err=%v, want rev 1 JSON untouched", rev, raw, ok, err)
		}
	})

	t.Run("worker_wrote_nothing", func(t *testing.T) {
		svc := newMetaAwareInMemory()
		base := reviewerCfgWithArtifacts(t, svc, true)
		base.IsReviewer = false
		base.Artifact = kindDocument
		base.NodeID = "lineup-analyst"

		saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, "summary text", StagedDelivery{}, nil, nil)

		rc := recordClient(base)
		docID, err := recordstore.IdentityFor(kindDocument, nil, DocumentHint(base.ChatID))
		if err != nil {
			t.Fatal(err)
		}
		raw, rev, ok, err := rc.Latest(context.Background(), docID)
		if err != nil || !ok {
			t.Fatalf("Latest: ok=%v err=%v", ok, err)
		}
		if rev != 1 || string(raw) != "summary text" {
			t.Fatalf("Latest = rev=%d %q, want rev=1 %q (answer saved as before)", rev, raw, "summary text")
		}
	})
}

// TestTextRoundWrite_NeverOverwritesWorkerOwnedText: a native worker that wrote text:<node> itself (seen by
// the session scan) keeps it; a later round's summary must not become the latest revision.
func TestTextRoundWrite_NeverOverwritesWorkerOwnedText(t *testing.T) {
	svc := newMetaAwareInMemory()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = ""
	base.NodeID = "web-researcher-1"
	textID, err := recordstore.IdentityFor(kindText, nil, base.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	rc := recordClient(base)
	lineage := recordstore.Lineage{NodeID: base.NodeID, Round: 1, Author: "worker"}
	if _, _, err := rc.SaveBlob(context.Background(), kindText, []byte("# the full report"), "text/markdown", base.NodeID, lineage); err != nil {
		t.Fatal(err)
	}

	st := saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, "# the full report", StagedDelivery{}, nil, []string{textID})
	saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 2, "All flagged issues are fixed in text:web-researcher-1", StagedDelivery{}, st, []string{textID})

	raw, rev, ok, err := rc.Latest(context.Background(), textID)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if string(raw) != "# the full report" || rev != 1 {
		t.Fatalf("latest text:<node> = rev %d %q, want the worker's report untouched", rev, raw)
	}
}

// TestTextRoundWrite_ArtifactBranchNeverOverwritesWorkerOwnedText: with a
// structured kind selected, the sticky summary fallback must not land on a text:<node> the worker wrote itself either.
func TestTextRoundWrite_ArtifactBranchNeverOverwritesWorkerOwnedText(t *testing.T) {
	svc := newMetaAwareInMemory()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = kindDocument
	base.NodeID = "lineup-analyst"
	rc := recordClient(base)
	docID, err := recordstore.IdentityFor(kindDocument, nil, DocumentHint(base.ChatID))
	if err != nil {
		t.Fatal(err)
	}
	textID, err := recordstore.IdentityFor(kindText, nil, base.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rc.SaveBlob(context.Background(), kindText, []byte("# worker notes"), "text/markdown", base.NodeID, recordstore.Lineage{NodeID: base.NodeID}); err != nil {
		t.Fatal(err)
	}
	written := []string{docID, textID}
	st := saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, "summary one", StagedDelivery{}, nil, written)
	saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 2, "summary two", StagedDelivery{}, st, written)

	raw, rev, ok, err := rc.Latest(context.Background(), textID)
	if err != nil || !ok || string(raw) != "# worker notes" || rev != 1 {
		t.Fatalf("latest text:<node> = rev %d %q ok=%v err=%v, want the worker's own revision untouched", rev, raw, ok, err)
	}
}
