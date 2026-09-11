// planstep.go: runs one incremental execute() step - the nodes a growing
// plan hasn't dispatched yet - synchronously from inside the execute tool
// call, so the orchestrator's own tool loop can read the step's results and
// keep going instead of ending its turn (#slice3).
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

// planStepSessionSuffix names the dedicated ADK session a plan's incremental
// steps run under. Kept separate from the chat session because RunPlanStep
// runs synchronously from inside the execute TOOL CALL, nested inside the
// orchestrator's own live runner.Run on the chat session - a second
// runner.Run against that same session while the first is mid-iteration
// risks corrupting its event/branch bookkeeping, so this step gets its own.
const planStepSessionSuffix = "::plan-step"

// PlanStepSessionID is the ADK session a plan's incremental steps run under -
// exported so the orchestrator's own pending-question/resume lookups (which
// otherwise only ever scan the chat session) know where to also look for a
// node paused mid-step.
func PlanStepSessionID(chatID string) string { return chatID + planStepSessionSuffix }

// RunPlanStep runs exactly the nodes named in run and streams their events
// through ctx's yield chain (stream.YieldFromContext) - the same one the
// calling tool's context already carries. workflow.RunNode (which
// RunPlanIncrement dispatches through) requires a live sub-scheduler, which
// only a workflow.NewDynamicNode body provides (see its doc), so this wraps
// the call in its own disposable workflow+runner rather than driving it
// directly from ctx. paused reports whether any run-set node parked on a
// HITL question - see ResumePlanStep to answer it.
func (e *Executor) RunPlanStep(ctx context.Context, plan Plan, appName, userID, chatID string, seeded map[string]string, run map[string]bool) (outputs map[string]string, paused bool, err error) {
	if len(run) == 0 {
		return map[string]string{}, false, nil
	}
	// Every node in run gets queued first, fresh hire or reused - the only
	// lawful way into "running" for a REUSED node, whose prior status can be
	// terminal (done/failed/cancelled -> queued -> running; CanTransition
	// refuses a direct done -> running, #slice3 review's rig regression). A
	// fresh node's own queued -> queued (or the empty-status default) is
	// just an idempotent re-queue. ResumePlanStep must NOT do this: its node
	// is mid-flight (paused/needs_input), and queued isn't a legal target
	// from there - driveStep, which both share, stays untouched.
	if sink, ok := stream.YieldFromContext(ctx); ok {
		for nid := range run {
			sink(stream.NodeQueued(nid))
		}
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "run"}}}
	return e.driveStep(ctx, plan, appName, userID, chatID, seeded, run, content)
}

// ResumePlanStep answers a node's question from a prior RunPlanStep call
// that parked on it (interruptID/answer - the same adk_request_input
// FunctionResponse shape orchestrator.go's startNodeRun already builds for a
// whole-plan resume). run must name exactly the node(s) still unresolved
// from that step (a sibling that already finished needs no seat here -
// dag_plan's own task_id already marks it done); workflowagent.New's runner
// (see its own doc) detects the FunctionResponse and resumes the SAME
// session's paused RunState instead of starting the step over.
func (e *Executor) ResumePlanStep(ctx context.Context, plan Plan, appName, userID, chatID string, seeded map[string]string, run map[string]bool, interruptID, answer string) (outputs map[string]string, paused bool, err error) {
	if len(run) == 0 {
		return map[string]string{}, false, nil
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

// driveStep builds the disposable per-chat workflow+runner RunPlanStep/
// ResumePlanStep share and drives it with content - "run" (fresh dispatch)
// or an adk_request_input answer (resume).
func (e *Executor) driveStep(ctx context.Context, plan Plan, appName, userID, chatID string, seeded map[string]string, run map[string]bool, content *genai.Content) (outputs map[string]string, paused bool, err error) {
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
		return nil, false, fmt.Errorf("dag: plan step workflow: %w", err)
	}
	r, err := runner.New(runner.Config{AppName: appName, Agent: wf, SessionService: e.sessions, ArtifactService: e.artifacts, AutoCreateSession: true})
	if err != nil {
		return nil, false, fmt.Errorf("dag: plan step runner: %w", err)
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
			return nodeOutputs, ds.Paused(), rerr
		}
		if ev == nil {
			continue
		}
		ds.Handle(ev)
	}
	ds.Finish()
	return nodeOutputs, ds.Paused(), nil
}
