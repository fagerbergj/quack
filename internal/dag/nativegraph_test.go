package dag

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// graphStub drives a 3-node plan: the ASK-TASK worker asks the user, the
// PLAIN-TASK worker answers immediately, the SYNTH-TASK worker combines. The
// judge always passes. Counters expose which workers ran.
type graphStub struct {
	mu          sync.Mutex
	plainRuns   int
	synthPrompt string
	sawAnswer   string
}

func (*graphStub) Name() string { return "graphStub" }

func (s *graphStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		txt := gUserText(req)
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.Contains(txt, "they have now answered"):
			if i := strings.Index(txt, "\nA: "); i >= 0 {
				line := txt[i+4:]
				if j := strings.IndexByte(line, '\n'); j >= 0 {
					line = line[:j]
				}
				s.sawAnswer = line
			}
			yield(gText("ASKER-RESULT"), nil)
		case strings.Contains(txt, "SYNTH-TASK"):
			s.synthPrompt = txt
			yield(gText("COMBINED-RESULT"), nil)
		case strings.Contains(txt, "PLAIN-TASK"):
			s.plainRuns++
			yield(gText("PLAIN-RESULT"), nil)
		default: // ASK-TASK
			yield(gCall(vetting.AskToolName, map[string]any{"question": "which direction?"}), nil)
		}
	}
}

// TestRunPlanAsGraph_HITLPauseResume is THE goal proof: a plan run as a native
// first-class ADK graph where one researcher pauses to ask the user. Run 1: the
// sibling completes (node_done), the asker parks (node_needs_input), the
// synthesizer waits — nothing false-fails. Run 2 (the answer as an
// adk_request_input FunctionResponse on the same session): ADK re-enters ONLY the
// asker (the sibling durably skips — its worker never re-runs), the join settles
// via the wrapper's state patch, and the synthesizer produces the terminal answer
// from both outputs.
func TestRunPlanAsGraph_HITLPauseResume(t *testing.T) {
	stub := &graphStub{}
	ag, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{newAskTool(t)},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": ag, "synthesizer": ag}, nil, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "p", UserMessage: "go", Nodes: []Node{
		{ID: "n1", AgentName: "blk", Task: "ASK-TASK"},
		{ID: "n2", AgentName: "blk", Task: "PLAIN-TASK"},
		{ID: "synth", AgentName: "synthesizer", Task: "SYNTH-TASK", DependsOn: []string{"n1", "n2"}},
	}}

	type evRec struct {
		name string
		node string
	}
	var mu sync.Mutex
	var events []evRec
	record := func(ev stream.SSEEvent, _ error) bool {
		mu.Lock()
		defer mu.Unlock()
		switch d := ev.Data.(type) {
		case stream.NodeDoneData:
			events = append(events, evRec{stream.EventNodeDone, d.NodeID})
		case stream.NodeFailedData:
			events = append(events, evRec{stream.EventNodeFailed, d.NodeID})
		case stream.NodeNeedsInputData:
			events = append(events, evRec{stream.EventNodeNeedsInput, d.NodeID + "|" + d.InterruptID + "|" + d.Message})
		}
		return true
	}
	has := func(name, node string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range events {
			if e.name == name && strings.HasPrefix(e.node, node) {
				return true
			}
		}
		return false
	}

	// ---- Run 1: fresh — parks at n1 ----
	outputs := map[string]string{}
	start := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}
	paused, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "chat", start, record, outputs, nil)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if !paused {
		t.Fatal("run1: expected a paused run")
	}
	if !has(stream.EventNodeDone, "n2") {
		t.Errorf("run1: sibling n2 should complete with node_done")
	}
	if !has(stream.EventNodeNeedsInput, "n1|hitl-n1-r1|which direction?") {
		t.Errorf("run1: expected node_needs_input for n1 with the question; events=%v", events)
	}
	if has(stream.EventNodeFailed, "") {
		t.Errorf("run1: nothing should fail; events=%v", events)
	}
	if has(stream.EventNodeDone, "synth") {
		t.Errorf("run1: synth must wait behind the pause")
	}
	if got := stub.plainRuns; got != 1 {
		t.Fatalf("run1: n2 worker ran %d times, want 1", got)
	}

	// ---- Run 2: deliver the answer — only n1 + downstream re-run ----
	mu.Lock()
	events = nil
	mu.Unlock()
	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID: "hitl-n1-r1", Name: workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": "north"},
		},
	}}}
	outputs2 := map[string]string{}
	paused2, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "chat", answer, record, outputs2, []string{"n1"})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if paused2 {
		t.Fatal("run2: should complete, not pause again")
	}
	if stub.plainRuns != 1 {
		t.Errorf("run2: sibling n2 re-ran (%d total) — durable skip broken", stub.plainRuns)
	}
	if stub.sawAnswer != "north" {
		t.Errorf("run2: asker never received the user's answer (saw %q)", stub.sawAnswer)
	}
	if !has(stream.EventNodeDone, "n1") || !has(stream.EventNodeDone, "synth") {
		t.Errorf("run2: n1 and synth should complete; events=%v", events)
	}
	if has(stream.EventNodeFailed, "") {
		t.Errorf("run2: nothing should fail; events=%v", events)
	}
	if outputs2["synth"] != "COMBINED-RESULT" {
		t.Errorf("run2: terminal output = %q, want COMBINED-RESULT", outputs2["synth"])
	}
	// The synthesizer must have seen BOTH upstreams: the resumed asker's answer and
	// the durably-skipped sibling's output (delivered through the patched join).
	if !strings.Contains(stub.synthPrompt, "ASKER-RESULT") || !strings.Contains(stub.synthPrompt, "PLAIN-RESULT") {
		t.Errorf("run2: synth prompt missing upstream outputs:\n%s", stub.synthPrompt)
	}
}
