package spike

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// Path B: mirror runDAG's nesting — top workflow Start→execNode(dynamic); execNode
// RunNode's two children (one pauses via ResumeOrRequestInput, one completes),
// collecting outputs itself (no JoinNode). Verifies a mid-child HITL pause parks the
// TOP workflow and resumes, and that seeding lets the finished child skip re-run.
func TestPathB_RunDAGNestingHITLResume(t *testing.T) {
	const interruptID = "askChildA"
	var aRuns, aResumes, bRuns atomic.Int32
	rerun := true

	childA := workflow.NewEmittingFunctionNode[any, any]("childA",
		func(nc adkagent.Context, _ any, emit func(*session.Event) error) (any, error) {
			aRuns.Add(1)
			reply, err := workflow.ResumeOrRequestInput(nc, emit, session.RequestInput{InterruptID: interruptID, Message: "?"})
			if err != nil {
				return nil, err
			}
			aResumes.Add(1)
			return "A:" + toStr(reply), nil
		}, workflow.NodeConfig{RerunOnResume: &rerun})
	childB := workflow.NewFunctionNode[any, any]("childB",
		func(adkagent.Context, any) (any, error) { bRuns.Add(1); return "B", nil }, workflow.NodeConfig{})

	// seeded lets execNode skip a child already completed in a prior run (mirrors
	// RetryNode seeding from the DagNode store).
	seeded := map[string]string{}
	outputs := map[string]string{}
	execNode := workflow.NewDynamicNode[any, string]("execNode",
		func(nctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			// childB: seed or run
			if v, ok := seeded["childB"]; ok {
				outputs["childB"] = v
			} else {
				b, err := workflow.RunNode[string](nctx, childB, nil)
				if err != nil {
					return "", err
				}
				outputs["childB"] = b
			}
			// childA: run (may pause)
			a, err := workflow.RunNode[string](nctx, childA, nil)
			if err != nil {
				return "", err // ErrNodeInterrupted bubbles → park
			}
			outputs["childA"] = a
			return outputs["childA"] + "|" + outputs["childB"], nil
		}, workflow.NodeConfig{})

	wf, err := workflowagent.New(workflowagent.Config{Name: "pathb", Edges: workflow.Chain(workflow.Start, execNode)})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	sessions := session.InMemoryService()
	newRunner := func() *runner.Runner {
		r, _ := runner.New(runner.Config{AppName: "pathb", Agent: wf, SessionService: sessions, AutoCreateSession: true})
		return r
	}
	ctx := context.Background()

	// Run 1: should park at childA.
	var paused bool
	for ev, err := range newRunner().Run(ctx, "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil && !errors.Is(err, workflow.ErrNodeInterrupted) {
			t.Fatalf("run1: %v", err)
		}
		if ev != nil && ev.RequestedInput != nil {
			paused = true
		}
	}
	t.Logf("after run1: paused=%v aRuns=%d bRuns=%d", paused, aRuns.Load(), bRuns.Load())
	if !paused {
		t.Fatal("run1: top workflow did not surface a pause from the RunNode child")
	}

	// Seed childB so resume doesn't re-run it (the durability workaround).
	seeded["childB"] = "B"

	// Run 2: resume.
	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: interruptID, Name: workflow.WorkflowInputFunctionCallName, Response: map[string]any{"payload": "north"}}}}}
	var finalText string
	for ev, err := range newRunner().Run(ctx, "u", "s", answer, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run2: %v", err)
		}
		if ev == nil {
			continue
		}
		if s, ok := ev.Output.(string); ok && s != "" {
			finalText = s
		}
	}
	t.Logf("RESULT path-b: aResumes=%d bRuns=%d(seeded→want1) final=%q outputs=%v", aResumes.Load(), bRuns.Load(), finalText, outputs)
	if aResumes.Load() != 1 {
		t.Errorf("childA resumes=%d want 1", aResumes.Load())
	}
	if bRuns.Load() != 1 {
		t.Errorf("childB ran %d times; seeding should have skipped it on resume", bRuns.Load())
	}
}
