package tools

import (
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

type planArgs struct {
	Nodes []dag.RawNode `json:"nodes"`
}

type planResult struct {
	PlanID  string `json:"plan_id"` // pass this to the execute tool
	Summary string `json:"summary"` // human-readable node list for the model
}

// NewPlanTool returns the plan tool. YOU (the orchestrator) author the DAG and
// submit it as `nodes`; this tool validates it (known agents, unique ids,
// acyclic, synthesizer hardened), caches it under a plan ID, and emits a dag_plan
// SSE event so the frontend can render the graph before execution. The execute
// tool then runs it by ID — the plan JSON is never copied between calls.
//
// attachments are the current turn's media parts and history the prior turns;
// both are stamped on the plan so every node sees them. message is the verbatim
// user request, stamped so nodes get the full ask (not the orchestrator's
// paraphrase).
func NewPlanTool(planner *dag.Planner, cache *PlanCache, attachments []*genai.Part, history []dag.HistoryTurn, message string) (tool.Tool, error) {
	return functiontool.New[planArgs, planResult](
		functiontool.Config{
			Name: "plan",
			Description: "Tool to run a DAG of specialist agents. Load the plan-work skill first, then YOU author " +
				"the DAG: pass `nodes`, each {id, agent (a name from the Agents list), task (self-contained — the " +
				"agent sees only this text), depends_on: [ids it needs output from]}. Optionally a `rubric`. " +
				"Returns a plan_id (pass to execute) plus a summary to review. Do NOT call for tasks you can answer " +
				"directly. If validation fails, fix the nodes and call again.",
		},
		func(tc agent.Context, a planArgs) (planResult, error) {
			p, err := planner.Build(a.Nodes, history, message, attachments)
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
