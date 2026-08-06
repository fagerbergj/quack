package acp

import (
	"context"
	"net/http/httptest"
	"strings"
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

// TestReviewMCP_StageReturnsID proves stage_review_comment's "create" leg of
// the CRUD surface (#562): it hands back the id needed to retract the finding
// later, formatted "<path>:<line>#<n>" so it's self-explanatory to a human or
// the judge without a lookup.
func TestReviewMCP_StageReturnsID(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	review := &vetting.ReviewStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: review})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stage_review_comment",
		Arguments: map[string]any{"path": "internal/judge.go", "line": 112, "body": "blocking: unchecked error"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("stage_review_comment errored: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "internal/judge.go:112#1") {
		t.Fatalf("stage result should carry the id, got %q", text)
	}
	// The id in the tool result must be the SAME id the buffer assigned.
	staged := review.ListComments()
	if len(staged) != 1 || !strings.Contains(text, staged[0].ID) {
		t.Fatalf("tool-returned id disagrees with the buffer: text=%q staged=%+v", text, staged)
	}
}

// TestReviewMCP_ListAndUnstageByID exercises the read + delete legs together:
// list shows the staged set (id, path, line, excerpt - no need to reproduce
// the body verbatim), unstage-by-id removes exactly that one and leaves the
// rest, and retracting the same id twice is a visible tool error, not a
// silent no-op (#562's read-before-you-stage loop).
func TestReviewMCP_ListAndUnstageByID(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	review := &vetting.ReviewStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: review})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	longBody := strings.Repeat("x", 200)
	for _, c := range []struct {
		path string
		line int
		body string
	}{
		{"a.go", 1, "question: what does this do"},
		{"a.go", 1, longBody},
		{"b.go", 9, "suggestion: extract helper"},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "stage_review_comment",
			Arguments: map[string]any{"path": c.path, "line": c.line, "body": c.body},
		})
		if err != nil || res.IsError {
			t.Fatalf("stage %+v: err=%v res=%v", c, err, res)
		}
	}

	// list_review_comments: full page, id/path/line/excerpt present, long body truncated.
	listRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_review_comments", Arguments: map[string]any{}})
	if err != nil || listRes.IsError {
		t.Fatalf("list_review_comments: err=%v res=%v", err, listRes)
	}
	listText := toolResultText(t, listRes)
	if !strings.Contains(listText, "showing 3 of 3") {
		t.Fatalf("list should report the full set as one page: %q", listText)
	}
	if strings.Contains(listText, longBody) {
		t.Fatal("list must excerpt a long body, not echo it whole")
	}
	if !strings.Contains(listText, "a.go:1#1") || !strings.Contains(listText, "a.go:1#2") || !strings.Contains(listText, "b.go:9#1") {
		t.Fatalf("list missing an expected id: %q", listText)
	}

	// Paginate: limit=1 offset=1 should show the second entry only, total still 3.
	page, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_review_comments", Arguments: map[string]any{"limit": 1, "offset": 1}})
	if err != nil || page.IsError {
		t.Fatalf("list_review_comments (paged): err=%v res=%v", err, page)
	}
	pageText := toolResultText(t, page)
	if !strings.Contains(pageText, "showing 1 of 3") {
		t.Fatalf("paginated list wrong: %q", pageText)
	}

	// Unstage the first comment by id, confirm it's gone from a fresh list.
	idToRemove := review.ListComments()[0].ID
	unstageRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "unstage_review_comment", Arguments: map[string]any{"id": idToRemove}})
	if err != nil {
		t.Fatalf("unstage_review_comment: %v", err)
	}
	if unstageRes.IsError {
		t.Fatalf("unstage of a known id should not error: %s", toolResultText(t, unstageRes))
	}
	if remaining := review.ListComments(); len(remaining) != 2 {
		t.Fatalf("want 2 comments remaining, got %+v", remaining)
	}

	// Retracting the same id again is a visible error, not a silent no-op.
	again, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "unstage_review_comment", Arguments: map[string]any{"id": idToRemove}})
	if err != nil {
		t.Fatalf("unstage_review_comment (repeat): %v", err)
	}
	if !again.IsError {
		t.Fatal("retracting an already-removed id must be a tool error")
	}

	// And an id that was never issued at all.
	unknown, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "unstage_review_comment", Arguments: map[string]any{"id": "never.go:1#1"}})
	if err != nil {
		t.Fatalf("unstage_review_comment (unknown): %v", err)
	}
	if !unknown.IsError {
		t.Fatal("retracting an id that was never issued must be a tool error")
	}
}

// TestStagePR_RequiresBothFields pins the Forbidden clause of #724: stage_pr
// keeps its required-fields contract (only stage_push's fields are optional),
// and a rejected call must not land in the buffer.
func TestStagePR_RequiresBothFields(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	pr := &vetting.PRStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{PRStage: pr})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_pr", Arguments: map[string]any{"title": "", "body": "does the thing"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("stage_pr with an empty title must be a tool error")
	}
	if _, ok := pr.Snapshot(); ok {
		t.Fatal("a rejected stage_pr call must not land in the buffer")
	}
}

// TestStagePushMCP_OptionalFieldsOmitTheKey drives stage_push exactly as an
// ACP implementer pushing onto an already-open PR does - #724 test cases 1-3:
// a bare call, a deliberate full update, and a partial (body-only) update.
func TestStagePushMCP_OptionalFieldsOmitTheKey(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	pr := &vetting.PRStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{PRStage: pr, ExistingPR: true})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	call := func(args map[string]any) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_push", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool stage_push: %v", err)
		}
		if res.IsError {
			t.Fatalf("stage_push errored: %s", toolResultText(t, res))
		}
	}

	// Test case 1: nothing to say - still satisfies delivery.
	call(map[string]any{})
	sd, ok := pr.Snapshot()
	if !ok {
		t.Fatal("a bare stage_push() call must still stage delivery")
	}
	if !sd.TitleOmitted || !sd.BodyOmitted {
		t.Fatalf("bare stage_push: got %+v, want both fields omitted", sd)
	}

	// Test case 2: a deliberate full update.
	call(map[string]any{"title": "fix: retry flaky upload", "body": "adds a backoff"})
	sd, ok = pr.Snapshot()
	if !ok || sd.TitleOmitted || sd.BodyOmitted || sd.Title != "fix: retry flaky upload" || sd.Body != "adds a backoff" {
		t.Fatalf("stage_push(title, body): got %+v (ok=%v), want both applied", sd, ok)
	}

	// Test case 3: a partial update - body only, title stays untouched downstream.
	call(map[string]any{"body": "adds a backoff and jitter"})
	sd, ok = pr.Snapshot()
	if !ok || !sd.TitleOmitted || sd.BodyOmitted || sd.Body != "adds a backoff and jitter" {
		t.Fatalf("stage_push(body only): got %+v (ok=%v), want title omitted, body set", sd, ok)
	}
}

// TestToolOfferIsExclusive pins #724 test case 5: a run is offered stage_pr
// XOR stage_push, keyed on MemSession.ExistingPR, never both in one round.
func TestToolOfferIsExclusive(t *testing.T) {
	for _, tt := range []struct {
		name        string
		existingPR  bool
		want        string
		mustNotHave string
	}{
		{"opens a new PR", false, "stage_pr", "stage_push"},
		{"pushes onto an existing PR", true, "stage_push", "stage_pr"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			secret := mustMemSecret(t)
			vetting.RegisterMemSession(secret, vetting.MemSession{PRStage: &vetting.PRStage{}, ExistingPR: tt.existingPR})
			defer vetting.UnregisterMemSession(secret)

			ts := httptest.NewServer(memoryMCPHandler())
			t.Cleanup(func() { ts.Close() })
			cs := connectMCP(t, ts, secret)

			res, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			names := map[string]bool{}
			for _, tool := range res.Tools {
				names[tool.Name] = true
			}
			if !names[tt.want] {
				t.Errorf("tools = %v, want %q registered", names, tt.want)
			}
			if names[tt.mustNotHave] {
				t.Errorf("tools = %v, must NOT register %q in the same round", names, tt.mustNotHave)
			}
		})
	}
}

// TestPlanOnlyNodeOffersNoStagePRTool pins #739 test case 2 at the wire
// level: a planOnly node's MemSession never carries a PRStage (dag's
// newGatedNode only sets one when prNode is true - see
// TestPlanOnlyOffersNoWritableNode in internal/dag), so the live MCP server
// registers neither stage_pr nor stage_push for it - the same server
// TestToolOfferIsExclusive above proves offers exactly one of them for a
// writable node.
func TestPlanOnlyNodeOffersNoStagePRTool(t *testing.T) {
	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{PRStage: nil})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "stage_pr" || tool.Name == "stage_push" {
			t.Errorf("tools = %+v, a planOnly node's session (PRStage=nil) must never register %q", res.Tools, tool.Name)
		}
	}
}
