package tools

import (
	"fmt"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

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
//
// attachments are the current turn's media parts; they are passed to the planner
// (which describes them for routing and stamps them on the plan) so the executor
// can deliver the raw bytes to a media-capable node.
//
// history is the prior conversation (nil for a fresh chat); it is passed to the
// planner so follow-up requests — including a re-plan after an upfront clarifying
// exchange — resolve references against what was already said.
func NewPlanTool(planner *dag.Planner, cache *PlanCache, attachments []*genai.Part, history []dag.HistoryTurn) (tool.Tool, error) {
	return functiontool.New[planArgs, planResult](
		functiontool.Config{
			Name: "plan",
			Description: "Tool to decompose a task into a DAG plan for specialist agents to execute. " +
				"Use when the task is too large, too complex, or requires capabilities you cannot perform directly. " +
				"Do NOT call for tasks you can complete in a single response. " +
				"Returns a plan_id plus the planned DAG (each node's agent, dependencies, and task). " +
				"Review it before executing: if a researcher is overloaded with unrelated topics, shared work is " +
				"duplicated across nodes instead of extracted into an upstream node, or dependencies are wrong, " +
				"call plan again to refine. When the plan looks right, pass plan_id to the execute tool.",
		},
		func(tc agent.ToolContext, a planArgs) (planResult, error) {
			p, err := planner.Plan(tc, history, a.Query, attachments)
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
				yieldFn(stream.DagPlan(p.ID, nodes, planEdges(p.Nodes)))
			}

			return planResult{PlanID: p.ID, Summary: summarizePlan(p)}, nil
		},
	)
}

// planEdges projects each node's DependsOn into the wire edge list. The dag_plan
// event carries edges separately for the frontend, though they are fully derived
// from DependsOn (the single source of truth on the plan itself).
func planEdges(nodes []dag.Node) []stream.DagEdgeDef {
	var edges []stream.DagEdgeDef
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			edges = append(edges, stream.DagEdgeDef{From: dep, To: n.ID})
		}
	}
	return edges
}

// summarizePlan renders the plan for the model to review before executing: each
// node's id, agent, dependencies, AND its full task text, so the model can judge
// the decomposition (Is any researcher overloaded? Is shared work duplicated
// instead of extracted into an upstream node? Are the dependencies right?) and
// re-plan if it isn't good. Informational only — execution uses the cached plan.
func summarizePlan(p *dag.Plan) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Planned DAG (%d node(s)) — review before executing:", len(p.Nodes))
	for _, n := range p.Nodes {
		fmt.Fprintf(&sb, "\n- %s (%s)", n.ID, n.AgentName)
		if len(n.DependsOn) > 0 {
			fmt.Fprintf(&sb, " depends on %s", strings.Join(n.DependsOn, ", "))
		}
		fmt.Fprintf(&sb, "\n    task: %s", strings.TrimSpace(n.Task))
	}
	return sb.String()
}
