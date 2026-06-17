package tools

import (
	"fmt"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

type planArgs struct {
	Query string `json:"query"`
}

type planResult struct {
	PlanID  string `json:"plan_id"` // pass this to the execute tool
	Summary string `json:"summary"` // human-readable node list for the model
}

// NewPlanTool returns a tool that decomposes a request into a DAG plan. The full
// plan is stored in cache keyed by a plan ID; the tool returns only that ID plus
// a short summary. The execute tool runs the plan by ID — the model never has to
// copy the (large) plan JSON between calls, which is where nodes were being
// dropped. A dag_plan SSE event is emitted immediately via the yield context so
// the frontend can render the DAG structure before execution begins.
func NewPlanTool(planner *dag.Planner, cache *PlanCache) (tool.Tool, error) {
	return functiontool.New[planArgs, planResult](
		functiontool.Config{
			Name: "plan",
			Description: "Tool to decompose a task into a DAG plan for specialist agents to execute. " +
				"Use when the task is too large, too complex, or requires capabilities you cannot perform directly. " +
				"Do NOT call for tasks you can complete in a single response. " +
				"Returns a plan_id; pass it to the execute tool to run the plan.",
		},
		func(tc agent.ToolContext, a planArgs) (planResult, error) {
			p, err := planner.Plan(tc, nil, a.Query)
			if err != nil {
				return planResult{}, fmt.Errorf("plan: %w", err)
			}

			cache.Put(*p)

			// Emit dag_plan immediately so the frontend knows the plan structure
			// before the execute tool starts running nodes.
			if yieldFn, ok := stream.YieldFromContext(tc); ok {
				nodes := make([]stream.DagNodeDef, len(p.Nodes))
				for i, n := range p.Nodes {
					nodes[i] = stream.DagNodeDef{ID: n.ID, Agent: n.AgentName, Task: n.Task, DependsOn: n.DependsOn}
				}
				edges := make([]stream.DagEdgeDef, len(p.Edges))
				for i, e := range p.Edges {
					edges[i] = stream.DagEdgeDef{From: e.From, To: e.To}
				}
				yieldFn(stream.DagPlan(p.ID, nodes, edges))
			}

			return planResult{PlanID: p.ID, Summary: summarizePlan(p)}, nil
		},
	)
}

// summarizePlan renders a one-line-per-node overview of the plan for the model.
// It is informational only — execution uses the cached plan, not this text.
func summarizePlan(p *dag.Plan) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d node(s):", len(p.Nodes))
	for _, n := range p.Nodes {
		fmt.Fprintf(&sb, "\n- %s (%s)", n.ID, n.AgentName)
		if len(n.DependsOn) > 0 {
			fmt.Fprintf(&sb, " depends on %s", strings.Join(n.DependsOn, ", "))
		}
	}
	return sb.String()
}
