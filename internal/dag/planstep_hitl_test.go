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
	outputs1, paused1, err := ex.RunPlanStep(ctx, plan, "quack", "u", "chat", nil, run)
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if !paused1 {
		t.Fatal("step 1: expected a paused step")
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
	outputs2, paused2, err := ex.ResumePlanStep(ctx, plan, "quack", "u", "chat", nil, run, "hitl-n1-r1", "north")
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if paused2 {
		t.Fatal("step 2: expected to finish, not pause again")
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
