package dag

import (
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/fagerbergj/quack/internal/vetting"
)

// BuildWorkflow turns a validated Plan into an ADK v2 workflow: one first-class
// gated-worker node per plan node (named node.ID), fanned out per DependsOn and
// joined via JoinNode barriers. Because each gated worker is a first-class graph
// node (not a RunNode child), a completed node is durably skipped on resume — the
// property the spike proved and the M8 durability win depends on.
//
// Each node's body assembles its worker prompt from the upstream outputs delivered
// along the graph edges (buildTask, reused from the legacy executor) and runs the
// trust-gate refine loop (vetting.RunGatedRefine). This is the v2 replacement for
// Executor.Execute (TopoSort + semaphore + per-node runner).
func BuildWorkflow(plan Plan, agents map[string]adkagent.Agent, judge vetting.JudgeFactory, cfg vetting.Config) (adkagent.Agent, error) {
	nodesByID := make(map[string]workflow.Node, len(plan.Nodes))
	var subAgents []adkagent.Agent
	seenAgent := map[string]bool{}

	// One gated-worker node per plan node.
	for _, n := range plan.Nodes {
		ag, ok := agents[n.AgentName]
		if !ok {
			return nil, fmt.Errorf("dag: no agent %q for node %q", n.AgentName, n.ID)
		}
		if !seenAgent[n.AgentName] {
			seenAgent[n.AgentName] = true
			subAgents = append(subAgents, ag) // dedup: author resolution only
		}
		workerNode, err := vetting.NewWorkerNode(ag)
		if err != nil {
			return nil, err
		}
		node := n // capture per iteration
		gated := workflow.NewDynamicNode[any, string](node.ID,
			func(ctx adkagent.Context, in any, _ func(*session.Event) error) (string, error) {
				upstream := upstreamFromInput(in, node.DependsOn)
				// ponytail: gateFailed (continue-but-warn) propagation is a follow-up;
				// empty map = no warnings until node pass/fail rides the graph.
				prompt := buildTask(plan, node, upstream, map[string]bool{})
				return vetting.RunGatedRefine(ctx, workerNode, judge, cfg, prompt)
			},
			workflow.NodeConfig{})
		nodesByID[node.ID] = gated
	}

	// Edges from DependsOn: leaves ← Start; one dep ← direct edge; N deps ← a
	// per-node JoinNode barrier (keyed by predecessor node name == dep ID).
	eb := workflow.NewEdgeBuilder()
	for _, n := range plan.Nodes {
		node := nodesByID[n.ID]
		switch len(n.DependsOn) {
		case 0:
			eb.Add(workflow.Start, node)
		case 1:
			eb.Add(nodesByID[n.DependsOn[0]], node)
		default:
			join := workflow.NewJoinNode(n.ID + "-join")
			deps := make([]workflow.Node, 0, len(n.DependsOn))
			for _, d := range n.DependsOn {
				deps = append(deps, nodesByID[d])
			}
			eb.AddFanIn(join, deps...)
			eb.Add(join, node)
		}
	}

	return workflowagent.New(workflowagent.Config{
		Name:      "quack-dag-" + plan.ID,
		SubAgents: subAgents,
		Edges:     eb.Build(),
	})
}

// upstreamFromInput converts a dynamic node's edge input into the upstream map
// (dep node ID → output text) that buildTask expects. A JoinNode fan-in delivers
// map[string]any keyed by predecessor node name (== dep node ID); a single
// predecessor delivers its bare string output; a leaf (from Start) gets nil.
func upstreamFromInput(in any, dependsOn []string) map[string]string {
	upstream := map[string]string{}
	switch v := in.(type) {
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok {
				upstream[k] = s
			}
		}
	case string:
		if len(dependsOn) == 1 && v != "" {
			upstream[dependsOn[0]] = v
		}
	}
	return upstream
}
