package bundle

import (
	"testing"
	"time"
)

// Native: each llm.call is a turn, functionCall parts are calls, and an errored response counts once
// though every later input replays it. The judge's own calls on the node never count.
func TestNodeActivity_Native(t *testing.T) {
	user := `{"role":"user","parts":[{"text":"synthesize"}]}`
	call := func(id string) string {
		return `{"role":"model","parts":[{"functionCall":{"id":"` + id + `","name":"read_artifact"}}]}`
	}
	bad := `{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"read_artifact","response":{"error":"not found"}}}]}`
	good := `{"role":"user","parts":[{"functionResponse":{"id":"c2","name":"read_artifact","response":{"result":"ok"}}}]}`
	turn := func(s int, round, in, out string) entry {
		return chat(t0().Add(time.Duration(s)*time.Second), "n1", "synthesizer", round, "m",
			map[string]any{"gen_ai.input.messages": in, "gen_ai.output.messages": out})
	}
	path := writeJSONL(t, []entry{
		turn(1, "worker-r0", "["+user+"]", call("c1")),
		turn(2, "worker-r0", "["+user+","+call("c1")+","+bad+"]", call("c2")),
		turn(3, "worker-r0", "["+user+","+call("c1")+","+bad+","+call("c2")+","+good+"]", `{"role":"model","parts":[{"text":"done"}]}`),
		turn(4, "worker-r1", "["+user+"]", `{"role":"model","parts":[{"text":"revised"}]}`),
		chat(t0(), "n1", "judge", "judge-r1", "m", map[string]any{"gen_ai.output.messages": call("j1")}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sess.NodeActivity("n1", "synthesizer"), (Activity{ModelTurns: 4, ToolCalls: 2, RejectedToolCalls: 1}); got != want {
		t.Fatalf("NodeActivity = %+v, want %+v", got, want)
	}
}

// ACP: a turn is a run of model output between tool frames, a call repeated across updates counts
// once, and a failed tool_call_update is a rejected call.
func TestNodeActivity_ACP(t *testing.T) {
	update := func(u string) string {
		return `{"jsonrpc":"2.0","method":"session/update","params":{"update":` + u + `}}`
	}
	received := []string{
		update(`{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"hm"}}`),
		update(`{"sessionUpdate":"usage_update","used":10}`),
		update(`{"sessionUpdate":"tool_call","toolCallId":"a","status":"in_progress"}`),
		update(`{"sessionUpdate":"tool_call","toolCallId":"b","status":"in_progress"}`),
		update(`{"sessionUpdate":"tool_call_update","toolCallId":"a","status":"completed"}`),
		update(`{"sessionUpdate":"tool_call_update","toolCallId":"b","status":"failed"}`),
		update(`{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"again"}}`),
		update(`{"sessionUpdate":"tool_call","toolCallId":"c","status":"in_progress"}`),
		update(`{"sessionUpdate":"tool_call","toolCallId":"c","status":"in_progress"}`),
		update(`{"sessionUpdate":"tool_call_update","toolCallId":"c","status":"completed"}`),
		// Create-on-update: a call announced only by its failed update is still a call.
		update(`{"sessionUpdate":"tool_call_update","toolCallId":"d","status":"failed"}`),
		update(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"LGTM"}}`),
		`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`,
	}
	// The round's per-call usage rows (acp emitLLMCall) carry no content, so they add no turns, and its
	// tool.call rows (acp recordTool) repeat calls the frames already count.
	usageRow := chat(t0(), "n1", "code-reviewer", "", "m", map[string]any{"gen_ai.usage.output_tokens": 7})
	toolRow := entry{ts: t0(), attrs: map[string]any{"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": "stage_review",
		"quack.node": "n1", "gen_ai.agent.name": "code-reviewer", "error.type": "notes: item 1 is 221 characters, max 200"}}
	path := writeJSONL(t, []entry{invokeAgent(t0(), "n1", "code-reviewer", "", nil, received), usageRow, usageRow, toolRow})
	sess, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sess.NodeActivity("n1", "code-reviewer"), (Activity{ModelTurns: 3, ToolCalls: 4, RejectedToolCalls: 2}); got != want {
		t.Fatalf("NodeActivity = %+v, want %+v", got, want)
	}
}
