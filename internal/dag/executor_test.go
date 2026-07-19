package dag

import (
	"fmt"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// drive feeds synthetic workflow events through a dagStream and returns the SSE
// events it produced. scoreOf is fixed so node_done carries a known judge result.
func drive(evs []*session.Event, agentByID map[string]string, score gateScore) []stream.SSEEvent {
	var got []stream.SSEEvent
	ds := newDagStream(agentByID,
		func(ev stream.SSEEvent, _ error) bool { got = append(got, ev); return true },
		map[string]string{},
		func(string) gateScore { return score },
		func(string) bool { return false },
		func(string) bool { return false },
		func(string, int) string { return "" },
	)
	for _, ev := range evs {
		ds.handle(ev)
	}
	ds.flush()
	return got
}

func names(evs []stream.SSEEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Name
	}
	return out
}

func ev(path string, parts ...*genai.Part) *session.Event {
	e := &session.Event{NodeInfo: &session.NodeInfo{Path: path}}
	if len(parts) > 0 {
		e.Content = &genai.Content{Role: "model", Parts: parts}
	}
	return e
}

// TestDagStream_WorkerActivityAndNodeDone: a worker run's thinking/tool/token
// activity is translated to SSE under the plan node, bracketed by agent_start/
// agent_complete, and the node's output event yields node_done with the judge
// score from state.
func TestDagStream_WorkerActivityAndNodeDone(t *testing.T) {
	const wpath = "quack-dag-p@1/n1@rr/web-researcher@worker-r0"
	const npath = "quack-dag-p@1/n1@rr"
	agentByID := map[string]string{"n1": "web-researcher"}

	nodeOut := &session.Event{NodeInfo: &session.NodeInfo{Path: npath}, Output: "final vetted answer"}
	evs := []*session.Event{
		ev(wpath, &genai.Part{Thought: true, Text: "let me think"}),
		ev(wpath, &genai.Part{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "web_search", Args: map[string]any{"query": "q"}}}),
		ev(wpath, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "web_search", Response: map[string]any{"results": "r"}}}),
		ev(wpath, &genai.Part{Text: "draft answer"}),
		nodeOut,
	}

	got := drive(evs, agentByID, gateScore{score: 0.9, passed: true, rounds: 2})

	want := []string{
		stream.EventNodeStart,
		stream.EventAgentStart,
		stream.EventAgentThinking,
		stream.EventAgentToolCall,
		stream.EventAgentToolResult,
		stream.EventAgentToken,
		stream.EventAgentComplete,
		stream.EventNodeDone,
	}
	if g := names(got); !equalStrings(g, want) {
		t.Fatalf("event sequence:\n got=%v\nwant=%v", g, want)
	}

	// agent_start is scoped to the plan node with the worker stage/run.
	as := got[1].Data.(stream.AgentStartData)
	if as.NodeID != "n1" || as.RunID != "worker-r0" || as.Stage != stream.StageWorker {
		t.Fatalf("agent_start = %+v", as)
	}
	// tool call/result pair by call id, scoped to the node.
	tc := got[3].Data.(stream.AgentToolCallData)
	if tc.NodeID != "n1" || tc.CallID != "c1" || tc.Name != "web_search" {
		t.Fatalf("tool_call = %+v", tc)
	}
	// node_done carries the captured output + the judge score from state.
	nd := got[7].Data.(stream.NodeDoneData)
	if nd.NodeID != "n1" || nd.Output != "final vetted answer" {
		t.Fatalf("node_done output = %+v", nd)
	}
	if nd.JudgeFinalScore != 0.9 || !nd.JudgePassed || nd.JudgeRounds != 2 {
		t.Fatalf("node_done judge fields = %+v", nd)
	}
}

// TestDagStream_ReviseRoundStage: a second worker run (worker-r1) is a revise
// stage; switching runs closes the prior run first.
func TestDagStream_ReviseRoundStage(t *testing.T) {
	const r0 = "quack-dag-p@1/n1@rr/web-researcher@worker-r0"
	const r1 = "quack-dag-p@1/n1@rr/web-researcher@worker-r1"
	agentByID := map[string]string{"n1": "web-researcher"}

	got := drive([]*session.Event{
		ev(r0, &genai.Part{Text: "draft"}),
		ev(r1, &genai.Part{Text: "revised"}),
	}, agentByID, gateScore{})

	// node_start, agent_start(r0), token, agent_complete(r0), agent_start(r1), token, [flush] agent_complete(r1)
	want := []string{
		stream.EventNodeStart,
		stream.EventAgentStart, stream.EventAgentToken, stream.EventAgentComplete,
		stream.EventAgentStart, stream.EventAgentToken, stream.EventAgentComplete,
	}
	if g := names(got); !equalStrings(g, want) {
		t.Fatalf("sequence:\n got=%v\nwant=%v", g, want)
	}
	if s := got[4].Data.(stream.AgentStartData); s.Stage != stream.StageRevise || s.Round != 1 {
		t.Fatalf("revise agent_start = %+v", s)
	}
}

func TestStageRound(t *testing.T) {
	cases := []struct {
		run   string
		stage string
		round int
	}{
		{"worker-r0", stream.StageWorker, 0},
		{"worker-r1", stream.StageRevise, 1},
		{"worker-r3", stream.StageRevise, 3},
		{"worker-finalize-1", stream.StageWorker, 0},
	}
	for _, c := range cases {
		if s, r := stageRound(c.run); s != c.stage || r != c.round {
			t.Errorf("stageRound(%q) = (%q,%d), want (%q,%d)", c.run, s, r, c.stage, c.round)
		}
	}
}

func TestPlanNodeInPath(t *testing.T) {
	agentByID := map[string]string{"research-openai": "web-researcher", "synth": "synthesizer"}
	cases := map[string]string{
		"quack-dag-p@1/research-openai@r/web-researcher@worker-r0": "research-openai",
		"quack-dag-p@1/synth@r":      "synth",
		"quack-dag-p@1/synth-join@r": "", // join node, not a plan node
		"quack-dag-p@1":              "",
	}
	for path, want := range cases {
		if got := planNodeInPath(path, agentByID); got != want {
			t.Errorf("planNodeInPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDagStream_SteeredRunEmitsNodeSteered: the first event of a -sN steered
// re-run announces node_steered (with the delivered guidance) BEFORE the run's
// agent_start — each generation exactly once. Regression: live e2e 2026-07-05,
// steers landed server-side but the UI never heard about them (no emitter).
func TestDagStream_SteeredRunEmitsNodeSteered(t *testing.T) {
	agentByID := map[string]string{"n1": "web-researcher"}
	var got []stream.SSEEvent
	ds := newDagStream(agentByID,
		func(e stream.SSEEvent, _ error) bool { got = append(got, e); return true },
		map[string]string{},
		func(string) gateScore { return gateScore{} },
		func(string) bool { return false },
		func(string) bool { return false },
		func(node string, gen int) string { return fmt.Sprintf("guidance-%s-%d", node, gen) },
	)
	evs := []*session.Event{
		ev("quack-dag-p@1/n1@rr/web-researcher@worker-r0", &genai.Part{Text: "draft"}),
		ev("quack-dag-p@1/n1@rr/web-researcher@worker-r0-s1", &genai.Part{Text: "steered draft"}),
	}
	for _, e := range evs {
		ds.handle(e)
	}

	var steered []stream.NodeSteeredData
	steerIdx, startIdx := -1, -1
	for i, e := range got {
		if d, ok := e.Data.(stream.NodeSteeredData); ok {
			steered = append(steered, d)
			steerIdx = i
		}
		if e.Name == stream.EventAgentStart {
			if d, ok := e.Data.(stream.AgentStartData); ok && d.RunID == "worker-r0-s1" {
				startIdx = i
			}
		}
	}
	if len(steered) != 1 {
		t.Fatalf("node_steered emitted %d times, want exactly 1 (once per generation); events=%v", len(steered), names(got))
	}
	if steered[0].NodeID != "n1" || steered[0].Guidance != "guidance-n1-1" {
		t.Errorf("node_steered = %+v, want node n1 with the delivered guidance", steered[0])
	}
	if steerIdx > startIdx {
		t.Errorf("node_steered (idx %d) must precede the steered run's agent_start (idx %d)", steerIdx, startIdx)
	}
}
