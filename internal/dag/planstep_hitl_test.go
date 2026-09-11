package dag

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestRunPlanStep_HITLPauseThenResume is BLOCKING 1's proof (#slice3
// review): a node paused mid-incremental-step, through the REAL
// RunPlanStep/ResumePlanStep pair (not a fake), parks cleanly on step 1 and
// finishes once step 2 answers it - the same adk_request_input
// FunctionResponse shape and interrupt id format (hitl-<node>-r<round>)
// orchestrator.go's resume path already uses for a whole-plan resume.
func TestRunPlanStep_HITLPauseThenResume(t *testing.T) {
	stub := &graphStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "asker", Model: stub, Description: "asker", Instruction: "ROLE:asker",
		Tools: []tool.Tool{newAskTool(t)},
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{"asker": worker}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "p", UserMessage: "go", Nodes: []Node{
		{ID: "n1", AgentName: "asker", Task: "ASK-TASK"},
	}}

	var paused []string
	var done []string
	record := func(ev stream.SSEEvent, _ error) bool {
		switch d := ev.Data.(type) {
		case stream.NodeNeedsInputData:
			paused = append(paused, d.NodeID+"|"+d.InterruptID+"|"+d.Message)
		case stream.NodeDoneData:
			done = append(done, d.NodeID)
		}
		return true
	}
	ctx := stream.WithYield(context.Background(), func(ev stream.SSEEvent) { record(ev, nil) })

	// ---- Step 1: fresh dispatch - parks on the question ----
	run := map[string]bool{"n1": true}
	outputs1, needsInput1, err := ex.RunPlanStep(ctx, plan, "quack", "u", "chat", nil, run)
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if !needsInput1["n1"] {
		t.Fatalf("step 1: needsInput = %v, want n1", needsInput1)
	}
	if outputs1["n1"] != "" {
		t.Errorf("step 1: outputs[n1] = %q, want empty - it hasn't answered yet", outputs1["n1"])
	}
	if len(paused) != 1 || paused[0] != "n1|hitl-n1-r1|which direction?" {
		t.Fatalf("step 1: node_needs_input = %v, want exactly one for n1 with the question", paused)
	}
	if len(done) != 0 {
		t.Fatalf("step 1: node_done = %v, want none", done)
	}

	// ---- Step 2: the answer arrives (StartNode's own content shape) - n1 resumes and finishes ----
	outputs2, needsInput2, err := ex.ResumePlanStep(ctx, plan, "quack", "u", "chat", nil, run, "hitl-n1-r1", "north")
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if len(needsInput2) > 0 {
		t.Fatalf("step 2: needsInput = %v, want none - expected to finish, not pause again", needsInput2)
	}
	if outputs2["n1"] != "ASKER-RESULT" {
		t.Errorf("step 2: outputs[n1] = %q, want ASKER-RESULT", outputs2["n1"])
	}
	if stub.sawAnswer != "north" {
		t.Errorf("step 2: worker never received the user's answer (saw %q)", stub.sawAnswer)
	}
	if len(done) != 1 || done[0] != "n1" {
		t.Fatalf("step 2: node_done = %v, want exactly one for n1", done)
	}
}

// TestRunPlanStep_ReusedNodeEmitsNodeQueuedFirst pins the QA rig regression
// (#slice3 review): "persistNodeEvent: dag_node status update failed ...
// illegal status transition done -> running" for a node reassigned in a
// later incremental step - dag.CanTransition refuses a direct done ->
// running (only done -> queued -> running is legal), so RunPlanStep must
// emit node_queued for a REUSED node exactly as a fresh dispatch does, not
// jump straight to node_start.
func TestRunPlanStep_ReusedNodeEmitsNodeQueuedFirst(t *testing.T) {
	stub := &graphStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "worker", Model: stub, Description: "worker", Instruction: "ROLE:worker",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{"worker": worker}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "p", UserMessage: "go", Nodes: []Node{
		{ID: "n1", AgentName: "worker", Task: "PLAIN-TASK"},
	}}

	var seq []string
	ctx := stream.WithYield(context.Background(), func(ev stream.SSEEvent) {
		switch d := ev.Data.(type) {
		case stream.NodeQueuedData:
			seq = append(seq, "queued:"+d.NodeID)
		case stream.NodeStartData:
			seq = append(seq, "start:"+d.NodeID)
		case stream.NodeDoneData:
			seq = append(seq, "done:"+d.NodeID)
		}
	})

	run := map[string]bool{"n1": true}
	if _, needsInput, err := ex.RunPlanStep(ctx, plan, "quack", "u", "chat", nil, run); err != nil || len(needsInput) > 0 {
		t.Fatalf("step 1: needsInput=%v err=%v", needsInput, err)
	}
	if got := seq[len(seq)-1]; got != "done:n1" {
		t.Fatalf("step 1: last event = %q, want done:n1; seq=%v", got, seq)
	}

	// Step 2: n1 reassigned (still the same node id, e.g. edit_plan gave it
	// a new task) - a fresh RunPlanStep call, reusing an already-"done" node.
	seq = nil
	if _, needsInput, err := ex.RunPlanStep(ctx, plan, "quack", "u", "chat", nil, run); err != nil || len(needsInput) > 0 {
		t.Fatalf("step 2: needsInput=%v err=%v", needsInput, err)
	}
	if len(seq) < 2 || seq[0] != "queued:n1" {
		t.Fatalf("step 2: events = %v, want to start with queued:n1 (a reused node must queue before running)", seq)
	}
	if seq[len(seq)-1] != "done:n1" {
		t.Fatalf("step 2: last event = %q, want done:n1", seq[len(seq)-1])
	}
}
