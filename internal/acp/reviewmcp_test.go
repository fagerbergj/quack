package acp

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/vetting"
)

// TestReviewMCP_StageToolsLandInBuffer drives the review MCP surface (#451)
// exactly as the ACP reviewer subprocess does: stage_review_comment per inline
// finding, stage_review for the verdict+summary. The gate reads the SAME buffer
// via ReviewStage.Snapshot, so asserting the snapshot asserts what delivery
// posts. It also pins the gating: a review-only session (no Memory) never
// exposes the memory tools, and a malformed verdict fails loudly.
func TestReviewMCP_StageToolsLandInBuffer(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	review := &vetting.ReviewStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: review})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	stageComment := func(path string, line int, body string) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "stage_review_comment",
			Arguments: map[string]any{"path": path, "line": line, "body": body},
		})
		if err != nil {
			t.Fatalf("CallTool stage_review_comment: %v", err)
		}
		if res.IsError {
			t.Fatalf("stage_review_comment errored: %s", toolResultText(t, res))
		}
	}
	stageComment("internal/server/router.go", 42, "blocking: route after SPA fallback never matches")
	stageComment("frontend/src/state/chatStore.ts", 118, "suggestion: roll back optimistic write on 409")

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stage_review",
		Arguments: map[string]any{"event": "request_changes", "body": "two blockers"},
	})
	if err != nil {
		t.Fatalf("CallTool stage_review: %v", err)
	}
	if res.IsError {
		t.Fatalf("stage_review errored: %s", toolResultText(t, res))
	}

	sd, ok := review.Snapshot()
	if !ok {
		t.Fatal("review buffer empty after staging")
	}
	if sd.Kind != "review" || sd.Event != "request_changes" || sd.Body != "two blockers" {
		t.Fatalf("staged review wrong: %+v", sd)
	}
	if len(sd.Comments) != 2 {
		t.Fatalf("want 2 inline comments, got %d: %+v", len(sd.Comments), sd.Comments)
	}
	if sd.Comments[0].Path != "internal/server/router.go" || sd.Comments[0].Line != 42 {
		t.Fatalf("first comment mis-anchored: %+v", sd.Comments[0])
	}

	// A review-only session (Memory nil) must NOT expose the memory tools.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_memory", Arguments: map[string]any{"content": "x"}}); err == nil {
		t.Fatal("stage_memory must not be registered on a review-only session")
	}
	// A malformed verdict is rejected loudly, not silently accepted.
	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_review", Arguments: map[string]any{"event": "lgtm", "body": "x"}})
	if err != nil {
		t.Fatalf("CallTool stage_review (bad event): %v", err)
	}
	if !bad.IsError {
		t.Fatal("an invalid verdict must return a tool error")
	}
}
