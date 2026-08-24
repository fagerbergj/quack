package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	otellog "go.opentelemetry.io/otel/log"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

type planArgs struct {
	Nodes    []dag.RawNode `json:"nodes"`
	Setup    *dag.Setup    `json:"setup,omitempty" jsonschema:"the working clone + branch to provision before any node runs: {repo, base_ref, work_branch}"`
	Delivery *dag.Delivery `json:"delivery,omitempty" jsonschema:"how the gated result reaches GitHub, run after the trust gate: {kind: pull_request|review|comment}"`
}

type planResult struct {
	PlanID  string `json:"plan_id"` // pass this to the execute tool
	Summary string `json:"summary"` // human-readable node list for the model
}

// NewPlanTool: validates and caches a DAG plan, emits dag_plan SSE event.
func NewPlanTool(planner *dag.Planner, cache *PlanCache, attachments []*genai.Part, history []dag.HistoryTurn, message string, githubSetup *dag.Setup, allowedKinds []string, workerAsk string, contextItems []dag.ContextItem, planOnly bool) (tool.Tool, error) {
	checksDesc := "Checks are currently unavailable (workspace.check_commands is empty) - omit `checks`."
	if cc := planner.CheckCommands(); len(cc) > 0 {
		checksDesc = fmt.Sprintf("`checks` are OPTIONAL - you have NOT seen the repo yet, so do NOT guess its "+
			"commands: the trust gate DERIVES a code node's checks from the repo itself (its own package.json "+
			"scripts / go.mod / Makefile) after the node clones it. Set `checks` (plus `workdir`, the "+
			"workspace-relative repo dir they run in) ONLY when the user named the exact commands to run; each "+
			"must then be exactly, or extend with a space, one of these allowed prefixes: %s.", strings.Join(cc, ", "))
	}
	return functiontool.New[planArgs, planResult](
		functiontool.Config{
			Name: "plan",
			Description: "Tool to run a DAG of specialist agents. Load the plan-work skill first, then YOU author " +
				"the DAG: pass `nodes`, each {id, agent (a name from the Agents list), task (self-contained - the " +
				"agent sees only this text), depends_on: [ids it needs output from]}. Optionally a `rubric`. " +
				checksDesc + " " +
				"Every plan MUST declare setup (the working clone + branch) and delivery (how the gated result " +
				"reaches GitHub). setup and delivery run deterministically AFTER the trust gate - you declare " +
				"intent, you never run git, push, or open a PR yourself. Pass `setup: {repo, base_ref, work_branch}` " +
				"naming the clone URL, the base ref, and the branch the work happens on - the harness clones and " +
				"checks it out before any node runs, at the ROOT of each repo-touching node's working directory; " +
				"that node's task says the repo is already there and never instructs cloning it. A node examining " +
				"a DIFFERENT repository (a comparison target) should be told to clone that repo into its own " +
				"working directory itself. And `delivery: {kind}` - exactly one of \"pull_request\" " +
				"(implement-and-deliver requests), \"review\" (PR/diff review requests), or \"comment\" (plan-only/" +
				"research requests that post a summary back); the harness authors and posts the PR/review from the " +
				"node's own work - you never write PR prose. For \"comment\" delivery a node's ANSWER TEXT is the " +
				"artifact posted verbatim - a node must never write its plan/report to a file and point at the path " +
				"instead: nothing commits it, the working directory is discarded when the run ends, and the path is " +
				"then a dangling pointer to nothing. Omit both only for a plan with no GitHub repo involved. " +
				"Returns a plan_id (pass to execute) plus a summary to review. Do NOT call for tasks you can answer " +
				"directly. If validation fails, fix the nodes and call again.",
		},
		func(tc agent.Context, a planArgs) (planResult, error) {
			// ChatID-only Coords: plan judge's chat call files under this chat.
			planCtx := ledger.WithCoords(tc, ledger.Coords{ChatID: tc.SessionID()})
			p, err := planner.Build(planCtx, a.Nodes, a.Setup, a.Delivery, history, message, attachments, allowedKinds)
			if err != nil {
				var rejected *dag.PlanRejectedError
				if errors.As(err, &rejected) {
					cache.RecordRejection(rejected.Reason)
				}
				return planResult{}, fmt.Errorf("plan: %w", err)
			}
			// GitHub already told us repo/base_ref/branch - never trust the planner's guess.
			if githubSetup != nil {
				setup := *githubSetup
				p.Setup = &setup
			}
			// Nodes get the ask-only background and per-node context detail.
			p.WorkerBackground = workerAsk
			p.ContextItems = contextItems
			p.PlanOnly = planOnly
			// The dispatch-declared Setup is the head-ref authority (sdk
			// Setup.ExistingHeadRef → CheckoutExistingHead); the old
			// WithGitHubPR ctx stamp is gone since the extension cutover.
			existingHead := ""
			if githubSetup != nil && githubSetup.CheckoutExistingHead {
				existingHead = githubSetup.WorkBranch
			}
			if err := dag.OverrideExistingPRHead(p, existingHead); err != nil {
				return planResult{}, fmt.Errorf("plan: %w", err)
			}

			cache.Put(*p)

			// Emit dag_plan so the frontend sees the plan before execution starts.
			if yieldFn, ok := stream.YieldFromContext(tc); ok {
				yieldFn(DagPlanEvent(tc, *p))
			}

			emitPlanEvent(tc, p)
			return planResult{PlanID: p.ID, Summary: summarizePlan(p)}, nil
		},
	)
}

// DagPlanEvent: builds the dag_plan SSE event. Shared by plan tool and HITL resume path.
func DagPlanEvent(ctx context.Context, p dag.Plan) stream.SSEEvent {
	nodes := make([]stream.DagNodeDef, len(p.Nodes))
	for i, n := range p.Nodes {
		nodes[i] = stream.DagNodeDef{ID: n.ID, Agent: n.AgentName, Task: n.Task, DependsOn: n.DependsOn, ContextWindow: n.ContextWindow}
	}
	return stream.WithTrace(stream.DagPlan(p.ID, nodes, planEdges(p.Nodes)), otelobs.TraceIDOf(ctx))
}

// planEdges: projects DependsOn into the wire edge list for the dag_plan event.
func planEdges(nodes []dag.Node) []stream.DagEdgeDef {
	var edges []stream.DagEdgeDef
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			edges = append(edges, stream.DagEdgeDef{From: dep, To: n.ID})
		}
	}
	return edges
}

// attachmentMeta is an input attachment's identifying shape, never its bytes -
// an oversized gen_ai.input.messages attribute gets silently dropped or
// truncated, which would lose the whole input field.
type attachmentMeta struct {
	MIMEType string `json:"mime_type,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
}

// summarizeAttachments: strips a plan's attachments down to attachmentMeta,
// index-aligned. Attachments are artifactref reference parts (FileData) in
// practice - Bytes is 0 for those (size lives in the artifact service, not here).
func summarizeAttachments(parts []*genai.Part) []attachmentMeta {
	if len(parts) == 0 {
		return nil
	}
	out := make([]attachmentMeta, len(parts))
	for i, part := range parts {
		switch {
		case part == nil:
			continue
		case part.InlineData != nil:
			out[i] = attachmentMeta{MIMEType: part.InlineData.MIMEType, Bytes: len(part.InlineData.Data)}
		case part.FileData != nil:
			out[i] = attachmentMeta{MIMEType: part.FileData.MIMEType}
		}
	}
	return out
}

// emitPlanEvent: records a gen_ai "plan" ledger event.
func emitPlanEvent(tc agent.Context, p *dag.Plan) {
	if !otelobs.LoggingEnabled("quack.planner") {
		return
	}
	ctx := ledger.WithCoords(tc, ledger.Coords{ChatID: tc.SessionID(), Agent: "orchestrator", Round: "plan"})
	attrs := []otellog.KeyValue{
		otellog.String(otelobs.GenAIOperationName, otelobs.GenAIOperationPlan),
		otellog.String(otelobs.GenAIWorkflowName, p.ID),
	}
	// The planner's actual ask - history/message/attachments Build stamped onto p - not a
	// reconstruction from the plan it produced.
	if b, err := json.Marshal(struct {
		History     []dag.HistoryTurn `json:"history,omitempty"`
		Message     string            `json:"message,omitempty"`
		Attachments []attachmentMeta  `json:"attachments,omitempty"`
	}{p.History, p.UserMessage, summarizeAttachments(p.Attachments)}); err == nil {
		attrs = append(attrs, otellog.String(otelobs.GenAIInputMessages, string(b)))
	}
	if b, err := json.Marshal(p); err == nil {
		attrs = append(attrs, otellog.String(otelobs.GenAIOutputMessages, string(b)))
	}
	otelobs.EmitLog(ctx, "quack.planner", "", attrs...)
}

// summarizePlan: renders the plan for model review before execution.
func summarizePlan(p *dag.Plan) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Planned DAG (%d node(s)) - review before executing:", len(p.Nodes))
	for _, n := range p.Nodes {
		fmt.Fprintf(&sb, "\n- %s (%s)", n.ID, n.AgentName)
		if len(n.DependsOn) > 0 {
			fmt.Fprintf(&sb, " depends on %s", strings.Join(n.DependsOn, ", "))
		}
		fmt.Fprintf(&sb, "\n    task: %s", strings.TrimSpace(n.Task))
	}
	if p.Setup != nil {
		fmt.Fprintf(&sb, "\nsetup: repo=%q base_ref=%q work_branch=%q", p.Setup.Repo, p.Setup.BaseRef, p.Setup.WorkBranch)
	}
	if p.Delivery != nil {
		fmt.Fprintf(&sb, "\ndelivery: kind=%q", p.Delivery.Kind)
	}
	return sb.String()
}
