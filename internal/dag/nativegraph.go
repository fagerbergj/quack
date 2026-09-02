package dag

import (
	"context"
	"fmt"
	"iter"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// planWrapperName: top agent wrapping the plan graph; must not collide with node IDs.
const planWrapperName = "quack-plan-graph"

// buildPlanGraph: wires gated nodes as a native ADK graph.
func buildPlanGraph(plan Plan, nodesByID map[string]workflow.Node) ([]workflow.Edge, error) {
	eb := workflow.NewEdgeBuilder()
	hasSuccessor := map[string]bool{}
	for _, n := range plan.Nodes {
		node, ok := nodesByID[n.ID]
		if !ok {
			return nil, fmt.Errorf("dag: plan graph: no built node for %q", n.ID)
		}
		for _, d := range n.DependsOn {
			hasSuccessor[d] = true
		}
		switch len(n.DependsOn) {
		case 0:
			eb.Add(workflow.Start, node)
		case 1:
			dep, ok := nodesByID[n.DependsOn[0]]
			if !ok {
				return nil, fmt.Errorf("dag: plan graph: node %q depends on unknown %q", n.ID, n.DependsOn[0])
			}
			eb.Add(dep, node)
		default:
			join := workflow.NewJoinNode("join-" + n.ID)
			for _, d := range n.DependsOn {
				dep, ok := nodesByID[d]
				if !ok {
					return nil, fmt.Errorf("dag: plan graph: node %q depends on unknown %q", n.ID, d)
				}
				eb.Add(dep, join)
			}
			eb.Add(join, node)
		}
	}
	// ADK allows one terminal node; synthesizer hardening guarantees this for multi-node plans.
	terminals := 0
	for _, n := range plan.Nodes {
		if !hasSuccessor[n.ID] {
			terminals++
		}
	}
	if terminals > 1 {
		return nil, fmt.Errorf("dag: plan graph: %d terminal nodes (want 1) - plan lacks a synthesizer fan-in", terminals)
	}
	return eb.Build(), nil
}

// newPlanWrapper: wraps plan workflow, patching ADK's rehydration gap for completed siblings.
func newPlanWrapper(wf *workflow.Workflow, nodeNames map[string]bool) (adkagent.Agent, error) {
	return adkagent.New(adkagent.Config{
		Name: planWrapperName, Description: "quack plan-graph runner",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				responses := workflowInputResponses(ctx.UserContent())
				if len(responses) > 0 {
					state, err := wf.ReconstructRunState(ctx.Session(), ctx.InvocationID())
					if err != nil {
						yield(nil, err)
						return
					}
					if state != nil {
						patchCompletedSiblings(state, ctx.Session(), ctx.InvocationID(), nodeNames)
						for ev, err := range wf.Resume(adkagent.Promote(ctx), state, responses) {
							if !yield(ev, err) {
								return
							}
						}
						return
					}
				}
				for ev, err := range wf.Run(ctx) {
					if !yield(ev, err) {
						return
					}
				}
			}
		},
	})
}

// workflowInputResponses: extracts adk_request_input FunctionResponses from user content.
func workflowInputResponses(uc *genai.Content) map[string]any {
	if uc == nil {
		return nil
	}
	responses := map[string]any{}
	for _, p := range uc.Parts {
		if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != workflow.WorkflowInputFunctionCallName {
			continue
		}
		resp := p.FunctionResponse.Response
		switch {
		case resp["payload"] != nil:
			responses[p.FunctionResponse.ID] = resp["payload"]
		case resp["response"] != nil:
			responses[p.FunctionResponse.ID] = resp["response"]
		default:
			responses[p.FunctionResponse.ID] = resp
		}
	}
	return responses
}

// patchCompletedSiblings: backfills NodeState for completed nodes missed by rehydration.
func patchCompletedSiblings(state *workflow.RunState, sess session.Session, invocationID string, nodeNames map[string]bool) {
	if state == nil || sess == nil {
		return
	}
	for ev := range sess.Events().All() {
		if ev == nil || ev.Output == nil || ev.NodeInfo == nil || ev.InvocationID != invocationID {
			continue
		}
		name := graphNodeNameFromPath(ev.NodeInfo.Path, nodeNames)
		if name == "" || state.Nodes[name] != nil {
			continue
		}
		ns := state.EnsureNode(name)
		ns.Status = workflow.NodeCompleted
		ns.Output = ev.Output
		ns.Branch = ev.Branch
	}
}

// graphNodeNameFromPath: finds graph-node name in a NodeInfo path.
func graphNodeNameFromPath(path string, known map[string]bool) string {
	for _, seg := range strings.Split(path, "/") {
		if i := strings.IndexByte(seg, '@'); i >= 0 {
			seg = seg[:i]
		}
		if known[seg] {
			return seg
		}
	}
	return ""
}

// RunPlanAsGraph: runs plan as a native ADK graph.
func (e *Executor) RunPlanAsGraph(ctx context.Context, plan Plan, appName, userID, chatID string, content *genai.Content, yield func(stream.SSEEvent, error) bool, nodeOutputs map[string]string, resumeNodes []string) (paused bool, err error) {
	// Empty resumeNodes = fresh run; setup runs once, never on resume. Same
	// signal clears any review fan-in left by a previous aborted run of this
	// plan ID (#1040) - a resume/retry must NOT reset it, since a peer
	// reviewer already staged is not a descendant and never re-runs.
	if len(resumeNodes) == 0 {
		// Cleared first: a push landing mid-clone must stay flagged. A resume
		// deliberately keeps the flag - the branch is still ahead of the tree.
		clearSetupStale(chatID)
		if serr := e.runPlanSetup(ctx, userID, chatID, plan); serr != nil {
			return false, fmt.Errorf("dag: plan setup: %w", serr)
		}
		vetting.ResetReviewFanout(plan.ID)
	}
	// Source travels on ctx up to THIS point only (buildGateNodes is called
	// synchronously, before workflow.RunNode ever schedules a child) - past
	// here it's carried on vetting.Config, same reason cfg.Agent is.
	source := ledger.CoordsFromContext(ctx).Source
	gateNodes, _, err := buildGateNodes(plan, e.agents, e.models, e.judge, e.cfgFor, e.mediaAgents, e.controls, chatID, source,
		func(nodeID string, score float64, passed bool, rounds int) {
			e.recordGateResult(chatID, nodeID, score, passed, rounds)
		}, e.admission, e.specFor, e.artifacts, func(nctx context.Context, node Node, cfg vetting.Config) bool {
			return e.refreshStaleSetup(nctx, userID, chatID, &plan, node, cfg)
		})
	if err != nil {
		return false, err
	}
	edges, err := buildPlanGraph(plan, gateNodes)
	if err != nil {
		return false, err
	}
	// e.maxActive is a host-resource ceiling (jail/clone CPU+RAM), not the GPU
	// limiter - the Admission ledger inside each gate node (#1007) is the real one.
	wf, err := workflow.New(planWrapperName, edges, workflow.WithMaxConcurrency(e.maxActive))
	if err != nil {
		return false, fmt.Errorf("dag: plan graph: %w", err)
	}
	nodeNames := map[string]bool{}
	for _, n := range plan.Nodes {
		nodeNames[n.ID] = true
		nodeNames["join-"+n.ID] = true
	}
	wrapper, err := newPlanWrapper(wf, nodeNames)
	if err != nil {
		return false, fmt.Errorf("dag: plan wrapper: %w", err)
	}
	r, err := runner.New(runner.Config{AppName: appName, Agent: wrapper, SessionService: e.sessions, ArtifactService: e.artifacts, AutoCreateSession: true})
	if err != nil {
		return false, fmt.Errorf("dag: plan graph runner: %w", err)
	}
	ds := e.NewDagStream(ctx, plan, appName, userID, chatID, chatID, yield, nodeOutputs)
	if len(resumeNodes) > 0 {
		ds.ScopeToResume(resumeNodes)
	}
	for ev, rerr := range r.Run(ctx, userID, chatID, content, adkagent.RunConfig{}) {
		if rerr != nil {
			return ds.Paused(), rerr
		}
		if ev == nil {
			continue
		}
		ds.Handle(ev)
	}
	ds.Finish()
	return ds.Paused(), nil
}
