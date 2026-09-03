// artifact_tools_test.go: end-to-end coverage for list_artifacts,
// edit_artifact, write_artifact and write_<kind> through the REAL loopback
// MCP call path (registered on an actual mcp.Server, invoked as a tool call -
// not the Go functions directly), mirroring read_artifact_test.go's pattern
// (#1091 adversarial review finding #2).
package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

// warnCapture installs a slog handler that records Warn+ records for the
// duration of the test, restoring the previous default logger on cleanup.
type warnCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnCapture) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}
func (w *warnCapture) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, r.Message)
	return nil
}
func (w *warnCapture) WithAttrs(_ []slog.Attr) slog.Handler { return w }
func (w *warnCapture) WithGroup(_ string) slog.Handler      { return w }

func captureWarnings(t *testing.T) *warnCapture {
	t.Helper()
	prev := slog.Default()
	w := &warnCapture{}
	slog.SetDefault(slog.New(w))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return w
}

// TestArtifactWriteToolsMCP_EveryKindRegistersWithoutWarning covers the
// "invalid generated JSON schema silently skips the tool" landmine: every
// kind recordstore.Kinds() currently returns must register a write_<kind>
// tool with no Warn/skip.
func TestArtifactWriteToolsMCP_EveryKindRegistersWithoutWarning(t *testing.T) {
	w := captureWarnings(t)

	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	announced := map[string]bool{}
	for _, tl := range tools.Tools {
		announced[tl.Name] = true
	}
	for _, spec := range recordstore.Kinds() {
		if !announced[writeKindPrefix+spec.Name()] {
			t.Errorf("write_%s was not registered on the loopback server (a bad JSONSchema silently drops the tool)", spec.Name())
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, msg := range w.msgs {
		if strings.Contains(msg, "write_<kind> tool skipped") {
			t.Errorf("a write_<kind> tool was skipped with a Warn: %s", msg)
		}
	}
}

// TestWriteFindingMCP_ValidInput calls write_finding through the real tool
// path and checks the returned id against recordstore.IdentityFor computed
// independently.
func TestWriteFindingMCP_ValidInput(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	args := map[string]any{"path": "a.go", "title": "leaked resource", "state": "new"}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_finding", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool write_finding: %v", err)
	}
	if res.IsError {
		t.Fatalf("write_finding returned an error: %s", toolResultText(t, res))
	}
	wantID, err := recordstore.IdentityFor("finding", args, "")
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "id="+wantID) {
		t.Fatalf("write_finding result = %q, want id=%s", text, wantID)
	}

	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	if _, _, ok, err := rc.Latest(ctx, wantID); err != nil || !ok {
		t.Fatalf("finding %s not found in the store: ok=%v err=%v", wantID, ok, err)
	}
}

// TestWriteFindingMCP_OffSchemaFailsWithoutWriting: a finding missing its
// required "path" field must error through the tool call and write nothing.
func TestWriteFindingMCP_OffSchemaFailsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	before, err := rc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_finding", Arguments: map[string]any{"title": "missing path"}})
	if err != nil {
		t.Fatalf("CallTool write_finding: %v", err)
	}
	if !res.IsError {
		t.Fatalf("write_finding with no path must error, got: %s", toolResultText(t, res))
	}

	after, err := rc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("off-schema write_finding wrote something: before=%v after=%v", before, after)
	}
}

// TestListArtifactsMCP_AfterWrite: a written artifact appears with its kind
// and latest revision.
func TestListArtifactsMCP_AfterWrite(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	args := map[string]any{"path": "b.go", "title": "list me", "state": "new"}
	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_finding", Arguments: args}); err != nil || res.IsError {
		t.Fatalf("write_finding: err=%v result=%v", err, res)
	}
	wantID, err := recordstore.IdentityFor("finding", args, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_artifacts", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool list_artifacts: %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, wantID) || !strings.Contains(text, "revision=1") || !strings.Contains(text, "kind=finding") {
		t.Fatalf("list_artifacts result = %q, want %s with revision=1 kind=finding", text, wantID)
	}
}

// TestEditArtifactMCP_DirectApply: base_revision matches latest - a plain
// search/replace applies.
func TestEditArtifactMCP_DirectApply(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	id, rev, err := rc.SaveBlob(ctx, "text", []byte("hello world"), "text/plain", "doc1", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "edit_artifact", Arguments: map[string]any{
		"id": id, "base_revision": float64(rev),
		"edits": []map[string]any{{"old": "world", "new": "there"}},
	}})
	if err != nil {
		t.Fatalf("CallTool edit_artifact: %v", err)
	}
	if res.IsError {
		t.Fatalf("edit_artifact returned an error: %s", toolResultText(t, res))
	}
	raw, _, ok, err := rc.Latest(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if string(raw) != "hello there" {
		t.Fatalf("content = %q, want %q", raw, "hello there")
	}
}

// TestEditArtifactMCP_StaleBaseMerges: base_revision is stale but the Old
// snippet still matches uniquely against the real latest - merges instead of
// failing.
func TestEditArtifactMCP_StaleBaseMerges(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	id, rev1, err := rc.SaveBlob(ctx, "text", []byte("line one\nline two\n"), "text/plain", "doc2", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	// Advance past rev1 with an edit unrelated to what the tool call below touches.
	if _, _, err := rc.Edit(ctx, id, rev1, []recordstore.EditOp{{Old: "line one", New: "LINE ONE"}}, recordstore.Lineage{NodeID: "n1"}); err != nil {
		t.Fatal(err)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "edit_artifact", Arguments: map[string]any{
		"id": id, "base_revision": float64(rev1), // stale on purpose
		"edits": []map[string]any{{"old": "line two", "new": "LINE TWO"}},
	}})
	if err != nil {
		t.Fatalf("CallTool edit_artifact: %v", err)
	}
	if res.IsError {
		t.Fatalf("a non-intersecting stale-base edit must merge, got error: %s", toolResultText(t, res))
	}
	raw, _, ok, err := rc.Latest(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if string(raw) != "LINE ONE\nLINE TWO\n" {
		t.Fatalf("content = %q, want both edits merged", raw)
	}
}

// TestEditArtifactMCP_ConflictReturnsCurrent: an Old string that no longer
// matches (real conflict, not just a stale base) fails with the current
// content/revision, not a partial write.
func TestEditArtifactMCP_ConflictReturnsCurrent(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	id, rev, err := rc.SaveBlob(ctx, "text", []byte("hello world"), "text/plain", "doc3", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "edit_artifact", Arguments: map[string]any{
		"id": id, "base_revision": float64(rev),
		"edits": []map[string]any{{"old": "not present anywhere", "new": "x"}},
	}})
	if err != nil {
		t.Fatalf("CallTool edit_artifact: %v", err)
	}
	// A conflict is an expected, actionable outcome for the calling agent, not
	// a tool failure - success carrying structured conflict data (#1108 finding 3).
	if res.IsError {
		t.Fatalf("a non-matching Old is a conflict, not a tool error: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "conflict") || !strings.Contains(text, "hello world") {
		t.Fatalf("conflict result = %q, want the current content/revision", text)
	}
	// Round-tripped over the wire, so structured content decodes as a plain map.
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %#v (%T), want a JSON object", res.StructuredContent, res.StructuredContent)
	}
	if conflict, _ := out["conflict"].(bool); !conflict {
		t.Fatalf("StructuredContent[conflict] = %v, want true", out["conflict"])
	}
	if gotRev, _ := out["revision"].(float64); int(gotRev) != rev {
		t.Fatalf("StructuredContent[revision] = %v, want %d", out["revision"], rev)
	}
	if content, _ := out["content"].(string); content != "hello world" {
		t.Fatalf("StructuredContent[content] = %q, want %q", content, "hello world")
	}
	raw, _, ok, err := rc.Latest(ctx, id)
	if err != nil || !ok || string(raw) != "hello world" {
		t.Fatalf("content must be untouched after a conflict: raw=%q ok=%v err=%v", raw, ok, err)
	}
}

// TestWriteArtifactMCP_Blob: write_artifact with a blob kind returns an id
// matching recordstore.IdentityFor's independently computed identity.
func TestWriteArtifactMCP_Blob(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	content := "plain content"
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_artifact", Arguments: map[string]any{
		"kind": "text", "mime": "text/plain", "bytes": content,
	}})
	if err != nil {
		t.Fatalf("CallTool write_artifact: %v", err)
	}
	if res.IsError {
		t.Fatalf("write_artifact returned an error: %s", toolResultText(t, res))
	}
	// "text"'s Identity (contentOrHintIdentity) hashes the raw content with no
	// hint - same sha256-prefix scheme reviewrecord.go's fallback kinds use.
	h := sha256.Sum256([]byte(content))
	wantID := "text:" + hex.EncodeToString(h[:])[:8]
	text := toolResultText(t, res)
	if !strings.Contains(text, "id="+wantID) {
		t.Fatalf("write_artifact result = %q, want id=%s", text, wantID)
	}
	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	if raw, _, ok, err := rc.Latest(ctx, wantID); err != nil || !ok || string(raw) != content {
		t.Fatalf("Latest(%s): raw=%q ok=%v err=%v", wantID, raw, ok, err)
	}
}

// TestWriteArtifactMCP_RecordsToolWritten: write_artifact must add its id to
// the session's ToolWritten stage exactly like write_<kind> does, so
// vetting's saveTextRound fallback can tell a tool-written round apart from
// one with no tool writes (#1095 adversarial review finding #1 - the blob
// path used to skip this, so a node that only called write_artifact still
// got a duplicate text:<node> revision).
func TestWriteArtifactMCP_RecordsToolWritten(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	stage := vetting.NewToolWrittenStage()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1", ToolWritten: stage})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_artifact", Arguments: map[string]any{
		"kind": "text", "mime": "text/plain", "bytes": "hello",
	}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool write_artifact: err=%v isError=%v text=%s", err, res != nil && res.IsError, toolResultText(t, res))
	}
	h := sha256.Sum256([]byte("hello"))
	wantID := "text:" + hex.EncodeToString(h[:])[:8]
	if snap := stage.Snapshot(); !snap[wantID] {
		t.Fatalf("ToolWritten after write_artifact = %v, want it to contain %s", snap, wantID)
	}
}

// TestEditArtifactMCP_RecordsToolWritten: edit_artifact must also record its
// id into ToolWritten (same reasoning as write_artifact above).
func TestEditArtifactMCP_RecordsToolWritten(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	id, rev, err := rc.SaveBlob(ctx, "text", []byte("hello world"), "text/plain", "edit-target", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatalf("seed SaveBlob: %v", err)
	}
	stage := vetting.NewToolWrittenStage()
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1", ToolWritten: stage})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "edit_artifact", Arguments: map[string]any{
		"id": id, "base_revision": float64(rev),
		"edits": []map[string]any{{"old": "world", "new": "there"}},
	}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool edit_artifact: err=%v isError=%v text=%s", err, res != nil && res.IsError, toolResultText(t, res))
	}
	if snap := stage.Snapshot(); !snap[id] {
		t.Fatalf("ToolWritten after edit_artifact = %v, want it to contain %s", snap, id)
	}
}

// TestWriteArtifactDescription_ListsBlobKinds: the write_artifact tool
// description must name every registered blob kind (#1108 B1 - Kinds() used
// to return structured kinds only, so this list silently rendered empty).
func TestWriteArtifactDescription_ListsBlobKinds(t *testing.T) {
	desc := writeArtifactDescription()
	for _, spec := range recordstore.KindsForClass(recordstore.Blob) {
		if !strings.Contains(desc, spec.Name()) {
			t.Errorf("write_artifact description = %q, want it to name blob kind %q", desc, spec.Name())
		}
	}
	if len(recordstore.KindsForClass(recordstore.Blob)) == 0 {
		t.Fatal("no blob kinds registered - test can't verify the list is non-empty")
	}
}

// TestWriteCodeReviewMCP_UsesSessionSubjectHint: write_code_review (#1108
// finding 1) must succeed for a github-derived chat id and mint exactly the
// id vetting.SubjectHint + code_review's Identity func (requireHint) produce -
// never "" (which requireHint always rejects).
func TestWriteCodeReviewMCP_UsesSessionSubjectHint(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	chatID := "ext:github:github-owner-repo-42"
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: chatID, NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	args := map[string]any{"verdict": "approve"}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_code_review", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool write_code_review: %v", err)
	}
	if res.IsError {
		t.Fatalf("write_code_review returned an error: %s", toolResultText(t, res))
	}
	wantID, err := recordstore.IdentityFor("code_review", nil, vetting.SubjectHint(chatID))
	if err != nil {
		t.Fatal(err)
	}
	if wantID != "code_review:pr:42" {
		t.Fatalf("unexpected computed id %q; SubjectHint's format may have changed", wantID)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "id="+wantID) {
		t.Fatalf("write_code_review result = %q, want id=%s", text, wantID)
	}
	rc := recordstore.New(svc, "quack", "u1", chatID)
	if _, _, ok, err := rc.Latest(ctx, wantID); err != nil || !ok {
		t.Fatalf("code_review %s not found: ok=%v err=%v", wantID, ok, err)
	}
}

// TestWriteArtifactMCP_HintRequiringKind: write_artifact with a hint-requiring
// blob kind ("document") must succeed by deriving its hint from the session,
// exactly like write_code_review - the same root cause as finding 1
// (#1108 finding 2).
func TestWriteArtifactMCP_HintRequiringKind(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	chatID := "ext:github:github-owner-repo-7"
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: chatID, NodeID: "n1"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_artifact", Arguments: map[string]any{
		"kind": "document", "mime": "text/markdown", "bytes": "# hi",
	}})
	if err != nil {
		t.Fatalf("CallTool write_artifact: %v", err)
	}
	if res.IsError {
		t.Fatalf("write_artifact(document) returned an error: %s", toolResultText(t, res))
	}
	wantID, err := recordstore.IdentityFor("document", nil, vetting.SubjectHint(chatID))
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "id="+wantID) {
		t.Fatalf("write_artifact(document) result = %q, want id=%s", text, wantID)
	}

	// A hint-optional kind (text) must still derive its id from content, not
	// collapse onto the session hint - the regression finding 2's naive fix
	// would introduce.
	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_artifact", Arguments: map[string]any{
		"kind": "text", "mime": "text/plain", "bytes": "distinct content",
	}})
	if err != nil {
		t.Fatalf("CallTool write_artifact: %v", err)
	}
	if res2.IsError {
		t.Fatalf("write_artifact(text) returned an error: %s", toolResultText(t, res2))
	}
	h := sha256.Sum256([]byte("distinct content"))
	wantTextID := "text:" + hex.EncodeToString(h[:])[:8]
	text2 := toolResultText(t, res2)
	if !strings.Contains(text2, "id="+wantTextID) {
		t.Fatalf("write_artifact(text) result = %q, want id=%s (content-hash identity, not session hint)", text2, wantTextID)
	}
}
