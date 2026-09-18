package vetting

import (
	"context"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// judgeToolCtx: StrictContextMock plus the ToolConfirmation stub
// functiontool.Run needs (mirrors internal/tools/artifacts_test.go's own).
type judgeToolCtx struct{ adkagent.StrictContextMock }

func (judgeToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// TestNewJudgeArtifactToolsListAndRead drives list_artifacts/read_artifact
// directly (not through a judge round) over an empty then seeded chat.
func TestNewJudgeArtifactToolsListAndRead(t *testing.T) {
	ctx := &judgeToolCtx{StrictContextMock: adkagent.NewStrictContextMock(context.Background())}
	svc := artifact.InMemoryService()
	rc := recordstore.New(svc, "quack", "u1", "chat1")

	toolset, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatalf("NewJudgeArtifactTools: %v", err)
	}
	if len(toolset) != 2 {
		t.Fatalf("NewJudgeArtifactTools returned %d tools, want 2", len(toolset))
	}
	list, ok := toolset[0].(runnableTool)
	if !ok || list.Name() != "list_artifacts" {
		t.Fatalf("toolset[0] = %v, want a runnable list_artifacts", toolset[0])
	}
	read, ok := toolset[1].(runnableTool)
	if !ok || read.Name() != "read_artifact" {
		t.Fatalf("toolset[1] = %v, want a runnable read_artifact", toolset[1])
	}

	out, err := list.Run(ctx, map[string]any{})
	if err != nil || out["result"] != "(no artifacts)" {
		t.Errorf("list_artifacts (empty chat) = %v, %v, want \"(no artifacts)\", nil", out["result"], err)
	}
	if _, err := read.Run(ctx, map[string]any{"id": "text:missing"}); err == nil {
		t.Error("read_artifact on a missing id: want an error")
	}

	id, _, err := rc.SaveBlob(context.Background(), "text", []byte("revision one"), "text/plain", "hint", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatalf("seed rev 1: %v", err)
	}
	if _, _, err := rc.SaveBlob(context.Background(), "text", []byte("revision two"), "text/plain", "hint", recordstore.Lineage{NodeID: "n1"}); err != nil {
		t.Fatalf("seed rev 2: %v", err)
	}

	out, err = list.Run(ctx, map[string]any{"kind": "text"})
	if err != nil {
		t.Fatalf("list_artifacts: %v", err)
	}
	if s, _ := out["result"].(string); !strings.Contains(s, id) {
		t.Errorf("list_artifacts = %q, want it to name %s", s, id)
	}

	out, err = read.Run(ctx, map[string]any{"id": id})
	if err != nil || out["result"] != "revision two" {
		t.Errorf("read_artifact (latest) = %v, %v, want \"revision two\", nil", out["result"], err)
	}
	out, err = read.Run(ctx, map[string]any{"id": id, "revision": float64(1)})
	if err != nil || out["result"] != "revision one" {
		t.Errorf("read_artifact (revision 1) = %v, %v, want \"revision one\", nil", out["result"], err)
	}
}
