package acp

import (
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

func TestTranslate_ThoughtAndMessageChunks(t *testing.T) {
	tr := newTranslator("/work")

	specs := tr.translate(sdk.UpdateAgentThoughtText("planning"))
	if len(specs) != 1 || !specs[0].partial || !specs[0].parts[0].Thought || specs[0].parts[0].Text != "planning" {
		t.Fatalf("thought chunk: got %+v", specs)
	}

	tr.translate(sdk.UpdateAgentMessageText("did the "))
	tr.translate(sdk.UpdateAgentMessageText("thing"))
	final := finalSpec(tr)
	if final.partial || final.parts[0].Text != "did the thing" {
		t.Fatalf("final answer: got partial=%v text=%q", final.partial, final.parts[0].Text)
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

// An edit's diff carries the interesting fields; the path must come back
// node-relative (the ledger/judge namespace), and a diff outside the node dir
// stays absolute so the jail refuses it downstream.
func TestTranslate_EditDiffToWriteFile(t *testing.T) {
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
	if call.Name != "write_file" || call.Args["path"] != "sub/main.go" {
		t.Fatalf("edit should map to write_file with a node-relative path, got %s %v", call.Name, call.Args)
	}
	if resp.Response["bytes"] != 3 || resp.Response["created"] != false {
		t.Fatalf("diff outcome lost: %+v", resp.Response)
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
// tool_call for (spec-sanctioned) — create-on-update, never a panic.
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
