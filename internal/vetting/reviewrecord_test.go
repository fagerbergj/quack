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

// TestSaveCodeReviewRound_ToolWriteSkipsTailFallback covers the #1091 gate
// fallback: a write_code_review call already wrote this round's record
// directly, so saveCodeReviewRound must not also parse the (bogus) answer
// tail and overwrite it - it just adopts the tool-written revision.
func TestSaveCodeReviewRound_ToolWriteSkipsTailFallback(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolWritten := CodeReviewRecord{Verdict: "approve", Summary: "written directly via write_code_review"}
	_, toolRev, err := rc.SaveStructured(context.Background(), kindCodeReview, toolWritten, SubjectHint(cfg.ChatID), recordstore.Lineage{NodeID: cfg.NodeID, Author: "worker"})
	if err != nil {
		t.Fatal(err)
	}

	// A malformed/contradictory tail: if this were parsed, verdict would
	// become request_changes - proving the fallback path never ran.
	answer := "VERDICT: request_changes\nFINDINGS:\n- a.go:1: should never be recorded\n"
	staged := StagedDelivery{Kind: "review", Recovered: true}
	st := newEpisodicRoundState()
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

// TestSaveCodeReviewRound_ToolWrittenFindingsSkipTailDuplicate covers #1091
// adversarial review finding #1: a worker that calls write_finding 3 times
// but never write_code_review must not have the tail-parse fallback re-stage
// (and duplicate, with a fabricated ParentRevision 0) those same 3 findings -
// only the answer tail's genuinely new 4th finding gets a new revision, and
// code_review.finding_ids lists all 4.
func TestSaveCodeReviewRound_ToolWrittenFindingsSkipTailDuplicate(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolStage := NewToolFindingStage()
	RegisterMemSession("sec-1091-f1", MemSession{ToolFindings: toolStage})
	MarkMemSessionConnected("sec-1091-f1")
	defer UnregisterMemSession("sec-1091-f1")
	token := "tok-1091-f1"
	RegisterAdvisorThread(token, AdvisorTask{MemSecret: "sec-1091-f1"})
	defer UnregisterAdvisorThread(token)
	cfg.AdvisorToken = token

	// Simulate 3 tool-written findings (as write_finding's MCP handler would
	// do: SaveStructured directly, then record the id on ToolFindings).
	// Snippet "" matches what fileLineAtForCfg resolves for a path that
	// doesn't exist in the probe repo - the tail parse below reads the same
	// (missing) file, so the hash-derived id lines up with the tool write.
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

	// The worker never called write_code_review; the answer tail happens to
	// describe the same 3 findings (matching hash: same path/title/snippet)
	// plus one genuinely new one.
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

// TestToolFindingStageResetsPerRound covers #1108 finding 2: an id written
// via write_finding in round 1 must not still suppress round N's tail-parse
// write of the SAME id - the stage scopes to "this round," not the whole node
// run, so it has to be drained between rounds.
func TestToolFindingStageResetsPerRound(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolStage := NewToolFindingStage()
	RegisterMemSession("sec-1108-f2", MemSession{ToolFindings: toolStage})
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
		t.Fatalf("ToolFindingStage still holds %s after round 1 - not drained", id)
	}

	// Round 2: the SAME id, now only known via the answer tail (the worker
	// didn't call write_finding again) - must be treated as a normal
	// round-2 write, not wrongly skipped as "already written this round."
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

// TestSaveCodeReviewRoundLogsAndSkipsOnSeedReadFailure covers #1108 finding
// 3a: when a tool-written id can't be re-read while seeding the round (here,
// the artifact.Service is swapped out from under the client so every Load
// fails), the finding must not silently vanish and the tail-parse fallback
// must not stamp a fabricated ParentRevision for it.
func TestSaveCodeReviewRoundLogsAndSkipsOnSeedReadFailure(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	rc := recordClient(cfg)

	toolStage := NewToolFindingStage()
	RegisterMemSession("sec-1108-f3a", MemSession{ToolFindings: toolStage})
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

	// Simulate the seed re-read failing: swap in an artifact.Service whose
	// Load always errors, while the id still resolves via the SAME
	// SubjectHint/kindCodeReview lookup for the early toolRev short-circuit
	// (which is unaffected since no code_review record exists yet).
	cfg.Artifacts = &alwaysFailLoadService{}

	st := newEpisodicRoundState()
	staged := StagedDelivery{Kind: "review", Recovered: true}
	answer := "VERDICT: request_changes\nFINDINGS:\n- a.go:1: bug one.\n"
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "t1", 1, answer, staged, st)

	if _, live := st.findings[id]; live {
		t.Fatalf("finding %s should not be recorded live this round when its seed-read failed", id)
	}
}

// alwaysFailLoadService makes every Load fail, simulating a seed re-read
// failure (#1108 finding 3a) without needing a real broken backend.
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

// TestFindingIdentityMatchesAcrossTailParseAndToolWrite covers #1108 finding
// 3b: the exact-hash dedup between a tail-parsed finding and its tool-written
// equivalent only holds if both code paths normalize into the same
// FindingRecord shape. The tail-parse path (saveCodeReviewRound) splits an
// answer-tail body via splitFirstSentence into Title/Rationale; a tool call
// (write_finding) supplies Title/Rationale directly. Same logical finding,
// same fields, must hash to the same id via recordstore.IdentityFor.
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
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, "", 1, "VERDICT: approve\n", StagedDelivery{Recovered: true}, newEpisodicRoundState())
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

// TestSecondInvocationSeedsFromStoreAndStampsParent covers #1090 adversarial
// review findings #1 and #2: a fresh RunGatedRefine invocation (prev state
// nil, as node.go passes on its very first round) on a chat that already
// has a code_review record must load it - a repeated finding gets
// "unchanged" (not "new" again) and a dropped one gets "resolved"; the new
// code_review revision's parent_revision is the real previous revision, not
// a fabricated 0.
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
	saveEpisodicRound(context.Background(), cfg, cfg.NodeID, "t1", 1, turn1, staged, nil)

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
	saveEpisodicRound(context.Background(), cfg, cfg.NodeID, "t2", 1, turn2, staged, nil)

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

// TestSetAdvisorThreadRound_ToolWriteGetsRealLineageAndPreloads covers #1091
// adversarial review finding #4: a tool-initiated write, stamped with the
// AdvisorTask's current Round/HeadSHA (as internal/acp/memorymcp.go's
// currentRound reads them, refreshed by SetAdvisorThreadRound at the start of
// the round) gets real lineage instead of Round:0/HeadSHA:"" - and
// BuildReviewPreload, which drops any finding with an empty HeadSHA, picks it up.
func TestSetAdvisorThreadRound_ToolWriteGetsRealLineageAndPreloads(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)

	token := "tok-1091-f4"
	RegisterAdvisorThread(token, AdvisorTask{})
	defer UnregisterAdvisorThread(token)
	SetAdvisorThreadRound(token, 2, "turn-abc", cfg.NodeBaseSHA)

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

// TestResumePreloadDropsUnreachableHead covers #1006 test case 3: a
// force-push (history rewrite) makes the record's head_sha unreachable ->
// preload is empty, though the store still holds the revision.
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

	st := newEpisodicRoundState()
	saveStage := func(nodeID, text string) {
		cfg := base
		cfg.NodeID = nodeID
		saveDocumentRound(context.Background(), cfg, nodeID, "", 1, text, st)
	}
	docID, err := recordstore.IdentityFor(kindDocument, nil, documentHint(base.ChatID))
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
