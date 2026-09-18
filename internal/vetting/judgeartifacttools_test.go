package vetting

import (
	"context"
	"encoding/base64"
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

// TestJudgeReadArtifactBase64ForBinary: a non-text artifact (a media agent's
// blob) must come back base64-encoded, never raw bytes - raw bytes are not
// valid UTF-8 and corrupt the judge's own genai request (review finding #2).
func TestJudgeReadArtifactBase64ForBinary(t *testing.T) {
	ctx := &judgeToolCtx{StrictContextMock: adkagent.NewStrictContextMock(context.Background())}
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	binary := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46}
	id, _, err := rc.SaveBlob(context.Background(), "bytes", binary, "application/octet-stream", "hint", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatalf("seed binary: %v", err)
	}
	toolset, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatalf("NewJudgeArtifactTools: %v", err)
	}
	read := toolset[1].(runnableTool)
	out, err := read.Run(ctx, map[string]any{"id": id})
	if err != nil {
		t.Fatalf("read_artifact: %v", err)
	}
	s, _ := out["result"].(string)
	if !strings.Contains(s, base64.StdEncoding.EncodeToString(binary)) {
		t.Errorf("read_artifact (binary) = %q, want base64 of the raw bytes", s)
	}
}

// TestJudgeReadArtifactWindow: offset/lines windows a large text artifact
// instead of returning it whole - the only way to read past judgeArtifactReadCap.
func TestJudgeReadArtifactWindow(t *testing.T) {
	ctx := &judgeToolCtx{StrictContextMock: adkagent.NewStrictContextMock(context.Background())}
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	id, _, err := rc.SaveBlob(context.Background(), "text", []byte("one\ntwo\nthree\nfour\nfive"), "text/plain", "hint", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	toolset, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatalf("NewJudgeArtifactTools: %v", err)
	}
	read := toolset[1].(runnableTool)
	out, err := read.Run(ctx, map[string]any{"id": id, "offset": float64(2), "lines": float64(2)})
	if err != nil {
		t.Fatalf("read_artifact (windowed): %v", err)
	}
	s, _ := out["result"].(string)
	if !strings.Contains(s, "two\nthree") || strings.Contains(s, "five") {
		t.Errorf("read_artifact (offset=2 lines=2) = %q, want just lines 2-3", s)
	}
}

// TestJudgeListArtifactsHidesGateOwnedKinds: judge_round/delivery_record are
// the gate's own verdict/delivery decisions - listing them lets a judge treat
// its own prior scoring as evidence, so they're excluded (review nit).
func TestJudgeListArtifactsHidesGateOwnedKinds(t *testing.T) {
	ctx := &judgeToolCtx{StrictContextMock: adkagent.NewStrictContextMock(context.Background())}
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	if _, _, err := rc.SaveStructured(context.Background(), kindJudgeRound, JudgeRoundRecord{Turn: "t1", Round: 1}, "t1-n1-1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed judge_round: %v", err)
	}
	id, _, err := rc.SaveBlob(context.Background(), "text", []byte("research findings"), "text/plain", "hint", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatalf("seed text: %v", err)
	}
	toolset, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatalf("NewJudgeArtifactTools: %v", err)
	}
	list := toolset[0].(runnableTool)
	out, err := list.Run(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("list_artifacts: %v", err)
	}
	s, _ := out["result"].(string)
	if !strings.Contains(s, id) || strings.Contains(s, "judge_round") {
		t.Errorf("list_artifacts = %q, want %s and no judge_round entry", s, id)
	}
}
