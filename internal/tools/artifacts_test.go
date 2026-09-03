// artifacts_test.go: happy-path coverage for the ADK-native artifact tool
// wrappers (mirrors internal/acp/artifact_tools_test.go's MCP-path coverage;
// #1091 adversarial review finding #2).
package tools

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/fagerbergj/quack/internal/recordstore"
)

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
	tl, err := NewWriteKindTool(rc, "n1", "finding", spec, RoundCoords{})
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

	tl, err := NewEditArtifactTool(rc, "n1", RoundCoords{})
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
