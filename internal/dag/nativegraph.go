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

	"github.com/fagerbergj/quack/internal/stream"
)

// planWrapperName is the top agent wrapping the plan graph. Must never collide
// with a plan node ID (planner IDs are short slugs like n1/research-x).
const planWrapperName = "quack-plan-graph"

// buildPlanGraph wires a plan's gated nodes as a native first-class ADK graph:
// leaves fan out from Start, single-dep nodes chain, and a node with ≥2
// dependencies gets a JoinNode barrier ("join-<id>") in front of it — including
// the synthesizer, whose planner-hardened depends-on-everything edge set makes it
// the single terminal (satisfying ADK's one-terminal-output rule).
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
	// ADK allows at most ONE terminal node producing output; the planner's
	// synthesizer hardening (depends on all) guarantees this for multi-node plans.
	// Guard it anyway so a degenerate plan fails loudly at build, not mid-run.
	terminals := 0
	for _, n := range plan.Nodes {
		if !hasSuccessor[n.ID] {
			terminals++
		}
	}
	if terminals > 1 {
		return nil, fmt.Errorf("dag: plan graph: %d terminal nodes (want 1) — plan lacks a synthesizer fan-in", terminals)
	}
	return eb.Build(), nil
}

// newPlanWrapper wraps the plan workflow in a thin custom agent that replicates
// workflowagent's resume dispatch AND patches ADK's rehydration gap: on resume,
// buildRunState only rebuilds NodeStates for interrupt-raising nodes, so a
// completed sibling has no state.Nodes entry and a downstream JoinNode can never
// satisfy its barrier (aggregatePredecessorOutputs: nil predecessor → not ready).
// patchCompletedSiblings backfills them from session history, spike-proven for
// both handoff and re-entry askers (.quack/node-hitl-spike.md Update 2). Delete
// this wrapper for plain workflowagent.New once ADK fixes buildRunState.
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

// workflowInputResponses extracts adk_request_input FunctionResponses from the
// inbound user content: InterruptID → payload. Mirrors workflowagent's
// detectResume + decodeWorkflowInputResponse (unexported upstream).
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

// patchCompletedSiblings backfills state.Nodes entries for graph nodes that
// completed with an output in THIS invocation's history but got no rehydrated
// NodeState. See newPlanWrapper.
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

// graphNodeNameFromPath finds the graph-node name in a NodeInfo path like
// "quack-plan-graph@1/n1@1/worker-r0@1" (segments are name@run; dynamic children
// fold into their static ancestor — first known segment wins).
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

// RunPlanAsGraph runs a plan as a native first-class-node ADK graph under its own
// runner: gated workers (and the synthesizer) are graph nodes, so ADK owns
// concurrency, durable completed-node skip, and HITL parking. content is the
// turn's trigger — the user message on a fresh run, or the adk_request_input
// FunctionResponse on a resume (ADK reuses the paused invocation and re-enters
// only the paused node; completed siblings skip). Node events stream through a
// DagStream onto yield; nodeOutputs collects node ID → vetted answer.
// resumeNodes, non-empty on a resume turn, scopes the DagStream's terminal sweep
// to the paused nodes + their downstream (skipped siblings emit nothing this run
// and must not be swept as failed).
func (e *Executor) RunPlanAsGraph(ctx context.Context, plan Plan, appName, userID, chatID string, content *genai.Content, yield func(stream.SSEEvent, error) bool, nodeOutputs map[string]string, resumeNodes []string) (paused bool, err error) {
	// An empty resumeNodes is exactly the fresh-run signal (every caller agrees:
	// a resume always names the node(s) it's re-entering) — setup must run
	// exactly ONCE, before the graph's first node, never again on a resume of
	// an already-provisioned plan.
	if len(resumeNodes) == 0 {
		if serr := e.runPlanSetup(ctx, userID, chatID, plan); serr != nil {
			return false, fmt.Errorf("dag: plan setup: %w", serr)
		}
	}
	gateNodes, _, err := buildGateNodes(plan, e.agents, e.models, e.judge, e.cfgFor, e.mediaAgents, e.controls, chatID,
		func(nodeID string, score float64, passed bool, rounds int) {
			e.recordGateResult(chatID, nodeID, score, passed, rounds)
		})
	if err != nil {
		return false, err
	}
	edges, err := buildPlanGraph(plan, gateNodes)
	if err != nil {
		return false, err
	}
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
	r, err := runner.New(runner.Config{AppName: appName, Agent: wrapper, SessionService: e.sessions, AutoCreateSession: true})
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
