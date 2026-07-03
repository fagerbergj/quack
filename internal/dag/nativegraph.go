package dag

import (
	"fmt"

	"google.golang.org/adk/v2/workflow"
)

// synthAgent is the agent name whose node is the plan's terminal fan-in. The
// planner hardens it to depend on every other node (planner.go), so it is NEVER a
// graph node in the Path A′ native shape — the orchestrator runs it in Go from the
// collected worker outputs (see .quack/node-hitl.md).
const synthAgent = "synthesizer"

// isSynth reports whether a node is the terminal synthesizer.
func isSynth(n Node) bool { return n.AgentName == synthAgent }

// workerNodes returns the plan's non-synthesizer nodes in plan order.
func workerNodes(plan Plan) []Node {
	out := make([]Node, 0, len(plan.Nodes))
	for _, n := range plan.Nodes {
		if !isSynth(n) {
			out = append(out, n)
		}
	}
	return out
}

// buildPlanGraph wires the Path A′ edge set for a plan's NON-synthesizer nodes,
// each already built into a first-class workflow.Node (nodesByID). A worker with no
// non-synth dependency fans out from Start; a worker with exactly one is chained
// after it. A worker with ≥2 non-synth dependencies is MID-DAG FAN-IN and is
// rejected: ADK v2.0.0's only fan-in (JoinNode) cannot settle on HITL resume when a
// predecessor was skipped (.quack/node-hitl-spike.md), and the planner's canonical
// shape (fan-out researchers → terminal synthesizer) never needs it. The
// synthesizer's own fan-in is handled in Go, not here.
//
// Returns the edges and the worker node IDs in plan order.
func buildPlanGraph(plan Plan, nodesByID map[string]workflow.Node) ([]workflow.Edge, []string, error) {
	synth := map[string]bool{}
	for _, n := range plan.Nodes {
		if isSynth(n) {
			synth[n.ID] = true
		}
	}

	eb := workflow.NewEdgeBuilder()
	var ids []string
	for _, n := range workerNodes(plan) {
		node, ok := nodesByID[n.ID]
		if !ok {
			return nil, nil, fmt.Errorf("dag: buildPlanGraph: no built node for %q", n.ID)
		}
		ids = append(ids, n.ID)

		// Only non-synth dependencies constrain graph ordering; a dependency on the
		// synthesizer is impossible (it's terminal), but filter defensively.
		var deps []string
		for _, d := range n.DependsOn {
			if !synth[d] {
				deps = append(deps, d)
			}
		}
		switch len(deps) {
		case 0:
			eb.Add(workflow.Start, node)
		case 1:
			dep, ok := nodesByID[deps[0]]
			if !ok {
				return nil, nil, fmt.Errorf("dag: buildPlanGraph: node %q depends on unknown %q", n.ID, deps[0])
			}
			eb.Add(dep, node)
		default:
			return nil, nil, fmt.Errorf("dag: buildPlanGraph: node %q has mid-DAG fan-in (deps %v); "+
				"native graph supports only fan-out + chains (JoinNode can't resume through HITL)", n.ID, deps)
		}
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("dag: buildPlanGraph: plan has no worker nodes")
	}
	return eb.Build(), ids, nil
}
