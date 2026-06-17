package tools

import (
	"encoding/json"
	"fmt"

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
	Plan string `json:"plan"` // JSON-encoded dag.Plan
}

// NewPlanTool returns a tool that decomposes a research query into a DAG plan.
// The returned plan JSON is passed to the execute tool to run the DAG.
// A dag_plan SSE event is emitted immediately via the yield context so the
// frontend can render the DAG structure before execution begins.
func NewPlanTool(planner *dag.Planner) (tool.Tool, error) {
	return functiontool.New[planArgs, planResult](
		functiontool.Config{
			Name: "plan",
			Description: "Tool to decompose a task into a DAG plan for specialist agents to execute. " +
				"Use when the task is too large, too complex, or requires capabilities you cannot perform directly. " +
				"Do NOT call for tasks you can complete in a single response.",
		},
		func(tc agent.ToolContext, a planArgs) (planResult, error) {
			p, err := planner.Plan(tc, nil, a.Query)
			if err != nil {
				return planResult{}, fmt.Errorf("plan: %w", err)
			}

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

			b, err := json.Marshal(p)
			if err != nil {
				return planResult{}, fmt.Errorf("plan: marshal: %w", err)
			}
			return planResult{Plan: string(b)}, nil
		},
	)
}
