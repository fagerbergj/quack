package dag

import (
	"context"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// planStepSessionSuffix names a dedicated ADK session: RunPlanStep runs inside the execute tool call,
// nested in the orchestrator's live runner.Run on the chat session, and a second Run there risks corruption.
const planStepSessionSuffix = "::plan-step"

// PlanStepSessionID is where the orchestrator's pending-question/resume lookups find a node paused mid-step.
func PlanStepSessionID(chatID string) string { return chatID + planStepSessionSuffix }

// RunPlanStep runs exactly the nodes in run in a disposable workflow+runner (RunNode needs a live sub-scheduler).
// needsInput: nodes parked on a HITL question; started: nodes that reached running (a later layer may never dispatch).
func (e *Executor) RunPlanStep(ctx context.Context, plan Plan, appName, userID, chatID string, seeded map[string]string, run map[string]bool) (outputs map[string]string, needsInput map[string]bool, started map[string]bool, err error) {
	if len(run) == 0 {
		return map[string]string{}, nil, nil, nil
	}
	// Every node is queued first: the only lawful way into running for a reused node whose status may be
	// terminal. ResumePlanStep must not do this - its node is mid-flight, where queued isn't legal.
	if sink, ok := stream.YieldFromContext(ctx); ok {
		for nid := range run {
			sink(stream.NodeQueued(nid))
		}
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "run"}}}
	return e.driveStep(ctx, plan, appName, userID, chatID, seeded, run, content)
}

// ResumePlanStep answers a node's question from a prior RunPlanStep that parked on it; run names the
// nodes still unresolved, and the runner resumes the same session's paused RunState.
func (e *Executor) ResumePlanStep(ctx context.Context, plan Plan, appName, userID, chatID string, seeded map[string]string, run map[string]bool, interruptID, answer string) (outputs map[string]string, needsInput map[string]bool, started map[string]bool, err error) {
	if len(run) == 0 {
		return map[string]string{}, nil, nil, nil
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID:       interruptID,
			Name:     workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": answer},
		},
	}}}
	return e.driveStep(ctx, plan, appName, userID, chatID, seeded, run, content)
}

// driveStep builds the disposable workflow+runner RunPlanStep/ResumePlanStep share and drives it with
// content: "run" (fresh dispatch) or an adk_request_input answer (resume).
func (e *Executor) driveStep(ctx context.Context, plan Plan, appName, userID, chatID string, seeded map[string]string, run map[string]bool, content *genai.Content) (outputs map[string]string, needsInput map[string]bool, started map[string]bool, err error) {
	nodeOutputs := make(map[string]string)
	stepNode := workflow.NewDynamicNode[any, string]("__exec-step",
		func(nctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			out, rerr := e.RunPlanIncrement(nctx, plan, chatID, seeded, run)
			if rerr != nil {
				return "", rerr
			}
			for k, v := range out {
				nodeOutputs[k] = v
			}
			return "done", nil
		}, workflow.NodeConfig{})
	wf, err := workflowagent.New(workflowagent.Config{Name: "orchestrator-plan-step", Edges: workflow.Chain(workflow.Start, stepNode)})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dag: plan step workflow: %w", err)
	}
	r, err := runner.New(runner.Config{AppName: appName, Agent: wf, SessionService: e.sessions, ArtifactService: e.artifacts, AutoCreateSession: true})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dag: plan step runner: %w", err)
	}

	sink, _ := stream.YieldFromContext(ctx)
	yield := func(ev stream.SSEEvent, _ error) bool {
		if sink != nil {
			sink(ev)
		}
		return true
	}
	ds := e.NewDagStream(ctx, plan, appName, userID, chatID, chatID, yield, nodeOutputs)
	ds.ScopeToStep(run)

	runSess := PlanStepSessionID(chatID)
	for ev, rerr := range r.Run(ctx, userID, runSess, content, adkagent.RunConfig{}) {
		if rerr != nil {
			ds.Abort(rerr)
			return nodeOutputs, ds.NeedsInput(), ds.Started(), rerr
		}
		if ev == nil {
			continue
		}
		ds.Handle(ev)
	}
	ds.Finish()
	return nodeOutputs, ds.NeedsInput(), ds.Started(), nil
}
