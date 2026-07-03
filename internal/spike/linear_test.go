package spike

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// Isolates whether resume drives a downstream node IN-STREAM for a LINEAR chain
// (no JoinNode). If synth runs in-stream here but not in the fan-out+join test,
// the JoinNode fan-in is what fails to settle on resume.
func TestNativeGraph_LinearHITLResume_DrivesToSynth(t *testing.T) {
	const interruptID = "askLin"
	var synthRuns, aResumes atomic.Int32
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

	synth := workflow.NewFunctionNode[any, any]("synth",
		func(_ adkagent.Context, in any) (any, error) {
			synthRuns.Add(1)
			return "synth(" + toStr(in) + ")", nil
		}, workflow.NodeConfig{})

	edges := workflow.Chain(workflow.Start, workerA, synth)
	wf, err := workflowagent.New(workflowagent.Config{Name: "lin", Edges: edges})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	sessions := session.InMemoryService()
	newRunner := func() *runner.Runner {
		r, _ := runner.New(runner.Config{AppName: "lin", Agent: wf, SessionService: sessions, AutoCreateSession: true})
		return r
	}
	ctx := context.Background()
	for _, err := range newRunner().Run(ctx, "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run1: %v", err)
		}
	}
	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: interruptID, Name: workflow.WorkflowInputFunctionCallName, Response: map[string]any{"payload": "north"}}}}}
	var finalText string
	for ev, err := range newRunner().Run(ctx, "u", "s", answer, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run2: %v", err)
		}
		if ev == nil {
			continue
		}
		path := ""
		if ev.NodeInfo != nil {
			path = ev.NodeInfo.Path
		}
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p.Text != "" {
					finalText = p.Text
				}
			}
		}
		if s, ok := ev.Output.(string); ok && s != "" {
			finalText = s
		}
		t.Logf("run2 ev: author=%s path=%q final=%v", ev.Author, path, ev.IsFinalResponse())
	}
	t.Logf("RESULT linear: synthRuns=%d aResumes=%d final=%q", synthRuns.Load(), aResumes.Load(), finalText)
	if synthRuns.Load() != 1 || !strings.Contains(finalText, "synth(") {
		t.Errorf("linear resume did NOT drive to synth in-stream: synthRuns=%d final=%q", synthRuns.Load(), finalText)
	}
}
