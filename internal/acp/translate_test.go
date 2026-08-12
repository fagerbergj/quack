package acp

import (
	"strings"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// A thought chunk small enough to miss both coalescing thresholds buffers
// rather than emitting immediately - it surfaces once a later update flushes it.
func TestTranslate_ThoughtAndMessageChunks(t *testing.T) {
	tr := newTranslator("/work")

	specs := tr.translate(sdk.UpdateAgentThoughtText("planning"))
	if specs != nil {
		t.Fatalf("small thought chunk should buffer, not emit: got %+v", specs)
	}

	specs = tr.translate(sdk.UpdateAgentMessageText("did the "))
	if len(specs) != 2 {
		t.Fatalf("message chunk should flush the buffered thought first: got %d specs", len(specs))
	}
	if !specs[0].partial || !specs[0].parts[0].Thought || specs[0].parts[0].Text != "planning" {
		t.Fatalf("flushed thought: got %+v", specs[0])
	}
	if specs[1].parts[0].Thought {
		t.Fatalf("message spec should not be a thought: got %+v", specs[1])
	}
	if specs[1].parts[0].Text != "did the " {
		t.Fatalf("message text: got %+v", specs[1])
	}

	tr.translate(sdk.UpdateAgentMessageText("thing"))
	final := finalSpec(tr)
	if final.partial || final.parts[0].Text != "did the thing" {
		t.Fatalf("final answer: got partial=%v text=%q", final.partial, final.parts[0].Text)
	}
}

// N small deltas under both thresholds stay buffered; nothing has been emitted yet.
func TestTranslate_ThoughtCoalescesSmallDeltas(t *testing.T) {
	tr := newTranslator("/work")
	for i := 0; i < 20; i++ {
		if specs := tr.translate(sdk.UpdateAgentThoughtText(" tok")); specs != nil {
			t.Fatalf("delta %d: expected no emission under threshold, got %+v", i, specs)
		}
	}
	if tr.thinking.String() != strings.Repeat(" tok", 20) {
		t.Fatalf("buffered text: got %q", tr.thinking.String())
	}
}

// Once the byte threshold is crossed mid-stream, the whole batch flushes as
// one event and the buffer resets for the next batch.
func TestTranslate_ThoughtFlushesOnByteThreshold(t *testing.T) {
	tr := newTranslator("/work")
	big := strings.Repeat("x", thinkFlushBytes)
	specs := tr.translate(sdk.UpdateAgentThoughtText(big))
	if len(specs) != 1 || !specs[0].parts[0].Thought || specs[0].parts[0].Text != big {
		t.Fatalf("byte-threshold flush: got %+v", specs)
	}
	if tr.thinking.Len() != 0 {
		t.Fatalf("buffer should reset after flush, got %d bytes left", tr.thinking.Len())
	}
}

// Once the elapsed-time threshold is crossed, the next delta flushes
// everything buffered so far (including itself) as one event.
func TestTranslate_ThoughtFlushesOnElapsed(t *testing.T) {
	tr := newTranslator("/work")
	tr.translate(sdk.UpdateAgentThoughtText("a"))
	tr.thinkOpen = time.Now().Add(-thinkFlushElapsed - time.Millisecond)
	specs := tr.translate(sdk.UpdateAgentThoughtText("b"))
	if len(specs) != 1 || specs[0].parts[0].Text != "ab" {
		t.Fatalf("elapsed-threshold flush: got %+v", specs)
	}
}

// A non-thinking event (here, a tool call) flushes any pending thought first,
// ordered ahead of the event's own spec.
func TestTranslate_ThoughtFlushesBeforeToolCall(t *testing.T) {
	tr := newTranslator("/work")
	tr.translate(sdk.UpdateAgentThoughtText("checking the config"))
	specs := tr.translate(sdk.StartToolCall("t1", "Read config.go", sdk.WithStartKind(sdk.ToolKindRead)))
	if len(specs) != 2 {
		t.Fatalf("want flushed-thought + tool-call specs, got %d", len(specs))
	}
	if !specs[0].parts[0].Thought || specs[0].parts[0].Text != "checking the config" {
		t.Fatalf("flushed thought: got %+v", specs[0])
	}
	if specs[1].parts[0].FunctionCall == nil {
		t.Fatalf("tool-call spec: got %+v", specs[1])
	}
}

// Narration before a tool call ("I'll investigate...") must not survive into
// the delivered answer - only the contiguous block after the last tool call
// does (#358).
func TestTranslate_AnswerResetsOnToolCall(t *testing.T) {
	tr := newTranslator("/work")

	tr.translate(sdk.UpdateAgentMessageText("I'll investigate the config now."))
	tr.translate(sdk.StartToolCall("t1", "Read config.go", sdk.WithStartKind(sdk.ToolKindRead)))
	tr.translate(sdk.UpdateToolCall("t1", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted)))
	tr.translate(sdk.UpdateAgentMessageText("Here is the final plan."))

	final := finalSpec(tr)
	if final.parts[0].Text != "Here is the final plan." {
		t.Fatalf("pre-tool-call narration leaked into the answer: got %q", final.parts[0].Text)
	}
}

// The durable call/response pair lands only at the TERMINAL update, in quack's
// tool vocabulary, ordered call-then-response so the ledger's pairing scan
// works within the single event.
func TestTranslate_ExecuteToolCallPair(t *testing.T) {
	tr := newTranslator("/work")

	specs := tr.translate(sdk.StartToolCall("t1", "go test ./...",
		sdk.WithStartKind(sdk.ToolKindExecute),
		sdk.WithStartRawInput(map[string]any{"command": "go test ./..."})))
	if len(specs) != 1 || !specs[0].partial || specs[0].parts[0].FunctionCall == nil {
		t.Fatalf("pending tool call should stream a partial FunctionCall, got %+v", specs)
	}
	if got := specs[0].parts[0].FunctionCall.Name; got != "run_command" {
		t.Fatalf("execute kind should map to run_command, got %q", got)
	}

	specs = tr.translate(sdk.UpdateToolCall("t1",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateRawOutput(map[string]any{"exit": float64(0), "output": "ok"})))
	if len(specs) != 1 || specs[0].partial {
		t.Fatalf("terminal update should emit one durable spec, got %+v", specs)
	}
	parts := specs[0].parts
	if len(parts) != 2 || parts[0].FunctionCall == nil || parts[1].FunctionResponse == nil {
		t.Fatalf("want call+response pair, got %+v", parts)
	}
	if parts[0].FunctionCall.ID != "t1" || parts[1].FunctionResponse.ID != "t1" {
		t.Fatalf("pair must share the toolCallId")
	}
	if parts[0].FunctionCall.Args["command"] != "go test ./..." {
		t.Fatalf("command arg lost: %+v", parts[0].FunctionCall.Args)
	}
	if parts[1].FunctionResponse.Response["exit_code"] != 0 {
		t.Fatalf("exit_code lost: %+v", parts[1].FunctionResponse.Response)
	}
}

// An edit's diff carries the interesting fields; #388 - this must map to
// "edit_file" (not "write_file") so the frontend's ToolCallView keys it to
// the before→after diff view native edit_file calls get, with the path
// resolved node-relative (the ledger/judge namespace) and old/new text
// carried in args (EditFileView's diff source), never absolute for a path
// inside the node dir.
func TestTranslate_EditDiffToEditFile(t *testing.T) {
	tr := newTranslator("/work")
	old := "a"
	tr.translate(sdk.StartToolCall("t2", "Edit main.go", sdk.WithStartKind(sdk.ToolKindEdit)))
	specs := tr.translate(sdk.UpdateToolCall("t2",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateContent([]sdk.ToolCallContent{{Diff: &sdk.ToolCallContentDiff{Path: "/work/sub/main.go", OldText: &old, NewText: "abc"}}})))
	if len(specs) != 1 {
		t.Fatalf("want one durable spec, got %d", len(specs))
	}
	call, resp := specs[0].parts[0].FunctionCall, specs[0].parts[1].FunctionResponse
	if call.Name != "edit_file" || call.Args["path"] != "sub/main.go" {
		t.Fatalf("edit should map to edit_file with a node-relative path, got %s %v", call.Name, call.Args)
	}
	if call.Args["old"] != "a" || call.Args["new"] != "abc" {
		t.Fatalf("diff text lost: %+v", call.Args)
	}
	if resp.Response["replacements"] != 1 {
		t.Fatalf("replacement note lost: %+v", resp.Response)
	}
}

// A new file has no OldText; edit_file's args carry an empty old string (the
// diff view then renders every line as added) rather than dropping the diff.
func TestTranslate_EditDiffNewFile(t *testing.T) {
	tr := newTranslator("/work")
	tr.translate(sdk.StartToolCall("t2", "Create main.go", sdk.WithStartKind(sdk.ToolKindEdit)))
	specs := tr.translate(sdk.UpdateToolCall("t2",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateContent([]sdk.ToolCallContent{{Diff: &sdk.ToolCallContentDiff{Path: "/work/main.go", NewText: "package main"}}})))
	call := specs[0].parts[0].FunctionCall
	if call.Name != "edit_file" || call.Args["old"] != "" || call.Args["new"] != "package main" {
		t.Fatalf("new-file diff mismapped: %+v", call.Args)
	}
}

// An edit with no diff content (some agents omit it) has nothing to show a
// before→after for - falls back to the plainer write_file view rather than
// rendering edit_file with an empty diff.
func TestTranslate_EditWithoutDiffFallsBackToWriteFile(t *testing.T) {
	tr := newTranslator("/work")
	specs := tr.translate(sdk.UpdateToolCall("t9",
		sdk.WithUpdateKind(sdk.ToolKindEdit),
		sdk.WithUpdateLocations([]sdk.ToolCallLocation{{Path: "/work/no-diff.go"}}),
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted)))
	call := specs[0].parts[0].FunctionCall
	if call.Name != "write_file" || call.Args["path"] != "no-diff.go" {
		t.Fatalf("diff-less edit should fall back to write_file, got %s %v", call.Name, call.Args)
	}
}

func TestTranslate_FailedToolCallCarriesError(t *testing.T) {
	tr := newTranslator("/work")
	tr.translate(sdk.StartToolCall("t3", "npm test", sdk.WithStartKind(sdk.ToolKindExecute)))
	specs := tr.translate(sdk.UpdateToolCall("t3",
		sdk.WithUpdateStatus(sdk.ToolCallStatusFailed),
		sdk.WithUpdateRawOutput(map[string]any{"output": "boom"})))
	resp := specs[0].parts[1].FunctionResponse.Response
	if resp["error"] != "boom" {
		t.Fatalf("failed call must carry an error key (recordWsOp marks it FAILED), got %+v", resp)
	}
}

// Agents may send a tool_call_update for an id the client never saw a
// tool_call for (spec-sanctioned) - create-on-update, never a panic.
func TestTranslate_CreateOnUpdate(t *testing.T) {
	tr := newTranslator("/work")
	kind := sdk.ToolKindExecute
	specs := tr.translate(sdk.UpdateToolCall("ghost",
		sdk.WithUpdateKind(kind),
		sdk.WithUpdateTitle("ls"),
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted)))
	if len(specs) != 1 || specs[0].parts[0].FunctionCall.Name != "run_command" {
		t.Fatalf("create-on-update failed: %+v", specs)
	}
}

func TestTranslate_UnknownVariantsSkipped(t *testing.T) {
	tr := newTranslator("/work")
	if specs := tr.translate(sdk.UpdatePlan(sdk.PlanEntry{Content: "step"})); specs != nil {
		t.Fatalf("plan updates should be skipped, got %+v", specs)
	}
	if specs := tr.translate(sdk.SessionUpdate{}); specs != nil {
		t.Fatalf("empty update should be skipped, got %+v", specs)
	}
}

func TestTranslate_UsageRidesFinalEvent(t *testing.T) {
	tr := newTranslator("/work")
	tr.translate(sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{Used: 1234, Size: 65536}})
	tr.translate(sdk.UpdateAgentMessageText("done"))
	final := finalSpec(tr)
	if final.usage == nil || final.usage.TotalTokenCount != 1234 {
		t.Fatalf("usage lost: %+v", final.usage)
	}
}

// A delete kind has a direct native twin (delete_path) - the frontend's
// DeletePathView reads args.path + result.deleted.
func TestTranslate_DeleteKindMapsToDeletePath(t *testing.T) {
	tr := newTranslator("/work")
	tr.translate(sdk.StartToolCall("t4", "Delete old.go", sdk.WithStartKind(sdk.ToolKindDelete)))
	specs := tr.translate(sdk.UpdateToolCall("t4",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateLocations([]sdk.ToolCallLocation{{Path: "/work/old.go"}})))
	call, resp := specs[0].parts[0].FunctionCall, specs[0].parts[1].FunctionResponse
	if call.Name != "delete_path" || call.Args["path"] != "old.go" {
		t.Fatalf("delete should map to delete_path with a node-relative path, got %s %v", call.Name, call.Args)
	}
	if resp.Response["deleted"] != true {
		t.Fatalf("deleted flag lost: %+v", resp.Response)
	}
}

// ACP's "search" kind has no tool-identity field to distinguish content grep
// from filename glob - both map onto grep's arg shape, carrying pattern/path/glob.
func TestTranslate_SearchKindMapsToGrep(t *testing.T) {
	tr := newTranslator("/work")
	specs := tr.translate(sdk.StartToolCall("t5", "grep TODO", sdk.WithStartKind(sdk.ToolKindSearch),
		sdk.WithStartRawInput(map[string]any{"pattern": "TODO", "path": "/work/internal", "include": "*.go"})))
	call := specs[0].parts[0].FunctionCall
	if call.Name != "grep" {
		t.Fatalf("search should map to grep, got %s", call.Name)
	}
	if call.Args["pattern"] != "TODO" || call.Args["path"] != "internal" || call.Args["glob"] != "*.go" {
		t.Fatalf("search args lost: %+v", call.Args)
	}
}

// A kind with no display twin keeps its real name but still carries its
// rawInput (not just the title) so the generic renderer shows substance.
func TestTranslate_UnmappedKindKeepsNameAndArgs(t *testing.T) {
	tr := newTranslator("/work")
	tr.translate(sdk.StartToolCall("t6", "Move file", sdk.WithStartKind(sdk.ToolKindMove),
		sdk.WithStartRawInput(map[string]any{"from": "a.go", "to": "b.go"})))
	specs := tr.translate(sdk.UpdateToolCall("t6",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateRawOutput(map[string]any{"output": "renamed"})))
	call, resp := specs[0].parts[0].FunctionCall, specs[0].parts[1].FunctionResponse
	if call.Name != "move" {
		t.Fatalf("unmapped kind should keep its real name, got %s", call.Name)
	}
	if call.Args["from"] != "a.go" || call.Args["to"] != "b.go" || call.Args["title"] != "Move file" {
		t.Fatalf("unmapped kind should carry rawInput + title, got %+v", call.Args)
	}
	if resp.Response["output"] != "renamed" {
		t.Fatalf("unmapped kind should still get a result preview, got %+v", resp.Response)
	}
}

// A long tool result is bounded, never dumped verbatim into the event stream.
func TestTranslate_ResultPreviewIsBounded(t *testing.T) {
	tr := newTranslator("/work")
	huge := strings.Repeat("o", 5000)
	tr.translate(sdk.StartToolCall("t7", "go test ./...", sdk.WithStartKind(sdk.ToolKindExecute)))
	specs := tr.translate(sdk.UpdateToolCall("t7",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateRawOutput(map[string]any{"output": huge})))
	resp := specs[0].parts[1].FunctionResponse.Response
	out, _ := resp["output"].(string)
	if len(out) >= len(huge) {
		t.Fatalf("result preview should be bounded, got %d bytes", len(out))
	}
	if !strings.HasSuffix(out, "…[truncated]") {
		t.Fatalf("bounded preview should mark truncation, got suffix %q", out[len(out)-20:])
	}
}
