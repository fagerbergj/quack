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
	// Setup is the plan's PRE-step, executed before any node runs — see the
	// tool description. Omit only for a plan with no GitHub repo involved.
	Setup *dag.Setup `json:"setup,omitempty" jsonschema:"the working clone + branch to provision before any node runs: {base_ref, work_branch}"`
	// Delivery is the plan's POST-step, executed once after the trust gate
	// passes — see the tool description.
	Delivery *dag.Delivery `json:"delivery,omitempty" jsonschema:"how the gated result reaches GitHub, run after the trust gate: {kind: pull_request|review|comment, title, body}"`
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
	checksDesc := "Checks are currently unavailable (workspace.check_commands is empty) — omit `checks`."
	if cc := planner.CheckCommands(); len(cc) > 0 {
		checksDesc = fmt.Sprintf("`checks` are OPTIONAL — you have NOT seen the repo yet, so do NOT guess its "+
			"commands: the trust gate DERIVES a code node's checks from the repo itself (its own package.json "+
			"scripts / go.mod / Makefile) after the node clones it. Set `checks` (plus `workdir`, the "+
			"workspace-relative repo dir they run in) ONLY when the user named the exact commands to run; each "+
			"must then be exactly, or extend with a space, one of these allowed prefixes: %s.", strings.Join(cc, ", "))
	}
	return functiontool.New[planArgs, planResult](
		functiontool.Config{
			Name: "plan",
			Description: "Tool to run a DAG of specialist agents. Load the plan-work skill first, then YOU author " +
				"the DAG: pass `nodes`, each {id, agent (a name from the Agents list), task (self-contained — the " +
				"agent sees only this text), depends_on: [ids it needs output from]}. Optionally a `rubric`. " +
				checksDesc + " " +
				"Every plan MUST declare setup (the working clone + branch) and delivery (how the gated result " +
				"reaches GitHub). setup and delivery run deterministically AFTER the trust gate — you declare " +
				"intent, you never run git, push, or open a PR yourself. Pass `setup: {base_ref, work_branch}` " +
				"naming the branch the work happens on, and `delivery: {kind, title, body}` where `kind` is " +
				"exactly one of \"pull_request\" (implement-and-deliver requests), \"review\" (PR/diff review " +
				"requests), or \"comment\" (plan-only/research requests that post a summary back). Omit both " +
				"only for a plan with no GitHub repo involved at all. " +
				"Returns a plan_id (pass to execute) plus a summary to review. Do NOT call for tasks you can answer " +
				"directly. If validation fails, fix the nodes and call again.",
		},
		func(tc agent.Context, a planArgs) (planResult, error) {
			p, err := planner.Build(tc, a.Nodes, a.Setup, a.Delivery, history, message, attachments)
			if err != nil {
				return planResult{}, fmt.Errorf("plan: %w", err)
			}

			cache.Put(*p)

			// Emit dag_plan immediately so the frontend knows the plan structure
			// before the execute tool starts running nodes.
			if yieldFn, ok := stream.YieldFromContext(tc); ok {
				yieldFn(DagPlanEvent(*p))
			}

			return planResult{PlanID: p.ID, Summary: summarizePlan(p)}, nil
		},
	)
}

// DagPlanEvent builds the dag_plan SSE event for a plan. Shared by the plan tool
// (fresh turns) and the orchestrator's HITL resume path, which re-emits the plan
// so the client can rebuild the DAG view for the resumed turn.
func DagPlanEvent(p dag.Plan) stream.SSEEvent {
	nodes := make([]stream.DagNodeDef, len(p.Nodes))
	for i, n := range p.Nodes {
		nodes[i] = stream.DagNodeDef{ID: n.ID, Agent: n.AgentName, Task: n.Task, DependsOn: n.DependsOn}
	}
	return stream.DagPlan(p.ID, nodes, planEdges(p.Nodes))
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
// the decomposition (Is any node overloaded? Is shared work duplicated
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
	if p.Setup != nil {
		fmt.Fprintf(&sb, "\nsetup: base_ref=%q work_branch=%q", p.Setup.BaseRef, p.Setup.WorkBranch)
	}
	if p.Delivery != nil {
		fmt.Fprintf(&sb, "\ndelivery: kind=%q title=%q", p.Delivery.Kind, p.Delivery.Title)
	}
	return sb.String()
}
