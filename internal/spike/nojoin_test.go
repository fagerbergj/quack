package spike

import (
	"context"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// Path A': fan-out to leaf workers, NO JoinNode, NO in-graph synthesis. The graph
// just runs workers durably; synthesis happens in Go afterward from collected
// outputs. Learns whether a no-fan-in graph resumes cleanly through a paused
// worker and exposes every worker's output.
func TestNativeGraph_NoJoinFanOut_ResumeCollectsOutputs(t *testing.T) {
	const interruptID = "askNJ"
	var bRuns, aResumes atomic.Int32
	rerun := true

	workerA := workflow.NewEmittingFunctionNode[any, any]("workerA",
		func(nc adkagent.Context, _ any, emit func(*session.Event) error) (any, error) {
			reply, err := workflow.ResumeOrRequestInput(nc, emit, session.RequestInput{InterruptID: interruptID, Message: "?"})
			if err != nil {
				return nil, err
			}
			aResumes.Add(1)
			return "A:" + toStr(reply), nil
		}, workflow.NodeConfig{RerunOnResume: &rerun})
	workerB := workflow.NewFunctionNode[any, any]("workerB",
		func(_ adkagent.Context, _ any) (any, error) { bRuns.Add(1); return "B", nil }, workflow.NodeConfig{})

	edges := workflow.NewEdgeBuilder().AddFanOut(workflow.Start, workerA, workerB).Build()
	wf, err := workflowagent.New(workflowagent.Config{Name: "nj", Edges: edges})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	sessions := session.InMemoryService()
	newRunner := func() *runner.Runner {
		r, _ := runner.New(runner.Config{AppName: "nj", Agent: wf, SessionService: sessions, AutoCreateSession: true})
		return r
	}
	ctx := context.Background()
	outputs := map[string]string{}
	collect := func(ev *session.Event) {
		if ev == nil || ev.NodeInfo == nil {
			return
		}
		if s, ok := ev.Output.(string); ok && s != "" {
			outputs[ev.NodeInfo.Path] = s
		}
	}
	for ev, err := range newRunner().Run(ctx, "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run1: %v", err)
		}
		collect(ev)
	}
	t.Logf("after run1: bRuns=%d outputs=%v", bRuns.Load(), outputs)
	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: interruptID, Name: workflow.WorkflowInputFunctionCallName, Response: map[string]any{"payload": "north"}}}}}
	for ev, err := range newRunner().Run(ctx, "u", "s", answer, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run2: %v", err)
		}
		p := ""
		if ev != nil && ev.NodeInfo != nil {
			p = ev.NodeInfo.Path
		}
		if ev != nil {
			t.Logf("run2 ev: path=%q final=%v", p, ev.IsFinalResponse())
		}
		collect(ev)
	}
	t.Logf("RESULT no-join: bRuns=%d(want1) aResumes=%d(want1) outputs=%v", bRuns.Load(), aResumes.Load(), outputs)
	if bRuns.Load() != 1 {
		t.Errorf("workerB re-ran: bRuns=%d", bRuns.Load())
	}
	if aResumes.Load() != 1 {
		t.Errorf("workerA resumes=%d want 1", aResumes.Load())
	}
}
