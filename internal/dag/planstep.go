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
// steps run under - separate from the chat session so a step's own workflow
// bookkeeping never interleaves with the orchestrator's live tool-calling
// turn on the same session id.
const planStepSessionSuffix = "::plan-step"

// RunPlanStep runs exactly the nodes named in run and streams their events
// through ctx's yield chain (stream.YieldFromContext) - the same one the
// calling tool's context already carries. workflow.RunNode (which
// RunPlanIncrement dispatches through) requires a live sub-scheduler, which
// only a workflow.NewDynamicNode body provides (see its doc), so this wraps
// the call in its own disposable workflow+runner rather than driving it
// directly from ctx. paused reports whether any run-set node parked on a
// HITL question (needsInput) - see the ponytail note below.
//
// ponytail: a node that pauses here isn't auto-resumable from chat yet -
// LatestPendingQuestion/resumeNodeRun (orchestrator.go) only scan the CHAT
// session, not this dedicated plan-step one, and this wrapper (plain
// workflowagent, unlike RunPlanAsGraph's newPlanWrapper) has no
// ReconstructRunState/Resume path to re-enter with an answer either. The
// node itself is NOT stuck (workflow.RunNode's ErrNodeInterrupted surfaces
// as a clean node_needs_input event, not a hang) and CancelNode/StopNode
// still reaches it (e.controls is keyed by chatID, independent of this
// session) - only the "answer it" path is missing. Upgrade path: switch this
// wrapper to workflow.New + newPlanWrapper (nativegraph.go) and add a
// ResumePlanStep mirroring RunPlanAsGraph's resume content shape, then wire
// it into orchestrator.Run()'s pending-interrupt check for this session.
func (e *Executor) RunPlanStep(ctx context.Context, plan Plan, appName, userID, chatID string, seeded map[string]string, run map[string]bool) (outputs map[string]string, paused bool, err error) {
	nodeOutputs := make(map[string]string)
	if len(run) == 0 {
		return nodeOutputs, false, nil
	}
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

	runSess := chatID + planStepSessionSuffix
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "run"}}}
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
