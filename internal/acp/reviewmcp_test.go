package acp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/vetting"
)

// TestReviewMCP_StageToolsLandInBuffer drives the review MCP surface as the reviewer does; the gate reads the
// same buffer via ReviewStage.Snapshot. Also: a review-only session gets no memory tools, a bad verdict fails.
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
		Arguments: map[string]any{"event": "request_changes", "takeaway": "two blockers, both in the fallback path"},
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
	if sd.Kind != "review" || sd.Event != "request_changes" || sd.Takeaway != "two blockers, both in the fallback path" {
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
	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_review", Arguments: map[string]any{"event": "lgtm", "takeaway": "x"}})
	if err != nil {
		t.Fatalf("CallTool stage_review (bad event): %v", err)
	}
	if !bad.IsError {
		t.Fatal("an invalid verdict must return a tool error")
	}
}

// TestReviewMCP_StageReviewCommentRequiresLabel proves an unlabeled body is
// rejected loudly rather than staged and rendered uncounted.
func TestReviewMCP_StageReviewCommentRequiresLabel(t *testing.T) {
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
		Arguments: map[string]any{"path": "a.go", "line": 3, "body": "Note: this looks off"},
	})
	if err != nil {
		t.Fatalf("CallTool stage_review_comment (unlabeled): %v", err)
	}
	if !res.IsError || !strings.Contains(toolResultText(t, res), "label is required") {
		t.Fatalf("an unlabeled body must be rejected naming the requirement, got %+v", res)
	}
	if _, ok := review.Snapshot(); ok {
		t.Fatal("a rejected comment must not be staged")
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stage_review_comment",
		Arguments: map[string]any{"path": "a.go", "line": 3, "body": "🚨 **blocking:** nil deref on the unchecked input"},
	})
	if err != nil {
		t.Fatalf("CallTool stage_review_comment (labeled): %v", err)
	}
	if res.IsError {
		t.Fatalf("a labeled body must be accepted: %s", toolResultText(t, res))
	}
}

// TestReviewMCP_StageReviewCommentProposedChange: proposed_change appends a plain go fence to the staged body,
// its absence leaves the body as written, and empty code is rejected without staging.
func TestReviewMCP_StageReviewCommentProposedChange(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	review := &vetting.ReviewStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: review})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)
	stage := func(line int, change map[string]any) *mcp.CallToolResult {
		t.Helper()
		args := map[string]any{"path": "a.go", "line": line, "body": "💡 **suggestion:** return the error"}
		if change != nil {
			args["proposed_change"] = change
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_review_comment", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool stage_review_comment: %v", err)
		}
		return res
	}

	if res := stage(3, map[string]any{"language": "go", "code": "return err"}); res.IsError {
		t.Fatalf("a valid proposed_change must be accepted: %s", toolResultText(t, res))
	}
	if res := stage(4, nil); res.IsError {
		t.Fatalf("a comment without proposed_change must be accepted: %s", toolResultText(t, res))
	}
	res := stage(5, map[string]any{"language": "go", "code": ""})
	if !res.IsError || !strings.Contains(toolResultText(t, res), "proposed_change.code is empty") {
		t.Fatalf("empty code must be rejected naming the field, got %+v", res)
	}

	got := review.ListComments()
	if len(got) != 2 {
		t.Fatalf("want 2 staged comments (the empty-code one rejected), got %+v", got)
	}
	if want := "💡 **suggestion:** return the error\n\n```go\nreturn err\n```"; got[0].Body != want {
		t.Fatalf("with proposed_change: body = %q, want %q", got[0].Body, want)
	}
	if want := "💡 **suggestion:** return the error"; got[1].Body != want {
		t.Fatalf("without proposed_change: body = %q, want %q", got[1].Body, want)
	}

	// The declared pattern/minLength would stop the SDK at its first hit; the precheck lists every problem.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_review_comment", Arguments: map[string]any{
		"path": "", "line": 3, "body": "looks off", "proposed_change": map[string]any{"language": "go lang", "code": ""}}})
	if err != nil {
		t.Fatalf("CallTool stage_review_comment: %v", err)
	}
	msg := toolResultText(t, res)
	for _, want := range []string{"path is required", "label is required", "proposed_change.code is empty", "short language tag"} {
		if !res.IsError || !strings.Contains(msg, want) {
			t.Fatalf("one rejection must list every problem; missing %q in %q", want, msg)
		}
	}
}

func TestStageReviewCommentSchemaDeclaresProposedChangeRules(t *testing.T) {
	pc := stageReviewCommentSchema.Properties["proposed_change"]
	if pc.Properties["language"].Pattern == "" || pc.Properties["code"].MinLength == nil || *pc.Properties["code"].MinLength != 1 {
		t.Fatalf("proposed_change schema lacks the declared rules: %+v", pc.Properties)
	}
}

// TestReviewMCP_StageReviewEnforcesCaps: the tool rejects a missing takeaway and over-cap notes with the
// same caps native write_code_review enforces (CheckCodeReviewCaps), so the surfaces can't drift.
func TestReviewMCP_StageReviewEnforcesCaps(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	review := &vetting.ReviewStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: review})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stage_review",
		Arguments: map[string]any{"event": "approve"},
	})
	if err != nil {
		t.Fatalf("CallTool stage_review (no takeaway): %v", err)
	}
	if !res.IsError || !strings.Contains(toolResultText(t, res), "takeaway") {
		t.Fatalf("stage_review without a takeaway must be rejected naming the field: %+v", res)
	}

	notes := make([]any, 11)
	for i := range notes {
		notes[i] = "note"
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stage_review",
		Arguments: map[string]any{"event": "approve", "takeaway": "Fine.", "notes": notes},
	})
	if err != nil {
		t.Fatalf("CallTool stage_review (too many notes): %v", err)
	}
	got := toolResultText(t, res)
	if !res.IsError || !strings.Contains(got, "11 items, max 8") || !strings.Contains(got, "stage_review_comment") {
		t.Fatalf("stage_review with 11 notes must be rejected naming the cap and where content belongs, got %q", got)
	}
}

// TestReviewMCP_StageReturnsID: stage_review_comment returns the id to retract the finding with,
// formatted "<path>:<line>#<n>" so it reads without a lookup.
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

// TestReviewMCP_StageDuplicateReportsExistingID: staging the same path/line/body twice stages one comment
// and the second call returns the first id.
func TestReviewMCP_StageDuplicateReportsExistingID(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	review := &vetting.ReviewStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: review})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	args := map[string]any{"path": "internal/judge.go", "line": 112, "body": "blocking: unchecked error"}
	first, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_review_comment", Arguments: args})
	if err != nil || first.IsError {
		t.Fatalf("first stage_review_comment: err=%v res=%v", err, first)
	}
	firstText := toolResultText(t, first)

	second, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_review_comment", Arguments: args})
	if err != nil {
		t.Fatalf("second stage_review_comment: %v", err)
	}
	if second.IsError {
		t.Fatalf("re-staging an identical finding must not be a tool error: %s", toolResultText(t, second))
	}
	secondText := toolResultText(t, second)
	if !strings.Contains(secondText, "duplicate of") || !strings.Contains(secondText, "internal/judge.go:112#1") {
		t.Fatalf("duplicate call should report the existing id, got %q", secondText)
	}
	if strings.Contains(firstText, "duplicate") {
		t.Fatalf("first staging must not be reported as a duplicate: %q", firstText)
	}

	staged := review.ListComments()
	if len(staged) != 1 {
		t.Fatalf("want exactly 1 staged comment after the duplicate call, got %+v", staged)
	}
}

// TestReviewMCP_ListAndUnstageByID: list shows id/path/line/excerpt, unstage removes exactly that one,
// and retracting the same id twice is a visible tool error, not a silent no-op.
func TestReviewMCP_ListAndUnstageByID(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	review := &vetting.ReviewStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: review})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	longBody := "nit: " + strings.Repeat("x", 200)
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

// TestStagePR_RequiresBothFields: stage_pr keeps its required fields (only stage_push's are optional),
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

// TestStagePushMCP_OptionalFieldsOmitTheKey drives stage_push onto an open PR: a bare call,
// a full update, and a body-only update.
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

// TestToolOfferIsExclusive: a run is offered stage_pr XOR stage_push, keyed on MemSession.ExistingPR.
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

// TestPlanOnlyNodeOffersNoStagePRTool: a planOnly node's MemSession has no PRStage,
// so the live MCP server registers neither stage_pr nor stage_push.
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

// TestReviewMCP_ReportsEveryProblem: one rejected call names every bad field, so one retry fixes them all.
func TestReviewMCP_ReportsEveryProblem(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: &vetting.ReviewStage{}})
	defer vetting.UnregisterMemSession(secret)
	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	notes := make([]any, 9)
	for i := range notes {
		notes[i] = "note"
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"stage_review_comment", map[string]any{"path": "", "line": 0, "body": "no label here"},
			[]string{"path is required", "line must be a positive", "label is required"}},
		{"stage_review", map[string]any{"event": "lgtm", "takeaway": " ", "notes": notes},
			[]string{"event must be approve", "takeaway is required", "notes: 9 items, max 8"}},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err != nil {
			t.Fatalf("CallTool %s: %v", tc.tool, err)
		}
		got := toolResultText(t, res)
		for _, w := range tc.want {
			if !res.IsError || !strings.Contains(got, w) {
				t.Errorf("%s: got %q, want an error including %q", tc.tool, got, w)
			}
		}
	}
}

// TestReviewMCP_CapsDeclaredAndEveryViolationReported: the schema the worker reads states each cap, and one
// rejection still lists every over-cap item, though the SDK's own schema check stops at the first.
func TestReviewMCP_CapsDeclaredAndEveryViolationReported(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		tool string
		sess vetting.MemSession
		args map[string]any
	}{
		{"stage_review", vetting.MemSession{Review: &vetting.ReviewStage{}}, map[string]any{"event": "approve", "takeaway": "Fine."}},
		{"write_code_review", vetting.MemSession{Artifacts: artifact.InMemoryService(), AppName: "quack", UserID: "u1", ChatID: "chat-a"},
			map[string]any{"verdict": "approve", "takeaway": "Fine."}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			secret := mustMemSecret(t)
			vetting.RegisterMemSession(secret, tc.sess)
			defer vetting.UnregisterMemSession(secret)
			ts := httptest.NewServer(memoryMCPHandler())
			t.Cleanup(ts.Close)
			cs := connectMCP(t, ts, secret)

			p := toolInputSchema(t, cs, tc.tool).Properties
			if *p["takeaway"].MaxLength != 240 || *p["verified"].MaxItems != 8 || *p["verified"].Items.MaxLength != 160 || *p["notes"].Items.MaxLength != 200 {
				t.Fatalf("%s schema does not declare the caps: %+v", tc.tool, p)
			}
			tc.args["verified"] = []any{strings.Repeat("v", 162), strings.Repeat("v", 170)}
			tc.args["notes"] = []any{strings.Repeat("n", 221)}
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			got := toolResultText(t, res)
			for _, want := range []string{"verified: item 1 is 162 characters", "verified: item 2 is 170 characters", "notes: item 1 is 221 characters"} {
				if !res.IsError || !strings.Contains(got, want) {
					t.Errorf("got %q, want one rejection including %q", got, want)
				}
			}
		})
	}
}

// TestReviewMCP_ReviewNodeHasOneVerdictTool: a delivering review node records its verdict through stage_review
// alone, so the same over-cap text can't be rejected twice by a second tool taking the same fields.
func TestReviewMCP_ReviewNodeHasOneVerdictTool(t *testing.T) {
	review := vetting.NewReviewStage(nil)
	sess := vetting.MemSession{Review: review, Artifacts: artifactStub{}, AppName: "quack", UserID: "u1", ChatID: "chat-a"}
	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, sess)
	defer vetting.UnregisterMemSession(secret)
	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(ts.Close)
	cs := connectMCP(t, ts, secret)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range tools.Tools {
		got[tl.Name] = true
	}
	names := mcpToolNames(sess, true)
	if !got["stage_review"] || got["write_code_review"] || hasToolName(names, "write_code_review") {
		t.Fatalf("review node must offer stage_review and not write_code_review; registered %v, announced %v", got, names)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "stage_review", Arguments: map[string]any{
		"event": "approve", "takeaway": "Fine.",
		"dismissed": []any{map[string]any{"path": "a.go", "line": 3, "note": "guarded upstream"}}, "clean": []any{"b.go"},
	}})
	if err != nil || res.IsError {
		t.Fatalf("stage_review: err=%v res=%v", err, res)
	}
	staged, _ := review.Snapshot()
	if len(staged.Dismissed) != 1 || staged.Dismissed[0].Path != "a.go" || len(staged.Clean) != 1 {
		t.Fatalf("staged dismissed/clean = %+v / %v, want the call's", staged.Dismissed, staged.Clean)
	}
}

func toolInputSchema(t *testing.T, cs *mcp.ClientSession, name string) *jsonschema.Schema {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tl := range res.Tools {
		if tl.Name != name {
			continue
		}
		var s jsonschema.Schema
		b, _ := json.Marshal(tl.InputSchema)
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		return &s
	}
	t.Fatalf("no %s tool", name)
	return nil
}

// label is its own enum field: a body without one stages as "<label>: <body>", and the schema lists the four labels.
func TestStageReviewComment_LabelField(t *testing.T) {
	if got := stageReviewCommentSchema.Properties["label"].Enum; len(got) != 4 || got[0] != "blocking" {
		t.Fatalf("label enum = %v, want the four Conventional Comments labels", got)
	}
	if err := checkReviewComment("a.go", 3, vetting.WithCommentLabel("question", "is nil possible here?"), nil); err != nil {
		t.Fatalf("a labeled field with an unlabeled body must pass: %v", err)
	}
}
