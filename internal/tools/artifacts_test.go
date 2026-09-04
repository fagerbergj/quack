// artifacts_test.go: happy-path coverage for the ADK-native artifact tool
// wrappers (mirrors internal/acp/artifact_tools_test.go's MCP-path coverage;
// #1091 adversarial review finding #2).
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

// fakeLedger: minimal in-memory ledger.LedgerStore double (mirrors
// internal/recordstore's own test copy) so a write_<kind> test can prove
// parent_revision without a database (#1153).
type fakeLedger struct {
	mu      sync.Mutex
	seqs    map[string]int64
	entries map[string][]ledger.Entry
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{seqs: map[string]int64{}, entries: map[string][]ledger.Entry{}}
}

func (f *fakeLedger) Append(context.Context, string, []byte) error { return nil }
func (f *fakeLedger) ReadStream(context.Context, string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeLedger) List(context.Context) ([]ledger.SessionRef, error) { return nil, nil }
func (f *fakeLedger) Delete(context.Context, string) error              { return nil }

func (f *fakeLedger) AppendIntent(_ context.Context, e ledger.Entry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seqs[e.ChatID]++
	e.Seq = f.seqs[e.ChatID]
	e.At = time.Now().UTC()
	f.entries[e.ChatID] = append(f.entries[e.ChatID], e)
	return e.Seq, nil
}

func (f *fakeLedger) ReadEntries(_ context.Context, chatID string, fromSeq int64) ([]ledger.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ledger.Entry
	for _, e := range f.entries[chatID] {
		if e.Seq >= fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// metaAwareInMemory implements the optional SaveWithMeta/LoadWithMeta pair
// over artifact.InMemoryService() so a test can read back real lineage
// without a database - production always wraps a store that supports these
// (internal/store.TurnAwareService); plain InMemoryService is a known
// zero-lineage ceiling (see internal/recordstore's own test copy of this).
type metaAwareInMemory struct {
	artifact.Service
	mu   sync.Mutex
	meta map[string][]byte
}

func newMetaAwareInMemory() *metaAwareInMemory {
	return &metaAwareInMemory{Service: artifact.InMemoryService(), meta: map[string][]byte{}}
}

func metaKey(appName, userID, sessionID, fileName string) string {
	return appName + "\x00" + userID + "\x00" + sessionID + "\x00" + fileName
}

func (m *metaAwareInMemory) SaveWithMeta(ctx context.Context, req *artifact.SaveRequest, kind, class string, lineageJSON []byte, turnID string) (*artifact.SaveResponse, error) {
	resp, err := m.Service.Save(ctx, req)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meta[metaKey(req.AppName, req.UserID, req.SessionID, req.FileName)] = lineageJSON
	return resp, nil
}

func (m *metaAwareInMemory) LoadWithMeta(ctx context.Context, req *artifact.LoadRequest) (*artifact.LoadResponse, string, string, []byte, error) {
	resp, err := m.Service.Load(ctx, req)
	if err != nil {
		return nil, "", "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return resp, "", "", m.meta[metaKey(req.AppName, req.UserID, req.SessionID, req.FileName)], nil
}

// artifactsToolCtx: fakeCtx plus the ToolConfirmation stub functiontool.Run
// requires (mirrors check_mermaid_test.go's checkMermaidToolCtx).
type artifactsToolCtx struct{ *fakeCtx }

func (artifactsToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

func newArtifactsToolCtx() artifactsToolCtx { return artifactsToolCtx{newFakeCtx()} }

func TestNewWriteKindTool_WriteFindingRegistersAndWrites(t *testing.T) {
	svc := artifact.InMemoryService()
	rc := recordstore.New(svc, "quack", "u1", "chat-a")

	spec, err := findKindSpec(t, "finding")
	if err != nil {
		t.Fatal(err)
	}
	tl, err := NewWriteKindTool(rc, "n1", "finding", spec, &RoundCoords{}, vetting.SubjectHint("chat-a"))
	if err != nil {
		t.Fatalf("NewWriteKindTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("write_finding tool is not runnable")
	}

	args := map[string]any{"path": "a.go", "title": "leaked resource", "state": "new"}
	out, err := rt.Run(newArtifactsToolCtx(), args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	result, _ := out["result"].(string)
	wantID, err := recordstore.IdentityFor("finding", args, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "id="+wantID) {
		t.Fatalf("result = %q, want id=%s", result, wantID)
	}
	if _, _, ok, err := rc.Latest(context.Background(), wantID); err != nil || !ok {
		t.Fatalf("finding %s not found: ok=%v err=%v", wantID, ok, err)
	}
}

func TestNewEditArtifactTool_DirectApply(t *testing.T) {
	svc := artifact.InMemoryService()
	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	id, rev, err := rc.SaveBlob(context.Background(), "text", []byte("hello world"), "text/plain", "doc1", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	tl, err := NewEditArtifactTool(rc, "n1", &RoundCoords{})
	if err != nil {
		t.Fatalf("NewEditArtifactTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("edit_artifact tool is not runnable")
	}
	out, err := rt.Run(newArtifactsToolCtx(), map[string]any{
		"id": id, "base_revision": rev,
		"edits": []editArtifactEdit{{Old: "world", New: "there"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	result, _ := out["result"].(string)
	if !strings.Contains(result, "ok:") {
		t.Fatalf("result = %q, want ok", result)
	}
	raw, _, ok, err := rc.Latest(context.Background(), id)
	if err != nil || !ok || string(raw) != "hello there" {
		t.Fatalf("Latest: raw=%q ok=%v err=%v", raw, ok, err)
	}
}

// TestNewWriteKindTools_EveryKindRegistersWithoutError: the ADK-native mirror
// of internal/acp's TestArtifactWriteToolsMCP_EveryKindRegistersWithoutWarning -
// every kind recordstore.Kinds() currently returns must produce a write_<kind>
// tool. recordstore.Register now rejects a bad JSONSchema at process startup
// (TestRegisterPanicsOnInvalidJSONSchema), so this can only regress if a
// second, un-guarded schema check is reintroduced here (#1108 finding 3).
func TestNewWriteKindTools_EveryKindRegistersWithoutError(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a")
	toolsList, err := NewWriteKindTools(rc, "n1", &RoundCoords{}, "")
	if err != nil {
		t.Fatalf("NewWriteKindTools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range toolsList {
		got[tl.Name()] = true
	}
	for _, spec := range recordstore.Kinds() {
		if !got["write_"+spec.Name()] {
			t.Errorf("write_%s was not registered (a bad JSONSchema silently dropped the tool)", spec.Name())
		}
	}
}

// TestWriteArtifactDescription_ListsBlobKinds: the write_artifact tool
// description must name every registered blob kind (#1108 B1 - Kinds() used
// to return structured kinds only, so this list silently rendered empty).
func TestWriteArtifactDescription_ListsBlobKinds(t *testing.T) {
	desc := writeArtifactDescription()
	blobKinds := recordstore.KindsForClass(recordstore.Blob)
	if len(blobKinds) == 0 {
		t.Fatal("no blob kinds registered - test can't verify the list is non-empty")
	}
	for _, spec := range blobKinds {
		if !strings.Contains(desc, spec.Name()) {
			t.Errorf("write_artifact description = %q, want it to name blob kind %q", desc, spec.Name())
		}
	}
}

// TestNewEditArtifactTool_ConflictIsStructuredSuccess: edit_artifact was
// already a success (not a tool error) on the ADK surface for a real
// conflict; this pins the JSON payload shape ({"conflict":true,"revision":N,
// "content":"..."} - same field names as the MCP surface's
// editConflictResult) so the two surfaces can't drift apart again
// (#1108 finding 3).
func TestNewEditArtifactTool_ConflictIsStructuredSuccess(t *testing.T) {
	svc := artifact.InMemoryService()
	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	id, rev, err := rc.SaveBlob(context.Background(), "text", []byte("hello world"), "text/plain", "doc1", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	tl, err := NewEditArtifactTool(rc, "n1", &RoundCoords{})
	if err != nil {
		t.Fatalf("NewEditArtifactTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("edit_artifact tool is not runnable")
	}
	out, err := rt.Run(newArtifactsToolCtx(), map[string]any{
		"id": id, "base_revision": rev,
		"edits": []editArtifactEdit{{Old: "not present anywhere", New: "x"}},
	})
	if err != nil {
		t.Fatalf("Run must succeed on a conflict, not error: %v", err)
	}
	result, _ := out["result"].(string)
	var payload struct {
		Conflict bool   `json:"conflict"`
		Revision int    `json:"revision"`
		Content  string `json:"content"`
	}
	if jErr := json.Unmarshal([]byte(result), &payload); jErr != nil {
		t.Fatalf("result %q is not the expected JSON conflict payload: %v", result, jErr)
	}
	if !payload.Conflict || payload.Revision != rev || payload.Content != "hello world" {
		t.Fatalf("payload = %+v, want {Conflict:true Revision:%d Content:hello world}", payload, rev)
	}
	raw, _, ok, err := rc.Latest(context.Background(), id)
	if err != nil || !ok || string(raw) != "hello world" {
		t.Fatalf("content must be untouched after a conflict: raw=%q ok=%v err=%v", raw, ok, err)
	}
}

// TestNewWriteKindTool_WriteCodeReviewUsesSessionHint: the ADK-native mirror
// of internal/acp's TestWriteCodeReviewMCP_UsesSessionSubjectHint (#1108
// finding 1) - write_code_review must succeed when given the caller's
// session-derived hint, and mint exactly the id code_review's Identity
// (requireHint) plus vetting.SubjectHint produce.
func TestNewWriteKindTool_WriteCodeReviewUsesSessionHint(t *testing.T) {
	svc := artifact.InMemoryService()
	chatID := "ext:github:github-owner-repo-42"
	rc := recordstore.New(svc, "quack", "u1", chatID)

	spec, err := findKindSpec(t, "code_review")
	if err != nil {
		t.Fatal(err)
	}
	hint := vetting.SubjectHint(chatID)
	tl, err := NewWriteKindTool(rc, "n1", "code_review", spec, &RoundCoords{}, hint)
	if err != nil {
		t.Fatalf("NewWriteKindTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("write_code_review tool is not runnable")
	}

	out, err := rt.Run(newArtifactsToolCtx(), map[string]any{"verdict": "approve"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	result, _ := out["result"].(string)
	wantID, err := recordstore.IdentityFor("code_review", nil, hint)
	if err != nil {
		t.Fatal(err)
	}
	if wantID != "code_review:pr:42" {
		t.Fatalf("unexpected computed id %q; SubjectHint's format may have changed", wantID)
	}
	if !strings.Contains(result, "id="+wantID) {
		t.Fatalf("result = %q, want id=%s", result, wantID)
	}
	if _, _, ok, err := rc.Latest(context.Background(), wantID); err != nil || !ok {
		t.Fatalf("code_review %s not found: ok=%v err=%v", wantID, ok, err)
	}
}

// TestNewWriteArtifactTool_HintRequiringAndHintOptionalKinds: the ADK-native
// mirror of TestWriteArtifactMCP_HintRequiringKind (#1108 finding 2) -
// a hint-requiring blob kind (document) succeeds using the session hint,
// while a hint-optional kind (text) keeps its content-hash identity.
func TestNewWriteArtifactTool_HintRequiringAndHintOptionalKinds(t *testing.T) {
	svc := artifact.InMemoryService()
	chatID := "ext:github:github-owner-repo-7"
	rc := recordstore.New(svc, "quack", "u1", chatID)
	hint := vetting.SubjectHint(chatID)

	tl, err := NewWriteArtifactTool(rc, "n1", &RoundCoords{}, hint)
	if err != nil {
		t.Fatalf("NewWriteArtifactTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("write_artifact tool is not runnable")
	}

	out, err := rt.Run(newArtifactsToolCtx(), map[string]any{"kind": "document", "mime": "text/markdown", "bytes": "# hi"})
	if err != nil {
		t.Fatalf("Run(document): %v", err)
	}
	result, _ := out["result"].(string)
	wantDocID, err := recordstore.IdentityFor("document", nil, hint)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "id="+wantDocID) {
		t.Fatalf("result = %q, want id=%s", result, wantDocID)
	}

	out2, err := rt.Run(newArtifactsToolCtx(), map[string]any{"kind": "text", "mime": "text/plain", "bytes": "distinct content"})
	if err != nil {
		t.Fatalf("Run(text): %v", err)
	}
	result2, _ := out2["result"].(string)
	h := sha256.Sum256([]byte("distinct content"))
	wantTextID := "text:" + hex.EncodeToString(h[:])[:8]
	if !strings.Contains(result2, "id="+wantTextID) {
		t.Fatalf("result = %q, want id=%s (content-hash identity, not session hint)", result2, wantTextID)
	}
}

// TestNewEditArtifactTool_RoundCoordsRestampBetweenRounds: a native gated
// node's artifact tools are built ONCE, before its judge/revise loop starts
// (#1123) - the gate restamps round/turn/head-sha/trigger-annotation onto
// the SAME *RoundCoords pointer every tool closure shares (mirrors
// vetting.SetAdvisorThreadRound / ledger.Coords' SetLedgerCoords pattern), so
// an edit made during round 2 must carry round 2's trigger_annotation (the
// prior round's judge_round id), not round 1's.
func TestNewEditArtifactTool_RoundCoordsRestampBetweenRounds(t *testing.T) {
	svc := newMetaAwareInMemory()
	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	id, rev, err := rc.SaveBlob(context.Background(), "text", []byte("draft one"), "text/plain", "doc1", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	coords := &RoundCoords{Round: 1, TurnID: "turn-1"}
	tl, err := NewEditArtifactTool(rc, "n1", coords)
	if err != nil {
		t.Fatalf("NewEditArtifactTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("edit_artifact tool is not runnable")
	}

	// Gate restamps for round 2, same as newGatedNode's cfg.RoundCoordsSink
	// would on the second judge/revise round.
	*coords = RoundCoords{Round: 2, TurnID: "turn-1", HeadSHA: "deadbeef", TriggerAnnotation: "judge-r1"}

	out, err := rt.Run(newArtifactsToolCtx(), map[string]any{
		"id": id, "base_revision": rev,
		"edits": []editArtifactEdit{{Old: "one", New: "two"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	result, _ := out["result"].(string)
	if !strings.Contains(result, "ok: "+id+" revision 2") {
		t.Fatalf("result = %q, want ok: %s revision 2", result, id)
	}

	raw, _, lineage, gotRev, ok, err := rc.LatestWithMeta(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if string(raw) != "draft two" {
		t.Fatalf("content = %q, want %q", raw, "draft two")
	}
	if gotRev != 2 {
		t.Fatalf("revision = %d, want 2", gotRev)
	}
	if lineage.TriggerAnnotation != "judge-r1" {
		t.Fatalf("lineage.TriggerAnnotation = %q, want %q (round 2's restamped coords, not round 1's zero value)", lineage.TriggerAnnotation, "judge-r1")
	}
	if lineage.Round != 2 || lineage.HeadSHA != "deadbeef" {
		t.Fatalf("lineage = %+v, want Round=2 HeadSHA=deadbeef", lineage)
	}
}

// findKindSpec looks up spec by name from recordstore.Kinds() - avoids
// importing internal/vetting just for its unexported kind constants.
func findKindSpec(t *testing.T, name string) (recordstore.KindSpec, error) {
	t.Helper()
	for _, spec := range recordstore.Kinds() {
		if spec.Name() == name {
			return spec, nil
		}
	}
	t.Fatalf("kind %q not registered - is internal/vetting imported (for its init) somewhere in this test binary?", name)
	return recordstore.KindSpec{}, nil
}

// TestNewWriteKindTool_ParentRevisionChain: write_<kind> (and write_artifact)
// build their own recordstore.Lineage with no ParentRevision (#1153) -
// saveLocked fills it in from the WAL's own last-revision read, but only
// when the client is WithLedger-armed, which every worker tool call site
// used to skip. Two saves through the tool must chain revision 2 to
// revision 1, both in the returned lineage and in the WAL's own
// artifact.revision intent (fold's parent-chain oracle, epic #1090 item 1.5).
func TestNewWriteKindTool_ParentRevisionChain(t *testing.T) {
	svc := newMetaAwareInMemory()
	fl := newFakeLedger()
	rc := recordstore.New(svc, "quack", "u1", "chat-a").WithLedger(fl)

	spec, err := findKindSpec(t, "finding")
	if err != nil {
		t.Fatal(err)
	}
	tl, err := NewWriteKindTool(rc, "n1", "finding", spec, &RoundCoords{}, vetting.SubjectHint("chat-a"))
	if err != nil {
		t.Fatalf("NewWriteKindTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("write_finding tool is not runnable")
	}

	args1 := map[string]any{"path": "a.go", "title": "leaked resource", "state": "new"}
	if _, err := rt.Run(newArtifactsToolCtx(), args1); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	id, err := recordstore.IdentityFor("finding", args1, "")
	if err != nil {
		t.Fatal(err)
	}
	// Same id, different body: a real second revision, not a no-op
	// identical-content skip (saveLocked's own guard, #1123).
	args2 := map[string]any{"path": "a.go", "title": "leaked resource", "state": "resolved"}
	out2, err := rt.Run(newArtifactsToolCtx(), args2)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if result, _ := out2["result"].(string); !strings.Contains(result, "revision=2") {
		t.Fatalf("result = %q, want revision=2", result)
	}

	_, _, lineage, rev, ok, err := rc.LatestWithMeta(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if rev != 2 || lineage.ParentRevision != 1 {
		t.Fatalf("revision=%d parent_revision=%d, want revision=2 parent_revision=1", rev, lineage.ParentRevision)
	}

	entries, err := fl.ReadEntries(context.Background(), "chat-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	var sawRev2Intent bool
	for _, e := range entries {
		if e.Key != id || e.Kind != ledger.KindArtifactRevision {
			continue
		}
		var payload struct {
			Revision       int `json:"revision"`
			ParentRevision int `json:"parent_revision"`
		}
		if jErr := json.Unmarshal(e.Payload, &payload); jErr != nil {
			t.Fatal(jErr)
		}
		if payload.Revision == 2 {
			sawRev2Intent = true
			if payload.ParentRevision != 1 {
				t.Fatalf("artifact.revision intent parent_revision = %d, want 1", payload.ParentRevision)
			}
		}
	}
	if !sawRev2Intent {
		t.Fatal("no artifact.revision WAL intent for revision 2")
	}
}
